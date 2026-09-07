package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// detachChildArg is the argv[1] marker TestMain (fakemsb_test.go) dispatches to runDetachChild
// on. TestSpawnDetached re-execs this test binary as its detached child rather than shelling out
// to `/bin/sh -c 'sleep 0.4; echo ok > ...'`: there is no /bin/sh on Windows (and no `sleep`
// binary on the hosted runner's PATH), which made this test — and with it `go test ./...` on the
// native Windows job — fail on a detail incidental to what it actually proves. Re-execing the
// test binary is the same trick this package's fake msb already uses, and it exercises
// spawnDetached's real Windows path (proc_windows.go's DETACHED_PROCESS) rather than skipping it.
const detachChildArg = "__krayt_detach_child"

// detachChildDelay is how long the detached child waits before producing its marker — long
// enough that the launcher provably returned first, short enough to keep the test quick.
const detachChildDelay = 400 * time.Millisecond

// runDetachChild is this test binary re-exec'd as TestSpawnDetached's detached child: it sleeps,
// then writes the marker file whose delayed appearance is the proof the child outlived the
// launcher's return.
func runDetachChild() int {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "detach-child: missing marker path")
		return 1
	}
	time.Sleep(detachChildDelay)
	if err := os.WriteFile(os.Args[2], []byte("ok\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "detach-child: write marker: %v\n", err)
		return 1
	}
	return 0
}

// TestSpawnDetached proves the detach mechanism (§6.2): the launcher returns immediately while
// the spawned process runs independently to completion (its delayed side effect appears after
// the launcher has already returned).
func TestSpawnDetached(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "done")
	logPath := filepath.Join(dir, "supervisor.log")

	start := time.Now()
	pid, err := spawnDetached(testBinPath, []string{detachChildArg, marker}, os.Environ(), logPath)
	if err != nil {
		t.Fatalf("spawnDetached: %v", err)
	}
	// The launcher must not block on the child's work.
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("spawnDetached blocked for %v; should return immediately", elapsed)
	}
	if pid <= 0 {
		t.Errorf("pid = %d, want > 0", pid)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("marker exists already; child did not run detached/asynchronously")
	}

	// The detached child finishes on its own after we've returned.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("detached child never produced its marker")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
