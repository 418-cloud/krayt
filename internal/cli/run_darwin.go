//go:build darwin

package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/provider/vfkit"
	"github.com/418-cloud/krayt/internal/vmimage"
)

// defaultCmdline is the Linux kernel command line for the base image's bootloader: serial
// console on hvc0 and the rootfs on the first virtio-blk disk (§11.6, §12).
const defaultCmdline = "console=hvc0 root=/dev/vda"

// newRunDeps builds the macOS run dependencies: the vfkit provider plus the cached,
// digest-verified base VM image (kernel + initrd + raw rootfs). The base image must have
// been fetched with `krayt image pull` first (§11.4).
func newRunDeps() (runDeps, error) {
	dir, err := baseImageDir()
	if err != nil {
		return runDeps{}, err
	}
	base := provider.VMSpec{
		Kernel:  filepath.Join(dir, vmimage.FileKernel),
		Initrd:  filepath.Join(dir, vmimage.FileInitrd),
		RootFS:  filepath.Join(dir, vmimage.FileRootFS),
		Cmdline: defaultCmdline,
	}
	for _, p := range []string{base.Kernel, base.Initrd, base.RootFS} {
		if _, err := os.Stat(p); err != nil {
			return runDeps{}, fmt.Errorf("base VM image not cached (run `krayt image pull`): missing %s", filepath.Base(p))
		}
	}

	runState, err := runStateDir()
	if err != nil {
		return runDeps{}, err
	}
	return runDeps{
		provider: vfkit.New("", runState),
		baseVM:   base,
	}, nil
}

// baseImageDir returns the local cache directory of the pinned base VM image.
func baseImageDir() (string, error) {
	if vmimage.PinnedDigest == "" {
		return "", fmt.Errorf("no pinned base image digest (see HUMAN_TODO.md)")
	}
	return cacheDir(vmimage.PinnedRef, vmimage.PinnedDigest)
}

// runStateDir is the base directory for per-VM working state (CoW clones, sockets).
func runStateDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve cache dir: %w", err)
	}
	d := filepath.Join(base, "krayt", "vms")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("create vm state dir: %w", err)
	}
	return d, nil
}
