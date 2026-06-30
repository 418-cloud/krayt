package guest

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestTarDirSkipsSymlinks guards the artifact-collection path: /output is written by
// untrusted agent code, so a symlink there must be skipped, not followed. Following it
// would fail the whole collection with ErrWriteTooLong (the symlink header is size 0); a
// benign symlink would otherwise break an honest run. Regular files must still be archived.
func TestTarDirSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "changes.patch"), []byte("real patch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("SENSITIVE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	var buf bytes.Buffer
	if err := tarDir(dir, &buf); err != nil {
		t.Fatalf("tarDir returned error (symlink should be skipped, not fail collection): %v", err)
	}

	names := map[string]bool{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		names[hdr.Name] = true
		if hdr.Typeflag == tar.TypeSymlink {
			t.Errorf("symlink %q was archived; it should be skipped", hdr.Name)
		}
	}
	if !names["changes.patch"] {
		t.Error("regular file changes.patch missing from archive")
	}
	if names["leak"] {
		t.Error("symlink leak was included in the archive")
	}
	// The symlink target's bytes must never appear in the stream.
	if bytes.Contains(buf.Bytes(), []byte("SENSITIVE")) {
		t.Error("symlink target bytes leaked into the artifact tar")
	}
}
