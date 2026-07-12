// Package imagecache is the shared bookkeeping over krayt's two host-side, digest-keyed
// image caches: the base micro-VM image (internal/vmimage, §11.4) and the user's agent
// image (internal/imagestore, §6.11). Both are directory-per-digest trees that grow
// unbounded; this package walks one, sums sizes, reads a last-used sentinel, and removes
// an entry — the primitives behind `krayt image ls/rm/prune`.
//
// It knows nothing about either cache's contents, only the shape they share, so it never
// imports vmimage or imagestore (they import it, to touch the sentinel on acquire).
package imagecache

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/opencontainers/go-digest"
)

// SentinelName is the per-entry last-used marker. Its own mtime is the signal — the file
// is empty; vmimage.Pull / imagestore.Acquire refresh it on every acquire (best-effort),
// and ls/prune read it (falling back to the directory mtime when it's absent).
const SentinelName = ".krayt-last-used"

// Entry is one cached image: a single digest-keyed directory under a cache root.
type Entry struct {
	Digest   string    // full "sha256:<hex>", or "" for a non-digest-named directory
	Dir      string    // cache directory
	SizeB    int64     // recursive size
	LastUsed time.Time // sentinel mtime, or dir mtime if no sentinel
}

// List returns every entry directly under root. Non-digest-named entries (e.g. stale
// sanitized-ref vmimage dirs from before a pin was set) are still listed, with Digest == "".
// A missing root is not an error — it just means nothing has been cached there yet.
func List(root string) ([]Entry, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("imagecache: list %q: %w", root, err)
	}
	var out []Entry
	for _, de := range ents {
		if !de.IsDir() {
			continue // caches only ever hold digest directories at the top level
		}
		dir := filepath.Join(root, de.Name())
		size, err := dirSize(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{
			Digest:   digestForName(de.Name()),
			Dir:      dir,
			SizeB:    size,
			LastUsed: lastUsed(dir),
		})
	}
	return out, nil
}

// Remove deletes an entry's directory.
func Remove(e Entry) error {
	if err := os.RemoveAll(e.Dir); err != nil {
		return fmt.Errorf("imagecache: remove %q: %w", e.Dir, err)
	}
	return nil
}

// Touch creates or refreshes an entry's last-used sentinel (best-effort; caller decides
// whether to surface an error). It sets the sentinel's mtime explicitly so a refresh
// always advances it, even when the file already existed.
func Touch(dir string) error {
	p := filepath.Join(dir, SentinelName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(p, now, now)
}

// digestForName reconstructs the full digest a cache directory is keyed by. Both caches
// name directories by the digest's bare encoded hex (digest.Encoded()), so a well-formed
// sha256 hex becomes "sha256:<hex>"; anything else (a sanitized ref) yields "".
func digestForName(name string) string {
	d := digest.NewDigestFromEncoded(digest.SHA256, name)
	if d.Validate() == nil {
		return d.String()
	}
	return ""
}

// lastUsed reads the entry's last-used time: the sentinel's mtime when present, else the
// directory's own mtime (an image cached before the sentinel existed — a slightly stale
// first reading that self-corrects on next use).
func lastUsed(dir string) time.Time {
	if fi, err := os.Stat(filepath.Join(dir, SentinelName)); err == nil {
		return fi.ModTime()
	}
	if fi, err := os.Stat(dir); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// dirSize sums the sizes of every regular file under dir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("imagecache: size %q: %w", dir, err)
	}
	return total, nil
}
