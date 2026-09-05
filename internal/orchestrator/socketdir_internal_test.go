package orchestrator

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// bindProbe binds a unix socket of exactly n bytes under dir and reports whether the kernel
// accepted it. It skips the whole test when this environment forbids binding unix sockets at all
// (a sandboxed CI runner), which is a different answer from "the path was too long".
func bindProbe(t *testing.T, dir string, n int) bool {
	t.Helper()
	const suffix = ".sock"
	pad := n - len(dir) - 1 - len(suffix)
	if pad < 1 {
		t.Skipf("temp dir %q (%d bytes) leaves no room for a %d-byte socket path", dir, len(dir), n)
	}
	path := filepath.Join(dir, strings.Repeat("x", pad)+suffix)
	if len(path) != n {
		t.Fatalf("built a %d-byte path, wanted %d", len(path), n)
	}
	lis, err := net.Listen("unix", path)
	if err == nil {
		_ = lis.Close()
		_ = os.Remove(path)
		return true
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("this environment forbids binding unix sockets (%v); cannot probe the length limit", err)
	}
	if !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("bind %d-byte path failed for an unrelated reason: %v", n, err)
	}
	return false
}

// TestMaxUnixSocketPathIsSafeOnThisPlatform is the ground truth the rest of this file rests on.
// It probes the kernel for the real sockaddr_un limit and asserts krayt's conservative constant
// sits at or below it — otherwise maxUnixSocketPath is just a number someone typed, and the
// budget it feeds would let a run through that then dies at bind with EINVAL, an error naming
// nothing useful ("bind: invalid argument").
func TestMaxUnixSocketPathIsSafeOnThisPlatform(t *testing.T) {
	dir := t.TempDir()
	if !bindProbe(t, dir, maxUnixSocketPath) {
		t.Fatalf("a %d-byte socket path was rejected; maxUnixSocketPath is too large for this platform", maxUnixSocketPath)
	}
	// And confirm a limit really exists up there, so the probe above is not vacuous.
	if bindProbe(t, dir, 4096) {
		t.Error("a 4096-byte socket path bound successfully; this platform has no sun_path limit to guard")
	}
}

// shortRunDir makes a run directory short enough that runSocketDir's PREFERRED branch is
// reachable. t.TempDir() cannot do that on macOS: it roots at $TMPDIR (~49 bytes) and embeds the
// test's own name, so a run dir under it is already over budget and every test would silently
// exercise the fallback and nothing else.
//
// This is not the workaround that used to live in question_test.go. That one forced a short path
// on the *production* code path to stop a real bug from firing; this one is a fixture for
// testing the branch that only a short path can reach — the fallback branch is tested right
// beside it, on a deliberately long path.
func shortRunDir(t *testing.T, runID string) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "kr")
	if err != nil {
		t.Skipf("cannot create a short run dir under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	dir := filepath.Join(base, runID)
	if !socketDirFits(filepath.Join(dir, "ask")) {
		t.Skipf("even %q is too long for the preferred branch on this system", dir)
	}
	return dir
}

// TestRunSocketDirPrefersRunDir is deliberately hermetic: choosing the preferred branch is pure
// length arithmetic and touches no filesystem, so a synthetic short path exercises it on every
// platform — including sandboxes where /tmp is not writable and shortRunDir would have to skip.
func TestRunSocketDirPrefersRunDir(t *testing.T) {
	runDir := "/tmp/kr/run_abcd1234" // never created; nothing below stats it
	dir, cleanup, err := runSocketDir(runDir, "run_abcd1234")
	if err != nil {
		t.Fatalf("runSocketDir: %v", err)
	}
	defer cleanup()
	if want := filepath.Join(runDir, "ask"); dir != want {
		t.Errorf("runSocketDir = %q, want the run's own dir %q", dir, want)
	}
}

// TestRunSocketDirFallsBackWhenRunDirTooLong is the regression guard for the failure a real run
// hit: a scratch repo under macOS's own $TMPDIR pushed <repo>/.krayt/runs/run_XXXXXXXX/ask/ask.sock
// to 106 bytes and every --on-question=wait run died on "bind: invalid argument".
func TestRunSocketDirFallsBackWhenRunDirTooLong(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), strings.Repeat("deep", 30), "run_abcd1234")
	if socketDirFits(filepath.Join(runDir, "ask")) {
		t.Fatal("test setup did not produce an over-long run dir")
	}
	dir, cleanup, err := runSocketDir(runDir, "run_abcd1234")
	if err != nil {
		t.Fatalf("runSocketDir: %v", err)
	}
	defer cleanup()

	if strings.HasPrefix(dir, runDir) {
		t.Errorf("runSocketDir = %q, still under the over-long run dir", dir)
	}
	if !socketDirFits(dir) {
		t.Errorf("fallback %q is itself %d bytes, over the %d-byte limit", dir, longestSocketPathLen(dir), maxUnixSocketPath)
	}
	// The fallback must be usable, not merely short: bind the real socket names in it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir fallback: %v", err)
	}
	for _, name := range runSocketNames {
		lis, err := net.Listen("unix", filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
				t.Skipf("this environment forbids binding unix sockets (%v)", err)
			}
			t.Fatalf("bind %s in fallback dir: %v", name, err)
		}
		_ = lis.Close()
	}
}

// TestRunSocketDirCleanupRemovesFallbackOnly: the fallback lives outside the run directory, so
// nothing else will ever collect it; the in-run-dir case must NOT be removed, since the run
// directory owns that lifetime and is the operator's artifact.
func TestRunSocketDirCleanupRemovesFallbackOnly(t *testing.T) {
	shortRun := shortRunDir(t, "run_abcd1234")
	dir, cleanup, err := runSocketDir(shortRun, "run_abcd1234")
	if err != nil {
		t.Fatalf("runSocketDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("cleanup removed the in-run-dir socket dir %q: %v", dir, err)
	}

	longRun := filepath.Join(t.TempDir(), strings.Repeat("deep", 30), "run_efgh5678")
	fb, cleanupFB, err := runSocketDir(longRun, "run_efgh5678")
	if err != nil {
		t.Fatalf("runSocketDir: %v", err)
	}
	if err := os.MkdirAll(fb, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cleanupFB()
	if _, err := os.Stat(fb); !os.IsNotExist(err) {
		t.Errorf("cleanup left the fallback dir %q behind (err=%v)", fb, err)
	}
}

// TestRunSocketNamesCoverEverythingBound guards the budget itself: runSocketDir clears the
// LONGEST name in runSocketNames, so a socket bound in that directory under a name missing from
// the list would slip past the check and fail at bind time instead.
func TestRunSocketNamesCoverEverythingBound(t *testing.T) {
	for _, name := range []string{"ask.sock", "control.sock"} {
		found := false
		for _, n := range runSocketNames {
			if n == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is bound in the run socket dir but missing from runSocketNames", name)
		}
	}
}
