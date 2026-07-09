# Task: prove and lock the egress allowlist against the proxyd-uid bypass

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.6 networking & egress, §10 security model) first. Proceed
autonomously. The live enforcement check needs a real Apple-Silicon Mac — write the test, then hand
it off via `HUMAN_TODO.md` (§14) rather than faking a result.**

**Depends on `harden-container-oci-spec.md`.** That task drops `CAP_SETUID`/`CAP_SETGID` and enforces
non-root, which is the actual code fix. This task is the egress finding's dedicated plan: it verifies
the bypass is closed, keeps/annotates the L3 lock, adds the egress regression test, and documents the
property in the spec. Do this **after** the hardening task merges.

## Reason (the finding)

**Finding #1 (Critical).** The `#1` safety property is default-deny egress. The nftables lock
(`internal/guest/proxy/firewall_linux.go:17-24`) is:

```
table inet krayt_egress {
  chain output {
    type filter hook output priority 0; policy drop;
    oif "lo" accept
    meta skuid "proxyd" accept
    ct state established,related accept
  }
}
```

Its comment (`firewall_linux.go:12-16`) claims the container "cannot bypass it" because it runs as a
different uid. That was **false**: the container shares the VM network namespace
(`internal/guest/runner/containerd_linux.go:95`) and, before the hardening task, kept `CAP_SETUID`.
A container process could:

1. Learn proxyd's numeric uid from `/proc/net/tcp` (the `:3128` listener's owner uid is visible in the
   shared netns) — or brute-force the small system-uid range. The `proxyd` user is created by
   `images/flake.nix:87-92`.
2. `setuid(proxyd_uid)` (allowed by `CAP_SETUID`), open a socket, and send egress that matches
   `meta skuid "proxyd" accept` — **bypassing the L7 allowlist entirely.**

Once the hardening task removes `CAP_SETUID`/`CAP_SETGID` from the container and forbids running as
root (root could setuid regardless), step 2 fails with `EPERM` and the L3 lock holds.

## Goal

- Confirm (in code review terms) that with the hardening task applied, no container process can
  acquire proxyd's uid, so the `skuid "proxyd"` rule is unbypassable at L3.
- Add an **egress regression test** that fails if the bypass ever reopens.
- Correct the misleading comment and document the real invariant in the spec: *the L3 lock is safe
  only because the container cannot change uid (no `CAP_SETUID`/`CAP_SETGID`, enforced non-root).*
- Harden the ruleset's robustness notes for future changes.

## Current behavior (grounding)

- Firewall applied before the container starts: `internal/guest/proxy/controller_linux.go:86`
  (`ApplyFirewall`) runs inside `Apply`, which the Service calls in `Start`
  (`internal/guest/service.go:264-270`) **before** `runner.Run`. Good — keep this ordering.
- Proxy launched as proxyd: `controller_linux.go:61-66` sets
  `SysProcAttr.Credential{Uid, Gid}` from `lookupUser("proxyd")`.
- DNS is resolved by the proxy as proxyd (`internal/guest/proxy/proxy.go:101-111`), so the container
  has no independent DNS path — correct, and unaffected by this task.

## Implement

1. **Fix the comment** in `firewall_linux.go:12-16` to state the real invariant: the L3 lock is
   unbypassable **only because** the container OCI spec drops `CAP_SETUID`/`CAP_SETGID` and forbids
   root (cross-reference §6.10 and `harden-container-oci-spec.md`). Note explicitly that this rule is
   correct **only** while the container shares the VM netns (single-netns assumption) — if a future
   change gives the container its own netns, the `output` hook will no longer see its traffic and a
   `forward` chain will be required.

2. **Add an egress regression test.** Two layers:
   - **In-VM integration test** (`//go:build integration`, real Mac; log in `HUMAN_TODO.md`):
     from inside the agent container, run a tiny helper that (a) reads the uid owning `127.0.0.1:3128`
     from `/proc/net/tcp`, (b) attempts `syscall.Setuid(thatUid)` and asserts it returns `EPERM`, and
     (c) attempts a direct TCP connect to a **non-allowlisted** public host and asserts it is dropped
     (timeout/refused), while a connect **through** `HTTP_PROXY` to an allowlisted host succeeds.
   - **Unit test** (no VM): assert the generated `egressRuleset` string still contains
     `policy drop`, `oif "lo" accept`, `meta skuid "proxyd" accept`, and is in the `inet` family
     (covers IPv4+IPv6). This is a cheap guard against an accidental rule regression.

3. **Do not change** the proxy allowlist logic here — the resolved-IP SSRF guard is a separate task
   (`add-proxy-ssrf-guard.md`).

## Tests

- Unit: `egressRuleset` shape assertions (family `inet`, `policy drop`, loopback + skuid accepts).
- Unit: `ApplyFirewall(ModeFull)` deletes the table; `allowlist`/`none` install it (already partially
  covered — extend `internal/guest/proxy/proxy_internal_test.go` if needed).
- Integration (hand-off): the `EPERM` setuid check + the allowlist enforcement described above.

## Docs (required)

- `KRAYT_SPEC.md` §6.6: state the egress invariant precisely — default-deny `output` lock in the
  `inet` family; the container reaches the network only via the proxy; **the lock depends on the
  container being unable to assume the proxyd uid** (no setuid caps, non-root). Note the single-netns
  assumption.
- `KRAYT_SPEC.md` §10: update the "Proxy-bypass via raw sockets" residual line — raw sockets are
  caught by the uid rule, but the real historical gap was uid assumption via `CAP_SETUID`, now closed
  by the hardened OCI spec.
- `docs/ai-tasks/README.md`: add this task.

## Verify (offline)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

The live drop/allow enforcement is the integration test — record the exact commands in
`HUMAN_TODO.md` for the maintainer's Mac.

## Done when

- The comment/spec state the real invariant; unit rule-shape test added; the integration
  setuid-`EPERM` + allowlist-enforcement test is written and logged in `HUMAN_TODO.md`.
- With `harden-container-oci-spec.md` applied, a code walk shows no path for the container to reach
  proxyd's uid.

## Constraints

- No dependency changes. No change to the nftables rule semantics (only comments + a
  robustness/single-netns note). The proxy allowlist logic is out of scope here.
