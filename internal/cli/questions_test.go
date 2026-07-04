package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedQuestions writes question JSON files into a run dir's questions/ dir (reuses seedRun/write
// from manage_test.go).
func seedQuestions(t *testing.T, runDir string, questions map[string]string) {
	t.Helper()
	qdir := filepath.Join(runDir, "questions")
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range questions {
		write(t, filepath.Join(qdir, name), body)
	}
}

// TestQuestionsCmd covers `krayt questions`: pending questions show the answer hint + choices,
// answered ones show the response, and an agent prompt with a terminal escape is sanitized (§6.13).
func TestQuestionsCmd(t *testing.T) {
	repo := t.TempDir()
	runDir := seedRun(t, repo, "run_q", "waiting")
	seedQuestions(t, runDir, map[string]string{
		"q1.json": `{"id":"q1","prompt":"Which DB?","choices":["postgres","sqlite"],"asked_at":"2026-07-04T21:06:41Z"}`,
		"q2.json": `{"id":"q2","prompt":"Deploy?","asked_at":"2026-07-04T21:00:00Z","response":"staging","answered_at":"2026-07-04T21:01:00Z"}`,
		// prompt carries a raw ESC that must not survive to the terminal.
		"q3.json": "{\"id\":\"q3\",\"prompt\":\"evil\\u001b[2Jwipe\",\"asked_at\":\"2026-07-04T21:07:00Z\"}",
	})

	out := run(t, newQuestionsCmd(), "--repo", repo, "run_q")

	for _, want := range []string{
		"q1", "[pending]", "postgres | sqlite",
		"krayt answer run_q q1 <response>",
		"answered by human", "→ staging",
		"2 pending", // q1 and q3 are unanswered; q2 is answered
	} {
		if !strings.Contains(out, want) {
			t.Errorf("questions output missing %q:\n%s", want, out)
		}
	}
	if strings.IndexByte(out, 0x1b) >= 0 {
		t.Errorf("output contains a raw ESC byte (unsanitized agent prompt):\n%q", out)
	}
}

// TestQuestionsFlags covers --pending-only (filters answered out) and --sort (reorders by
// pending status while keeping chronological order within groups); an unknown --sort errors.
func TestQuestionsFlags(t *testing.T) {
	repo := t.TempDir()
	runDir := seedRun(t, repo, "run_q", "waiting")
	seedQuestions(t, runDir, map[string]string{
		// q2 asked earliest and answered; q1 and q3 asked later and pending.
		"q1.json": `{"id":"q1","prompt":"first pending?","asked_at":"2026-07-04T21:06:41Z"}`,
		"q2.json": `{"id":"q2","prompt":"Deploy?","asked_at":"2026-07-04T21:00:00Z","response":"staging","answered_at":"2026-07-04T21:01:00Z"}`,
		"q3.json": `{"id":"q3","prompt":"second pending?","asked_at":"2026-07-04T21:07:00Z"}`,
	})

	// --pending-only drops the answered q2.
	pend := run(t, newQuestionsCmd(), "--repo", repo, "--pending-only", "run_q")
	if strings.Contains(pend, "answered by human") || strings.Contains(pend, "staging") {
		t.Errorf("--pending-only should hide answered questions:\n%s", pend)
	}
	if !strings.Contains(pend, "q1") || !strings.Contains(pend, "q3") {
		t.Errorf("--pending-only should still show pending questions:\n%s", pend)
	}

	// default (asked, oldest-first): q2 answered first → answered marker precedes pending.
	def := run(t, newQuestionsCmd(), "--repo", repo, "run_q")
	if strings.Index(def, "[answered by human]") > strings.Index(def, "[pending]") {
		t.Errorf("default sort should be chronological (answered q2 first):\n%s", def)
	}

	// --sort pending-first flips it: a pending marker precedes the answered one.
	pf := run(t, newQuestionsCmd(), "--repo", repo, "--sort", "pending-first", "run_q")
	if strings.Index(pf, "[pending]") > strings.Index(pf, "[answered by human]") {
		t.Errorf("--sort pending-first should list pending before answered:\n%s", pf)
	}

	// unknown --sort errors.
	cmd := newQuestionsCmd()
	cmd.SetArgs([]string{"--repo", repo, "--sort", "bogus", "run_q"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	if err := cmd.Execute(); err == nil {
		t.Error("--sort bogus should error")
	}
}

// TestLsPendingHint: a `waiting` run's `ls` row shows the pending-question count, nudging toward
// `krayt questions` (§6.13).
func TestLsPendingHint(t *testing.T) {
	repo := t.TempDir()
	runDir := seedRun(t, repo, "run_w", "waiting")
	seedQuestions(t, runDir, map[string]string{
		"q1.json": `{"id":"q1","prompt":"Q?","asked_at":"2026-07-04T21:06:41Z"}`,
	})

	out := run(t, newLsCmd(), "--repo", repo)
	if !strings.Contains(out, "waiting (1?)") {
		t.Errorf("ls should hint the pending-question count for a waiting run; got:\n%s", out)
	}
}
