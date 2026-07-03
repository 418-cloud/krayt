package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/418-cloud/krayt/internal/protocol/pb"
)

// QuestionRecord is a persisted agent → human Q&A pair at
// `.krayt/runs/<id>/questions/<qid>.json` (§6.13), so the patch review shows what the agent
// asked and what it was told. The prompt is sanitized on display (it comes from untrusted
// agent code), not here.
type QuestionRecord struct {
	ID       string   `json:"id"`
	Prompt   string   `json:"prompt"`
	Choices  []string `json:"choices,omitempty"`
	AskedAt  string   `json:"asked_at"`
	Response string   `json:"response,omitempty"`
	NoAnswer bool     `json:"no_answer,omitempty"`
	AnswerAt string   `json:"answered_at,omitempty"`
}

func questionsDir(runDir string) string { return filepath.Join(runDir, "questions") }

// writeQuestion records a newly-asked question (before it is answered).
func writeQuestion(runDir string, q *pb.Question) error {
	rec := QuestionRecord{
		ID:      q.GetId(),
		Prompt:  q.GetPrompt(),
		Choices: q.GetChoices(),
		AskedAt: nowStamp(),
	}
	return writeQuestionRecord(runDir, rec)
}

func writeQuestionRecord(runDir string, rec QuestionRecord) error {
	dir := questionsDir(runDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("orchestrator: create questions dir: %w", err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("orchestrator: marshal question: %w", err)
	}
	// Write-temp-then-rename so a concurrent cross-process reader (`krayt answer` scanning for
	// the newest question) never observes a half-written file — matching writeRecord (§6.13).
	path := filepath.Join(dir, rec.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("orchestrator: write question: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("orchestrator: commit question: %w", err)
	}
	return nil
}

// RecordAnswer completes a persisted question with the answer it received, so the on-disk Q&A
// history shows both what the agent asked and what it was told (§6.13). Every answer-delivery
// path (`krayt answer`, the in-process answerer, the timeout sentinel) calls it. It is a no-op
// if the question was never persisted (e.g. a fail-mode sentinel that never blocked).
func RecordAnswer(runDir, questionID, response string, noAnswer bool) error {
	b, err := os.ReadFile(filepath.Join(questionsDir(runDir), questionID+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var rec QuestionRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return fmt.Errorf("orchestrator: parse question %s: %w", questionID, err)
	}
	rec.Response, rec.NoAnswer, rec.AnswerAt = response, noAnswer, nowStamp()
	return writeQuestionRecord(runDir, rec)
}

// ReadQuestions returns a run's persisted Q&A pairs, oldest first.
func ReadQuestions(runDir string) ([]QuestionRecord, error) {
	entries, err := os.ReadDir(questionsDir(runDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []QuestionRecord
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(questionsDir(runDir), e.Name()))
		if err != nil {
			continue
		}
		var rec QuestionRecord
		if json.Unmarshal(b, &rec) == nil {
			recs = append(recs, rec)
		}
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].AskedAt < recs[j].AskedAt })
	return recs, nil
}
