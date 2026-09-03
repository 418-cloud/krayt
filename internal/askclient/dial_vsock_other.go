//go:build !linux

package askclient

import (
	"fmt"
	"net"
	"runtime"
)

// dialVsock has no meaning outside a linux guest — krayt-ask's vsock transport only ever runs
// inside the sandboxed Linux VM (dial-ask-channel-over-vsock.md), and the embedded guest binary
// is always cross-compiled with GOOS=linux (see Makefile), so this stub is never reached by a
// shipped krayt-ask. It exists so go build ./... still compiles the OS-agnostic parsing/wire
// logic in this package on darwin. It still wraps ErrVsockUnsupported, not a bare error, so that
// if it is ever reached (e.g. cmd/krayt-ask built and run directly on a non-linux host), a
// well-formed vsock:// value fails loudly as a usage error instead of masquerading as "no human
// available".
func dialVsock(_, _ uint32) (net.Conn, error) {
	return nil, fmt.Errorf("%w: %s", ErrVsockUnsupported, runtime.GOOS)
}
