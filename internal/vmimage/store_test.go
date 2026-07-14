package vmimage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
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

// multiArchArtifact packs a multi-arch base image: one artifact per architecture, gathered under
// an OCI image index whose entries carry a platform. The index digest is the single thing krayt
// pins (§11.5); which arch actually gets pulled is decided at pull time from the host.
//
// Each arch's payload is distinct ("KERNEL-<arch>") so a test can prove not just that the pull
// succeeded but that it extracted the RIGHT one — the failure mode worth guarding against is a
// silent wrong-arch pull, which would surface much later as an unbootable VM.
func multiArchArtifact(t *testing.T, arches ...string) (oras.ReadOnlyTarget, string, digest.Digest) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()

	manifests := make([]ocispec.Descriptor, 0, len(arches))
	for _, arch := range arches {
		layer := func(mediaType, title string, data []byte) ocispec.Descriptor {
			desc := content.NewDescriptorFromBytes(mediaType, data)
			desc.Annotations = map[string]string{ocispec.AnnotationTitle: title}
			if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
				t.Fatalf("push %s: %v", title, err)
			}
			return desc
		}
		layers := []ocispec.Descriptor{
			layer(vmimage.MediaTypeKernel, vmimage.FileKernel, []byte("KERNEL-"+arch)),
			layer(vmimage.MediaTypeInitrd, vmimage.FileInitrd, []byte("INITRD-"+arch)),
			layer(vmimage.MediaTypeRootFS, vmimage.FileRootFS, []byte("ROOTFS-"+arch)),
		}
		m, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1,
			"application/vnd.krayt.vmimage", oras.PackManifestOptions{Layers: layers})
		if err != nil {
			t.Fatalf("pack manifest (%s): %v", arch, err)
		}
		// The platform on the INDEX ENTRY is what selection matches on — these are artifacts with
		// custom media types, so there is no image config to infer an architecture from.
		m.Platform = &ocispec.Platform{OS: "linux", Architecture: arch}
		manifests = append(manifests, m)
	}

	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifests,
	}
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, raw)
	if err := store.Push(ctx, desc, bytes.NewReader(raw)); err != nil {
		t.Fatalf("push index: %v", err)
	}
	const ref = "multi"
	if err := store.Tag(ctx, desc, ref); err != nil {
		t.Fatalf("tag index: %v", err)
	}
	return store, ref, desc.Digest
}

// TestPullSelectsHostArchFromIndex is the multi-arch pin (§11.5): one pinned index digest,
// resolved to this host's architecture at pull time. It asserts the pull extracts THIS arch's
// content and, critically, that the digest check still validates against the INDEX digest — the
// thing pinned in pinned.go — even though oras.Copy internally descends to a child manifest.
func TestPullSelectsHostArchFromIndex(t *testing.T) {
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	src, ref, indexDigest := multiArchArtifact(t, runtime.GOARCH, other)

	dest := filepath.Join(t.TempDir(), "dest")
	img, err := vmimage.Pull(context.Background(), src, ref, indexDigest, dest)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	// The host's arch, not the other one.
	if got, want := readFile(t, img.Kernel), "KERNEL-"+runtime.GOARCH; got != want {
		t.Errorf("pulled the wrong architecture: kernel = %q, want %q", got, want)
	}
	// The reported digest must remain the index's — that is what pinned.go pins and what a user
	// would compare against, not the per-arch child oras.Copy actually descended into.
	if img.Digest != indexDigest.String() {
		t.Errorf("Image.Digest = %s, want the pinned index digest %s", img.Digest, indexDigest)
	}
}

// TestPullRejectsIndexWithoutHostArch: an index that has no variant for this host must fail with
// a clear error rather than pull someone else's architecture and hand the provider a kernel it
// cannot boot.
func TestPullRejectsIndexWithoutHostArch(t *testing.T) {
	src, ref, indexDigest := multiArchArtifact(t, "mips64") // deliberately not this host
	dest := filepath.Join(t.TempDir(), "dest")

	_, err := vmimage.Pull(context.Background(), src, ref, indexDigest, dest)
	if err == nil {
		t.Fatal("expected a no-matching-architecture error, got nil")
	}
	if !strings.Contains(err.Error(), "linux/"+runtime.GOARCH) {
		t.Errorf("error should name the missing architecture; got: %v", err)
	}
}

// twoFacedTarget answers the same reference with a different manifest each time it is resolved —
// a moving tag, or a registry that lies. Everything else (Fetch/Exists) is served honestly from a
// store that holds BOTH artifacts, so whichever manifest wins is genuinely pullable.
type twoFacedTarget struct {
	oras.ReadOnlyTarget // backing store, holds both artifacts

	mu     sync.Mutex
	calls  int
	first  ocispec.Descriptor // handed out on the first Resolve
	second ocispec.Descriptor // handed out on every Resolve after that
}

func (t *twoFacedTarget) Resolve(_ context.Context, _ string) (ocispec.Descriptor, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.calls == 1 {
		return t.first, nil
	}
	return t.second, nil
}

// TestPullVerifiesTheArtifactItActuallyCopies is the regression for a TOCTOU in the digest pin.
//
// The digest is a supply-chain control (§11.4): it is what makes "krayt image pull" trustworthy
// against a registry that has been tampered with. That guarantee only holds if the digest checked
// is the digest of the bytes extracted. It is easy to get this subtly wrong, because oras.Copy
// resolves the reference itself, internally: verifying a *separately* resolved descriptor leaves a
// window where a moving tag can answer the check with one manifest and the copy with another, so
// krayt would extract content it never verified while reporting the digest it did.
//
// Here the target hands out the good artifact on the first resolve and a substituted one on every
// resolve after, so a Pull that resolves twice will verify the good digest and then extract the
// bad content. The assertion is not "an error happened" but the invariant itself: whatever ends up
// on disk must be the artifact whose digest was verified.
func TestPullVerifiesTheArtifactItActuallyCopies(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	pack := func(marker string) ocispec.Descriptor {
		t.Helper()
		layer := func(mediaType, title string, data []byte) ocispec.Descriptor {
			desc := content.NewDescriptorFromBytes(mediaType, data)
			desc.Annotations = map[string]string{ocispec.AnnotationTitle: title}
			if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
				t.Fatalf("push %s: %v", title, err)
			}
			return desc
		}
		layers := []ocispec.Descriptor{
			layer(vmimage.MediaTypeKernel, vmimage.FileKernel, []byte("KERNEL-"+marker)),
			layer(vmimage.MediaTypeInitrd, vmimage.FileInitrd, []byte("INITRD-"+marker)),
			layer(vmimage.MediaTypeRootFS, vmimage.FileRootFS, []byte("ROOTFS-"+marker)),
		}
		m, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1,
			"application/vnd.krayt.vmimage", oras.PackManifestOptions{Layers: layers})
		if err != nil {
			t.Fatalf("pack manifest (%s): %v", marker, err)
		}
		return m
	}

	good := pack("good")
	substituted := pack("substituted")

	src := &twoFacedTarget{ReadOnlyTarget: store, first: good, second: substituted}
	dest := filepath.Join(t.TempDir(), "dest")

	// Pin the good artifact — exactly what pinned.go does.
	img, err := vmimage.Pull(ctx, src, "moving-tag", good.Digest, dest)
	if err != nil {
		// Refusing the pull outright is a perfectly acceptable outcome; extracting unverified
		// content is not. Nothing more to check.
		return
	}

	if got := readFile(t, img.Kernel); got != "KERNEL-good" {
		t.Fatalf("Pull extracted content it never verified: kernel = %q, want %q. The digest was "+
			"checked against one manifest and the copy took another — the digest pin is not "+
			"protecting the bytes on disk.", got, "KERNEL-good")
	}
	if img.Digest != good.Digest.String() {
		t.Errorf("Image.Digest = %s, want the verified digest %s", img.Digest, good.Digest)
	}
}
