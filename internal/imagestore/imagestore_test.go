package imagestore_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"

	"github.com/418-cloud/krayt/internal/imagestore"
)

// TestAcquireExportCache builds a minimal OCI image in an in-memory source (no registry,
// no network), acquires it into a digest-keyed cache, and checks blob enumeration, archive
// export (full + incremental), and cache reuse (§6.11).
func TestAcquireExportCache(t *testing.T) {
	ctx := context.Background()
	src := memory.New()

	configBlob := []byte(`{"architecture":"arm64","os":"linux"}`)
	configDesc := push(ctx, t, src, ocispec.MediaTypeImageConfig, configBlob)
	layerBlob := []byte("a fake layer")
	layerDesc := push(ctx, t, src, ocispec.MediaTypeImageLayer, layerBlob)

	manifestBlob, _ := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	})
	manifestDesc := push(ctx, t, src, ocispec.MediaTypeImageManifest, manifestBlob)
	if err := src.Tag(ctx, manifestDesc, "latest"); err != nil {
		t.Fatalf("tag: %v", err)
	}

	cache := t.TempDir()
	img, err := imagestore.Acquire(ctx, src, "latest", cache)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if img.Digest != manifestDesc.Digest.String() {
		t.Errorf("digest = %q, want %q", img.Digest, manifestDesc.Digest.String())
	}

	// Blob enumeration must list config, layer, and manifest.
	digs, err := img.BlobDigests()
	if err != nil {
		t.Fatalf("BlobDigests: %v", err)
	}
	for _, want := range []string{configDesc.Digest.String(), layerDesc.Digest.String(), manifestDesc.Digest.String()} {
		if !contains(digs, want) {
			t.Errorf("BlobDigests %v missing %s", digs, want)
		}
	}

	// Full archive: valid OCI layout tar containing metadata + all blobs.
	full := archiveNames(t, img, nil)
	for _, want := range []string{"oci-layout", "index.json"} {
		if !full[want] {
			t.Errorf("full archive missing %s", want)
		}
	}
	if !full[blobPath(layerDesc.Digest.String())] {
		t.Errorf("full archive missing layer blob")
	}

	// Incremental archive: omit the layer (guest already has it), keep metadata + manifest.
	only := map[string]bool{configDesc.Digest.String(): true, manifestDesc.Digest.String(): true}
	inc := archiveNames(t, img, only)
	if inc[blobPath(layerDesc.Digest.String())] {
		t.Errorf("incremental archive should have skipped the layer blob")
	}
	if !inc["index.json"] || !inc[blobPath(manifestDesc.Digest.String())] {
		t.Errorf("incremental archive missing required metadata/manifest")
	}

	// Cache reuse: a second Acquire must not rewrite the layout (sentinel survives).
	sentinel := filepath.Join(img.Dir, "krayt-cache-sentinel")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatalf("sentinel: %v", err)
	}
	if _, err := imagestore.Acquire(ctx, src, "latest", cache); err != nil {
		t.Fatalf("Acquire (cached): %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("cache was rewritten on second Acquire (sentinel gone): %v", err)
	}
}

func TestMissing(t *testing.T) {
	have := map[string]bool{"sha256:a": true}
	got := imagestore.Missing([]string{"sha256:a", "sha256:b", "sha256:c"}, have)
	want := []string{"sha256:b", "sha256:c"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Missing = %v, want %v", got, want)
	}
}

// --- helpers ---

func push(ctx context.Context, t *testing.T, store *memory.Store, mt string, blob []byte) ocispec.Descriptor {
	t.Helper()
	desc := content.NewDescriptorFromBytes(mt, blob)
	if err := store.Push(ctx, desc, bytes.NewReader(blob)); err != nil {
		t.Fatalf("push %s: %v", mt, err)
	}
	return desc
}

// archiveNames returns the set of tar entry names produced by WriteArchive(only).
func archiveNames(t *testing.T, img *imagestore.Image, only map[string]bool) map[string]bool {
	t.Helper()
	var buf bytes.Buffer
	if err := img.WriteArchive(&buf, only); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		names[hdr.Name] = true
	}
	return names
}

func blobPath(dig string) string {
	// "sha256:<hex>" -> "blobs/sha256/<hex>"
	for i := 0; i < len(dig); i++ {
		if dig[i] == ':' {
			return "blobs/" + dig[:i] + "/" + dig[i+1:]
		}
	}
	return dig
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
