//go:build linux

// Command krayt-vsock-forward is the guest-side half of the host-side egress proxy
// (move-egress-proxy-to-host.md, §6.6): a dumb TCP<->vsock pipe with no policy of its own. It
// accepts TCP connections on --listen (the container's HTTP_PROXY target,
// 127.0.0.1:3128) and, for each one, dials the host's fixed egress vsock port
// (provider.EgressPort, --vsock-port) and splices the two byte streams together — one vsock
// connection per accepted TCP connection, since HTTP_PROXY keep-alive means the container
// opens several concurrently; no multiplexing.
//
// It parses NOTHING: no HTTP, no TLS, no allowlist. That is the whole point. The
// adversarially-exposed parser — the component that used to be `krayt-proxy`, in-guest — now
// runs on the host as `krayt __egress-proxy` (internal/proxy, internal/cli), reached over the
// vsock channel this binary dials. Nothing in the guest looks at a container-controlled byte
// past the TCP framing anymore; if a future change makes this binary want to inspect one, the
// design has regressed.
//
// The guest-agent execs this as the dedicated `proxyd` uid (internal/guest/proxy) — no longer
// load-bearing for the nftables lock (which is loopback-only now), but kept as defense in
// depth. Built into the VM image alongside krayt-agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/mdlayher/vsock"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:3128", "TCP address to accept container connections on")
	vsockPort := flag.Uint("vsock-port", 1025, "host egress vsock port to dial for each accepted connection")
	flag.Parse()

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "krayt-vsock-forward: listen:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	f := newForwarder(lis, uint32(*vsockPort), dialVsock)
	go func() {
		<-ctx.Done()
		_ = f.close()
	}()

	if err := f.serve(); err != nil {
		fmt.Fprintln(os.Stderr, "krayt-vsock-forward:", err)
		os.Exit(1)
	}
}

// dialVsock dials the host's egress vsock port (VMADDR_CID_HOST, i.e. the hypervisor side of
// the guest→host channel VM.ListenEgress opens, §6.12).
func dialVsock(port uint32) (net.Conn, error) {
	return vsock.Dial(vsock.Host, port, nil)
}
