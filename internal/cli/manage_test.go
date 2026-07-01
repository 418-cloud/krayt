package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
)

// seedRun writes a minimal run dir (meta.json + agent.log + changes.patch) under repo/.krayt,
// matching what a real run persists, so the management commands have something to read.
func seedRun(t *testing.T, repo, id, state string) string {
	t.Helper()
	sd := filepath.Join(repo, ".krayt")
	runDir := orchestrator.RunDir(sd, id)
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"` + id + `","state":"` + state + `","exit_code":0,"image_ref":"img:1","started_at":"2026-07-01T00:00:00Z","pid":0}`
	write(t, filepath.Join(runDir, "meta.json"), meta)
	write(t, orchestrator.LogPath(runDir), "hello from the agent\n")
	write(t, filepath.Join(runDir, "changes.patch"), "diff --git a/x b/x\n")
	return runDir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run executes a cobra command with args, capturing stdout.
func run(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("command failed: %v", err)
	}
	return buf.String()
}

func TestManageCommands(t *testing.T) {
	repo := t.TempDir()
	seedRun(t, repo, "run_done", "done")
	seedRun(t, repo, "run_running", "running")

	// ls lists both with their state.
	out := run(t, newLsCmd(), "--repo", repo)
	for _, want := range []string{"run_done", "run_running", "done", "running"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output missing %q; got:\n%s", want, out)
		}
	}

	// logs prints the persisted log.
	if got := run(t, newLogsCmd(), "--repo", repo, "run_done"); !strings.Contains(got, "hello from the agent") {
		t.Errorf("logs output = %q", got)
	}

	// patch prints the patch path.
	if got := run(t, newPatchCmd(), "--repo", repo, "run_done"); !strings.Contains(got, "changes.patch") {
		t.Errorf("patch output = %q", got)
	}

	// rm refuses a non-terminal run without --force, then removes a finished one.
	if err := execErr(newRmCmd(), "--repo", repo, "run_running"); err == nil {
		t.Error("rm of a running run without --force should error")
	}
	_ = run(t, newRmCmd(), "--repo", repo, "run_done")
	if _, err := os.Stat(orchestrator.RunDir(filepath.Join(repo, ".krayt"), "run_done")); !os.IsNotExist(err) {
		t.Errorf("run_done dir should be gone after rm: %v", err)
	}
}

// execErr runs a command and returns its error (for the negative case).
func execErr(cmd *cobra.Command, args ...string) error {
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	return cmd.Execute()
}
