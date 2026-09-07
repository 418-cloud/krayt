//go:build windows

package askclient

import (
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

// pipePrefixes are the two spellings of the Win32 named-pipe namespace. `\\.\pipe\` is what
// askbridge.Listen (listen_windows.go) constructs; `\\?\pipe\` is the equivalent extended-length
// form the OS may hand back, so an address that round-tripped through a listener's Addr() is
// recognized either way.
var pipePrefixes = []string{`\\.\pipe\`, `\\?\pipe\`}

// dialLocal dials the ask channel's host endpoint on Windows, where — unlike every other target
// (dial_local_other.go) — that endpoint is not necessarily a unix socket. askbridge.Listen binds
// the ask channel to a named pipe here (listen_windows.go), which is what orchestrator hands msb
// as the `--vsock` route's host path, so a pipe name has to be dialed through go-winio rather
// than net.Dial("unix", ...), which cannot reach the pipe namespace at all.
//
// A path that is not a pipe name still falls through to a unix dial: unix sockets are natively
// supported on Windows (10 1803 / Go 1.16) and krayt still binds a real one for the run's
// control socket (socketdir_windows.go), so both transports are genuinely live on this platform.
func dialLocal(path string) (net.Conn, error) {
	for _, p := range pipePrefixes {
		if strings.HasPrefix(path, p) {
			return winio.DialPipe(path, nil)
		}
	}
	return net.Dial("unix", path)
}
