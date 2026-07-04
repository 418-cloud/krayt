package vmimage

import "github.com/opencontainers/go-digest"

// Pinned identifies the base VM image this build of krayt expects. CI builds the image
// on an arm64 Linux runner, pushes it as an OCI artifact, and records the resulting
// digest; that digest is pinned here and verified on `krayt image pull` (§11.4/§11.5).
//
// To bump: build + publish via the vm-image workflow (push a new tag), then set both
// PinnedRef (the name@sha256:… digest reference) and PinnedDigest to the digest from its
// `::notice` output.
const (
	// PinnedRef is the default registry reference, pinned by digest so it resolves to
	// exactly the boot-tested image regardless of tag. The registry is interchangeable;
	// any OCI-compliant registry works (ghcr.io is the convenient default, §11.4).
	PinnedRef = "ghcr.io/418-cloud/krayt-vmimage@sha256:9c4cb96bea15ef6c1cc8095e2ccca03b870d35eed98ce53d9c84429c06418a05"

	// PinnedDigest is the expected manifest digest; Pull verifies the pulled artifact
	// against it (§11.4). This is v0.1.0
	PinnedDigest digest.Digest = "sha256:9c4cb96bea15ef6c1cc8095e2ccca03b870d35eed98ce53d9c84429c06418a05"
)
