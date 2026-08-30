// Command vsock-echo-host is the host side of P1 (hack/msb-probes/p1-vsock-nonroot.sh): a
// throwaway unix-socket line-echo server. msb's `--vsock HOST_PATH:PORT` flag exposes this
// socket to the guest at CID 2 on PORT (docs/networking/host-sockets.mdx), so whatever dials it
// from inside the sandbox is exercising the exact path krayt-ask would use to reach the host
// directly (dial-ask-channel-over-vsock.md).
//
// Plain net.Listen("unix", ...) — this program never touches AF_VSOCK itself; msb is what maps
// the guest's vsock connection onto this socket. It is OS-agnostic on purpose so the probe
// script can `go run` it straight on the operator's Mac.
//
// Not a general-purpose echo server: it exits after handling -conns connections (default 1) or
// after -timeout, whichever comes first, so a probe run that never manages to connect doesn't
// hang forever.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	conns := flag.Int("conns", 1, "number of connections to accept before exiting")
	timeout := flag.Duration("timeout", 90*time.Second, "give up waiting for a connection after this long")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: vsock-echo-host [-conns N] [-timeout D] <unix-socket-path>")
		os.Exit(2)
	}
	sockPath := flag.Arg(0)

	if err := run(sockPath, *conns, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "vsock-echo-host: %v\n", err)
		os.Exit(1)
	}
}

func run(sockPath string, conns int, timeout time.Duration) error {
	_ = os.Remove(sockPath) // stale socket left by a previous, killed run
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	defer func() { _ = lis.Close() }()
	defer func() { _ = os.Remove(sockPath) }()

	ul, ok := lis.(*net.UnixListener)
	if !ok {
		return fmt.Errorf("listener for %s is not a *net.UnixListener", sockPath)
	}

	deadline := time.Now().Add(timeout)
	for i := 0; i < conns; i++ {
		if err := ul.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set accept deadline: %w", err)
		}
		conn, err := lis.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		echoLine(conn)
	}
	return nil
}

// echoLine reads one newline-terminated line and writes it straight back. Errors are logged,
// not fatal, so one bad connection doesn't stop the listener from serving the next -conns slot.
func echoLine(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "vsock-echo-host: read: %v\n", err)
		return
	}
	if _, err := conn.Write([]byte(line)); err != nil {
		fmt.Fprintf(os.Stderr, "vsock-echo-host: write: %v\n", err)
	}
}
