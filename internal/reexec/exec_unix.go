//go:build !windows

package reexec

import "syscall"

// execSelf replaces the running image, keeping the same pid and the same inherited stdio — so the
// parent's exec.Cmd pipes, which are what the fake msb writes its scripted output to, carry
// straight through. It returns only if the exec failed.
func execSelf(path string, argv, env []string) {
	_ = syscall.Exec(path, argv, env)
}
