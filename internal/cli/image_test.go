package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/imagecache"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/vmimage"
)

// 64-char sha256 hex names for seeded container cache entries.
const (
	digInUse  = "1111111111111111111111111111111111111111111111111111111111111111"
	digOld    = "2222222222222222222222222222222222222222222222222222222222222222"
	digRecent = "3333333333333333333333333333333333333333333333333333333333333333"
)

// seedCacheEntry writes a one-file cache directory <base>/krayt/<kind>/<hex> and returns it.
func seedCacheEntry(t *testing.T, base, kind, hex string, size int) string {
	t.Helper()
	dir := filepath.Join(base, "krayt", kind, hex)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "rootfs.img"
	if kind == "imagestore" {
		name = "index.json"
	}
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ageDir backdates a cache dir's mtime so List (absent a sentinel) reads it as last-used then.
func ageDir(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
}

// seedRunImage writes a minimal non-terminal run meta.json with a custom image_ref.
func seedRunImage(t *testing.T, repo, id, state, imageRef string) {
	t.Helper()
	runDir := orchestrator.RunDir(filepath.Join(repo, ".krayt"), id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"` + id + `","state":"` + state + `","exit_code":0,"image_ref":"` + imageRef + `","started_at":"2026-07-01T00:00:00Z","pid":0}`
	write(t, filepath.Join(runDir, "meta.json"), meta)
}

func pinnedHex(t *testing.T) string {
	t.Helper()
	if vmimage.PinnedDigest == "" {
		t.Skip("no pinned base image digest set")
	}
	return vmimage.PinnedDigest.Encoded()
}

func TestImageLs(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KRAYT_CACHE_DIR", base)
	pinned := pinnedHex(t)
	seedCacheEntry(t, base, "vmimage", pinned, 1024)
	seedCacheEntry(t, base, "imagestore", digRecent, 2048)

	out := run(t, newImageLsCmd())
	for _, want := range []string{"vmimage", "container", pinned[:12], digRecent[:12], "(pinned)", "2 images"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output missing %q; got:\n%s", want, out)
		}
	}
	// The pinned mark must sit on the vmimage row, not the container row.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "(pinned)") && !strings.Contains(line, "vmimage") {
			t.Errorf("(pinned) on a non-vmimage row: %q", line)
		}
	}
}

func TestImageRm(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KRAYT_CACHE_DIR", base)
	dir := seedCacheEntry(t, base, "imagestore", digRecent, 100)

	// A no-match prefix errors and deletes nothing.
	if err := execErr(newImageRmCmd(), "deadbeef"); err == nil {
		t.Error("rm of a non-existent digest should error")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("no-match rm should not have deleted anything: %v", err)
	}

	// An unambiguous prefix removes exactly that entry.
	out := run(t, newImageRmCmd(), digRecent[:8])
	if !strings.Contains(out, "reclaimed") {
		t.Errorf("rm output = %q", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("entry should be gone after rm: %v", err)
	}
}

func TestImageRmAmbiguous(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KRAYT_CACHE_DIR", base)
	// Two container digests sharing a prefix.
	a := "ab11111111111111111111111111111111111111111111111111111111111111"
	b := "ab22222222222222222222222222222222222222222222222222222222222222"
	dirA := seedCacheEntry(t, base, "imagestore", a, 10)
	dirB := seedCacheEntry(t, base, "imagestore", b, 10)

	if err := execErr(newImageRmCmd(), "ab"); err == nil {
		t.Error("ambiguous prefix should error")
	}
	for _, d := range []string{dirA, dirB} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("ambiguous rm should not have deleted %s: %v", d, err)
		}
	}
}

func TestImageRmPinnedNeedsForce(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KRAYT_CACHE_DIR", base)
	pinned := pinnedHex(t)
	dir := seedCacheEntry(t, base, "vmimage", pinned, 100)

	// Without --force the pinned base image is refused, untouched.
	if err := execErr(newImageRmCmd(), pinned[:12]); err == nil {
		t.Error("rm of the pinned base image without --force should error")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("pinned image should survive a --force-less rm: %v", err)
	}

	// With --force it is removed.
	_ = run(t, newImageRmCmd(), "--force", pinned[:12])
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("pinned image should be gone after rm --force: %v", err)
	}
}

func TestImagePruneRetention(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KRAYT_CACHE_DIR", base)
	repo := t.TempDir()
	pinned := pinnedHex(t)

	pinnedDir := seedCacheEntry(t, base, "vmimage", pinned, 100)
	// A stale, non-pinned vmimage entry — always removed.
	staleVM := seedCacheEntry(t, base, "vmimage", digOld, 100)
	inUseDir := seedCacheEntry(t, base, "imagestore", digInUse, 100)
	oldDir := seedCacheEntry(t, base, "imagestore", digOld, 100)
	ageDir(t, oldDir, 72*time.Hour) // beyond the default 24h window

	// A running run pins digInUse by a digest ref → protected.
	seedRunImage(t, repo, "run_live", "running", "ghcr.io/x/y@sha256:"+digInUse)

	out := run(t, newImagePruneCmd(), "--repo", repo)
	// Kept: pinned base image + the in-use container. Removed: stale vmimage + old container.
	if _, err := os.Stat(pinnedDir); err != nil {
		t.Errorf("pinned base image must survive prune: %v", err)
	}
	if _, err := os.Stat(inUseDir); err != nil {
		t.Errorf("in-use container must survive prune: %v", err)
	}
	if _, err := os.Stat(staleVM); !os.IsNotExist(err) {
		t.Errorf("stale non-pinned vmimage should be pruned: %v", err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("old unreferenced container should be pruned: %v", err)
	}
	if !strings.Contains(out, "in use by run_live") {
		t.Errorf("prune summary should explain the in-use keep; got:\n%s", out)
	}
}

func TestImagePruneOlderThanZero(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KRAYT_CACHE_DIR", base)
	repo := t.TempDir()
	pinned := pinnedHex(t)

	pinnedDir := seedCacheEntry(t, base, "vmimage", pinned, 100)
	inUseDir := seedCacheEntry(t, base, "imagestore", digInUse, 100)
	recentDir := seedCacheEntry(t, base, "imagestore", digRecent, 100) // touched now
	seedRunImage(t, repo, "run_live", "running", "ghcr.io/x/y@sha256:"+digInUse)

	_ = run(t, newImagePruneCmd(), "--repo", repo, "--older-than", "0s")
	// Age protection is off, but pinned + in-use still hold; the recent one goes.
	if _, err := os.Stat(pinnedDir); err != nil {
		t.Errorf("pinned base image must survive --older-than 0s: %v", err)
	}
	if _, err := os.Stat(inUseDir); err != nil {
		t.Errorf("in-use container must survive --older-than 0s: %v", err)
	}
	if _, err := os.Stat(recentDir); !os.IsNotExist(err) {
		t.Errorf("recent unreferenced container should be pruned by --older-than 0s: %v", err)
	}
}

func TestImagePruneAll(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KRAYT_CACHE_DIR", base)
	repo := t.TempDir()
	pinned := pinnedHex(t)

	pinnedDir := seedCacheEntry(t, base, "vmimage", pinned, 100)
	inUseDir := seedCacheEntry(t, base, "imagestore", digInUse, 100)
	recentDir := seedCacheEntry(t, base, "imagestore", digRecent, 100)
	seedRunImage(t, repo, "run_live", "running", "ghcr.io/x/y@sha256:"+digInUse)

	_ = run(t, newImagePruneCmd(), "--repo", repo, "--all")
	// --all bypasses both container protections but never the pinned base image.
	if _, err := os.Stat(pinnedDir); err != nil {
		t.Errorf("pinned base image must survive --all: %v", err)
	}
	if _, err := os.Stat(inUseDir); !os.IsNotExist(err) {
		t.Errorf("--all should remove even an in-use container: %v", err)
	}
	if _, err := os.Stat(recentDir); !os.IsNotExist(err) {
		t.Errorf("--all should remove even a recent container: %v", err)
	}
}

func TestImagePruneDryRun(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KRAYT_CACHE_DIR", base)
	repo := t.TempDir()

	oldDir := seedCacheEntry(t, base, "imagestore", digOld, 100)
	ageDir(t, oldDir, 72*time.Hour)

	out := run(t, newImagePruneCmd(), "--repo", repo, "--dry-run")
	if _, err := os.Stat(oldDir); err != nil {
		t.Errorf("--dry-run must not delete anything: %v", err)
	}
	if !strings.Contains(out, "would remove") {
		t.Errorf("--dry-run should say what it would remove; got:\n%s", out)
	}
}

// TestImageLsLastUsedSentinel checks the LAST USED column reads the sentinel mtime.
func TestImageLsLastUsedSentinel(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KRAYT_CACHE_DIR", base)
	dir := seedCacheEntry(t, base, "imagestore", digRecent, 100)
	if err := imagecache.Touch(dir); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, imagecache.SentinelName), when, when); err != nil {
		t.Fatal(err)
	}
	out := run(t, newImageLsCmd())
	if !strings.Contains(out, "2026-03-04") {
		t.Errorf("ls LAST USED should reflect the sentinel mtime; got:\n%s", out)
	}
}
