// Package fake provides an in-process Provider implementation for tests. Its VM loops
// back a real gRPC guest server over an in-memory connection, so the orchestrator,
// protocol, patch, imagestore (host side), and CLI can be unit-tested on any OS without
// a real micro-VM (§14 test strategy).
package fake

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider"
)

const bufSize = 1 << 20 // 1 MiB in-memory pipe buffer

// Provider is an in-process provider.Provider. The optional Register hook lets a test
// install a custom guest service; by default it serves guest.NewService().
type Provider struct {
	// Register installs handlers on the per-VM gRPC server. If nil, the default
	// guest.Service is registered.
	Register func(s *grpc.Server)
}

// New returns a fake provider that serves the default guest service.
func New() *Provider { return &Provider{} }

// Create implements provider.Provider.
func (p *Provider) Create(_ context.Context, spec provider.VMSpec) (provider.VM, error) {
	return &vm{id: spec.ID, register: p.Register}, nil
}

type vm struct {
	id       string
	register func(s *grpc.Server)

	mu     sync.Mutex
	lis    *bufconn.Listener
	server *grpc.Server

	egressDir string // set the first time ListenEgress is called; removed by Destroy
}

// Start brings up the in-process gRPC guest server on a bufconn listener.
func (v *vm) Start(_ context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.server != nil {
		return fmt.Errorf("fake vm %s already started", v.id)
	}
	v.lis = bufconn.Listen(bufSize)
	v.server = grpc.NewServer()
	if v.register != nil {
		v.register(v.server)
	} else {
		pb.RegisterGuestAgentServer(v.server, guest.NewService())
	}
	go func() { _ = v.server.Serve(v.lis) }()
	return nil
}

// DialControl returns an in-memory net.Conn to the guest gRPC server. port is accepted
// to satisfy the interface (the real providers use it as the guest vsock port) but is
// not meaningful for the in-process loopback.
func (v *vm) DialControl(ctx context.Context, _ uint32) (net.Conn, error) {
	v.mu.Lock()
	lis := v.lis
	v.mu.Unlock()
	if lis == nil {
		return nil, fmt.Errorf("fake vm %s not started", v.id)
	}
	return lis.DialContext(ctx)
}

// Stop shuts the guest server down and releases the listener, leaving the VM in a
// clean stopped state (Start may be called again, and DialControl fails until it is).
func (v *vm) Stop(_ context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.server != nil {
		v.server.GracefulStop()
		v.server = nil
	}
	if v.lis != nil {
		_ = v.lis.Close()
		v.lis = nil
	}
	return nil
}

// ListenEgress implements provider.VM with a REAL unix-socket listener (not an in-memory
// pipe): the fd-passing path in the orchestrator's `krayt __egress-proxy` spawn is genuinely
// exercised by fakeProvider-backed tests this way, exactly as the vfkit/firecracker
// providers behave (§6.6, move-egress-proxy-to-host.md §5). The per-VM directory is removed
// by Destroy.
func (v *vm) ListenEgress(_ context.Context, _ uint32) (net.Listener, error) {
	dir, err := os.MkdirTemp("", "krayt-fake-egress-"+v.id+"-")
	if err != nil {
		return nil, fmt.Errorf("fake vm %s: create egress socket dir: %w", v.id, err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "egress.sock"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("fake vm %s: listen egress socket: %w", v.id, err)
	}
	v.mu.Lock()
	v.egressDir = dir
	v.mu.Unlock()
	return ln, nil
}

// Destroy tears the fake VM down. There is no CoW clone to remove, so it is just Stop plus
// removing the egress socket dir ListenEgress may have created.
func (v *vm) Destroy(ctx context.Context) error {
	err := v.Stop(ctx)
	v.mu.Lock()
	dir := v.egressDir
	v.egressDir = ""
	v.mu.Unlock()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	return err
}

func (v *vm) ID() string { return v.id }

// LogPaths implements provider.VM. The fake VM is an in-process gRPC loopback with no
// subprocess and no guest console, so there is nothing to point at.
func (v *vm) LogPaths() (providerLog, consoleLog string) { return "", "" }

// compile-time interface checks.
var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.VM       = (*vm)(nil)
)
