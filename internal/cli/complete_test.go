package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
)

// seedRunFull writes a run dir like seedRun but lets a test set a specific image_ref and
// network.allow, so history-based completion (§4) has real values to surface.
func seedRunFull(t *testing.T, repo, id, state, imageRef string, allow []string) {
	t.Helper()
	seedRunRecord(t, repo, orchestrator.RunRecord{
		ID:        id,
		State:     state,
		ImageRef:  imageRef,
		StartedAt: "2026-07-01T00:00:00Z",
		Network:   orchestrator.NetworkMeta{Mode: "allowlist", Allow: allow},
	})
}

// seedRunFullAt is seedRunFull with an explicit started_at, so tests can pin newest-first order.
func seedRunFullAt(t *testing.T, repo, id, state, imageRef, startedAt string) {
	t.Helper()
	seedRunRecord(t, repo, orchestrator.RunRecord{
		ID:        id,
		State:     state,
		ImageRef:  imageRef,
		StartedAt: startedAt,
	})
}

func seedRunRecord(t *testing.T, repo string, rec orchestrator.RunRecord) {
	t.Helper()
	sd := filepath.Join(repo, ".krayt")
	runDir := orchestrator.RunDir(sd, rec.ID)
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(runDir, "meta.json"), string(b))
}

// seedQuestionRec writes a run's questions/<qid>.json matching orchestrator.ReadQuestions'
// shape. answerAt marks the question answered when non-empty (an empty value leaves it pending).
func seedQuestionRec(t *testing.T, repo, id, qid, prompt, answerAt string) {
	t.Helper()
	sd := filepath.Join(repo, ".krayt")
	dir := filepath.Join(orchestrator.RunDir(sd, id), "questions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := orchestrator.QuestionRecord{
		ID:       qid,
		Prompt:   prompt,
		AskedAt:  "2026-07-01T00:00:00Z",
		AnswerAt: answerAt,
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, qid+".json"), string(b))
}

// compIDs returns just the choice portion (before any TAB description) of a completion result.
func compIDs(comps []string) []string {
	ids := make([]string, len(comps))
	for i, c := range comps {
		ids[i], _, _ = strings.Cut(c, "\t")
	}
	return ids
}

func TestCompleteRunIDsFiltering(t *testing.T) {
	repo := t.TempDir()
	seedRun(t, repo, "run_done", "done")
	seedRun(t, repo, "run_running", "running")
	seedRun(t, repo, "run_waiting", "waiting")
	seedRun(t, repo, "run_failed", "failed")

	cases := []struct {
		name string
		cmd  *cobra.Command
		want []string
	}{
		{"logs", newLogsCmd(), []string{"run_done", "run_failed", "run_running", "run_waiting"}},
		{"patch", newPatchCmd(), []string{"run_done", "run_failed", "run_running", "run_waiting"}},
		{"apply", newApplyCmd(), []string{"run_done", "run_failed", "run_running", "run_waiting"}},
		{"questions", newQuestionsCmd(), []string{"run_done", "run_failed", "run_running", "run_waiting"}},
		{"stop", newStopCmd(), []string{"run_running", "run_waiting"}},
		{"attach", newAttachCmd(), []string{"run_running", "run_waiting"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cmd.Flags().Set("repo", repo); err != nil {
				t.Fatal(err)
			}
			comps, dir := tc.cmd.ValidArgsFunction(tc.cmd, nil, "")
			if dir != cobra.ShellCompDirectiveNoFileComp {
				t.Errorf("directive = %v, want NoFileComp", dir)
			}
			got := compIDs(comps)
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("completions = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompleteRunIDsDescription(t *testing.T) {
	repo := t.TempDir()
	seedRunFull(t, repo, "run_done", "done", "ghcr.io/example/img:latest", nil)

	cmd := newLogsCmd()
	if err := cmd.Flags().Set("repo", repo); err != nil {
		t.Fatal(err)
	}
	comps, _ := cmd.ValidArgsFunction(cmd, nil, "")
	if len(comps) != 1 {
		t.Fatalf("want 1 completion, got %v", comps)
	}
	if want := "run_done\tdone, ghcr.io/example/img:latest"; comps[0] != want {
		t.Errorf("completion = %q, want %q", comps[0], want)
	}
}

func TestCompleteRunIDsRmForce(t *testing.T) {
	repo := t.TempDir()
	seedRun(t, repo, "run_done", "done")
	seedRun(t, repo, "run_running", "running")

	// Without --force: only terminal runs.
	cmd := newRmCmd()
	if err := cmd.Flags().Set("repo", repo); err != nil {
		t.Fatal(err)
	}
	if got := compIDs(mustComplete(t, cmd)); strings.Join(got, ",") != "run_done" {
		t.Errorf("rm without --force = %v, want [run_done]", got)
	}

	// With --force: all runs.
	cmd = newRmCmd()
	if err := cmd.Flags().Set("repo", repo); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("force", "true"); err != nil {
		t.Fatal(err)
	}
	got := compIDs(mustComplete(t, cmd))
	sort.Strings(got)
	if strings.Join(got, ",") != "run_done,run_running" {
		t.Errorf("rm with --force = %v, want [run_done run_running]", got)
	}
}

func mustComplete(t *testing.T, cmd *cobra.Command) []string {
	t.Helper()
	comps, _ := cmd.ValidArgsFunction(cmd, nil, "")
	return comps
}

func TestCompleteRunIDsSecondArg(t *testing.T) {
	repo := t.TempDir()
	seedRun(t, repo, "run_done", "done")
	for _, mk := range []func() *cobra.Command{newLogsCmd, newPatchCmd, newApplyCmd, newQuestionsCmd, newStopCmd, newAttachCmd, newRmCmd} {
		cmd := mk()
		if err := cmd.Flags().Set("repo", repo); err != nil {
			t.Fatal(err)
		}
		comps, dir := cmd.ValidArgsFunction(cmd, []string{"already-here"}, "")
		if comps != nil || dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("%s second-arg completion = (%v, %v), want (nil, NoFileComp)", cmd.Name(), comps, dir)
		}
	}
}

func TestCompleteRunIDsNoState(t *testing.T) {
	repo := t.TempDir() // no .krayt at all
	cmd := newLogsCmd()
	if err := cmd.Flags().Set("repo", repo); err != nil {
		t.Fatal(err)
	}
	comps, dir := cmd.ValidArgsFunction(cmd, nil, "")
	if comps != nil || dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("no-state completion = (%v, %v), want (nil, NoFileComp)", comps, dir)
	}
}

func TestCompleteQuestionIDs(t *testing.T) {
	repo := t.TempDir()
	seedRun(t, repo, "run_waiting", "waiting")
	seedQuestionRec(t, repo, "run_waiting", "q_answered", "old", "2026-07-01T01:00:00Z")
	// A prompt containing an ANSI escape + a control char that Sanitize strips.
	seedQuestionRec(t, repo, "run_waiting", "q_pending", "pick\x1b[31m one\x07", "")

	cmd := newAnswerCmd()
	if err := cmd.Flags().Set("repo", repo); err != nil {
		t.Fatal(err)
	}
	comps, dir := cmd.ValidArgsFunction(cmd, []string{"run_waiting"}, "")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", dir)
	}
	if len(comps) != 1 {
		t.Fatalf("want 1 pending question completion, got %v", comps)
	}
	id, desc, _ := strings.Cut(comps[0], "\t")
	if id != "q_pending" {
		t.Errorf("question id = %q, want q_pending", id)
	}
	if want := orchestrator.Sanitize("pick\x1b[31m one\x07"); desc != want {
		t.Errorf("description = %q, want sanitized %q", desc, want)
	}
	if strings.ContainsAny(desc, "\x1b\x07") {
		t.Errorf("description %q still contains raw control chars", desc)
	}
}

func TestCompleteAnswerRunID(t *testing.T) {
	repo := t.TempDir()
	seedRun(t, repo, "run_done", "done")
	seedRun(t, repo, "run_waiting", "waiting")

	cmd := newAnswerCmd()
	if err := cmd.Flags().Set("repo", repo); err != nil {
		t.Fatal(err)
	}
	// Position 0 offers only waiting runs (the state answer acts on).
	got := compIDs(mustCompleteArgs(t, cmd, nil))
	if strings.Join(got, ",") != "run_waiting" {
		t.Errorf("answer run-id completion = %v, want [run_waiting]", got)
	}
	// Position 2 (the response) is free text.
	comps, dir := cmd.ValidArgsFunction(cmd, []string{"run_waiting", "q1"}, "")
	if comps != nil || dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("answer response completion = (%v, %v), want (nil, NoFileComp)", comps, dir)
	}
}

func mustCompleteArgs(t *testing.T, cmd *cobra.Command, args []string) []string {
	t.Helper()
	comps, _ := cmd.ValidArgsFunction(cmd, args, "")
	return comps
}

func TestCompleteFixedFlags(t *testing.T) {
	runCmd := newRunCmd()
	cases := []struct {
		flag string
		want []string
	}{
		{"net", []string{"allowlist", "full", "none"}},
		{"on-question", []string{"fail", "wait"}},
		{"on-question-timeout", []string{"sentinel", "abort"}},
		{"agent", []string{"none", "claude-code", "gemini-cli"}},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			fn, ok := runCmd.GetFlagCompletionFunc(tc.flag)
			if !ok {
				t.Fatalf("no registered completion for --%s", tc.flag)
			}
			comps, dir := fn(runCmd, nil, "")
			if dir != cobra.ShellCompDirectiveNoFileComp {
				t.Errorf("directive = %v, want NoFileComp", dir)
			}
			if strings.Join(compIDs(comps), ",") != strings.Join(tc.want, ",") {
				t.Errorf("--%s completions = %v, want %v", tc.flag, comps, tc.want)
			}
		})
	}

	qCmd := newQuestionsCmd()
	fn, ok := qCmd.GetFlagCompletionFunc("sort")
	if !ok {
		t.Fatal("no registered completion for --sort")
	}
	comps, _ := fn(qCmd, nil, "")
	if strings.Join(compIDs(comps), ",") != "asked,pending-first,pending-last" {
		t.Errorf("--sort completions = %v", comps)
	}
}

func TestCompleteImageRef(t *testing.T) {
	repo := t.TempDir()
	// Newest-first is by StartedAt; give distinct stamps so order is deterministic.
	seedRunFullAt(t, repo, "run_old", "done", "img:old", "2026-07-01T00:00:00Z")
	seedRunFullAt(t, repo, "run_new", "done", "img:new", "2026-07-02T00:00:00Z")
	seedRunFullAt(t, repo, "run_dup", "done", "img:new", "2026-07-01T12:00:00Z")

	cmd := newRunCmd()
	if err := cmd.Flags().Set("repo", repo); err != nil {
		t.Fatal(err)
	}
	comps, dir := completeImageRef(cmd, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", dir)
	}
	if strings.Join(comps, ",") != "img:new,img:old" {
		t.Errorf("image completions = %v, want [img:new img:old] (newest-first, deduped)", comps)
	}
}

func TestCompleteAllowDomain(t *testing.T) {
	repo := t.TempDir()
	seedRunFull(t, repo, "run_a", "done", "img:1", []string{"example.internal", "github.com"})

	cmd := newRunCmd()
	if err := cmd.Flags().Set("repo", repo); err != nil {
		t.Fatal(err)
	}
	comps, dir := completeAllowDomain(cmd, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", dir)
	}
	if comps[0] != "example.internal" {
		t.Errorf("history domain should come first, got %v", comps)
	}
	// The well-known seed list is included, and github.com (in both history and seed) appears once.
	if n := countOccurrences(comps, "github.com"); n != 1 {
		t.Errorf("github.com appears %d times, want 1 (deduped across history+seed)", n)
	}
	for _, d := range wellKnownAllowDomains {
		if countOccurrences(comps, d) != 1 {
			t.Errorf("well-known domain %q missing/duplicated in %v", d, comps)
		}
	}
}

func countOccurrences(ss []string, target string) int {
	n := 0
	for _, s := range ss {
		if s == target {
			n++
		}
	}
	return n
}
