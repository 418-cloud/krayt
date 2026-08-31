package guestbin

import (
	"os"
	"strings"
	"testing"
)

// TestBinaryMissingNamesRemedy asserts a nonexistent binary name is a clear, actionable error —
// this is guaranteed to be the "embed is empty" case regardless of whether `make guest-bins` has
// been run locally, since this name is never embedded.
func TestBinaryMissingNamesRemedy(t *testing.T) {
	_, err := Binary("does-not-exist", "amd64")
	if err == nil {
		t.Fatal("Binary for a nonexistent name succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "make guest-bins") {
		t.Errorf("error does not name the `make guest-bins` remedy: %v", err)
	}
}

func TestGuestPath(t *testing.T) {
	if got, want := GuestPath(HelperName), "/.krayt/krayt-helper"; got != want {
		t.Errorf("GuestPath(%q) = %q, want %q", HelperName, got, want)
	}
}

// TestEmbeddedBinariesPresentInCI is the Done-when assertion that the embed is non-empty in CI,
// which runs `make guest-bins` before the test suite (.github/workflows/ci.yml). Gated on the CI
// env var GitHub Actions sets, so it is a hard failure there — a test that silently skips
// everywhere would never actually run.
func TestEmbeddedBinariesPresentInCI(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("guest binaries are only guaranteed present after `make guest-bins`; run it locally to exercise this")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		b, err := Binary(HelperName, arch)
		if err != nil {
			t.Errorf("Binary(%q, %q): %v", HelperName, arch, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("Binary(%q, %q) returned empty bytes", HelperName, arch)
		}
	}
}
