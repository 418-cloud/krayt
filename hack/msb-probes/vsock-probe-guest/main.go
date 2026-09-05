//go:build linux

// Command vsock-probe-guest is the guest side of P1 (hack/msb-probes/p1-vsock-nonroot.sh): it
// dials AF_VSOCK CID 2 (the host, per vsock.Host) on the port msb's `--vsock` flag exposed,
// writes one line, reads it back, and exits 0 only if every echo matches what was sent.
//
// It knows nothing about which user it's running as — the probe script `msb copy`'s this same
// binary in once and runs it via both `msb exec --user agent` and `msb exec --user root`, and
// draws the "can a non-root guest process open AF_VSOCK" conclusion from which invocation
// succeeded.
//
// **It repeats the round trip <iterations> times and reports a rate**, because the failure this
// probe is chasing is intermittent: across three runs on msb 0.6.16 the same non-linger shape
// passed, then failed twice, then passed twice and failed once — always the same way (the host
// accepts, reads the line and echoes it; the guest reads EOF having received nothing). One
// sample per shape cannot tell an intermittent relay race from a property of the shape, and a
// verdict drawn from one sample is how the 2026-09-02 13:11 run concluded "only the lingering
// host works" while the bare non-lingering host had just passed in the same run. Each iteration
// carries its own index in the message, so a stale reply can never pass for a fresh one.
//
// Each leg exits with its own code (exitDial/exitWrite/exitRead/exitMismatch) instead of a
// blanket 1. A 2026-09-02 run (msb 0.6.16) is why: the script reported "AF_VSOCK dial failed
// for both agent and root" when the dial had in fact succeeded — the host accepted the
// connection and logged the right peer uid — and only the echo came back empty. A probe whose
// verdict cannot tell "the kernel refused an AF_VSOCK socket to uid 1000" (the question P1
// exists to answer) from "the round trip did not complete" (a different problem entirely) will
// send the reader after the wrong bug.
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

// Exit codes, one per leg of the round trip. p1-vsock-nonroot.sh maps them straight onto its
// FAIL text, so the finding names the leg that broke rather than guessing at the first one.
const (
	exitUsage    = 2
	exitDial     = 3 // socket(AF_VSOCK)/connect refused — the privilege question P1 asks
	exitWrite    = 4 // connected, but the request could not be written
	exitRead     = 5 // request written, no (complete) reply came back
	exitMismatch = 6 // a reply came back, but it was not what was sent
)

func legName(code int) string {
	switch code {
	case exitDial:
		return "dial"
	case exitWrite:
		return "write"
	case exitRead:
		return "read"
	case exitMismatch:
		return "mismatch"
	default:
		return "unknown"
	}
}

func main() {
	if len(os.Args) != 3 && len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: vsock-probe-guest <port> <message> [iterations]")
		os.Exit(exitUsage)
	}
	port, err := strconv.ParseUint(os.Args[1], 10, 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad port %q: %v\n", os.Args[1], err)
		os.Exit(exitUsage)
	}
	msg := os.Args[2]
	iterations := 1
	if len(os.Args) == 4 {
		if iterations, err = strconv.Atoi(os.Args[3]); err != nil || iterations < 1 {
			fmt.Fprintf(os.Stderr, "bad iteration count %q\n", os.Args[3])
			os.Exit(exitUsage)
		}
	}

	fmt.Fprintf(os.Stderr, "dialing vsock host port=%d as uid %d, %d iteration(s)\n", port, os.Getuid(), iterations)

	ok := 0
	legs := map[string]int{}
	first := 0
	for i := 1; i <= iterations; i++ {
		code, err := roundTrip(uint32(port), fmt.Sprintf("%s-%d", msg, i))
		if code == 0 {
			ok++
			continue
		}
		if first == 0 {
			first = code
		}
		legs[legName(code)]++
		fmt.Fprintf(os.Stderr, "iteration %d/%d failed at %s: %v\n", i, iterations, legName(code), err)
	}

	// One machine-readable line the script parses for the matrix. Printed on every path,
	// including total failure, so a run always yields a rate rather than only an exit code.
	fmt.Printf("SUMMARY port=%d ok=%d fail=%d legs=%s\n", port, ok, iterations-ok, formatLegs(legs))
	os.Exit(first)
}

func formatLegs(legs map[string]int) string {
	if len(legs) == 0 {
		return "none"
	}
	out := ""
	for _, leg := range []string{"dial", "write", "read", "mismatch", "unknown"} {
		if n := legs[leg]; n > 0 {
			if out != "" {
				out += ","
			}
			out += fmt.Sprintf("%s:%d", leg, n)
		}
	}
	return out
}

// roundTrip performs one dial/write/read/compare and returns the exit code of the leg that broke
// (0 on success) alongside the error, so the caller can both count legs and print the detail.
func roundTrip(port uint32, msg string) (int, error) {
	conn, err := vsock.Dial(vsock.Host, port, nil)
	if err != nil {
		return exitDial, fmt.Errorf("dial vsock host port=%d: %w", port, err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return exitWrite, fmt.Errorf("set deadline: %w", err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", msg); err != nil {
		return exitWrite, fmt.Errorf("write: %w", err)
	}

	echo, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		// Which error this is, is the finding: EOF means the host end of msb's relay closed the
		// connection (a dropped or raced reply), while an i/o timeout means it stayed open and
		// nothing ever came back. Report the partial read too — bytes that are not the echo would
		// mean msb speaks something of its own on this channel.
		return exitRead, fmt.Errorf("read after %d byte(s) %q: %w", len(echo), echo, err)
	}
	if echo != msg+"\n" {
		return exitMismatch, fmt.Errorf("echo mismatch: sent %q, got %q", msg, echo)
	}
	return 0, nil
}
