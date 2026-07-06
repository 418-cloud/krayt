# Task: block egress-proxy targets that resolve to internal/link-local/metadata IPs

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.6 networking & egress) first. Proceed autonomously. The
proxy logic is OS-agnostic and unit-testable without a VM.**

## Reason (the finding)

**Finding (Low) — no resolved-IP guard (SSRF).** The allowlist proxy checks the requested **host
string** against the policy, then resolves and dials it with **no restriction on the resulting IP**
(`internal/guest/proxy/proxy.go:120-143` `allowed`, `:146-171` `connect`, `:174-189` `forward`).
So an allowlisted name (or, in `full` mode, any name) that resolves to a private/link-local/metadata
address is dialed. Because the VM has NAT to the host network
(`internal/provider/vfkit/vfkit.go:175-178`), a name resolving to `169.254.169.254`, `127.0.0.1`, or a
host on the LAN gives the container reach into internal/host services via the proxy. Operator-set
allowlists make this low-likelihood, but `full` mode exposes the host LAN outright.

**Decision (already made):** after resolution, **deny loopback/link-local/metadata in every mode**;
**deny private/ULA ranges unless `mode == full`**; allow public addresses (subject to the allowlist).
Fail closed.

## Goal

Add a post-resolution IP guard to the proxy's dial path so a target that resolves to a disallowed
range is refused with a clear `403`/`502`, in both CONNECT and plain-HTTP forwarding, for both the
handler's allow-check and the actual dial.

## Current behavior (grounding)

- `internal/guest/proxy/proxy.go:73-84` — `HandRolledDNS` builds a `net.Dialer` with a custom
  resolver (`resolverVia`, `:101-111`) that resolves via the configured DNS server as proxyd.
- `:120-131` `ServeHTTP` calls `allowed(host)` then `connect`/`forward`.
- `:146-147` `connect` dials `r.Host`; `:174-176` `forward` round-trips (dials `r.URL.Host`).
- Modes: `ModeAllowlist`, `ModeFull`, `ModeNone` (`:22-26`).

## Implement (`internal/guest/proxy/proxy.go`)

1. **Add a control-plane guard on dial.** Wrap the dialer so every upstream connection is checked
   after resolution. The cleanest hook is the `net.Dialer.Control` callback, which runs with the
   resolved `address` (`ip:port`) just before connect:
   ```go
   d := &net.Dialer{
       Timeout:  dialTimeout,
       Resolver: resolverVia(dnsServer),
       Control: func(network, address string, _ syscall.RawConn) error {
           return checkDialAddr(mode, address) // mode captured from the policy
       },
   }
   ```
   `Control` fires for **each** resolved address the dialer tries (so multi-A/AAAA and Happy-Eyeballs
   are all covered), and it governs both the CONNECT tunnel dial and the HTTP transport dial since
   both use this dialer (`d.DialContext` is the transport's `DialContext` and `connect`'s `h.dial`).
   This also closes the DNS-rebinding window (the *resolved* IP is what's checked, not the name).
2. **`checkDialAddr(mode, address string) error`:** split host/port, parse the IP, and reject:
   - **Always (all modes):** loopback (`127.0.0.0/8`, `::1`), link-local (`169.254.0.0/16`,
     `fe80::/10`), the cloud metadata IP `169.254.169.254`, unspecified (`0.0.0.0`, `::`), and
     multicast. Use `net.IP` helpers (`IsLoopback`, `IsLinkLocalUnicast`, `IsUnspecified`,
     `IsMulticast`) plus an explicit `169.254.169.254` check.
   - **Unless `mode == ModeFull`:** private/ULA ranges — `10.0.0.0/8`, `172.16.0.0/12`,
     `192.168.0.0/16`, `100.64.0.0/10` (CGNAT), and `fc00::/7`. Use `netip.Addr.IsPrivate()` plus a
     CGNAT check.
   - On rejection return a distinct error; the handler maps it to a clear `403` (policy) response,
     e.g. `"krayt: egress to <host> resolves to a blocked (internal/link-local) address"`.
   Note: loopback here refers to the *proxy's* own resolution of an upstream; the container's own
   `NO_PROXY=localhost,127.0.0.1` traffic never transits the proxy, so this does not affect legitimate
   loopback use inside the container.
3. **Keep it fail-closed:** if the IP can't be parsed, refuse. Do not special-case `full` beyond the
   private-range relaxation — link-local/metadata stays blocked even in `full`.

## Tests (`internal/guest/proxy`, no VM)

Unit-test `checkDialAddr` directly (pure function):
- `169.254.169.254`, `127.0.0.1`, `::1`, `fe80::1`, `0.0.0.0` → rejected in **all** modes
  (`allowlist`, `full`, `none`).
- `10.1.2.3`, `192.168.1.1`, `172.16.0.1`, `100.64.0.1`, `fc00::1` → rejected in `allowlist`, allowed
  in `full`.
- `1.1.1.1`, a public IPv6 → allowed (mode-independent, still subject to the allowlist elsewhere).
- Handler-level: using the existing injectable transport/dialer test seam (`newHandler`,
  `:89-99`), assert a CONNECT/forward to a name that resolves to a blocked IP returns 403 and never
  dials. (Drive resolution with a fake dialer/`Control` or a stub resolver, mirroring existing proxy
  tests that avoid real network.)

## Docs (required)

- `KRAYT_SPEC.md` §6.6: document the resolved-IP guard — link-local/loopback/metadata are always
  refused by the proxy; private ranges are refused unless `mode: full`; note this also mitigates
  DNS-rebinding to internal addresses, and that `full` mode still exposes the host LAN (private ranges)
  by design.
- `docs/ai-tasks/README.md`: add this task.

## Verify (offline)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./internal/guest/proxy/...
golangci-lint run
```

## Done when

- Every upstream dial is checked post-resolution; blocked ranges are refused per mode; unit tests
  cover all range/mode combinations; spec §6.6 updated.

## Constraints

- Keep the proxy logic OS-agnostic and behind the existing `Factory`/`newHandler` seam so it stays
  unit-testable without real network.
- No new dependency (stdlib `net`/`netip`/`syscall`).
- Do not weaken the existing host-string allowlist; this guard is **additional** and applies after it.
