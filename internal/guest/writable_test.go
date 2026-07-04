package guest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMakeContainerWritableSkipsSymlinks is the regression for the symlink-escape review finding:
// makeContainerWritable runs as root over an untrusted repo, and os.Chmod follows symlinks — so a
// workspace symlink pointing outside the tree must NOT get its target chmod'd (§10). Real files in
// the tree are still relaxed.
func TestMakeContainerWritableSkipsSymlinks(t *testing.T) {
	// A file OUTSIDE the workspace whose perms must remain untouched.
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	wsFile := filepath.Join(ws, "real.txt")
	if err := os.WriteFile(wsFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "escape")); err != nil {
		t.Skipf("symlinks unavailable in this sandbox: %v", err)
	}

	if err := makeContainerWritable(ws); err != nil {
		t.Fatalf("makeContainerWritable: %v", err)
	}

	// The real workspace file is relaxed so the non-root container can write it.
	if fi, err := os.Stat(wsFile); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o006 == 0 {
		t.Errorf("workspace file not relaxed: mode %v", fi.Mode().Perm())
	}
	// The symlink target outside the workspace was NOT followed/chmod'd.
	if fi, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("symlink target outside the workspace was chmod'd to %v — symlink was followed", fi.Mode().Perm())
	}
}
