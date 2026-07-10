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
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
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

	desc, err := oras.Copy(ctx, src, ref, fs, ref, oras.DefaultCopyOptions)
	if err != nil {
		_ = os.RemoveAll(destDir)
		return nil, fmt.Errorf("vmimage: pull %q: %w", ref, err)
	}
	if want != "" && desc.Digest != want {
		_ = os.RemoveAll(destDir)
		return nil, fmt.Errorf("vmimage: digest mismatch for %q: got %s, want %s", ref, desc.Digest, want)
	}

	img := &Image{
		Digest: desc.Digest.String(),
		Dir:    destDir,
		Kernel: filepath.Join(destDir, FileKernel),
		Initrd: filepath.Join(destDir, FileInitrd),
		RootFS: filepath.Join(destDir, FileRootFS),
	}
	if err := img.verifyFiles(); err != nil {
		return nil, err
	}
	return img, nil
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
