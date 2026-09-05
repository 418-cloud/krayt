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
//
// It also logs the peer uid of whatever connects (peerUID, platform-specific: SO_PEERCRED on
// Linux, LOCAL_PEERCRED on macOS) — dial-ask-channel-over-vsock.md decision 10's open question:
// production will bind the real ask-bridge socket 0600 inside a private 0700 directory (§6.12's
// hardening, reused via internal/sockroot/internal/askbridge.Listen), which is only actually
// private if msb's local backend connects to it as the invoking user rather than as root or a
// system daemon under another uid. 0600 alone doesn't tell you that; only inspecting who
// connected does.
//
// Every step is logged with a timestamp and the -label the probe script gave this instance,
// because one 2026-09-02 attempt (msb 0.6.16) failed in a way the previous log lines could not
// tell apart: the guest's dial and write succeeded, the host accepted a connection from the right
// uid, and then the guest read EOF instead of its own line back. "The host never received the
// bytes" and "the host echoed them and the reply was dropped" are opposite findings — one is a
// forward-path failure, one a close-race on the return path — and only logging what was actually
// read, what was written back, and who closed first separates them. -linger tests the second
// directly: hold the connection open after the echo and let the guest close first — and so far
// that is the shape that has never lost a round trip, while the shapes where the host closes
// first lose one intermittently. These lines are what make that a finding — the exact bytes that
// crossed, in both directions, and who tore the connection down — rather than an exit code to be
// taken on trust.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// readTimeout bounds the wait for the guest's line. Without it a forward-path failure looks like
// a hang, the probe script's cleanup kills this process, and the diagnosis never gets printed —
// which is exactly what the 2026-09-02 run lost.
const readTimeout = 15 * time.Second

// lingerTimeout bounds -linger's wait for the peer to close. Reaching it is itself the finding
// (the guest never closed), so it is short enough not to stall the probe.
const lingerTimeout = 5 * time.Second

var label string

func main() {
	conns := flag.Int("conns", 1, "number of connections to accept before exiting")
	timeout := flag.Duration("timeout", 90*time.Second, "give up waiting for a connection after this long")
	flag.StringVar(&label, "label", "", "name for this instance in log lines (the probe runs several at once)")
	linger := flag.Bool("linger", false, "after echoing, wait for the peer to close instead of closing first")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: vsock-echo-host [-conns N] [-timeout D] [-label L] [-linger] <unix-socket-path>")
		os.Exit(2)
	}
	sockPath := flag.Arg(0)

	if err := run(sockPath, *conns, *timeout, *linger); err != nil {
		logf("%v", err)
		os.Exit(1)
	}
}

// logf prints one timestamped, labelled line. The probe interleaves three instances' output with
// the script's own, so a bare message is not attributable to a variant.
func logf(format string, args ...any) {
	prefix := "vsock-echo-host"
	if label != "" {
		prefix = fmt.Sprintf("vsock-echo-host[%s]", label)
	}
	fmt.Printf("%s %s %s\n", prefix, time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

func run(sockPath string, conns int, timeout time.Duration, linger bool) error {
	// Decision 10: the caller (p1-vsock-nonroot.sh) is expected to have placed sockPath inside a
	// private 0700 directory, matching what production binds the real ask-bridge socket into
	// (internal/askbridge.Listen). Log what we actually found rather than assuming the script got
	// it right, so a probe run's output is itself evidence, not a claim.
	if fi, err := os.Stat(filepath.Dir(sockPath)); err == nil {
		logf("socket dir %s mode %o", filepath.Dir(sockPath), fi.Mode().Perm())
	}

	_ = os.Remove(sockPath) // stale socket left by a previous, killed run
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	defer func() { _ = lis.Close() }()
	defer func() { _ = os.Remove(sockPath) }()
	logf("listening on %s (linger=%v)", sockPath, linger)

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
		echoLine(conn, linger)
	}
	return nil
}

// echoLine reads one newline-terminated line and writes it straight back, logging every step:
// who connected (decision 10's peer uid), the exact bytes that arrived, the echo, and — under
// -linger — who closed the connection first. Errors are logged, not fatal, so one bad connection
// doesn't stop the listener from serving the next -conns slot.
func echoLine(conn net.Conn, linger bool) {
	defer func() { _ = conn.Close() }()
	logf("accepted a connection")
	if uc, ok := conn.(*net.UnixConn); ok {
		if uid, ok := peerUID(uc); ok {
			logf("peer uid = %d (this process uid = %d)", uid, os.Getuid())
		} else {
			logf("could not determine peer uid on this platform")
		}
	}

	if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		logf("set read deadline: %v", err)
		return
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		// The partial read matters as much as the error: no bytes at all means msb never
		// forwarded the guest's write, while bytes that are not the guest's message mean msb
		// speaks a handshake on this socket that the probe (and internal/askbridge) would have
		// to answer.
		logf("read failed after %d byte(s) %q: %v", len(line), line, err)
		return
	}
	logf("read %d byte(s): %q", len(line), line)

	if _, err := conn.Write([]byte(line)); err != nil {
		logf("write: %v", err)
		return
	}
	logf("echoed %d byte(s) back", len(line))

	if linger {
		waitForPeerClose(conn)
	} else {
		logf("closing first (no -linger)")
	}
}

// waitForPeerClose blocks until the peer closes or lingerTimeout elapses, so the probe can tell a
// return-path close-race apart from a return path that never delivers at all. If the guest reads
// its echo and exits, this returns on EOF and the echo demonstrably arrived; if the guest is
// still waiting when this times out, the reply was dropped somewhere in msb's relay even though
// nothing on the host closed early.
func waitForPeerClose(conn net.Conn) {
	if err := conn.SetReadDeadline(time.Now().Add(lingerTimeout)); err != nil {
		logf("set linger deadline: %v", err)
		return
	}
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			logf("linger: peer sent %d unexpected byte(s) %q", n, buf[:n])
			continue
		}
		switch {
		case errors.Is(err, io.EOF):
			logf("linger: peer closed first — the echo reached it before teardown")
		case os.IsTimeout(err):
			logf("linger: peer still open after %s — it never got the echo", lingerTimeout)
		default:
			logf("linger: read: %v", err)
		}
		return
	}
}
