//go:build windows

package askbridge

import (
	"fmt"
	"net"
	"path/filepath"

	"github.com/Microsoft/go-winio"
)

// Listen creates a Windows named pipe for the ask_human channel. msb's `--vsock
// HOST_PATH:PORT` route takes a named pipe path on Windows (`\\.\pipe\name`), not a
// filesystem-bound unix socket (expand-platforms-under-msb.md), so unlike listen_unix.go's dir
// is not a location anything gets created in: named pipes live in their own kernel namespace,
// with no sockaddr_un-style path-length limit to defend against and no directory to harden. dir
// is used only as the source of a name unique to this run — orchestrator's Windows
// runSocketDir (socketdir_windows.go) always hands this filepath.Join(runDir, "ask"), so its
// parent directory's basename is the run ID (KRAYT_SPEC.md §12).
//
// go-winio's default security descriptor (a nil PipeConfig) restricts the pipe to the creating
// user's token plus the usual SYSTEM/Administrators grants — narrower than "everyone", the same
// intent as the unix implementation's 0600 socket/0700 directory — though this is unverified on
// real hardware; see HUMAN_TODO.md's Windows `krayt run` entry.
func Listen(dir string) (net.Listener, error) {
	name := `\\.\pipe\krayt-ask-` + filepath.Base(filepath.Dir(dir))
	lis, err := winio.ListenPipe(name, nil)
	if err != nil {
		return nil, fmt.Errorf("askbridge: listen %s: %w", name, err)
	}
	return lis, nil
}
