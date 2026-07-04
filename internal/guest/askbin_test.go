package guest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAskBinaryIn resolves the krayt-ask binary shipped next to the guest-agent (§6.13): present
// → its path; absent or a directory → "" so the mount is simply skipped.
func TestAskBinaryIn(t *testing.T) {
	dir := t.TempDir()
	if got := askBinaryIn(dir); got != "" {
		t.Errorf("no binary yet: got %q, want empty", got)
	}

	bin := filepath.Join(dir, "krayt-ask")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := askBinaryIn(dir); got != bin {
		t.Errorf("with binary: got %q, want %q", got, bin)
	}

	// A directory named krayt-ask must not be treated as the binary.
	dir2 := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir2, "krayt-ask"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := askBinaryIn(dir2); got != "" {
		t.Errorf("directory should not resolve: got %q", got)
	}
}
