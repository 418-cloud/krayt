package ask

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ErrMalformedSocket is wrapped into the error parseDialAddr returns for a KRAYT_ASK_SOCKET value
// that names the vsock:// scheme but is otherwise malformed. It is distinct from a dial/connection
// failure (bridge unreachable, fail mode, timeout) so a caller — krayt-ask's CLI front-end — can
// tell a misconfiguration apart from "no human is available" and fail with a usage error instead
// of the no-answer sentinel: a silent fallback here would turn a misconfiguration into "the agent
// quietly never asks", the exact failure mode §6.13 is designed to avoid.
var ErrMalformedSocket = errors.New("ask: malformed KRAYT_ASK_SOCKET")

// vsockScheme is the KRAYT_ASK_SOCKET URL form dial-ask-channel-over-vsock.md decision 2 adds
// alongside today's bare filesystem path. The env var keeps its name; a bare path still means a
// unix socket, so nothing that exists today breaks.
const vsockScheme = "vsock://"

// dialAddr is a parsed KRAYT_ASK_SOCKET value: either a bare unix socket path (today's form,
// unchanged) or a vsock://cid:port URL (the msb form, dial-ask-channel-over-vsock.md).
type dialAddr struct {
	unix bool
	path string // set when unix
	cid  uint32 // set when !unix
	port uint32 // set when !unix
}

// parseDialAddr parses a KRAYT_ASK_SOCKET value. A bare path is unix; "vsock://cid:port" is the
// new form. Anything naming the vsock:// scheme but not matching that shape is ErrMalformedSocket,
// not a silent fallback to unix (which would try to dial a nonsense filesystem path and merely
// look like "bridge unreachable").
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

// dial connects to addr, dispatching to a unix dial (OS-agnostic) or the platform's vsock dial
// (dialVsock, confined to a build-tagged file — see dial_vsock_linux.go / dial_vsock_other.go).
func dial(addr dialAddr) (net.Conn, error) {
	if addr.unix {
		return net.Dial("unix", addr.path)
	}
	return dialVsock(addr.cid, addr.port)
}
