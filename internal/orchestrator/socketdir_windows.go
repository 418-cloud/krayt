//go:build windows

package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
)

// runSocketNames is every basename bound inside a run's socket directory — kept here too (not
// only in the unix build) since runctl.go's control.sock still binds a real unix-domain socket
// on Windows (natively supported since Windows 10 1803 / Go 1.16), so the directory it lives in
// still has to exist even though askbridge's own ask channel is a named pipe that ignores it.
var runSocketNames = []string{"ask.sock", "control.sock"}

// runSocketDir on Windows has no sockaddr_un-length constraint to work around: the ask channel is
// a named pipe (askbridge.Listen, listen_windows.go), which lives in its own kernel namespace
// with no path-length budget shared with a filesystem directory, and msb's own `--vsock` route
// takes that pipe name directly rather than a host filesystem path (KRAYT_SPEC.md §12). There is
// therefore no short-root fallback to compute here, unlike socketdir_unix.go — this always
// returns the run's own directory, creating it if necessary for control.sock's sake (that
// listener IS a real unix-domain socket even on Windows, and needs somewhere to bind).
func runSocketDir(runDir, _ string) (dir string, cleanup func(), err error) {
	dir = filepath.Join(runDir, "ask")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("orchestrator: create %s: %w", dir, err)
	}
	return dir, func() {}, nil
}
