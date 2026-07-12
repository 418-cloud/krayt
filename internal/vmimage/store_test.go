package vmimage_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"

	"github.com/418-cloud/krayt/internal/vmimage"
)

// fakeArtifact packs a base-image OCI artifact (kernel+initrd+rootfs) into an in-memory
// store and returns the source, ref, and manifest digest. No registry/socket needed.
func fakeArtifact(t *testing.T) (oras.ReadOnlyTarget, string, digest.Digest) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()

	layer := func(mediaType, title string, data []byte) ocispec.Descriptor {
		desc := content.NewDescriptorFromBytes(mediaType, data)
		desc.Annotations = map[string]string{ocispec.AnnotationTitle: title}
		if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
			t.Fatalf("push %s: %v", title, err)
		}
		return desc
	}

	layers := []ocispec.Descriptor{
		layer(vmimage.MediaTypeKernel, vmimage.FileKernel, []byte("KERNEL")),
		layer(vmimage.MediaTypeInitrd, vmimage.FileInitrd, []byte("INITRD")),
		layer(vmimage.MediaTypeRootFS, vmimage.FileRootFS, []byte("ROOTFS")),
	}
	manifest, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1,
		"application/vnd.krayt.vmimage", oras.PackManifestOptions{Layers: layers})
	if err != nil {
		t.Fatalf("pack manifest: %v", err)
	}
	const ref = "v0"
	if err := store.Tag(ctx, manifest, ref); err != nil {
		t.Fatalf("tag: %v", err)
	}
	return store, ref, manifest.Digest
}

func TestPullExtractsAndVerifies(t *testing.T) {
	src, ref, want := fakeArtifact(t)
	dest := t.TempDir()

	img, err := vmimage.Pull(context.Background(), src, ref, want, dest)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if img.Digest != want.String() {
		t.Errorf("digest = %s, want %s", img.Digest, want)
	}
	for name, path := range map[string]string{
		"kernel": img.Kernel, "initrd": img.Initrd, "rootfs": img.RootFS,
	} {
		if filepath.Dir(path) != dest {
			t.Errorf("%s path %q not under dest %q", name, path, dest)
		}
	}
	if got := readFile(t, img.Kernel); got != "KERNEL" {
		t.Errorf("kernel contents = %q", got)
	}
	if got := readFile(t, img.RootFS); got != "ROOTFS" {
		t.Errorf("rootfs contents = %q", got)
	}
}

// TestPullTouchesLastUsed asserts Pull writes .krayt-last-used after a successful pull — the
// last-used signal `krayt image ls/prune` read for the base VM image cache.
func TestPullTouchesLastUsed(t *testing.T) {
	src, ref, want := fakeArtifact(t)
	dest := t.TempDir()

	if _, err := vmimage.Pull(context.Background(), src, ref, want, dest); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".krayt-last-used")); err != nil {
		t.Fatalf("Pull did not create the last-used sentinel: %v", err)
	}
}

func TestPullRejectsDigestMismatch(t *testing.T) {
	src, ref, _ := fakeArtifact(t)
	wrong := digest.FromString("not-the-image")
	dest := filepath.Join(t.TempDir(), "dest")

	_, err := vmimage.Pull(context.Background(), src, ref, wrong, dest)
	if err == nil {
		t.Fatal("expected digest mismatch error, got nil")
	}
	// A rejected artifact must not leave extracted content behind (§CVE-2026-50163: oras-go's
	// file store can write to disk before this function's caller ever sees the mismatch, so a
	// leftover destDir would keep whatever was extracted from a bad/tampered artifact).
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("destDir %s should have been removed after a digest mismatch; stat err = %v", dest, statErr)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
