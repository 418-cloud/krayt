//go:build windows

package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
)

// runSocketDir on Windows still has to work around the same sockaddr_un-length limit as Unix:
// the ask channel is a named pipe (askbridge.Listen, listen_windows.go) with no path budget of
// its own, but control.sock (runctl.go) IS a real unix-domain socket even on Windows (natively
// supported since Windows 10 1803 / Go 1.16), and Windows' AF_UNIX (afunix.h) caps sun_path at
// the same ~108 bytes POSIX does. A real windows-latest run hit exactly this: a deep runner temp
// path pushed control.sock's bind over the limit and failed with the same unhelpful EINVAL/"bind:
// invalid argument" Unix gives. So the same short-root fallback as socketdir_unix.go applies.
// It skips sockroot's unix-specific hardening (setuid/symlink attacks): os.TempDir() on Windows
// already resolves to a directory private to the invoking user (AppData\Local\Temp), unlike
// /tmp, so there is no shared-root attacker here to guard against.
//
// Unlike listen_unix.go's askbridge.Listen, which creates and hardens its directory via
// sockroot.Ensure, listen_windows.go's named pipe never touches the filesystem — so this function
// must create dir itself, in both the preferred and fallback case, for control.sock's sake.
func runSocketDir(runDir, runID string) (dir string, cleanup func(), err error) {
	noop := func() {}

	preferred := filepath.Join(runDir, "ask")
	if socketDirFits(preferred) {
		if err := os.MkdirAll(preferred, 0o700); err != nil {
			return "", noop, fmt.Errorf("orchestrator: create %s: %w", preferred, err)
		}
		return preferred, noop, nil
	}

	fallback := filepath.Join(os.TempDir(), "krayt-ask", runID)
	if !socketDirFits(fallback) {
		return "", noop, fmt.Errorf("orchestrator: no unix socket path short enough for run %s: "+
			"%q needs %d bytes, over the %d-byte limit, and the fallback %q does not fit either",
			runID, preferred, longestSocketPathLen(preferred), maxUnixSocketPath, fallback)
	}
	if err := os.MkdirAll(fallback, 0o700); err != nil {
		return "", noop, fmt.Errorf("orchestrator: create %s: %w", fallback, err)
	}
	return fallback, func() { _ = os.RemoveAll(fallback) }, nil
}
