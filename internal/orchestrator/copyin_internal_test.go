package orchestrator

import (
	"path"
	"slices"
	"testing"

	"github.com/418-cloud/krayt/internal/sandbox/guestbin"
)

// TestGuestParentDirsCoversEveryCopyDestination pins the property the copy-in step depends on:
// every guest path krayt copies to gets its parent created first. `msb copy` does not create a
// missing parent — it fails with "sandbox fs error: open: No such file or directory" — so a
// destination whose parent is absent from this list is a run that dies on copy-in.
func TestGuestParentDirsCoversEveryCopyDestination(t *testing.T) {
	copies := []copySpec{
		{"/host/repo.bundle", containerBundlePath},
		{"/host/prompt.md", containerTaskFile},
		{"/host/krayt-helper", guestbin.GuestPath(guestbin.HelperName)},
		{"/host/krayt-ask", containerAskBinPath},
	}
	got := guestParentDirs(copies)

	for _, c := range copies {
		want := path.Dir(c.guest)
		if !slices.Contains(got, want) {
			t.Errorf("guestParentDirs() = %q, missing parent %q of destination %q", got, want, c.guest)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("guestParentDirs() = %q, want sorted", got)
	}
}

func TestGuestParentDirsDedupesAndSkipsRoot(t *testing.T) {
	got := guestParentDirs([]copySpec{
		{"a", "/task/prompt.md"},
		{"b", "/task/extra.md"}, // same parent, must not be repeated
		{"c", "/rootfile"},      // parent is "/", which always exists
	})
	want := []string{"/task"}
	if !slices.Equal(got, want) {
		t.Errorf("guestParentDirs() = %q, want %q", got, want)
	}
}
