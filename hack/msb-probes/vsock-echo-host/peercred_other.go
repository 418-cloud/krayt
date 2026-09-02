//go:build !linux && !darwin

package main

import "net"

// peerUID has no portable answer outside linux/darwin, the two OSes krayt hosts on; this stub
// exists only so vsock-echo-host still builds elsewhere.
func peerUID(*net.UnixConn) (uint32, bool) { return 0, false }
