package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
)

// newTestRunCmd builds a run command with flags bound to a fresh runFlags, mirroring the
// config-precedence tests' pattern (bind flags onto a real *cobra.Command, then ParseFlags).
func newTestRunCmd(f *runFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "run", RunE: func(*cobra.Command, []string) error { return nil }}
	bindRunFlags(cmd, f)
	return cmd
}

// TestReadTaskPromptStdin covers --task - reading the prompt from cmd.InOrStdin() rather than
// the OS's real stdin, so it's testable without process-level plumbing.
func TestReadTaskPromptStdin(t *testing.T) {
	var f runFlags
	cmd := newTestRunCmd(&f)
	cmd.SetIn(strings.NewReader("fix the flaky test\n"))

	got, err := readTaskPrompt(cmd, stdinTaskArg)
	if err != nil {
		t.Fatalf("readTaskPrompt: %v", err)
	}
	if string(got) != "fix the flaky test\n" {
		t.Errorf("prompt = %q, want the piped stdin bytes", got)
	}
}

// TestReadTaskPromptFile is the regression case: --task <file> is unaffected by the stdin path.
func TestReadTaskPromptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	if err := os.WriteFile(path, []byte("do the thing"), 0o644); err != nil {
		t.Fatal(err)
	}
	var f runFlags
	cmd := newTestRunCmd(&f)

	got, err := readTaskPrompt(cmd, path)
	if err != nil {
		t.Fatalf("readTaskPrompt: %v", err)
	}
	if string(got) != "do the thing" {
		t.Errorf("prompt = %q, want file contents", got)
	}
}

// TestReadTaskPromptSpooledEnvWins proves the detached-child side of the handoff: when
// KRAYT_TASK_FILE is set (the parent already spooled a stdin-read prompt for it), it takes
// precedence over both stdin and any --task file value.
func TestReadTaskPromptSpooledEnvWins(t *testing.T) {
	dir := t.TempDir()
	spool := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(spool, []byte("spooled prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envTaskFile, spool)

	var f runFlags
	cmd := newTestRunCmd(&f)
	cmd.SetIn(strings.NewReader("should be ignored"))

	got, err := readTaskPrompt(cmd, stdinTaskArg)
	if err != nil {
		t.Fatalf("readTaskPrompt: %v", err)
	}
	if string(got) != "spooled prompt" {
		t.Errorf("prompt = %q, want the spooled file's contents", got)
	}
}

// TestSpoolTaskPrompt checks the parent side of the handoff: a stdin-read prompt is written into
// the run dir so the detached child (whose stdin is gone after re-exec) can read it back.
func TestSpoolTaskPrompt(t *testing.T) {
	stateDir := t.TempDir()
	path, err := spoolTaskPrompt(stateDir, "run_abc123", []byte("multi\nline\nprompt"))
	if err != nil {
		t.Fatalf("spoolTaskPrompt: %v", err)
	}
	want := filepath.Join(orchestrator.RunDir(stateDir, "run_abc123"), "prompt.md")
	if path != want {
		t.Errorf("spool path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spooled file: %v", err)
	}
	if string(got) != "multi\nline\nprompt" {
		t.Errorf("spooled contents = %q, want the piped bytes", got)
	}
}

// TestRunRunEmptyPrompt checks that both an empty stdin and an empty task file are rejected with
// a clear error, before any VM/provider setup happens.
func TestRunRunEmptyPrompt(t *testing.T) {
	t.Run("stdin", func(t *testing.T) {
		var f runFlags
		cmd := newTestRunCmd(&f)
		cmd.SetIn(strings.NewReader("   \n\t\n"))
		if err := cmd.ParseFlags([]string{"--image", "img:1", "--task", "-", "--repo", t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		err := runRun(cmd, &f)
		if err == nil || !strings.Contains(err.Error(), "task prompt is empty") {
			t.Fatalf("runRun err = %v, want a task-prompt-is-empty error", err)
		}
	})

	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		empty := filepath.Join(dir, "empty.md")
		if err := os.WriteFile(empty, []byte("  \n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var f runFlags
		cmd := newTestRunCmd(&f)
		if err := cmd.ParseFlags([]string{"--image", "img:1", "--task", empty, "--repo", dir}); err != nil {
			t.Fatal(err)
		}
		err := runRun(cmd, &f)
		if err == nil || !strings.Contains(err.Error(), "task prompt is empty") {
			t.Fatalf("runRun err = %v, want a task-prompt-is-empty error", err)
		}
	})
}
