//go:build linux

// Command krayt-proxy is the in-VM egress allowlist proxy (§6.6). The guest-agent execs it
// as the dedicated `proxyd` uid so the nftables lock (which permits egress only for
// skuid proxyd) makes it unbypassable. It runs until signaled. Built into the VM image
// alongside krayt-agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/418-cloud/krayt/internal/guest/proxy"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:3128", "address to listen on")
	mode := flag.String("mode", proxy.ModeAllowlist, "policy mode: allowlist | full | none")
	allowCSV := flag.String("allow", "", "comma-separated allowlist of egress hosts")
	flag.Parse()

	var allow []string
	if *allowCSV != "" {
		allow = strings.Split(*allowCSV, ",")
	}

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "krayt-proxy: listen:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := proxy.Serve(ctx, lis, proxy.Policy{Mode: *mode, Allow: allow}, nil); err != nil {
		fmt.Fprintln(os.Stderr, "krayt-proxy:", err)
		os.Exit(1)
	}
}
