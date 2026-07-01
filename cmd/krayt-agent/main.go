//go:build linux

// Command krayt-agent is the in-VM guest-agent. It listens on a fixed vsock port and
// serves the gRPC control protocol to the host (§6.4, §6.12). It is baked into the Nix
// VM image and run as the systemd service `krayt-agent.service` (§11.6). Built for
// linux/arm64 (and linux/amd64 for the future firecracker backend).
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/mdlayher/vsock"
	"google.golang.org/grpc"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/guest/proxy"
	"github.com/418-cloud/krayt/internal/guest/runner"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider"
)

// containerdSocket is where the in-VM containerd daemon listens (§11.6).
const containerdSocket = "/run/containerd/containerd.sock"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "krayt-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	// Guest side is identical on every backend: listen on the fixed vsock port and hand
	// the net.Listener straight to gRPC (§6.12). The host connects through the provider.
	lis, err := vsock.Listen(provider.ControlPort, nil)
	if err != nil {
		return fmt.Errorf("listen vsock port %d: %w", provider.ControlPort, err)
	}

	// Wire the containerd Runner (§6.10). If containerd is not reachable yet, still serve
	// so the host's boot-readiness Hello succeeds; Start then fails with a clear message
	// rather than the VM appearing dead.
	// Secrets are materialized on tmpfs (/run is tmpfs, §11.6) so they never touch
	// persistent disk (§6.8). The egress proxy + nftables lock enforce the per-task network
	// policy (§6.6).
	opts := []guest.Option{
		guest.WithSecretsDir("/run/krayt-secrets"),
		guest.WithNetwork(proxy.NewController()),
	}
	if r, err := runner.New(containerdSocket); err != nil {
		log.Printf("containerd runner unavailable (%v): Start will fail until containerd is up", err)
	} else {
		opts = append(opts, guest.WithRunner(r))
	}

	srv := grpc.NewServer()
	pb.RegisterGuestAgentServer(srv, guest.NewService(opts...))

	// The unit is Type=notify (§11.6): tell systemd we are ready once the vsock listener
	// is up, so service ordering (and the host's boot-readiness wait) is accurate.
	if _, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		log.Printf("sd_notify: %v", err) // non-fatal: still serve if not under systemd
	}

	log.Printf("krayt-agent %s serving on vsock port %d", guest.Version, provider.ControlPort)
	if err := srv.Serve(lis); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
