//go:build unix

// Package sockroot is the shared "verify or create" hardening check for a private, short-pathed
// directory that guards unix control sockets (§6.12): refuse a pre-existing hostile directory
// (wrong owner, wrong mode, or a symlink) rather than trusting it, and never chmod/chown a
// directory this process does not own.
//
// Extracted, not duplicated (dial-ask-channel-over-vsock.md decision 12): the check used to live
// twice, once each in the vfkit and Firecracker providers (harden-vfkit-socket-dir.md), and
// internal/askbridge's host socket directory needs the exact same property under msb — a third
// copy would have been the wrong move once a second consumer existed outside those two
// OS-specific, build-tagged packages. This package carries no opinion about WHERE the directory
// lives (that stays caller-specific — vfkit/Firecracker keep their own `/tmp/krayt-<uid>` root;
// askbridge uses the run's own private state directory instead); it only knows how to make one
// directory safe.
package sockroot

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Ensure makes root a private directory (mode 0700) owned by the current user, or fails closed.
// It uses Lstat (never following a symlink) + os.Mkdir (fails if the path already exists, unlike
// MkdirAll's silent no-op on a pre-existing directory), so a symlink or a foreign-owned/loose-mode
// directory pre-placed at root is refused rather than trusted. Callers never chmod/chown a
// directory they do not own — on any mismatch this returns an error naming the fix instead.
func Ensure(root string) error {
	fi, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(root, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				// Lost a race with a concurrent creator (root can be shared across every VM/run
				// this user starts) — re-validate whatever now exists rather than failing a
				// legitimate concurrent caller on a spurious EEXIST.
				return Ensure(root)
			}
			return fmt.Errorf("sockroot: create %s: %w", root, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("sockroot: stat %s: %w", root, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("sockroot: %s: cannot read owner/mode", root)
	}
	if !fi.IsDir() || int(st.Uid) != os.Getuid() || fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("sockroot: %s is not a private directory owned by this user "+
			"(mode %o, uid %d); refusing to place control sockets there — remove or fix it",
			root, fi.Mode().Perm(), st.Uid)
	}
	return nil
}
