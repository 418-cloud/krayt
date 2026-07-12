package imagecache_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/imagecache"
)

// digest hex names (64-char sha256) used as cache-dir keys.
const (
	digA = "a0c489cda054f0195bf8086406ddd8f4c762bb9dc9466b39b7c0b66ae616152b"
	digB = "9f3e21ab00000000000000000000000000000000000000000000000000000000"
)

func TestListSizesAndEnumerates(t *testing.T) {
	root := t.TempDir()
	// Two digest-named entries with known file sizes, and one non-digest (sanitized-ref) dir.
	writeFile(t, filepath.Join(root, digA, "rootfs.img"), 1000)
	writeFile(t, filepath.Join(root, digA, "sub", "vmlinuz"), 24) // nested file counts too
	writeFile(t, filepath.Join(root, digB, "index.json"), 50)
	writeFile(t, filepath.Join(root, "krayt-vmimage_v1", "rootfs.img"), 7)

	got, err := imagecache.List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d entries, want 3", len(got))
	}

	byDir := map[string]imagecache.Entry{}
	for _, e := range got {
		byDir[filepath.Base(e.Dir)] = e
	}
	if e := byDir[digA]; e.SizeB != 1024 {
		t.Errorf("%s size = %d, want 1024", digA, e.SizeB)
	}
	if e := byDir[digA]; e.Digest != "sha256:"+digA {
		t.Errorf("%s digest = %q, want sha256:%s", digA, e.Digest, digA)
	}
	if e := byDir[digB]; e.SizeB != 50 {
		t.Errorf("%s size = %d, want 50", digB, e.SizeB)
	}
	// The non-digest directory is listed, with Digest == "".
	e, ok := byDir["krayt-vmimage_v1"]
	if !ok {
		t.Fatal("non-digest-named directory not listed")
	}
	if e.Digest != "" {
		t.Errorf("non-digest dir Digest = %q, want \"\"", e.Digest)
	}
	if e.SizeB != 7 {
		t.Errorf("non-digest dir size = %d, want 7", e.SizeB)
	}
}

func TestListMissingRoot(t *testing.T) {
	got, err := imagecache.List(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("List of a missing root should not error: %v", err)
	}
	if got != nil {
		t.Errorf("List of a missing root = %v, want nil", got)
	}
}

func TestTouchCreatesThenRefreshes(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, imagecache.SentinelName)

	if err := imagecache.Touch(dir); err != nil {
		t.Fatalf("Touch (create): %v", err)
	}
	fi, err := os.Stat(sentinel)
	if err != nil {
		t.Fatalf("sentinel not created: %v", err)
	}
	first := fi.ModTime()

	// A second Touch must advance the sentinel's mtime — it is the last-used signal.
	setMtime(t, sentinel, first.Add(-time.Hour)) // rewind so any real advance is visible
	if err := imagecache.Touch(dir); err != nil {
		t.Fatalf("Touch (refresh): %v", err)
	}
	fi, err = os.Stat(sentinel)
	if err != nil {
		t.Fatalf("stat after refresh: %v", err)
	}
	if !fi.ModTime().After(first.Add(-time.Hour)) {
		t.Errorf("Touch did not refresh mtime: %v not after %v", fi.ModTime(), first.Add(-time.Hour))
	}
}

func TestLastUsedFromSentinelThenDir(t *testing.T) {
	root := t.TempDir()
	// Entry with a sentinel: LastUsed is the sentinel mtime.
	writeFile(t, filepath.Join(root, digA, "rootfs.img"), 10)
	if err := imagecache.Touch(filepath.Join(root, digA)); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	want := time.Now().Add(-48 * time.Hour)
	setMtime(t, filepath.Join(root, digA, imagecache.SentinelName), want)

	// Entry without a sentinel: LastUsed falls back to the dir mtime.
	writeFile(t, filepath.Join(root, digB, "rootfs.img"), 10)
	dirWant := time.Now().Add(-72 * time.Hour)
	setMtime(t, filepath.Join(root, digB), dirWant)

	got, err := imagecache.List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range got {
		switch filepath.Base(e.Dir) {
		case digA:
			if !e.LastUsed.Equal(want) {
				t.Errorf("sentinel LastUsed = %v, want %v", e.LastUsed, want)
			}
		case digB:
			if !e.LastUsed.Equal(dirWant) {
				t.Errorf("dir-fallback LastUsed = %v, want %v", e.LastUsed, dirWant)
			}
		}
	}
}

func TestRemoveDeletesDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, digA, "rootfs.img"), 10)
	got, err := imagecache.List(root)
	if err != nil || len(got) != 1 {
		t.Fatalf("List: %v (%d entries)", err, len(got))
	}
	if err := imagecache.Remove(got[0]); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(got[0].Dir); !os.IsNotExist(err) {
		t.Errorf("entry dir should be gone after Remove: %v", err)
	}
}

// writeFile writes n bytes to path, creating parent dirs.
func writeFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setMtime(t *testing.T, path string, mt time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}
