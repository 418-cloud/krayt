//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// maxAckLen bounds the handshake reply so a misbehaving peer cannot make us read forever.
// "OK 4294967295\n" is 14 bytes; 64 is generous.
const maxAckLen = 64

// handshakeTimeout bounds the CONNECT handshake. A var, not a const, only so the test that covers
// the stalled-peer case does not have to sleep for it.
var handshakeTimeout = 10 * time.Second

// dialVsock opens a host→guest vsock channel through firecracker.
//
// This is the detail §6.12 gets wrong, so it is worth stating precisely. Firecracker does
// NOT put the guest on the host's AF_VSOCK: it deliberately bypasses the host's vhost stack
// and mediates between an AF_UNIX socket on the host and AF_VSOCK inside the guest. Per the
// Firecracker vsock documentation (verified against v1.16.1, docs/vsock.md), a host-initiated
// connection goes:
//
//  1. host connect()s to the device's uds_path;
//  2. host sends "CONNECT <port>\n";
//  3. firecracker forwards to whatever is listening on that AF_VSOCK port in the guest and
//     replies "OK <assigned_hostside_port>\n" — or closes the connection if nobody is
//     listening;
//  4. from then on the socket is a plain byte stream to the guest.
//
// The returned net.Conn is positioned immediately after the acknowledgement, so it is a clean
// gRPC transport (§6.12).
//
// A closed connection at step 3 is the normal state of affairs while the VM is still booting
// and the guest-agent has not yet called vsock.Listen. The caller retries: gRPC invokes the
// dialer again on every reconnect, which is what drives controlclient.WaitReady's boot poll.
func dialVsock(ctx context.Context, uds string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", uds)
	if err != nil {
		return nil, fmt.Errorf("firecracker vsock: dial %s: %w", uds, err)
	}

	// Bound the handshake ALWAYS, not just when ctx happens to carry a deadline.
	//
	// In practice firecracker answers immediately or closes the connection (it closes when nothing
	// is listening on the port, which is the normal state while the VM is still booting). But
	// "in practice" is doing too much work there: bridge.handle dials with context.Background(),
	// so with no deadline of our own, a firecracker that accepted the connection and then said
	// nothing would block readAck forever. That is not a stray goroutine — the bridge only
	// registers the guest connection for shutdown *after* the handshake, so close() cannot reach
	// it, and close() is called from VM.Destroy. Teardown would hang and leak the VM, its tap
	// device and its multi-GiB rootfs clone. Never trust the peer to end a wait for you.
	//
	// Take whichever bound comes first, so a caller with a tight deadline still wins and one with
	// a long-running ctx (WaitReady's minutes-long boot poll) does not lose the bound entirely.
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("firecracker vsock: send CONNECT: %w", err)
	}

	ack, err := readAck(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("firecracker vsock: guest not listening on port %d yet: %w", port, err)
	}
	if !strings.HasPrefix(ack, "OK ") {
		_ = conn.Close()
		return nil, fmt.Errorf("firecracker vsock: unexpected handshake reply %q (want \"OK <port>\")", ack)
	}

	// Clear the handshake deadline — the connection now belongs to gRPC, which manages its own
	// per-RPC timeouts. Leaving it set would break every later read.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("firecracker vsock: clear deadline: %w", err)
	}
	return conn, nil
}

// readAck reads the "\n"-terminated handshake reply one byte at a time, leaving everything
// after it unread on the connection. A buffered reader would be wrong here: it could over-read
// past the newline into the guest's first gRPC bytes and silently truncate the stream, since
// the reader is discarded once the handshake is done.
func readAck(conn net.Conn) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for range maxAckLen {
		if _, err := conn.Read(buf); err != nil {
			return "", err
		}
		if buf[0] == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(buf[0])
	}
	return "", fmt.Errorf("handshake reply exceeded %d bytes", maxAckLen)
}
