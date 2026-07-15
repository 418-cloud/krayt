// Package vmimage acquires the base micro-VM image — the kernel, initrd, and raw rootfs
// that the macOS provider boots. The image is distributed as a digest-addressed OCI
// artifact (§11.4/§11.5); krayt pins a version→digest, pulls it from a configurable
// registry, verifies the digest, and caches it locally for CoW cloning (§11.4).
//
// This is distinct from internal/imagestore (§6.11), which handles the user's agent
// image. Both are content-addressed OCI, but this one is the trusted boot artifact.
package vmimage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/418-cloud/krayt/internal/imagecache"
)

// Artifact file names, carried as org.opencontainers.image.title annotations on the OCI
// layers (set by the CI `oras push`, §11.5) and used to restore the files on pull.
const (
	FileKernel = "vmlinuz"
	FileInitrd = "initrd"
	FileRootFS = "rootfs.img"
)

// OCI layer media types for the base-image artifact (§11.5).
const (
	MediaTypeKernel = "application/vnd.krayt.kernel"
	MediaTypeInitrd = "application/vnd.krayt.initrd"
	MediaTypeRootFS = "application/vnd.krayt.rootfs"
)

// Image is a pulled, verified base VM image on local disk.
type Image struct {
	Digest string // resolved manifest digest (the content address)
	Dir    string // directory holding the extracted files
	Kernel string // path to vmlinuz
	Initrd string // path to initrd
	RootFS string // path to the raw rootfs.img (the CoW base)
}

// Open builds a read-only OCI target for a registry reference (e.g.
// ghcr.io/418-cloud/krayt-vmimage:v1). It authenticates using the Docker credential
// store (~/.docker/config.json + native keychain helpers), so a prior
// `docker login ghcr.io` / `oras login ghcr.io` lets it pull from private registries;
// with no stored credentials it falls back to anonymous (fine for public artifacts).
// Used in production; tests pass an in-memory or oci-layout target to Pull instead.
func Open(ref string) (oras.ReadOnlyTarget, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("vmimage: open %q: %w", ref, err)
	}
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("vmimage: load docker credentials: %w", err)
	}
	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.NewCache(),
		Credential: credentials.Credential(store),
	}
	return repo, nil
}

// Pull copies the artifact at ref from src into destDir, verifies the resolved manifest
// digest matches want (when non-empty), and returns the extracted Image. oras verifies
// every blob digest during the copy, so a corrupted layer fails here too (§11.4).
//
// oras.Copy extracts to disk as it goes, before this function ever checks want — so on a
// copy error or a digest mismatch, destDir may already hold content from the rejected
// artifact. Both paths remove destDir rather than leave it behind, matching
// imagestore.Acquire's existing pattern (leaving a half-written/rejected extraction on disk
// risks it being mistaken for good content on a later look, and is exactly the kind of
// leftover CVE-2026-50163 — a hardlink path-traversal bug fixed upstream in oras-go v2.6.2 —
// warns against trusting).
func Pull(ctx context.Context, src oras.ReadOnlyTarget, ref string, want digest.Digest, destDir string) (*Image, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("vmimage: create dest dir: %w", err)
	}
	fs, err := file.New(destDir)
	if err != nil {
		return nil, fmt.Errorf("vmimage: file store: %w", err)
	}
	defer func() { _ = fs.Close() }()

	// Verify the pinned digest, and pick this host's architecture, in the SAME hook — on the root
	// that oras.Copy itself resolved.
	//
	// Both halves of that matter. Resolving the reference separately and checking *that* would be
	// a TOCTOU: oras.Copy resolves the reference again internally, so for a moving tag a registry
	// could answer the check with one manifest and the copy with another, and we would extract
	// content we never verified while reporting the digest we did. Checking Copy's *return value*
	// instead does not work either, because with platform selection in play it returns the mapped
	// per-arch child, not the index we pinned (§11.5). MapRoot is the one place that sees exactly
	// the root being copied, before a single blob is fetched — so verify there.
	var root ocispec.Descriptor
	opts := oras.DefaultCopyOptions
	opts.MapRoot = func(ctx context.Context, src content.ReadOnlyStorage, desc ocispec.Descriptor) (ocispec.Descriptor, error) {
		if want != "" && desc.Digest != want {
			return ocispec.Descriptor{}, fmt.Errorf("digest mismatch for %q: got %s, want %s", ref, desc.Digest, want)
		}
		root = desc
		return selectPlatform(ctx, src, desc)
	}

	if _, err := oras.Copy(ctx, src, ref, fs, ref, opts); err != nil {
		// MapRoot runs before any blob is fetched, so a rejected artifact has not reached the disk
		// — but a copy that failed partway through has, and either way destDir must not survive as
		// something a later look could mistake for good content.
		_ = os.RemoveAll(destDir)
		return nil, fmt.Errorf("vmimage: pull %q: %w", ref, err)
	}

	img := &Image{
		Digest: root.Digest.String(),
		Dir:    destDir,
		Kernel: filepath.Join(destDir, FileKernel),
		Initrd: filepath.Join(destDir, FileInitrd),
		RootFS: filepath.Join(destDir, FileRootFS),
	}
	if err := img.verifyFiles(); err != nil {
		return nil, err
	}
	_ = imagecache.Touch(destDir) // best-effort last-used bookkeeping for `krayt image ls/prune`
	return img, nil
}

// selectPlatform resolves a multi-arch base image to the one variant this host can boot.
//
// krayt has two VM backends and therefore two guest architectures — arm64 under vfkit on Apple
// Silicon, amd64 under firecracker on Linux/KVM (§6.3) — so the base image is published as an
// OCI image index with one artifact per arch (§11.5). Pinning stays a single ref + a single
// digest (the index's), exactly as it was when there was only one arch; this is what makes that
// pin arch-transparent.
//
// Without this, oras.Copy would walk the whole graph: it would download *both* architectures
// (~2 GiB each) and extract both into the same directory, where their identical
// org.opencontainers.image.title annotations (vmlinuz/initrd/rootfs.img) collide.
//
// A root that is NOT an index is passed through untouched. That is deliberate and load-bearing:
// a single-arch artifact carries no platform information at all (it is packed with an empty
// config, not an OCI image config), so there is nothing to select on and nothing to check. It
// keeps every pre-index artifact — including the one currently pinned — pulling exactly as before.
func selectPlatform(ctx context.Context, src content.ReadOnlyStorage, root ocispec.Descriptor) (ocispec.Descriptor, error) {
	if root.MediaType != ocispec.MediaTypeImageIndex {
		return root, nil // single-arch artifact: nothing to select
	}

	raw, err := content.FetchAll(ctx, src, root)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("fetch index: %w", err)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("parse index: %w", err)
	}

	available := make([]string, 0, len(idx.Manifests))
	for _, m := range idx.Manifests {
		// The platform must come from the index entry itself. There is no fallback to the child's
		// config here — these are artifacts with custom media types, not container images, so they
		// have no image config to read an architecture out of. CI is what puts it here (§11.5), and
		// an entry without it can never match.
		if m.Platform == nil {
			continue
		}
		available = append(available, m.Platform.OS+"/"+m.Platform.Architecture)
		if m.Platform.OS == "linux" && m.Platform.Architecture == runtime.GOARCH {
			return m, nil
		}
	}
	return ocispec.Descriptor{}, fmt.Errorf(
		"base VM image has no linux/%s variant (index %s offers: %s) — the image must be published "+
			"for this host's architecture; see HUMAN_TODO.md",
		runtime.GOARCH, root.Digest, strings.Join(available, ", "))
}

// verifyFiles ensures the artifact contained the three expected files.
func (img *Image) verifyFiles() error {
	for _, p := range []string{img.Kernel, img.Initrd, img.RootFS} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("vmimage: missing artifact file %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}
