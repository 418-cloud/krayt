package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/418-cloud/krayt/internal/imagecache"
	"github.com/418-cloud/krayt/internal/vmimage"
)

// Cache kinds shown in the `krayt image ls` KIND column.
const (
	kindVMImage   = "vmimage"
	kindContainer = "container"
)

// cachedImage pairs an imagecache.Entry with the cache it came from — the unit `image
// ls/rm/prune` reason about.
type cachedImage struct {
	kind   string // kindVMImage | kindContainer
	pinned bool   // vmimage entry matching vmimage.PinnedDigest (never auto-removed)
	entry  imagecache.Entry
}

// listCachedImages enumerates both cache roots into one list: every base VM image, then
// every user/container image. The pinned base image (if any) is flagged.
func listCachedImages() ([]cachedImage, error) {
	vmRoot, storeRoot, err := imageCacheRoots()
	if err != nil {
		return nil, err
	}
	var out []cachedImage
	vms, err := imagecache.List(vmRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range vms {
		out = append(out, cachedImage{kind: kindVMImage, pinned: isPinnedVMImage(e), entry: e})
	}
	conts, err := imagecache.List(storeRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range conts {
		out = append(out, cachedImage{kind: kindContainer, entry: e})
	}
	return out, nil
}

// isPinnedVMImage reports whether a vmimage entry is the currently-pinned base image.
func isPinnedVMImage(e imagecache.Entry) bool {
	return vmimage.PinnedDigest != "" && e.Digest == vmimage.PinnedDigest.String()
}

// resolveCachedImage finds the single cached image whose digest equals or is prefixed by
// prefix (docker-rmi style), across both roots. Non-digest-named entries are not addressable
// by digest and are skipped. It errors — without touching anything — on no match or an
// ambiguous prefix.
func resolveCachedImage(prefix string) (cachedImage, error) {
	all, err := listCachedImages()
	if err != nil {
		return cachedImage{}, err
	}
	var matches []cachedImage
	for _, ci := range all {
		if ci.entry.Digest == "" {
			continue
		}
		enc := digestEncoded(ci.entry.Digest)
		if ci.entry.Digest == prefix || strings.HasPrefix(enc, prefix) {
			matches = append(matches, ci)
		}
	}
	switch len(matches) {
	case 0:
		return cachedImage{}, fmt.Errorf("no cached image matches %q", prefix)
	case 1:
		return matches[0], nil
	default:
		var shorts []string
		for _, m := range matches {
			shorts = append(shorts, shortDigest(m.entry.Digest))
		}
		return cachedImage{}, fmt.Errorf("%q is ambiguous: matches %d images (%s)", prefix, len(matches), strings.Join(shorts, ", "))
	}
}

// refFor returns a best-effort human-readable reference for a cache entry, or "-" when none
// is recoverable. The pinned base image reports its pinned ref; a container reports the
// ref-name annotation oras recorded in the layout's index.json, if present.
func refFor(ci cachedImage) string {
	if ci.pinned {
		return vmimage.PinnedRef
	}
	if ci.kind == kindContainer {
		if ref := indexRefName(ci.entry.Dir); ref != "" {
			return ref
		}
	}
	return "-"
}

// indexRefName reads the org.opencontainers.image.ref.name annotation oras writes into an
// OCI layout's index.json (the tag the image was acquired under). Best-effort: any read/parse
// failure yields "".
func indexRefName(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return ""
	}
	var idx struct {
		Manifests []struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if json.Unmarshal(b, &idx) != nil {
		return ""
	}
	for _, m := range idx.Manifests {
		if ref := m.Annotations["org.opencontainers.image.ref.name"]; ref != "" {
			return ref
		}
	}
	return ""
}

// digestEncoded returns the bare hex of a "sha256:<hex>" digest (the part cache dirs are
// named by), or the string unchanged if it isn't a well-formed digest.
func digestEncoded(d string) string {
	dig := digest.Digest(d)
	if dig.Validate() != nil {
		return d
	}
	return dig.Encoded()
}

// shortDigest is the 12-hex-char display form of a digest, or "-" for a non-digest entry.
func shortDigest(d string) string {
	enc := digestEncoded(d)
	if enc == "" {
		return "-"
	}
	if len(enc) > 12 {
		return enc[:12]
	}
	return enc
}

// humanSize renders a byte count as a compact IEC size (e.g. 412MiB, 1.8GiB) — one decimal
// place under 10 units, none above, matching how the `ls` example reads.
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	val := float64(b) / float64(div)
	if val < 10 {
		return fmt.Sprintf("%.1f%s", val, units[exp])
	}
	return fmt.Sprintf("%.0f%s", val, units[exp])
}
