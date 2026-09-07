//go:build windows

package cli

import (
	"os"
	"syscall"
)

// killSupervisor hard-terminates the supervising `krayt run` process via TerminateProcess
// (os.Process.Kill) — Windows has no SIGTERM equivalent Go can deliver to an arbitrary process
// (os.Process.Signal only supports os.Kill there), so unlike the unix implementation
// (proc_unix.go) this does NOT run the supervisor's own signal handler, which is what guarantees
// msb `stop`/`rm` teardown on the graceful path. A `krayt stop` on Windows can therefore leave the
// sandbox running until msb's own cleanup reclaims it or the operator runs `msb stop`/`msb rm` by
// hand — a documented residual (KRAYT_SPEC.md §12's Windows section), not a bug disguised as one.
func killSupervisor(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// detachedProcess (Win32's DETACHED_PROCESS CreateProcess flag, 0x00000008) is not exposed as a
// named constant by the standard syscall package on Windows, unlike CREATE_NEW_PROCESS_GROUP.
const detachedProcess = 0x00000008

// detachSysProcAttr puts the detached supervisor in its own process group with no console
// (DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP) — the Windows analogue of the unix
// implementation's Setsid (proc_unix.go): it detaches the process from the launching shell's
// console so it outlives that shell (§6.2, run.go's spawnDetached).
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess}
}
