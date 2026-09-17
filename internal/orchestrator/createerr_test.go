package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/task"
)

// msbStartFailure is msb 0.6.16's own create failure for a sandbox that never started, including
// its `msb logs` hint (run_985a6aae).
func msbStartFailure(name string) string {
	return "error: failed to start \"" + name + "\"\n" +
		"  → other: sandbox process exited (exit status: 0) before agent relay became available\n" +
		"  → run `msb logs --source system " + name + "` for full diagnostics"
}

// TestCreateFailurePointsAtSavedConsoleLog: once teardown has removed the sandbox, msb's `msb
// logs` hint finds nothing, so a create failure whose diagnostics were saved names console.log
// instead — in the returned error and in meta.json alike, for Run and Shell.
func TestCreateFailurePointsAtSavedConsoleLog(t *testing.T) {
	for _, mode := range []string{"run", "shell"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			const id = "run_createfail"
			home := t.TempDir()
			sb := newFakeSandbox(t, home, fakeMsbScript{
				CreateExitCode: 1,
				CreateStderr:   msbStartFailure("krayt-" + id),
			})
			runDir := filepath.Join(t.TempDir(), "run")
			repo := newRepo(t, map[string]string{"a.txt": "1\n"})
			var err error
			if mode == "run" {
				_, err = orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, task.RunSpec{
					ID: id, ImageRef: "img", RepoPath: repo, BundleDepth: 1,
					TaskPrompt: []byte("t"), Network: allowlistAll,
				}, runDir)
			} else {
				_, err = orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec(id, repo), runDir, false, nil)
			}
			if err == nil {
				t.Fatal("expected the create failure")
			}
			logPath := orchestrator.ConsoleLogPath(runDir)
			if _, serr := os.Stat(logPath); serr != nil {
				t.Fatalf("console.log not written: %v", serr)
			}
			want := "full diagnostics saved to " + logPath
			if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "msb logs --source system") {
				t.Errorf("err = %q, want msb's hint replaced by %q", err, want)
			}
			rec, rerr := orchestrator.ReadRecord(runDir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if rec.Error != err.Error() {
				t.Errorf("meta.json error = %q, want the same message as returned", rec.Error)
			}
		})
	}
}

// TestCreateFailureKeepsMsbHintWithoutConsoleLog: if no diagnostics could be saved, the message
// stays exactly as msb wrote it.
func TestCreateFailureKeepsMsbHintWithoutConsoleLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const id = "run_createfail_nolog"
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{
		CreateExitCode: 1,
		CreateStderr:   msbStartFailure("krayt-" + id),
		NoSystemLogs:   true,
	})
	runDir := filepath.Join(t.TempDir(), "run")
	_, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec(id, newRepo(t, map[string]string{"a.txt": "1\n"})), runDir, false, nil)
	if err == nil {
		t.Fatal("expected the create failure")
	}
	if !strings.Contains(err.Error(), "run `msb logs --source system krayt-"+id+"` for full diagnostics") ||
		strings.Contains(err.Error(), "full diagnostics saved to") {
		t.Errorf("err = %q, want msb's own hint unchanged", err)
	}
	if _, serr := os.Stat(orchestrator.ConsoleLogPath(runDir)); !os.IsNotExist(serr) {
		t.Errorf("console.log exists (%v), want none", serr)
	}
}
