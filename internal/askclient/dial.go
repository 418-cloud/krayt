package askclient

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrMalformedSocket is wrapped into the error parseDialAddr returns for a KRAYT_ASK_SOCKET value
// that names the vsock:// scheme but is otherwise malformed. It is distinct from a dial/connection
// failure (bridge unreachable, fail mode, timeout) so a caller — krayt-ask's CLI front-end — can
// tell a misconfiguration apart from "no human is available" and fail with a usage error instead
// of the no-answer sentinel: a silent fallback here would turn a misconfiguration into "the agent
// quietly never asks", the exact failure mode §6.13 is designed to avoid.
var ErrMalformedSocket = errors.New("askclient: malformed KRAYT_ASK_SOCKET")

// ErrVsockUnsupported is what dialVsock returns on a platform where the vsock:// transport has
// no meaning (see dial_vsock_other.go) — a well-formed vsock://cid:port value that simply can't
// be dialed here. Like ErrMalformedSocket, this is a configuration problem, not "bridge
// unreachable": a caller should treat it as a usage error, not silently fall back to the
// no-answer sentinel (§6.13).
var ErrVsockUnsupported = errors.New("askclient: vsock transport not supported on this platform")

// vsockScheme is the KRAYT_ASK_SOCKET URL form dial-ask-channel-over-vsock.md decision 2 adds
// alongside the bare filesystem path msb's guest→host vsock route requires under B1 (§6.13).
const vsockScheme = "vsock://"

// dialAddr is a parsed KRAYT_ASK_SOCKET value: either a bare unix socket path or a
// vsock://cid:port URL (the msb form).
type dialAddr struct {
	unix bool
	path string // set when unix
	cid  uint32 // set when !unix
	port uint32 // set when !unix
}

// parseDialAddr parses a KRAYT_ASK_SOCKET value. A bare path is unix; "vsock://cid:port" is the
// msb form. Anything naming the vsock:// scheme but not matching that shape is
// ErrMalformedSocket, not a silent fallback to unix (which would try to dial a nonsense
// filesystem path and merely look like "bridge unreachable").
func parseDialAddr(socket string) (dialAddr, error) {
	if !strings.HasPrefix(socket, vsockScheme) {
		return dialAddr{unix: true, path: socket}, nil
	}
	rest := strings.TrimPrefix(socket, vsockScheme)
	cidStr, portStr, ok := strings.Cut(rest, ":")
	if !ok {
		return dialAddr{}, fmt.Errorf("%w %q: want vsock://cid:port", ErrMalformedSocket, socket)
	}
	cid, err := strconv.ParseUint(cidStr, 10, 32)
	if err != nil {
		return dialAddr{}, fmt.Errorf("%w %q: bad cid: %v", ErrMalformedSocket, socket, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		return dialAddr{}, fmt.Errorf("%w %q: bad port: %v", ErrMalformedSocket, socket, err)
	}
	return dialAddr{cid: uint32(cid), port: uint32(port)}, nil
}
