//go:build unix

package sockroot

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// TestEnsure mirrors the vfkit/Firecracker ensureSockRoot tests this package replaces
// (harden-vfkit-socket-dir.md) — the same properties must hold regardless of which caller reuses
// the check (dial-ask-channel-over-vsock.md decision 12).
func TestEnsure(t *testing.T) {
	uid := os.Getuid()

	t.Run("creates a fresh 0700 self-owned root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "krayt-root")
		if err := Ensure(root); err != nil {
			t.Fatalf("Ensure on fresh path: %v", err)
		}
		fi, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.IsDir() {
			t.Fatalf("root %s is not a directory", root)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("root mode = %o, want 0700", fi.Mode().Perm())
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
			t.Errorf("root uid = %d, want %d", st.Uid, uid)
		}
	})

	t.Run("accepts an already-correct root (idempotent)", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "krayt-root")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := Ensure(root); err != nil {
			t.Fatalf("Ensure on good root: %v", err)
		}
	})

	t.Run("refuses a world-writable pre-existing root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "krayt-root")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		// Chmod separately: Mkdir's mode is masked by umask, so set it explicitly.
		if err := os.Chmod(root, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := Ensure(root); err == nil {
			t.Fatal("Ensure accepted a 0777 root; want refusal")
		}
	})

	t.Run("refuses a symlink at the root path (not followed)", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target") // a valid 0700 dir the link points at
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(dir, "krayt-root")
		if err := os.Symlink(target, root); err != nil {
			t.Fatal(err)
		}
		if err := Ensure(root); err == nil {
			t.Fatal("Ensure accepted a symlink root; want refusal")
		}
	})

	// The root can be shared across every VM/run the same user starts, so concurrent callers
	// race Ensure on a fresh path: one wins the Mkdir, the rest must see EEXIST and fall back to
	// validating what now exists — not fail outright.
	t.Run("concurrent creators all succeed on a fresh root (EEXIST race)", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "krayt-root")
		const n = 50
		var wg sync.WaitGroup
		errs := make([]error, n)
		wg.Add(n)
		for i := range n {
			go func(i int) {
				defer wg.Done()
				errs[i] = Ensure(root)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("goroutine %d: Ensure: %v", i, err)
			}
		}
	})
}
