package fake_test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/provider/fake"
)

// TestHelloRoundTrip is the Phase 0 "Done when" check: a Hello RPC round-trips host↔guest
// over the fake provider's in-process gRPC loopback (§14, Phase 0).
func TestHelloRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p := fake.New()
	vm, err := p.Create(ctx, provider.VMSpec{ID: "run_test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := vm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = vm.Destroy(context.Background()) })

	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(dctx context.Context, _ string) (net.Conn, error) {
			return vm.DialControl(dctx, provider.ControlPort)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := pb.NewGuestAgentClient(conn)
	resp, err := client.Hello(ctx, &pb.HelloRequest{ClientVersion: "test"})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if resp.GetAgentVersion() != guest.Version {
		t.Errorf("agent_version = %q, want %q", resp.GetAgentVersion(), guest.Version)
	}
}
