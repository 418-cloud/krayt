//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/provider/firecracker"
	"github.com/418-cloud/krayt/internal/vmimage"
)

// defaultCmdline is the Linux kernel command line for the base image. Firecracker gives the
// guest an 8250 serial port rather than vfkit's virtio-console, so the console is ttyS0, and
// the rootfs is the first virtio-blk disk (§11.6). The provider appends the rest — the per-VM
// `ifname=`/`ip=`/`nameserver=` that address the guest's NIC — since only it knows which tap the
// VM is wired to (§6.6).
const defaultCmdline = "console=ttyS0 root=/dev/vda"

// newRunDeps builds the Linux run dependencies: the firecracker provider plus the cached,
// digest-verified base VM image (kernel + initrd + raw rootfs). The base image must have been
// fetched with `krayt image pull` first (§11.4).
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
		provider: firecracker.New("", runState),
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

// runStateDir is the base directory for per-VM working state (CoW clones, scratch disks).
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
