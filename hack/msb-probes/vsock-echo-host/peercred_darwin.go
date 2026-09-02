//go:build darwin

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reports the uid of the process on the other end of conn, via LOCAL_PEERCRED — the
// macOS counterpart of Linux's SO_PEERCRED, answering the same question (dial-ask-channel-over-
// vsock.md decision 10): who actually connects to the host socket msb bridges the guest's vsock
// dial to.
func peerUID(conn *net.UnixConn) (uint32, bool) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var uid uint32
	var ok bool
	_ = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			return
		}
		uid, ok = cred.Uid, true
	})
	return uid, ok
}
