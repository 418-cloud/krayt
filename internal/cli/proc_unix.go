//go:build !windows

package cli

import (
	"errors"
	"syscall"
)

// killSupervisor SIGTERMs the supervising `krayt run` process (manage.go's `stop` command); its
// signal handler cancels the run context, which guarantees VM teardown (§6.2, §7).
func killSupervisor(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// supervisorAlive reports whether pid still names a running process: signal 0 checks existence
// without delivering anything, and EPERM means the process exists but belongs to another user.
// A recycled pid reads as alive — the conservative direction for both callers (doctor reports
// nothing, stop signals as before).
func supervisorAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// detachSysProcAttr puts the detached supervisor in its own session (Setsid) so it detaches from
// the controlling terminal and outlives the launching shell (§6.2, run.go's spawnDetached).
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
