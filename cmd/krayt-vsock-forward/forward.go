//go:build linux

package main

import (
	"io"
	"net"
	"sync"
)

// dialFunc dials the host's egress vsock port for one accepted TCP connection. A separate
// vsock.Dial per accepted TCP connection — see the package doc for why that (not
// multiplexing) is the contract.
type dialFunc func(port uint32) (net.Conn, error)

// forwarder is the dumb one-TCP-conn-to-one-vsock-conn pipe described in the package doc. It
// parses nothing: no HTTP, no TLS, no allowlist. That is the whole point — the
// adversarially-exposed parser moved to the host (internal/proxy), and nothing here may look
// at a byte or the design has regressed. Modeled on
// internal/provider/firecracker/bridge.go's connection-tracking/shutdown shape.
type forwarder struct {
	lis  net.Listener
	port uint32
	dial dialFunc

	mu     sync.Mutex
	conns  map[net.Conn]struct{} // in-flight connections (both TCP and vsock legs), closed on shutdown
	closed bool

	wg sync.WaitGroup
}

func newForwarder(lis net.Listener, port uint32, dial dialFunc) *forwarder {
	return &forwarder{lis: lis, port: port, dial: dial, conns: map[net.Conn]struct{}{}}
}

// serve accepts TCP connections until the listener is closed (by close()), handling each on
// its own goroutine so N concurrent accepts produce N distinct upstream vsock dials.
func (f *forwarder) serve() error {
	for {
		conn, err := f.lis.Accept()
		if err != nil {
			f.wg.Wait()
			return nil // the only expected error is the listener being closed by close()
		}
		if !f.beginHandler(conn) { // shutting down
			_ = conn.Close()
			continue
		}
		go func() {
			defer f.wg.Done()
			defer f.untrack(conn)
			f.handle(conn)
		}()
	}
}

// handle splices one accepted TCP connection to a freshly dialed vsock connection. If the
// dial fails, the client connection is simply closed — nothing here retries or interprets.
func (f *forwarder) handle(client net.Conn) {
	defer func() { _ = client.Close() }()

	upstream, err := f.dial(f.port)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	if !f.track(upstream) {
		return
	}
	defer f.untrack(upstream)

	// Splice both directions and return as soon as either finishes, so a hung peer cannot pin
	// the goroutine pair forever; the deferred Closes above unblock the other copy — which is
	// also how closing one side of a pair tears down the other.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// track registers a connection for shutdown, reporting false if the forwarder is already
// closing (in which case the caller must close it itself and give up).
func (f *forwarder) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.conns[c] = struct{}{}
	return true
}

// beginHandler is track plus the handler goroutine's wg.Add(1), done atomically under the same
// lock. Doing the Add here — rather than after track() returns, as a separate statement — closes
// a WaitGroup race: if it happened later, close() could acquire the lock, see closed==true (or
// even just run its own wg.Wait()) in the window between track() returning true and the Add
// happening, observing a zero counter and returning before this connection's handler goroutine
// is even launched. Adding under the same critical section that registers the connection means
// close() can never observe the connection as tracked without the Add having already happened.
func (f *forwarder) beginHandler(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.conns[c] = struct{}{}
	f.wg.Add(1)
	return true
}

func (f *forwarder) untrack(c net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.conns, c)
}

// close stops the listener, closes every in-flight connection, and waits for the goroutines to
// drain — closing only the listener would leave in-flight copies running forever.
func (f *forwarder) close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		f.wg.Wait()
		return nil
	}
	f.closed = true
	conns := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.conns = map[net.Conn]struct{}{}
	f.mu.Unlock()

	err := f.lis.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	f.wg.Wait()
	return err
}
