//go:build !windows

package cli

import "syscall"

// killSupervisor SIGTERMs the supervising `krayt run` process (manage.go's `stop` command); its
// signal handler cancels the run context, which guarantees VM teardown (§6.2, §7).
func killSupervisor(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// detachSysProcAttr puts the detached supervisor in its own session (Setsid) so it detaches from
// the controlling terminal and outlives the launching shell (§6.2, run.go's spawnDetached).
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
