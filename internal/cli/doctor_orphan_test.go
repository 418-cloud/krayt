package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
)

// seedRunWithSandboxName writes a minimal run dir like seedRun, but also records SandboxName —
// what orphanSandboxCheck cross-references `msb ls` against.
func seedRunWithSandboxName(t *testing.T, repo, id, state, sandboxName string) {
	t.Helper()
	runDir := filepath.Join(repo, ".krayt", "runs", id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"` + id + `","state":"` + state + `","exit_code":0,"image_ref":"img:1",` +
		`"started_at":"2026-07-01T00:00:00Z","pid":0,"sandbox_name":"` + sandboxName + `"}`
	write(t, filepath.Join(runDir, "meta.json"), meta)
}

func TestOrphanSandboxCheckReportsUntrackedKraytSandbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sandbox.BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
		"ls --format json": {ExitCode: 0, Stdout: `[` +
			`{"name":"krayt-run_tracked"},` +
			`{"name":"krayt-run_orphaned"},` +
			`{"name":"some-other-tools-sandbox"}` +
			`]`},
	}})

	repo := t.TempDir()
	seedRunWithSandboxName(t, repo, "run_tracked", orchestrator.StateKept, "krayt-run_tracked")

	res := orphanSandboxCheck(context.Background(), repo)
	if res.ok {
		t.Fatalf("orphanSandboxCheck.ok = true, want false (one orphan present): %+v", res)
	}
	if !res.optional {
		t.Error("orphanSandboxCheck must be optional (a warning, never a [FAIL] — decision 6)")
	}
	if !strings.Contains(res.detail, "krayt-run_orphaned") {
		t.Errorf("detail = %q, want it to name the orphaned sandbox", res.detail)
	}
	if strings.Contains(res.detail, "krayt-run_tracked") {
		t.Errorf("detail = %q, must not name the tracked sandbox", res.detail)
	}
	if strings.Contains(res.detail, "some-other-tools-sandbox") {
		t.Errorf("detail = %q, must not name a non-krayt-prefixed sandbox", res.detail)
	}
	if !strings.Contains(res.detail, "msb stop krayt-run_orphaned") {
		t.Errorf("detail = %q, want it to name the command to stop the orphan", res.detail)
	}
}

func TestOrphanSandboxCheckSilentWhenNoOrphans(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sandbox.BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
		"ls --format json": {ExitCode: 0, Stdout: `[{"name":"krayt-run_tracked"}]`},
	}})

	repo := t.TempDir()
	seedRunWithSandboxName(t, repo, "run_tracked", orchestrator.StateKept, "krayt-run_tracked")

	res := orphanSandboxCheck(context.Background(), repo)
	if !res.ok {
		t.Errorf("orphanSandboxCheck = %+v, want ok=true (every krayt-* sandbox is tracked)", res)
	}
}

// TestOrphanSandboxCheckDegradesWithoutRepo proves the check reports nothing — never fails the
// command — when there's no repo (or no .krayt/) to compare against, since it's the first doctor
// check that needs any repo state at all.
func TestOrphanSandboxCheckDegradesWithoutRepo(t *testing.T) {
	res := orphanSandboxCheck(context.Background(), "")
	if !res.optional {
		t.Error("orphanSandboxCheck must be optional even when skipped")
	}
	if res.ok {
		t.Error("a skipped check should not report ok=true — that would read as a real 'no orphans' verification")
	}

	repoWithNoKrayt := t.TempDir()
	pinMissingMsb(t, t.TempDir()) // isolate from whatever msb (if any) is actually on this machine's PATH
	res2 := orphanSandboxCheck(context.Background(), repoWithNoKrayt)
	if res2.ok {
		t.Error("a repo with no .krayt/runs/ and no reachable msb should degrade, not claim a verified 'ok'")
	}
}

// stubProcessAlive makes every recorded krayt pid read as alive (or dead) for one test.
func stubProcessAlive(t *testing.T, alive bool) {
	t.Helper()
	orig := processAlive
	processAlive = func(int) bool { return alive }
	t.Cleanup(func() { processAlive = orig })
}

// TestOrphanSandboxCheckLiveRecordsOnly proves decision 6's "no LIVE run record": a sandbox is
// owned only by a kept shell session or by a run whose krayt process is still alive. A finished
// run's sandbox (teardown failed) and a dead process's sandbox (kill -9, a crash) are orphans even
// though a record exists, and the dead run's hint is `krayt stop`, which also fixes its record.
func TestOrphanSandboxCheckLiveRecordsOnly(t *testing.T) {
	for _, tc := range []struct {
		name, state  string
		alive        bool
		wantReported bool
		wantHint     string
	}{
		{name: "kept", state: orchestrator.StateKept, wantReported: false},
		{name: "running, process alive", state: orchestrator.StateRunning, alive: true, wantReported: false},
		{name: "running, process dead", state: orchestrator.StateRunning, wantReported: true,
			wantHint: "krayt stop --repo "},
		{name: "starting, process dead", state: orchestrator.StateStarting, wantReported: true,
			wantHint: "krayt stop --repo "},
		{name: "done", state: orchestrator.StateDone, alive: true, wantReported: true,
			wantHint: "msb stop krayt-run_x && msb rm krayt-run_x"},
		{name: "failed", state: orchestrator.StateFailed, wantReported: true,
			wantHint: "msb stop krayt-run_x && msb rm krayt-run_x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(sandbox.BinEnv, testBinPath)
			writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
				"ls --format json": {ExitCode: 0, Stdout: `[{"name":"krayt-run_x"}]`},
			}})
			stubProcessAlive(t, tc.alive)

			repo := t.TempDir()
			seedRunWithSandboxName(t, repo, "run_x", tc.state, "krayt-run_x")

			res := orphanSandboxCheck(context.Background(), repo)
			if reported := !res.ok; reported != tc.wantReported {
				t.Fatalf("reported = %v, want %v (%+v)", reported, tc.wantReported, res)
			}
			if tc.wantHint != "" && !strings.Contains(res.detail, tc.wantHint) {
				t.Errorf("detail = %q, want it to contain %q", res.detail, tc.wantHint)
			}
			if strings.Contains(tc.wantHint, "krayt stop") && !strings.Contains(res.detail, repo+" run_x") {
				t.Errorf("detail = %q, want the krayt stop hint to name the repo and run id", res.detail)
			}
		})
	}
}
