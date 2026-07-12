//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

// bridge exposes the guest's control channel as a *plain* unix socket — one that a caller can
// dial and immediately speak gRPC on, with no handshake.
//
// It exists to keep one promise the rest of krayt already relies on. `krayt answer` and
// `krayt stop` reach a running VM from a *separate process* by dialing the socket path the
// orchestrator recorded from VM.ControlSocket(), via controlclient.DialSocket — which is a
// bare net.Dial("unix", …) (§6.2, §6.13). That works on vfkit, where the host-side vsock
// bridge really is a plain socket. On Firecracker it would not: firecracker's uds_path
// demands a "CONNECT <port>\n" handshake first (§6.12, vsock.go).
//
// Rather than teach the OS-agnostic core about a Firecracker-specific handshake, the provider
// absorbs the difference — which is exactly what the Provider seam is for. Each connection
// accepted here is paired with a freshly dialed, already-handshaken connection to the guest,
// and the two are spliced. The listener lives as long as the VM: the run's supervisor process
// owns both (§6.2).
type bridge struct {
	ln   net.Listener
	uds  string
	port uint32

	mu     sync.Mutex
	conns  map[net.Conn]struct{} // in-flight connections, closed on shutdown
	closed bool

	wg sync.WaitGroup
}

// newBridge starts listening on path and proxying to the guest's vsock port through uds.
func newBridge(path, uds string, port uint32) (*bridge, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("firecracker: listen on control socket %s: %w", path, err)
	}
	b := &bridge{ln: ln, uds: uds, port: port, conns: map[net.Conn]struct{}{}}
	b.wg.Add(1)
	go b.serve()
	return b, nil
}

func (b *bridge) serve() {
	defer b.wg.Done()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			// The only expected error is the listener being closed by close().
			return
		}
		if !b.track(conn) { // shutting down
			_ = conn.Close()
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer b.untrack(conn)
			b.handle(conn)
		}()
	}
}

// handle splices one accepted connection to a freshly handshaken guest connection. If the
// guest is not listening yet the dial fails and we simply close the client's connection — the
// caller retries, exactly as it would against a raw DialControl.
func (b *bridge) handle(client net.Conn) {
	defer func() { _ = client.Close() }()

	guest, err := dialVsock(context.Background(), b.uds, b.port)
	if err != nil {
		return
	}
	defer func() { _ = guest.Close() }()
	if !b.track(guest) {
		return
	}
	defer b.untrack(guest)

	// Splice both directions and return as soon as either finishes, so a hung peer cannot pin
	// the goroutine pair forever. The deferred Closes above unblock the other copy.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(guest, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, guest); done <- struct{}{} }()
	<-done
}

// track registers a connection for shutdown, reporting false if the bridge is already closing
// (in which case the caller must close it itself and give up).
func (b *bridge) track(c net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.conns[c] = struct{}{}
	return true
}

func (b *bridge) untrack(c net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.conns, c)
}

// close stops the listener, closes every in-flight connection, and waits for the goroutines to
// drain. Closing the connections is what unblocks the io.Copy pairs — closing only the
// listener would leave an idle-but-open control channel copying forever, and Destroy would
// hang behind it.
func (b *bridge) close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.wg.Wait()
		return nil
	}
	b.closed = true
	conns := make([]net.Conn, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.conns = map[net.Conn]struct{}{}
	b.mu.Unlock()

	err := b.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	b.wg.Wait()
	return err
}
