//go:build linux

package ask

import (
	"net"

	"github.com/mdlayher/vsock"
)

// dialVsock is the real AF_VSOCK dial krayt-ask uses to reach the host-side ask bridge directly
// (dial-ask-channel-over-vsock.md) — no guest daemon in between. This is the linux-guest-only
// half of the transport; the parsing above stays OS-agnostic so go build ./... still covers
// darwin (see dial_vsock_other.go).
func dialVsock(cid, port uint32) (net.Conn, error) {
	return vsock.Dial(cid, port, nil)
}
