//go:build linux

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reports the uid of the process on the other end of conn, via SO_PEERCRED — used to
// answer dial-ask-channel-over-vsock.md decision 10's open question: does msb's local backend
// open the bridged host socket as the invoking user, or as root/a system daemon under another
// uid? (0700/0600 alone don't tell you that; only inspecting who actually connected does.)
func peerUID(conn *net.UnixConn) (uint32, bool) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var uid uint32
	var ok bool
	_ = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			return
		}
		uid, ok = cred.Uid, true
	})
	return uid, ok
}
