//go:build darwin

package vfkit

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile makes a copy-on-write clone of src at dst using APFS clonefile(2), so each
// run gets an isolated rootfs that shares blocks with the base image until written
// (§6.3). dst must not already exist. On filesystems without clone support (e.g. a
// non-APFS volume) it falls back to a plain byte copy.
func cloneFile(src, dst string) error {
	err := unix.Clonefile(src, dst, 0)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) {
		return copyFile(src, dst)
	}
	return fmt.Errorf("clonefile %s -> %s: %w", src, dst, err)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
