package cli

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
)

// TestPatchLiveShellRoutesLiveSessionsThroughPatchLiveShell checks the branch decision 9 adds:
// `krayt patch` on a live (running or kept) `kind: shell` record must attempt the live re-derive
// path, not the plain stat — distinguished here by the error it produces, since there's no real
// sandbox to derive a patch from in this unit test (pinMissingMsb makes the attempt fail fast,
// deterministically, rather than skip when a real msb happens to be on the test machine's PATH).
func TestPatchLiveShellRoutesLiveSessionsThroughPatchLiveShell(t *testing.T) {
	for _, state := range []string{orchestrator.StateRunning, orchestrator.StateKept} {
		t.Run(state, func(t *testing.T) {
			repo := t.TempDir()
			pinMissingMsb(t, t.TempDir())
			seedShellRun(t, repo, "run_live", state)

			err := execErr(newPatchCmd(), "--repo", repo, "run_live")
			if err == nil {
				t.Fatal("expected an error (no real sandbox to derive a patch from)")
			}
			if !strings.Contains(err.Error(), "patch run") {
				t.Errorf("err = %v, want it to come from the live-shell path (patchLiveShell's own wrap)", err)
			}
			if strings.Contains(err.Error(), "no patch for run") {
				t.Errorf("err = %v, must NOT be the plain-stat path's error for a live shell session", err)
			}
		})
	}
}

// TestPatchPlainStatForFinishedShellSession checks the companion case: a `kind: shell` session
// that already ended without --keep (state `done`) falls through to the ordinary stat path,
// unchanged from before this task.
func TestPatchPlainStatForFinishedShellSession(t *testing.T) {
	repo := t.TempDir()
	seedShellRun(t, repo, "run_finished", orchestrator.StateDone)
	// seedShellRun writes no changes.patch file, so the plain-stat path must fail with the
	// existing "no patch for run" error, not attempt a live re-derive.
	err := execErr(newPatchCmd(), "--repo", repo, "run_finished")
	if err == nil || !strings.Contains(err.Error(), "no patch for run") {
		t.Fatalf("err = %v, want the plain-stat 'no patch for run' error", err)
	}
}

// TestStopKeptShellDestroysSandboxDirectly checks decision 3's escape hatch: a `kept` shell
// session has no live supervisor to signal (Shell clears PID on the way to `kept`), so `krayt
// stop` must stop+remove the sandbox by name directly instead, and flip the record to `done`.
func TestStopKeptShellDestroysSandboxDirectly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sandbox.BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Default: fakeResponse{ExitCode: 0}})

	repo := t.TempDir()
	runDir := seedShellRun(t, repo, "run_kept", orchestrator.StateKept)

	out := run(t, newStopCmd(), "--repo", repo, "run_kept")
	if !strings.Contains(out, "run_kept") {
		t.Errorf("stop output = %q, want it to name the run", out)
	}

	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if rec.State != orchestrator.StateDone {
		t.Errorf("State after stop = %q, want %q (the sandbox that made it 'kept' is gone now)", rec.State, orchestrator.StateDone)
	}

	var sawStop, sawRm bool
	for _, c := range readFakeCalls(t, home) {
		if len(c.Args) == 0 {
			continue
		}
		if c.Args[0] == "stop" {
			sawStop = true
		}
		if c.Args[0] == "rm" {
			sawRm = true
		}
	}
	if !sawStop || !sawRm {
		t.Errorf("sawStop=%v sawRm=%v, want both true", sawStop, sawRm)
	}
}

// TestStopRunningRunStillSignalsSupervisor is the regression check: an ordinary live run (or a
// shell session still mid-session, PID belonging to a real process) must keep using the existing
// PID-signal path, not the kept-shell or dead-run branches.
func TestStopRunningRunStillSignalsSupervisor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("killSupervisor hard-terminates on Windows; the signal path is unix-only")
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	t.Cleanup(func() { _ = child.Process.Kill() })

	repo := t.TempDir()
	runDir := seedShellRun(t, repo, "run_live", orchestrator.StateRunning)
	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatal(err)
	}
	rec.PID = child.Process.Pid
	if err := orchestrator.WriteRecord(runDir, rec); err != nil {
		t.Fatal(err)
	}

	out := run(t, newStopCmd(), "--repo", repo, "run_live")
	if !strings.Contains(out, "stopping run_live") {
		t.Errorf("stop output = %q, want the signal path's message", out)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor process was not signalled")
	}
}

// TestStopDeadRunCleansUpSandbox covers a run whose krayt process died without tearing down:
// `krayt stop` removes the sandbox by name (if msb still lists it) and marks the record failed,
// instead of trying to signal a pid that no longer exists.
func TestStopDeadRunCleansUpSandbox(t *testing.T) {
	for _, tc := range []struct {
		name        string
		listed      bool
		wantOutput  string
		wantRemoved bool
	}{
		{name: "sandbox still listed", listed: true, wantOutput: "removed sandbox krayt-run_dead", wantRemoved: true},
		{name: "sandbox already gone", listed: false, wantOutput: "no sandbox was left to remove"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(sandbox.BinEnv, testBinPath)
			list := `[]`
			if tc.listed {
				list = `[{"name":"krayt-run_dead"}]`
			}
			writeFakeScript(t, home, fakeScript{
				Responses: map[string]fakeResponse{"ls --format json": {Stdout: list}},
				Default:   fakeResponse{ExitCode: 0},
			})
			stubProcessAlive(t, false)

			repo := t.TempDir()
			runDir := seedShellRun(t, repo, "run_dead", orchestrator.StateRunning)
			rec, err := orchestrator.ReadRecord(runDir)
			if err != nil {
				t.Fatal(err)
			}
			rec.PID = 81003
			if err := orchestrator.WriteRecord(runDir, rec); err != nil {
				t.Fatal(err)
			}

			out := run(t, newStopCmd(), "--repo", repo, "run_dead")
			if !strings.Contains(out, "pid 81003") || !strings.Contains(out, tc.wantOutput) {
				t.Errorf("stop output = %q, want the pid and %q", out, tc.wantOutput)
			}

			var sawStop, sawRm bool
			for _, c := range readFakeCalls(t, home) {
				switch c.Args[0] {
				case "stop":
					sawStop = true
				case "rm":
					sawRm = true
				}
			}
			if sawStop != tc.wantRemoved || sawRm != tc.wantRemoved {
				t.Errorf("sawStop=%v sawRm=%v, want both %v", sawStop, sawRm, tc.wantRemoved)
			}

			got, err := orchestrator.ReadRecord(runDir)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != orchestrator.StateFailed || got.PID != 0 || !strings.Contains(got.Error, "pid 81003") {
				t.Errorf("record state=%q pid=%d error=%q, want failed, pid 0, and an error naming the pid", got.State, got.PID, got.Error)
			}
		})
	}
}
