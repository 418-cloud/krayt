// Command netprobe is a throwaway "agent" image entrypoint that proves the §6.6 egress control
// on real hardware. It is the probe behind TestEgressEnforcement (KRAYT_NETPROBE_IMAGE) — the
// on-hardware half of the Phase 3 "Done when", and the regression test for security-review
// finding #1 (egress-allowlist bypass).
//
// It exits 0 ONLY when all three of these hold at once:
//
//  1. ALLOWED, through the proxy: an HTTPS request to the allowlisted host via HTTPS_PROXY
//     succeeds. Proves the L7 allowlist lets the task's own traffic out.
//  2. DENIED, through the proxy: an HTTPS request to a NON-allowlisted host via the same proxy
//     fails. Proves the L7 allowlist is actually consulted, not merely present.
//  3. DENIED, around the proxy: a RAW TCP connect that ignores HTTP(S)_PROXY entirely fails.
//     This is the one that matters. The proxy is only advisory — a hostile agent would simply
//     not use it — so the real lock is the nftables ruleset, which drops all egress except
//     `skuid "proxyd"`. If this connect succeeds, the allowlist is decorative and the whole
//     egress model is broken (§6.6, finding #1).
//
// Two subtleties, both of which would otherwise make this probe pass for the wrong reason:
//
//   - The raw connect targets an IP LITERAL, not a hostname. The container is deliberately
//     DNS-blocked (only proxyd may resolve; the proxy does lookups on the container's behalf),
//     so a raw connect to a *name* would fail at DNS resolution and look like the firewall
//     working even if nftables were wide open. Dialing an IP skips DNS and tests the actual
//     packet-level drop.
//   - The proxied checks use hostnames precisely BECAUSE the container cannot resolve them:
//     the CONNECT request carries the name to the proxy, which resolves it as proxyd. That is
//     the intended path, so it exercises the real one.
//
// It speaks only the stdlib (no krayt imports), so a green run proves the enforcement itself and
// not any client code, and it uses a distinct exit code per failure so a regression is obvious
// from `krayt ls` (the EXIT column) or the run log.
//
//	exit 0  — every check passed: allowlisted host reachable, non-allowlisted blocked, raw socket blocked
//	exit 20 — no HTTPS_PROXY in the environment (the guest never injected the proxy config)
//	exit 21 — the allowlisted host was NOT reachable through the proxy (egress is over-blocked)
//	exit 22 — the NON-allowlisted host WAS reachable through the proxy (L7 allowlist not enforced)
//	exit 23 — the RAW socket connect SUCCEEDED (the nftables L3 lock is open — egress bypass)
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// rawTarget is the IP:port the raw-socket check dials. An IP literal, not a name — see the
// package comment: the container has no DNS, so a hostname here would fail at resolution and
// mask an open firewall. 1.1.1.1:443 is a stable, always-listening public endpoint.
const rawTarget = "1.1.1.1:443"

func main() {
	// The host the run's network policy allowlists. Baked into the image (see Dockerfile) rather
	// than passed per-run, because TestEgressEnforcement puts this same value in the policy via
	// KRAYT_ALLOW_HOST — the two must agree.
	allowHost := envOr("KRAYT_ALLOW_HOST", "example.com")
	denyHost := envOr("KRAYT_DENY_HOST", "www.wikipedia.org")

	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		proxy = os.Getenv("https_proxy")
	}
	fmt.Printf("netprobe: proxy=%q allow=%q deny=%q raw=%q\n", proxy, allowHost, denyHost, rawTarget)
	if proxy == "" {
		fmt.Fprintln(os.Stderr, "netprobe: FAIL — no HTTPS_PROXY set; the guest did not inject the proxy config")
		os.Exit(20)
	}

	// 1. The allowlisted host must be reachable through the proxy.
	if err := getViaProxy(allowHost); err != nil {
		fmt.Fprintf(os.Stderr, "netprobe: FAIL — allowlisted host %s was NOT reachable via the proxy: %v\n", allowHost, err)
		os.Exit(21)
	}
	fmt.Printf("netprobe: ok — allowlisted host %s reachable via the proxy\n", allowHost)

	// 2. A non-allowlisted host must NOT be reachable through the proxy.
	if err := getViaProxy(denyHost); err == nil {
		fmt.Fprintf(os.Stderr, "netprobe: FAIL — non-allowlisted host %s WAS reachable via the proxy; "+
			"the L7 allowlist is not being enforced\n", denyHost)
		os.Exit(22)
	}
	fmt.Printf("netprobe: ok — non-allowlisted host %s blocked by the proxy\n", denyHost)

	// 3. A raw socket that ignores the proxy must be dropped by nftables. This is the load-bearing
	//    one: everything above it is advisory.
	conn, err := net.DialTimeout("tcp", rawTarget, 8*time.Second)
	if err == nil {
		_ = conn.Close()
		fmt.Fprintf(os.Stderr, "netprobe: FAIL — RAW connect to %s SUCCEEDED; the nftables L3 lock is "+
			"open and the egress allowlist can be bypassed entirely (finding #1)\n", rawTarget)
		os.Exit(23)
	}
	fmt.Printf("netprobe: ok — raw connect to %s blocked (%v)\n", rawTarget, err)

	fmt.Println("netprobe: PASS — allowlisted reachable, non-allowlisted blocked, raw socket blocked")
}

// getViaProxy performs an HTTPS request through the proxy named in the environment.
// http.ProxyFromEnvironment is what routes it, so this is exactly the path a well-behaved agent
// takes. A non-2xx/3xx status still counts as reachable — we are testing whether the egress is
// permitted, not what the far end says.
func getViaProxy(host string) error {
	client := &http.Client{
		Timeout: 25 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	resp, err := client.Get("https://" + host + "/")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	fmt.Printf("netprobe:   %s -> HTTP %d\n", host, resp.StatusCode)
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
