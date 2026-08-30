//go:build linux

// Command vsock-probe-guest is the guest side of P1 (hack/msb-probes/p1-vsock-nonroot.sh): it
// dials AF_VSOCK CID 2 (the host, per vsock.Host) on the port msb's `--vsock` flag exposed,
// writes one line, reads it back, and exits 0 only if the echo matches what was sent.
//
// It knows nothing about which user it's running as — the probe script `msb copy`'s this same
// binary in once and runs it via both `msb exec --user agent` and `msb exec --user root`, and
// draws the "can a non-root guest process open AF_VSOCK" conclusion from which invocation
// succeeds.
//
// linux-only, cross-compiled by the probe script (CGO_ENABLED=0 GOOS=linux GOARCH=arm64) since
// it only ever runs inside the msb guest, matching cmd/krayt-vsock-forward's own guest-side
// build tag.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/mdlayher/vsock"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: vsock-probe-guest <port> <message>")
		os.Exit(2)
	}
	port, err := strconv.ParseUint(os.Args[1], 10, 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad port %q: %v\n", os.Args[1], err)
		os.Exit(2)
	}
	msg := os.Args[2]

	conn, err := vsock.Dial(vsock.Host, uint32(port), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial vsock host port=%d: %v\n", port, err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		fmt.Fprintf(os.Stderr, "set deadline: %v\n", err)
		os.Exit(1)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", msg); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
	echo, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	if echo != msg+"\n" {
		fmt.Fprintf(os.Stderr, "echo mismatch: sent %q, got %q\n", msg, echo)
		os.Exit(1)
	}
	fmt.Println("ok")
}
