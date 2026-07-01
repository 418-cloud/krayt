// Package controlclient is the host side of the vsock control protocol. It dials the
// guest-agent through the provider's DialControl seam and speaks plain gRPC, so it is
// identical across the vfkit, vz, and firecracker backends (§6.5, §6.12). It also
// implements the boot-readiness contract: poll Hello until the guest answers (§11.6).
package controlclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider"
)

// chunkSize bounds each Chunk on the wire; matches the guest side (§6.5).
const chunkSize = 1 << 20 // 1 MiB

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

// DialSocket opens a gRPC connection straight to a guest control socket path (the unix socket
// a vfkit VM bridges vsock to). It backs `krayt answer`/`stop`, which reach a running VM's
// guest from a separate invocation using the socket path recorded in the run's meta.json —
// the daemon-less, process-agnostic path of §6.2/§6.13.
func DialSocket(socketPath string) (*Client, error) {
	conn, err := grpc.NewClient(
		"passthrough:///krayt-guest",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("controlclient: dial socket: %w", err)
	}
	return &Client{conn: conn, Agent: pb.NewGuestAgentClient(conn)}, nil
}

// Close releases the gRPC connection.
func (c *Client) Close() error { return c.conn.Close() }

// PushImage client-streams the OCI image archive from r to the guest (§6.11). r is
// typically the host imagestore's archive of the blobs the guest is missing.
func (c *Client) PushImage(ctx context.Context, r io.Reader) error {
	stream, err := c.Agent.PushImage(ctx)
	if err != nil {
		return fmt.Errorf("controlclient: open PushImage: %w", err)
	}
	return sendChunks(stream, r)
}

// PushCode client-streams the git bundle from r to the guest (§6.7). A bundle is just a
// byte stream — structurally identical to PushImage, just smaller.
func (c *Client) PushCode(ctx context.Context, r io.Reader) error {
	stream, err := c.Agent.PushCode(ctx)
	if err != nil {
		return fmt.Errorf("controlclient: open PushCode: %w", err)
	}
	return sendChunks(stream, r)
}

// CollectArtifacts reads the guest's artifact tar (patch + report + optional commits
// bundle) into w (§6.7).
func (c *Client) CollectArtifacts(ctx context.Context, w io.Writer) error {
	stream, err := c.Agent.CollectArtifacts(ctx, &pb.CollectRequest{})
	if err != nil {
		return fmt.Errorf("controlclient: open CollectArtifacts: %w", err)
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("controlclient: recv artifact: %w", err)
		}
		if _, err := w.Write(chunk.GetData()); err != nil {
			return err
		}
	}
}

// chunkSender is the shared shape of the PushImage/PushCode client streams.
type chunkSender interface {
	Send(*pb.Chunk) error
	CloseAndRecv() (*pb.Ack, error)
}

// sendChunks streams r as ~1 MiB Chunks and closes the stream, never buffering the whole
// payload in memory (§6.5).
func sendChunks(stream chunkSender, r io.Reader) error {
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.Chunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	ack, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("controlclient: close stream: %w", err)
	}
	if !ack.GetOk() {
		return fmt.Errorf("controlclient: guest rejected stream: %s", ack.GetError())
	}
	return nil
}

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
