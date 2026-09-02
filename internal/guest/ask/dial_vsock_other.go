//go:build !linux

package ask

import (
	"fmt"
	"net"
	"runtime"
)

// dialVsock has no meaning outside a linux guest — krayt-ask's vsock transport only ever runs
// inside the sandboxed Linux VM (dial-ask-channel-over-vsock.md). This stub exists solely so
// go build ./... still compiles the OS-agnostic parsing/wire logic above on darwin; it is never
// reached in a real run, since a non-linux host never sets KRAYT_ASK_SOCKET to a vsock:// value.
func dialVsock(_, _ uint32) (net.Conn, error) {
	return nil, fmt.Errorf("ask: vsock dial not supported on %s (krayt-ask's vsock transport is linux-guest-only)", runtime.GOOS)
}
