//go:build !windows

package askclient

import "net"

// dialLocal dials the ask channel's host endpoint. Everywhere but Windows that endpoint is a
// plain unix socket — askbridge.Listen (listen_unix.go) binds dir/ask.sock — so a bare
// KRAYT_ASK_SOCKET path is dialed as one.
func dialLocal(path string) (net.Conn, error) {
	return net.Dial("unix", path)
}
