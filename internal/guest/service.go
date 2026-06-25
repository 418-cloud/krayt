// Package guest holds the guest-agent that runs inside the micro-VM. The OS-specific
// wiring (vsock listener, containerd runner, nftables/egress proxy) lives in
// build-tagged (//go:build linux) files added in later phases; this file is the
// OS-agnostic control-server logic so it can also back the fakeProvider loopback in
// host-side unit tests (§14 test strategy).
package guest

import (
	"context"

	"github.com/418-cloud/krayt/internal/protocol/pb"
)

// Version is the guest-agent version reported in the Hello handshake (§6.5).
const Version = "0.0.0-dev"

// Service implements the pb.GuestAgentServer control protocol. Only Hello is wired in
// Phase 0; the remaining RPCs are filled in by later phases. Embedding
// UnimplementedGuestAgentServer makes the unimplemented ones return codes.Unimplemented.
type Service struct {
	pb.UnimplementedGuestAgentServer

	// ContainerdVersion is reported in HelloResponse; empty until the runner is wired.
	ContainerdVersion string
}

// NewService returns a guest control service ready to register on a gRPC server.
func NewService() *Service {
	return &Service{}
}

// Hello performs the handshake + version negotiation (§6.5).
func (s *Service) Hello(_ context.Context, _ *pb.HelloRequest) (*pb.HelloResponse, error) {
	return &pb.HelloResponse{
		AgentVersion:      Version,
		ContainerdVersion: s.ContainerdVersion,
	}, nil
}
