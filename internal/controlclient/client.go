// Package controlclient is the host side of the vsock control protocol. It dials the
// guest-agent through the provider's DialControl seam and speaks plain gRPC, so it is
// identical across the vfkit, vz, and firecracker backends (§6.5, §6.12). It also
// implements the boot-readiness contract: poll Hello until the guest answers (§11.6).
package controlclient

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider"
)

// ClientVersion is the host control-client version sent in the Hello handshake.
const ClientVersion = "0.0.0-dev"

// Client is a host-side gRPC connection to a VM's guest-agent.
type Client struct {
	conn  *grpc.ClientConn
	Agent pb.GuestAgentClient
}

// Dial opens a gRPC connection to the guest-agent over the VM's control channel. The
// transport is the provider's DialControl conn (a vfkit unix socket, a vz vsock device,
// or an AF_VSOCK connection); insecure credentials are correct because the link reaches
// exactly one VM and is not on any network (§6.12).
func Dial(vm provider.VM, port uint32) (*Client, error) {
	conn, err := grpc.NewClient(
		"passthrough:///krayt-guest",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return vm.DialControl(ctx, port)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("controlclient: dial: %w", err)
	}
	return &Client{conn: conn, Agent: pb.NewGuestAgentClient(conn)}, nil
}

// Close releases the gRPC connection.
func (c *Client) Close() error { return c.conn.Close() }

// WaitReady polls Hello until the guest-agent answers or the deadline passes, then
// returns its HelloResponse. This is the host's "VM ready" signal: vfkit's process being
// up does not mean the guest-agent is listening yet, so we retry until it is (§11.6).
func (c *Client) WaitReady(ctx context.Context, timeout, interval time.Duration) (*pb.HelloResponse, error) {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var lastErr error
	for {
		resp, err := c.Agent.Hello(ctx, &pb.HelloRequest{ClientVersion: ClientVersion})
		if err == nil {
			return resp, nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("controlclient: guest not ready within %s: %w", timeout, lastErr)
		case <-time.After(interval):
		}
	}
}
