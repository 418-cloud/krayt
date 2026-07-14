//go:build linux

package firecracker

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile makes a copy of the base rootfs at dst so each run gets an isolated disk and
// never mutates the shared base image (§6.3). dst must not already exist.
//
// macOS gets this for free from APFS clonefile(2). Linux has no single equivalent, so:
//
//   - FICLONE (reflink) is tried first. On a filesystem that supports it — Btrfs, or XFS
//     with reflink=1, which is the mkfs default — this is an O(1) metadata operation and the
//     clone shares blocks with the base image until written, exactly like APFS.
//
//   - Otherwise we fall back to a full copy. ext4, the common case, has NO reflink support of
//     any kind, so there is no cheaper correct option: firecracker takes a raw block device,
//     so a qcow2 backing file is not available either. The copy is sparse-aware (see
//     copyFile), so holes in the base image stay holes.
//
// The practical consequence on ext4 is that each VM costs a real copy of the rootfs (~2 GiB
// for the current base image) in both time and disk. Putting the run dir on XFS or Btrfs
// removes that cost entirely with no code change — which is why the fallback is silent rather
// than fatal.
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	if err == nil {
		return out.Close()
	}
	// EOPNOTSUPP/EINVAL: filesystem has no reflink support (ext4, tmpfs, overlayfs).
	// EXDEV: src and dst are on different filesystems, so blocks cannot be shared.
	if !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EXDEV) {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("reflink %s -> %s: %w", src, dst, err)
	}

	if err := copyFile(in, out); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return out.Close()
}

// copyFile does a plain streaming copy. io.Copy uses copy_file_range(2) under the hood for
// two regular files on Linux, which keeps the copy in the kernel and preserves sparse holes
// rather than materialising them as zeroes.
func copyFile(in *os.File, out *os.File) error {
	_, err := io.Copy(out, in)
	return err
}
