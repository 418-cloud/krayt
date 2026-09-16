package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	seedRunWithSandboxName(t, repo, "run_tracked", "done", "krayt-run_tracked")

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
	seedRunWithSandboxName(t, repo, "run_tracked", "done", "krayt-run_tracked")

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
