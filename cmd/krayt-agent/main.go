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
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider"
)

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

	srv := grpc.NewServer()
	pb.RegisterGuestAgentServer(srv, guest.NewService())

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
