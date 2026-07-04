package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSpawnDetached proves the detach mechanism (§6.2): the launcher returns immediately while
// the spawned process runs independently to completion (its delayed side effect appears after
// the launcher has already returned).
func TestSpawnDetached(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "done")
	logPath := filepath.Join(dir, "supervisor.log")

	start := time.Now()
	pid, err := spawnDetached("/bin/sh", []string{"-c", "sleep 0.4; echo ok > " + marker}, os.Environ(), logPath)
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
