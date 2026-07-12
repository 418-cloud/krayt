package vmimage

import "github.com/opencontainers/go-digest"

// Pinned identifies the base VM image this build of krayt expects. CI builds the image for both
// architectures on native runners, publishes them as a single multi-arch OCI index, and records
// that index's digest; the digest is pinned here and verified on `krayt image pull` (§11.4/§11.5).
//
// There is deliberately no architecture here. krayt has two VM backends and therefore two guest
// arches — arm64 under vfkit, amd64 under firecracker (§6.3) — but the pin is the *index* digest,
// and Pull resolves it to whichever variant this host can boot (vmimage.selectPlatform). One pin,
// both platforms, and a host with no published variant fails closed with a clear error rather than
// silently booting someone else's kernel.
//
// To bump: build + publish via the vm-image workflow (push a new tag), then set both PinnedRef
// (the name@sha256:… digest reference) and PinnedDigest to the INDEX digest from its `::notice`
// output — not a per-arch child digest, which would pin krayt to one architecture.
const (
	// PinnedRef is the default registry reference, pinned by digest so it resolves to
	// exactly the boot-tested image regardless of tag. The registry is interchangeable;
	// any OCI-compliant registry works (ghcr.io is the convenient default, §11.4).
	PinnedRef = "ghcr.io/418-cloud/krayt-vmimage@sha256:68bc9efe9b649cc79309ff11925ed8d8e3c5c6dc14b272ae8e07f1c32cb07661"

	// PinnedDigest is the expected manifest digest; Pull verifies the pulled artifact
	// against it (§11.4). This is v0.3.0-rc1
	PinnedDigest digest.Digest = "sha256:68bc9efe9b649cc79309ff11925ed8d8e3c5c6dc14b272ae8e07f1c32cb07661"
)
