//go:build !windows

package orchestrator

import (
	"os"
	"syscall"
)

// tryLockSlot attempts a non-blocking exclusive flock on f. ok=false with a nil error means the
// slot is held by someone else (the caller tries the next one); a non-nil error is persistent
// (e.g. a filesystem that doesn't support flock) and should abort rather than be retried by
// polling.
func tryLockSlot(f *os.File) (ok bool, err error) {
	lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if lerr == nil {
		return true, nil
	}
	if lerr == syscall.EWOULDBLOCK || lerr == syscall.EAGAIN {
		return false, nil
	}
	return false, lerr
}

// unlockSlot releases the flock taken by tryLockSlot. The OS also drops it automatically when f
// closes (including on crash), so this is belt-and-suspenders for the orderly-release path.
func unlockSlot(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
