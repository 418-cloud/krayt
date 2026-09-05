//go:build windows

package orchestrator

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLockSlot takes a non-blocking, whole-file exclusive lock via LockFileEx
// (LOCKFILE_FAIL_IMMEDIATELY | LOCKFILE_EXCLUSIVE_LOCK) — Windows's equivalent of the unix
// implementation's flock(LOCK_EX|LOCK_NB) (climit_unix.go). A lock already held by another
// handle — including one whose process crashed, since Windows releases the lock when the last
// handle to the file closes — reports ERROR_LOCK_VIOLATION here rather than blocking, which the
// caller treats exactly like the unix EWOULDBLOCK/EAGAIN case: move on to the next slot instead
// of failing. The byte range [0,1) is locked rather than the whole (empty) file, since LockFileEx
// has no whole-file shorthand; Windows permits locking past EOF, so the slot files never need to
// actually contain that byte.
func tryLockSlot(f *os.File) (ok bool, err error) {
	ol := new(windows.Overlapped)
	lerr := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_FAIL_IMMEDIATELY|windows.LOCKFILE_EXCLUSIVE_LOCK,
		0, 1, 0, ol,
	)
	if lerr == nil {
		return true, nil
	}
	if lerr == windows.ERROR_LOCK_VIOLATION {
		return false, nil
	}
	return false, lerr
}

// unlockSlot releases the lock taken by tryLockSlot. Windows also drops it automatically when f's
// handle closes (including on crash), so this is belt-and-suspenders for the orderly-release path.
func unlockSlot(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
