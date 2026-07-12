// Package imagestore is the host side of image acquisition (§6.11): the host is the only
// component that touches a registry. It pulls the user's OCI image into a digest-keyed
// local OCI layout cache and exports it as an OCI archive stream that the guest imports
// into containerd over the same vsock control channel used for code and task — so the VM
// never needs registry egress.
//
// This is distinct from internal/vmimage (the trusted base-VM artifact); both are
// content-addressed OCI, but this one carries the untrusted user agent image.
package imagestore

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/418-cloud/krayt/internal/imagecache"
)

// Image is a user OCI image acquired into the host's local OCI layout cache. The layout
// directory (oci-layout + index.json + blobs/) is both the digest-keyed cache and the
// source for the OCI archive streamed to the guest (§6.11).
type Image struct {
	Ref    string // the reference the user asked for (tag or digest)
	Digest string // resolved manifest digest — the content address / cache key
	Dir    string // OCI layout directory on the host
}

// Remote builds a read-only OCI target for a registry reference, authenticating via the
// Docker credential store (a prior `docker login` works) and falling back to anonymous —
// the same pattern as vmimage.Open. Tests pass an in-memory or oci-layout target to
// Acquire directly instead.
func Remote(ref string) (oras.ReadOnlyTarget, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("imagestore: open %q: %w", ref, err)
	}
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("imagestore: load docker credentials: %w", err)
	}
	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.NewCache(),
		Credential: credentials.Credential(store),
	}
	return repo, nil
}

// Acquire copies the image at ref from src into a digest-keyed OCI layout under cacheRoot,
// reusing the cache when the digest is already present so repeat runs skip pull + export
// (§6.11). oras verifies every blob digest during the copy, so a corrupted layer fails
// here. It returns the cached Image.
func Acquire(ctx context.Context, src oras.ReadOnlyTarget, ref, cacheRoot string) (*Image, error) {
	// Resolve first so the cache is keyed by the content address, not the (mutable) tag.
	desc, err := src.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("imagestore: resolve %q: %w", ref, err)
	}
	dir := filepath.Join(cacheRoot, desc.Digest.Encoded())

	// Cache hit when the layout already has an index.json for this digest.
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
		_ = imagecache.Touch(dir) // best-effort last-used bookkeeping for `krayt image ls/prune`
		return &Image{Ref: ref, Digest: desc.Digest.String(), Dir: dir}, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("imagestore: create cache dir: %w", err)
	}
	dst, err := oci.New(dir)
	if err != nil {
		return nil, fmt.Errorf("imagestore: open layout: %w", err)
	}
	got, err := oras.Copy(ctx, src, ref, dst, ref, oras.DefaultCopyOptions)
	if err != nil {
		// Leave no half-written layout behind to be mistaken for a cache hit.
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("imagestore: copy %q: %w", ref, err)
	}
	_ = imagecache.Touch(dir) // best-effort last-used bookkeeping for `krayt image ls/prune`
	return &Image{Ref: ref, Digest: got.Digest.String(), Dir: dir}, nil
}

// BlobDigests lists every blob the image's layout contains (config, layers, manifest),
// keyed as "sha256:<hex>". This is what the host sends in QueryImageBlobs so the guest can
// reply with only the digests its content store is missing (§6.5, §6.11). Enumerating the
// blobs/ directory is exact because oras.Copy stored precisely this image's blobs here.
func (img *Image) BlobDigests() ([]string, error) {
	blobsDir := filepath.Join(img.Dir, "blobs")
	var digests []string
	err := filepath.WalkDir(blobsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Layout is blobs/<algo>/<hex>.
		algo := filepath.Base(filepath.Dir(path))
		digests = append(digests, algo+":"+d.Name())
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("imagestore: enumerate blobs: %w", err)
	}
	sort.Strings(digests)
	return digests, nil
}

// WriteArchive streams the image as an OCI archive (a tar of the oci-layout) to w, the
// payload of PushImage (§6.11). When only is non-nil, blob files whose digest is absent
// from it are skipped — the incremental path, where the guest already has those blobs from
// a prior run; the metadata (oci-layout, index.json) is always included. When only is nil,
// the full archive is written (the Phase 2 single-run case: a fresh ephemeral guest is
// missing everything).
func (img *Image) WriteArchive(w io.Writer, only map[string]bool) error {
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	root := img.Dir
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return nil // tar entries for files carry their parent path; dirs are implicit
		}
		if only != nil && isBlob(rel) && !only[blobDigest(rel)] {
			return nil // guest already has this blob
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("imagestore: write archive: %w", err)
	}
	return tw.Close()
}

// isBlob reports whether a layout-relative path is a content blob (blobs/<algo>/<hex>).
func isBlob(rel string) bool {
	return strings.HasPrefix(filepath.ToSlash(rel), "blobs/")
}

// blobDigest maps a layout-relative blob path (blobs/<algo>/<hex>) to "<algo>:<hex>".
func blobDigest(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 {
		return ""
	}
	return parts[1] + ":" + parts[2]
}

// Missing returns the subset of want not present in have, preserving want's order — the
// host-side companion to the guest's BlobPresence reply (§6.5).
func Missing(want []string, have map[string]bool) []string {
	var missing []string
	for _, d := range want {
		if !have[d] {
			missing = append(missing, d)
		}
	}
	return missing
}

// ParseDigest validates a "sha256:<hex>" string, used when keying caches by digest.
func ParseDigest(s string) (digest.Digest, error) { return digest.Parse(s) }
