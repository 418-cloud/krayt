package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// reportName is the human-readable run summary the host writes on completion (§8.4).
const reportName = "report.md"

// writeReport renders the fixed-section human summary to runDir/report.md (§8.4). The host
// always owns the Run/Changes sections (derived from the machine record); the agent's own
// notes — anything it wrote to /output/report.md, collected into the run dir — are surfaced
// verbatim (sanitized) under Notes. Called from the run finalizer, so every run, success or
// failure, leaves a report.
func writeReport(runDir string, rec RunRecord, agentNotes string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Run %s\n", rec.ID)
	fmt.Fprintf(&b, "- Image: %s   Task: %s\n", rec.ImageRef, rec.TaskSummary)
	fmt.Fprintf(&b, "- Result: %s   Exit: %d   Duration: %s\n", resultWord(rec), rec.ExitCode, hms(rec.DurationSecs))
	fmt.Fprintf(&b, "- Network: %s\n", networkLine(rec.Network))
	if rec.Error != "" {
		fmt.Fprintf(&b, "- Error: %s\n", sanitize(rec.Error))
	}

	b.WriteString("\n## Changes\n")
	if rec.Patch != nil {
		noun := "files"
		if rec.Patch.FilesChanged == 1 {
			noun = "file"
		}
		fmt.Fprintf(&b, "%d %s, +%d/-%d. See changes.patch.\n",
			rec.Patch.FilesChanged, noun, rec.Patch.Insertions, rec.Patch.Deletions)
	} else {
		b.WriteString("No changes.patch was produced.\n")
	}

	if len(rec.Safety) > 0 {
		b.WriteString("\n## Safety\n")
		b.WriteString("The patch touches paths that can execute outside the workspace — review carefully:\n")
		for _, s := range rec.Safety {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}

	if len(rec.Questions) > 0 {
		b.WriteString("\n## Questions\n")
		for _, q := range rec.Questions {
			answer := q.Answer
			if q.AnsweredBy == "timeout" {
				answer = "(no answer — timed out)"
			} else if answer == "" {
				answer = "(unanswered)"
			}
			fmt.Fprintf(&b, "- %s → %s\n", q.Prompt, answer)
		}
	}

	b.WriteString("\n## Notes\n")
	if n := strings.TrimSpace(sanitize(agentNotes)); n != "" {
		b.WriteString(n)
		b.WriteString("\n")
	} else {
		b.WriteString("(none)\n")
	}

	return os.WriteFile(filepath.Join(runDir, reportName), []byte(b.String()), 0o644)
}

// agentNotes reads any agent-written report.md the guest collected into the run dir, so the
// host can fold it into the Notes section before overwriting the file with the canonical
// report. Absent/unreadable → empty (a normal run where the image wrote no report).
func agentNotes(runDir string) string {
	b, err := os.ReadFile(filepath.Join(runDir, reportName))
	if err != nil {
		return ""
	}
	return string(b)
}

// summarizeTask is the meta.json task_summary: the first 200 characters of the task prompt,
// sanitized and single-lined (§8.4).
func summarizeTask(prompt []byte) string {
	s := sanitize(strings.Join(strings.Fields(string(prompt)), " "))
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}

// summarizeQuestions turns the persisted Q&A files into meta.json/report.md summaries (§6.13,
// §8.4), sanitizing agent-originated prompt/answer text and deriving who answered and how long
// the run waited.
func summarizeQuestions(runDir string) []QuestionMeta {
	qs, err := ReadQuestions(runDir)
	if err != nil || len(qs) == 0 {
		return nil
	}
	out := make([]QuestionMeta, 0, len(qs))
	for _, q := range qs {
		m := QuestionMeta{ID: q.ID, Prompt: sanitize(q.Prompt)}
		if q.AnswerAt != "" {
			m.WaitedSecs = durationSecs(q.AskedAt, q.AnswerAt)
			if q.NoAnswer {
				m.AnsweredBy = "timeout"
			} else {
				m.Answer, m.AnsweredBy = sanitize(q.Response), "human"
			}
		}
		out = append(out, m)
	}
	return out
}

// resultWord maps a finalized record to the report's success|failed|timed out word (§8.4).
func resultWord(rec RunRecord) string {
	switch {
	case rec.TimedOut:
		return "timed out"
	case rec.State == StateDone && rec.ExitCode == 0:
		return "success"
	default:
		return "failed"
	}
}

// hms renders a duration in seconds as e.g. "7m42s" (§8.4).
func hms(secs int) string {
	if secs <= 0 {
		return "0s"
	}
	return (time.Duration(secs) * time.Second).String()
}

// networkLine renders the egress policy for the report, e.g. "allowlist (api.anthropic.com)".
func networkLine(n NetworkMeta) string {
	mode := n.Mode
	if mode == "" {
		mode = "allowlist"
	}
	if len(n.Allow) == 0 {
		return mode
	}
	return fmt.Sprintf("%s (%s)", mode, strings.Join(n.Allow, ", "))
}

// durationSecs returns the whole seconds between two RFC3339 stamps, or 0 if either is missing
// or unparseable (both come from nowStamp, so parse failures are not expected).
func durationSecs(start, end string) int {
	t0, err0 := time.Parse(time.RFC3339, start)
	t1, err1 := time.Parse(time.RFC3339, end)
	if err0 != nil || err1 != nil {
		return 0
	}
	if d := int(t1.Sub(t0).Seconds()); d > 0 {
		return d
	}
	return 0
}

// ansiEscape matches terminal escape sequences (CSI/OSC and a bare ESC) so agent-originated
// text can't inject color/cursor control into the host's terminal when a report is catted or
// a question is displayed (§6.13 "sanitize on display").
var ansiEscape = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-_][0-?]*[ -/]*[@-~]|\x1b`)

// sanitize strips terminal escape sequences and other C0 control characters (keeping tab and
// newline) from untrusted agent text before it lands in a review artifact (§6.13).
func sanitize(s string) string {
	s = ansiEscape.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
