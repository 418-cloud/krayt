//go:build linux

package main

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startEchoServer runs a tiny TCP echo server for use as the "upstream" a forwarder's dial
// func connects to, so a full TCP<->(fake vsock)<->TCP round trip is exercised.
func startEchoServer(t *testing.T) (addr string, done func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo server: %v", err)
	}
	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return lis.Addr().String(), func() {
		_ = lis.Close()
		wg.Wait()
	}
}

// TestForwarderSplicesBothWays proves one accepted TCP connection is spliced to one dialed
// upstream connection in both directions: bytes written on the TCP side are read back after
// bouncing off the echo upstream through the forwarder.
func TestForwarderSplicesBothWays(t *testing.T) {
	echoAddr, closeEcho := startEchoServer(t)
	defer closeEcho()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dial := func(uint32) (net.Conn, error) {
		var d net.Dialer
		return d.Dial("tcp", echoAddr)
	}
	f := newForwarder(lis, 1025, dial)
	go func() { _ = f.serve() }()
	defer func() { _ = f.close() }()

	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer func() { _ = conn.Close() }()

	want := []byte("hello through the vsock pipe")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestForwarderConcurrentAcceptsDistinctDials asserts N concurrent TCP accepts produce N
// distinct upstream dials — no multiplexing over one vsock connection (§Tests: "one vsock
// connection per accepted TCP connection").
func TestForwarderConcurrentAcceptsDistinctDials(t *testing.T) {
	const n = 8
	var dials int64

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Each dial gets its own private in-memory pipe so every connection is provably distinct
	// (not one shared upstream fanned out); nothing needs to flow over it for this test.
	dial := func(uint32) (net.Conn, error) {
		atomic.AddInt64(&dials, 1)
		client, server := net.Pipe()
		go func() { <-time.After(200 * time.Millisecond); _ = server.Close() }()
		return client, nil
	}
	f := newForwarder(lis, 1025, dial)
	go func() { _ = f.serve() }()
	defer func() { _ = f.close() }()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", lis.Addr().String())
			if err != nil {
				t.Errorf("dial forwarder: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			time.Sleep(20 * time.Millisecond) // give handle() time to reach dial()
		}()
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond) // let any trailing dial goroutines land

	if got := atomic.LoadInt64(&dials); got != n {
		t.Errorf("dials = %d, want %d (one vsock dial per accepted TCP connection)", got, n)
	}
}

// TestForwarderClosingOneSideTearsDownOther asserts that closing the TCP (client) side of a
// spliced pair unblocks and closes the upstream side too, rather than leaking a goroutine pair
// forever on a hung peer.
func TestForwarderClosingOneSideTearsDownOther(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	upClient, upServer := net.Pipe()
	closedCh := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		_, _ = upServer.Read(buf) // blocks until upClient is closed by the forwarder's teardown
		close(closedCh)
	}()
	dial := func(uint32) (net.Conn, error) { return upClient, nil }

	f := newForwarder(lis, 1025, dial)
	go func() { _ = f.serve() }()
	defer func() { _ = f.close() }()

	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	// Closing the client (TCP) side must propagate to the upstream (vsock-standin) side.
	_ = conn.Close()

	select {
	case <-closedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the TCP side did not tear down the upstream side in time")
	}
}
