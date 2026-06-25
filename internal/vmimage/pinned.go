package vmimage

import "github.com/opencontainers/go-digest"

// Pinned identifies the base VM image this build of krayt expects. CI builds the image
// on an arm64 Linux runner, pushes it as an OCI artifact, and records the resulting
// digest; that digest is pinned here and verified on `krayt image pull` (§11.4/§11.5).
//
// PinnedDigest is empty until the first CI publish — see HUMAN_TODO.md "[Phase 1] Pin
// the published image digest". With it empty, Pull skips verification (no trusted value
// to check against), so the pin MUST be filled before relying on the image.
const (
	// PinnedRef is the default registry reference. The registry is interchangeable; any
	// OCI-compliant registry works (ghcr.io is the convenient default, §11.4).
	PinnedRef = "ghcr.io/418-cloud/krayt-vmimage:v0"

	// PinnedDigest is the expected manifest digest, e.g.
	// "sha256:…". Empty until CI publishes the first image.
	PinnedDigest digest.Digest = ""
)
