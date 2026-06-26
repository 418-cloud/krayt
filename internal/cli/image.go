package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/vmimage"
)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage the base micro-VM image",
	}
	cmd.AddCommand(newImagePullCmd())
	return cmd
}

func newImagePullCmd() *cobra.Command {
	var ref, dig string
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Pull and verify the base VM image (kernel + initrd + rootfs)",
		Long: "Pulls the digest-addressed base VM image OCI artifact, verifies its digest, " +
			"and caches it locally for CoW cloning (§11.4).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ref == "" {
				ref = vmimage.PinnedRef
			}
			want := vmimage.PinnedDigest
			if dig != "" {
				want = digest.Digest(dig)
				if err := want.Validate(); err != nil {
					return fmt.Errorf("invalid --digest %q: %w", dig, err)
				}
			}
			return runImagePull(cmd, ref, want)
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "registry reference (default: pinned "+vmimage.PinnedRef+")")
	cmd.Flags().StringVar(&dig, "digest", "", "expected manifest digest (default: pinned)")
	return cmd
}

func runImagePull(cmd *cobra.Command, ref string, want digest.Digest) error {
	w := cmd.OutOrStdout()
	if want == "" {
		if _, err := fmt.Fprintln(w, "warning: no pinned digest set — pulling without digest verification (see HUMAN_TODO.md)"); err != nil {
			return err
		}
	}

	dest, err := cacheDir(ref, want)
	if err != nil {
		return err
	}
	src, err := vmimage.Open(ref)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "pulling %s …\n", ref); err != nil {
		return err
	}
	img, err := vmimage.Pull(cmd.Context(), src, ref, want, dest)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "verified %s\n  kernel: %s\n  initrd: %s\n  rootfs: %s\n",
		img.Digest, img.Kernel, img.Initrd, img.RootFS)
	return err
}

// cacheDir returns the local cache directory for a base image, keyed by digest when
// known (otherwise by a sanitized ref) so different images never collide.
func cacheDir(ref string, want digest.Digest) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve cache dir: %w", err)
	}
	key := want.Encoded()
	if key == "" {
		key = sanitizeRef(ref)
	}
	return filepath.Join(base, "krayt", "vmimage", key), nil
}

func sanitizeRef(ref string) string {
	r := strings.NewReplacer("/", "_", ":", "_", "@", "_")
	return r.Replace(ref)
}

// baseImageCheck reports whether the base VM image is pinned and cached locally. It is
// optional (a warning, not a failure): the pin is filled in after CI first publishes the
// image, and `krayt image pull` populates the cache (§11.4).
func baseImageCheck() checkResult {
	c := checkResult{name: "base VM image", optional: true}
	if vmimage.PinnedDigest == "" {
		c.detail = "no pinned digest yet (image not published; see HUMAN_TODO.md)"
		return c
	}
	dir, err := cacheDir(vmimage.PinnedRef, vmimage.PinnedDigest)
	if err != nil {
		c.detail = err.Error()
		return c
	}
	// A boot needs all three artifacts, so a partial cache is not "cached".
	for _, f := range []string{vmimage.FileKernel, vmimage.FileInitrd, vmimage.FileRootFS} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			c.detail = "pinned " + vmimage.PinnedDigest.String() + " not cached — run `krayt image pull`"
			return c
		}
	}
	c.ok = true
	c.detail = "cached " + vmimage.PinnedDigest.String()
	return c
}
