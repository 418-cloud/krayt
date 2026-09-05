//go:build !windows

package askbridge

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestListenCreatesPrivateDirAndSocket: the parent dir is 0700 and the socket is 0600 inside it
// (decision 10).
func TestListenCreatesPrivateDirAndSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run-state")
	lis, err := Listen(dir)
	if err != nil {
		t.Skipf("unix socket bind unavailable in this sandbox: %v", err)
	}
	defer func() { _ = lis.Close() }()

	dfi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !dfi.IsDir() || dfi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want a 0700 directory", dfi.Mode())
	}

	sockPath := filepath.Join(dir, "ask.sock")
	sfi, err := os.Lstat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if sfi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want 0600", sfi.Mode().Perm())
	}
	if st, ok := sfi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		t.Errorf("socket uid = %d, want %d", st.Uid, os.Getuid())
	}
}

// TestListenRefusesHostileDir: a pre-existing world-writable directory at the target path is
// refused rather than reused — reusing harden-vfkit-socket-dir.md's sockroot.Ensure check rather
// than a second one (decision 4/12).
func TestListenRefusesHostileDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run-state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(dir); err == nil {
		t.Fatal("Listen accepted a 0777 pre-existing dir; want refusal")
	}
}
