package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func seedQuestion(t *testing.T, runDir, id, askedAt string) {
	t.Helper()
	dir := filepath.Join(runDir, "questions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"` + id + `","prompt":"proceed?","asked_at":"` + askedAt + `"}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveAnswerArgs(t *testing.T) {
	runDir := t.TempDir()
	seedQuestion(t, runDir, "q1", "2026-07-02T00:00:00Z")
	seedQuestion(t, runDir, "q2", "2026-07-02T00:01:00Z") // newest

	// explicit qid + response
	if qid, resp, err := resolveAnswerArgs(runDir, []string{"q1", "yes"}, false); qid != "q1" || resp != "yes" || err != nil {
		t.Errorf("explicit form = (%q,%q,%v)", qid, resp, err)
	}
	// response only -> newest question
	if qid, resp, err := resolveAnswerArgs(runDir, []string{"go"}, false); qid != "q2" || resp != "go" || err != nil {
		t.Errorf("response-only form = (%q,%q,%v)", qid, resp, err)
	}
	// --no-answer, no positional -> newest question, empty response
	if qid, resp, err := resolveAnswerArgs(runDir, nil, true); qid != "q2" || resp != "" || err != nil {
		t.Errorf("no-answer form = (%q,%q,%v)", qid, resp, err)
	}
	// missing response without --no-answer -> error
	if _, _, err := resolveAnswerArgs(runDir, nil, false); err == nil {
		t.Error("expected an error when no response and not --no-answer")
	}
}

func TestNewestQuestionIDEmpty(t *testing.T) {
	if _, err := newestQuestionID(t.TempDir()); err == nil {
		t.Error("expected an error when there are no recorded questions")
	}
}
