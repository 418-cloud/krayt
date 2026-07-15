//go:build linux

package firecracker

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

// TestBridgeCloseWithStalledHandshake pins down a teardown hang.
//
// A host→guest vsock connection is a unix dial plus a "CONNECT <port>" handshake (§6.12). The
// bridge dials with context.Background(), which carries no deadline, and it only registers the
// guest connection for shutdown AFTER that handshake completes. So a firecracker that accepts the
// unix connection but never answers would block the handler in readAck forever, where close()
// cannot reach it: close() shuts the listener and the connections it knows about, then waits for
// the handlers to drain — and this one never does.
//
// The blast radius is not a stray goroutine. bridge.close() is called from VM.Destroy, so a stall
// here hangs teardown and leaks the VM, its tap device and its multi-GiB rootfs clone.
//
// The stand-in for firecracker is a unix listener that accepts and then says nothing at all —
// which is exactly the case the handshake must not trust the peer to get right.
func TestBridgeCloseWithStalledHandshake(t *testing.T) {
	// Keep the test quick: the fix bounds the handshake, and this is the bound.
	old := handshakeTimeout
	handshakeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	dir := t.TempDir()
	uds := filepath.Join(dir, "v.sock")
	ctrl := filepath.Join(dir, "control.sock")

	// A "firecracker" that accepts the connection and never replies, never closes.
	silent, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			// Hold it open, answer nothing. Deliberately leaked until the test ends.
			_ = c
		}
	}()

	b, err := newBridge(ctrl, uds, 1024)
	if err != nil {
		t.Fatalf("newBridge: %v", err)
	}

	// A client connection drives the bridge into the handshake it will never finish.
	client, err := net.Dial("unix", ctrl)
	if err != nil {
		t.Fatalf("dial control socket: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Give the handler time to reach readAck and block there.
	time.Sleep(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- b.close() }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge.close() hung on a stalled handshake — VM.Destroy would hang with it, " +
			"leaking the VM, its tap and its rootfs clone")
	}
}
