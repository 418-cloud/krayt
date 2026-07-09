// Command hardening-probe is a throwaway "agent" image entrypoint that proves the
// least-privilege container OCI spec (§6.10, §10, security-review findings #1/#3) on real
// hardware — the confirmation described in HUMAN_TODO.md
// ("[Security review] Run the container-hardening integration tests on a Mac"). It exits 0
// ONLY when every hardening control krayt applies to the untrusted agent container holds:
//
//   - /proc/self/status: CapEff and CapAmb are all-zero, NoNewPrivs is 1, Seccomp is 2
//     (SECCOMP_MODE_FILTER)
//   - the process uid is not 0 (root)
//   - setuid() to proxyd's uid — read from /proc/net/tcp, visible because the container
//     shares the VM's network namespace (§6.6) — fails with EPERM. This is the
//     egress-allowlist-bypass regression (finding #1): without dropped CAP_SETUID/CAP_SETGID
//     and enforced non-root, a container process could assume proxyd's uid and satisfy the
//     nftables `skuid "proxyd"` rule directly, skipping the L7 allowlist entirely.
//
// It speaks only the stdlib (no krayt imports) so a green run proves the OCI spec itself, not
// any client code, and logs every check with a distinct non-zero exit code per failure so a
// hardware regression is point-blank obvious from `krayt ls` (the EXIT column) or the logs.
//
//	exit 0  — every check passed
//	exit 10 — /proc/self/status unreadable or missing an expected field
//	exit 11 — CapEff is not all-zero (a capability leaked through)
//	exit 12 — CapAmb is not all-zero (ambient set not cleared)
//	exit 13 — NoNewPrivs != 1
//	exit 14 — Seccomp != 2 (filter mode not engaged)
//	exit 15 — running as uid 0 (root was not rejected — should never even reach here)
//	exit 16 — could not find proxyd's listening socket (127.0.0.1:3128) in /proc/net/tcp
//	exit 17 — setuid(proxyd) unexpectedly SUCCEEDED — the egress bypass is open
//	exit 18 — setuid(proxyd) failed, but not with EPERM
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	statusPath = "/proc/self/status"
	tcpPath    = "/proc/net/tcp"
	proxyLocal = "0100007F:0C38" // 127.0.0.1:3128 (krayt-proxy's listen addr, /proc/net/tcp hex form)
	tcpListen  = "0A"            // TCP_LISTEN, per include/net/tcp_states.h
)

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[hardening-probe] "+format+"\n", a...)
}

func main() { os.Exit(run()) }

func run() int {
	logf("start: probing the container hardening controls (§6.10, §10, findings #1/#3)")

	status, err := readStatus(statusPath)
	if err != nil {
		logf("FAIL(10): read %s: %v", statusPath, err)
		return 10
	}

	if !isAllZero(status["CapEff"]) {
		logf("FAIL(11): CapEff = %s, want all-zero (a capability leaked through)", status["CapEff"])
		return 11
	}
	logf("ok: CapEff = %s (all capabilities dropped)", status["CapEff"])

	if !isAllZero(status["CapAmb"]) {
		logf("FAIL(12): CapAmb = %s, want all-zero (ambient set not cleared)", status["CapAmb"])
		return 12
	}
	logf("ok: CapAmb = %s (ambient set cleared)", status["CapAmb"])

	if status["NoNewPrivs"] != "1" {
		logf("FAIL(13): NoNewPrivs = %q, want \"1\"", status["NoNewPrivs"])
		return 13
	}
	logf("ok: NoNewPrivs = 1")

	if status["Seccomp"] != "2" {
		logf("FAIL(14): Seccomp = %q, want \"2\" (SECCOMP_MODE_FILTER)", status["Seccomp"])
		return 14
	}
	logf("ok: Seccomp = 2 (filter mode engaged)")

	uid := os.Getuid()
	if uid == 0 {
		logf("FAIL(15): running as uid 0 (root) — the non-root enforcement did not hold")
		return 15
	}
	logf("ok: running as uid %d (non-root)", uid)

	proxydUID, err := findListenerUID(tcpPath, proxyLocal)
	if err != nil {
		logf("FAIL(16): find proxyd's uid via %s: %v", tcpPath, err)
		return 16
	}
	logf("ok: found proxyd listening on 127.0.0.1:3128, owned by uid %d", proxydUID)

	if err := syscall.Setuid(proxydUID); err == nil {
		logf("FAIL(17): setuid(%d) SUCCEEDED — the egress-allowlist bypass is open (finding #1)", proxydUID)
		return 17
	} else if err != syscall.EPERM { //nolint:errorlint // syscall.Setuid returns a bare syscall.Errno
		logf("FAIL(18): setuid(%d) failed, but not with EPERM: %v", proxydUID, err)
		return 18
	}
	logf("ok: setuid(%d) failed with EPERM — the egress lock is unbypassable", proxydUID)

	logf("done: success — all hardening controls hold")
	return 0
}

// readStatus parses /proc/<pid>/status into a field->raw-value map (tab-trimmed).
func readStatus(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	fields := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(name)] = strings.TrimSpace(val)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, want := range []string{"CapEff", "CapAmb", "NoNewPrivs", "Seccomp"} {
		if _, ok := fields[want]; !ok {
			return nil, fmt.Errorf("missing %q field", want)
		}
	}
	return fields, nil
}

// isAllZero reports whether a hex capability mask (e.g. "0000000000000000") is all zero.
func isAllZero(hexMask string) bool {
	v, err := strconv.ParseUint(hexMask, 16, 64)
	return err == nil && v == 0
}

// findListenerUID scans /proc/net/tcp for a socket in LISTEN state at localAddr (the hex
// "IP:PORT" form /proc/net/tcp uses) and returns the uid that owns it. The container shares the
// VM's netns with the proxy (§6.6), so this is visible without any special privilege — the exact
// technique finding #1 is about: an attacker learns proxyd's uid this way, then tries to assume
// it via setuid().
func findListenerUID(path, localAddr string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Scan() // header line
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid ...
		if len(fields) < 8 {
			continue
		}
		if fields[1] != localAddr || fields[3] != tcpListen {
			continue
		}
		uid, err := strconv.Atoi(fields[7])
		if err != nil {
			return 0, fmt.Errorf("parse uid field %q: %w", fields[7], err)
		}
		return uid, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("no LISTEN socket at %s found in %s", localAddr, path)
}
