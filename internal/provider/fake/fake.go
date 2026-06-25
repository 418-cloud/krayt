// Package fake provides an in-process Provider implementation for tests. Its VM loops
// back a real gRPC guest server over an in-memory connection, so the orchestrator,
// protocol, patch, imagestore (host side), and CLI can be unit-tested on any OS without
// a real micro-VM (§14 test strategy).
package fake

import (
	"context"
	"fmt"
	"net"
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

func (v *vm) Stop(_ context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.server != nil {
		v.server.GracefulStop()
		v.server = nil
	}
	return nil
}

func (v *vm) Destroy(ctx context.Context) error {
	if err := v.Stop(ctx); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.lis != nil {
		_ = v.lis.Close()
		v.lis = nil
	}
	return nil
}

func (v *vm) ID() string { return v.id }

// compile-time interface checks.
var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.VM       = (*vm)(nil)
)
