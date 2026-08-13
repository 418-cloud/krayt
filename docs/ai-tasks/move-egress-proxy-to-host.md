# Task: move the egress allowlist proxy from the guest to the host, over vsock

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.6 networking & egress, §6.12 vsock transport, §10 security
model) first.** This is step 1 of a three-step arc:

1. **this task** — move the proxy to the host as a *pure CONNECT tunnel* with the identical allowlist.
   No TLS termination, no credential injection. Behavior-preserving for the container.
2. [`add-tls-mitm-credential-injection.md`](./add-tls-mitm-credential-injection.md) — make it a TLS
   MITM proxy that injects auth headers host-side.
3. [`inject-claude-oauth-token-at-proxy.md`](./inject-claude-oauth-token-at-proxy.md) — OAuth-token
   support for Claude Code on top of step 2.

**Do not pull step 2 or 3 work into this task.** The whole point of the split is that step 1 is an
unambiguous security *win* that can land and be verified on hardware on its own, while step 2 is a
trust-model *trade* that must be independently revertable.

## Working mode: decide, don't ask

**Complete this task end-to-end without asking questions.** Every decision it depends on — vsock over
a gateway TCP proxy, separate process via hidden self-exec, fd-3 listener passing, the hard SSRF
block, the spec statements to reverse — is already made and written down below. This file **is** the
approved plan, so `CLAUDE.md`'s "give a short plan and wait for my OK before writing code" step is
waived here; start implementing.

Where this file and `KRAYT_SPEC.md` disagree, this file wins and amending the spec is part of the
deliverable (see the section directly below) — do not ask whether to amend. If something is
underspecified, pick the option most consistent with the stated design, record the choice and its
rationale in the commit/PR description, and keep going.

The only legitimate reasons to stop and involve a human:

- A `[HUMAN]` step you genuinely cannot perform — real Apple-Silicon hardware, a Linux/KVM box, the
  `PinnedRef` digest that does not exist until CI publishes it (§11). Do everything around it, append
  the `HUMAN_TODO.md` entry per §14, then **continue** if other work remains and stop only if it
  blocks everything left.
- This file is factually wrong about the codebase — a cited file, symbol, or line no longer exists, or
  the behavior described differs. Say so, state the correction you made, proceed on the corrected
  understanding.

Do **not** ask for plan approval, for confirmation of a decision already settled here, or for a choice
between options this file has already picked. Never fabricate a hardware result to avoid a handoff
(`CLAUDE.md`) — an honestly-blocked step is the correct outcome; a question is not.

## ⚠️ This contradicts the spec — amend it, don't just code around it

Per `CLAUDE.md` ("the spec is the source of truth… flag the conflict instead of guessing"), this task
knowingly reverses several explicit spec statements. Amending them is **part of the deliverable**:

- §6.6 "**Single-layer, in-guest only.** The L7 proxy and the L3 nftables lock above are the *entire*
  egress enforcement — there is no host/hypervisor-level firewall backstopping them."
- §6.6 / §10 the `skuid "proxyd"` uid lock and its dependency on §6.10 container hardening.
- §6.8 "Redaction scope (**all in the guest**, so no secret value crosses the vsock un-redacted)" —
  this task adds the first *host-side* redaction path (`proxy.log`, see §9).
- §10 trust-boundary table row "Network egress", and the residual "**Egress control is single-layer
  (in-guest).** The host applies no network filtering… Host-side NAT/firewall filtering on
  macOS/vfkit is impractical for v1; a host backstop is a possible future follow-up task."
- §11.6 image contents (`krayt-proxy` leaves the image; a new forwarder binary replaces it).
- §14 Phase 3 "Done when" evidence — the hardware egress suite must be re-run, not assumed.

## Reason

The current L3 lock is `meta skuid "proxyd"` (`internal/guest/proxy/firewall_linux.go:32-39`). Its
correctness does **not** live in that file — it lives in `internal/guest/runner`, in the dropped
`CAP_SETUID`/`CAP_SETGID` and enforced non-root of §6.10. There is a 13-line `SAFETY INVARIANT`
comment at `firewall_linux.go:17-24` saying exactly that, and §10 names those container-hardening
controls "the primary mitigations, not one layer among several". That is a cross-module invariant a
future OCI-spec change can silently break, reopening finding #1.

Moving the proxy off-box **deletes the invariant** rather than defending it. The guest chain stops
keying on uid at all, so there is nothing for a capability regression to unlock.

Second benefit, which the follow-ups depend on: with the proxy on the host, the guest needs no DNS
(the proxy resolves), no registry egress (§6.11 already pre-loads images over vsock), and no bundle
egress (§6.7 rides vsock). In `allowlist`/`none` the guest's egress chain becomes `policy drop` with
**one** accept (loopback), and every byte the container sends leaves through a channel that reaches
exactly one VM by construction.

## Why vsock and not TCP to the gateway

"Point `HTTP_PROXY` at the VM's gateway IP" is the tempting version and it is **wrong**:

- §6.6 gives Firecracker a `/30` per VM specifically so "two concurrent runs share no L2 segment".
  vfkit/vmnet NAT gives **no such isolation** — every concurrent VM on the host shares the segment, so
  a gateway-bound proxy is reachable by every other run's VM.
- One `0.0.0.0` bind mistake exposes it to the host's LAN. After step 2 this process holds the user's
  real credentials, so network position must not be the authenticator.

vsock has neither problem: on vfkit each VM gets its own host unix socket, on Firecracker its own
`uds_path`. The channel is authenticated by construction, needs no bearer token, and is not routable.

## Current behavior (grounding)

| Piece | Where | Today |
|---|---|---|
| Allowlist logic (OS-agnostic, unit-tested) | `internal/guest/proxy/proxy.go` | `HandRolled`/`HandRolledDNS`, `handler.ServeHTTP` (`:185`), `checkDialAddr` SSRF guard (`:157`) |
| nftables lock | `internal/guest/proxy/firewall_linux.go:32-57` | `policy drop` + `oif lo` + `meta skuid "proxyd"` + `ct state established,related`; deleted entirely for `full` |
| Guest controller | `internal/guest/proxy/controller_linux.go:42-100` | execs `krayt-proxy` as the `proxyd` uid, applies the firewall, returns `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` env |
| Guest binary | `cmd/krayt-proxy/main.go` | `--listen --mode --allow --dns`, serves `proxy.Serve` |
| Seam | `internal/guest/network.go:25` `Network` iface | `Apply(ctx, policy) (env, error)`, called at `internal/guest/service.go:292-301` |
| Control channel | `provider.VM.DialControl` (`internal/provider/provider.go:46`), `ControlPort = 1024` (`:63`) | **host → guest** only |
| vfkit vsock device | `internal/provider/vfkit/vfkit.go:236-241` | `config.VirtioVsockNew(ControlPort, ctrlSock, false)` — `listen=false` ⇒ host→guest |
| Firecracker vsock | `internal/provider/firecracker/{firecracker.go:437,452,vsock.go,bridge.go}` | host dials `uds_path` + `"CONNECT <port>\n"` handshake |
| Nix image | `images/flake.nix:48,118-129,168-171` | builds `cmd/krayt-proxy`, creates the `proxyd` user/group, puts `nft` on the agent's PATH |
| Image pin | `internal/vmimage/pinned.go:22` `PinnedRef` | a single hardcoded digest — host and guest ship in lockstep |

The direction that does **not** exist yet is **guest → host**. That is the one new primitive.

## The new primitive: a guest-initiated vsock channel

Both backends support it; the asymmetry is provider-shaped, exactly like `bridge.go` already absorbs
for the other direction.

- **vfkit** (verified against `crc-org/vfkit@v0.6.4`): `config.VirtioVsock.Listen` is documented in
  `pkg/config/virtio.go:58-60` as *"If true, vsock connections will have to be done from guest to
  host. If false, vsock connections will only be possible from host to guest"*, and
  `pkg/vf/vsock.go:75` `listenVsock` "proxies connections from a vsock port to a host unix socket".
  So: add a **second** vsock device with `VirtioVsockNew(EgressPort, egressSock, true)`. The **host**
  listens on `egressSock`; the guest dials `AF_VSOCK` CID 2 (`VMADDR_CID_HOST`), port `EgressPort`.
  Multiple `--device virtio-vsock` entries are explicitly supported ("There will only be a single
  virtio-vsock device added to the VM regardless of the number of occurrences").
- **Firecracker**: guest→host needs **no** `CONNECT` handshake (that is the host→guest direction
  only). The host listens on the unix socket `<uds_path>_<port>`; the guest dials CID 2, port
  `EgressPort`, and firecracker connects it to that socket.

Because vfkit fixes its device list at `Create` time, the device must be added unconditionally in
`Create`, and the host-side listener created before `Start`.

## Implement

### 1. Move the allowlist proxy out of `internal/guest`

- `git mv internal/guest/proxy/proxy.go internal/proxy/proxy.go` and likewise
  `proxy_internal_test.go`. New package `proxy` under `internal/proxy` — it is host-side now, but
  keep it **OS-agnostic** and build-tag-free (it must still compile for `linux/arm64`).
- Keep the `Factory` seam and the `newHandler(p, rt, dial)` injectable-transport test seam verbatim.
  §6.6's "swappable, memory-safety-critical component" argument gets *stronger* on the host, not
  weaker — do not inline it into the orchestrator.
- `internal/guest/proxy` keeps only `firewall_linux.go` and `controller_linux.go`. Its package doc
  must stop claiming it is the L7 enforcement point.

### 2. Tighten the SSRF guard — hard block, no escape hatch

`checkDialAddr` (`proxy.go:157`) currently permits RFC1918/ULA/CGNAT when `mode == full`. That was
written when the dialer lived inside a VM. On the host, `127.0.0.1` is the user's actual machine and
`192.168.0.0/16` is their actual LAN, so the carve-out would change from "the VM's NAT can reach the
host LAN" to "the sandbox can reach the user's LAN and loopback services from a trusted host process".

- **Remove the `mode == ModeFull` relaxation entirely. Add no replacement opt-in.** Loopback,
  link-local, metadata, unspecified, multicast, RFC1918, ULA and CGNAT are refused in **every** mode,
  `full` included. Keep it fail-closed on unparseable addresses.
- This deletes the only mode-dependent branch in `checkDialAddr`; simplify the signature accordingly
  (it no longer needs `mode`) and update every call site and the `Control` closure.
- Update `TestCheckDialAddr`: the matrix collapses to "blocked everywhere" for every private and
  special range, and public addresses allowed in all modes.
- **Document the deliberate casualty** in §6.6 and the README: reaching a service on the host or LAN
  from the sandbox — a local Ollama/LM Studio on `127.0.0.1:11434`, a LAN package mirror — is now
  impossible in every mode. That use case, if wanted, needs a purpose-built mechanism (an explicitly
  named forward target), not a range unblock. Record it as a possible follow-up task; do not build it
  here.

### 3. Split the binary in two: host proxy and guest forwarder

Step 1 already forces a new VM image (the guest binary changes), so renaming is free — take it, and
end up with names that are true.

- **Delete `cmd/krayt-proxy`.** The host proxy is not a shipped binary at all; see §4.
- **Add `cmd/krayt-vsock-forward`** (`//go:build linux`), the guest-side dumb pipe:

  ```
  krayt-vsock-forward --listen 127.0.0.1:3128 --vsock-port 1025
  ```

  - Accept TCP on `--listen`; for each accepted connection dial `vsock.Dial(vsock.Host, port, nil)`
    (`github.com/mdlayher/vsock`, already in `go.mod`) and splice the two with two `io.Copy`s.
  - **One vsock connection per accepted TCP connection** — `HTTP_PROXY` keep-alive means the
    container opens several concurrently. Do not multiplex.
  - Reuse the connection-tracking/shutdown shape from `internal/provider/firecracker/bridge.go:28-60`
    (tracked conn set, closed on shutdown, `sync.WaitGroup` drain).
  - **No parsing of any kind.** No HTTP, no TLS, no allowlist. It is a pipe. That is the whole point:
    the adversarially-exposed parser is what we are moving off the guest, so nothing may follow it
    back. If a future change makes this binary want to look at a byte, the design has regressed.
- Update `images/flake.nix:48` `subPackages` to build `cmd/krayt-vsock-forward` instead of
  `cmd/krayt-proxy`, and `controller_linux.go`'s `defaultBinary`.

### 4. The host proxy runs as a separate process — self-exec, not a shipped binary

**Decision: separate process, implemented as a hidden self-exec subcommand.** The proxy must not share
an address space with the process that holds the user's credentials (step 2), writes their repo, and
runs their run supervisor. But a second *distributed* binary would ripple into `RELEASING.md`, the
release workflow, `internal/selfupdate` (which atomically swaps *the running binary* and would have to
swap two), `krayt doctor`, and PATH discovery — for no isolation benefit over re-exec.

- Add a **hidden cobra subcommand** `krayt __egress-proxy` (`Hidden: true`, excluded from the shell
  completion added by `shell-completion.md` — verify it is, in `internal/cli/complete_test.go`).
- The run supervisor spawns it with `os.Executable()` as argv[0]. **Preserve the swap seam**: if
  `KRAYT_EGRESS_PROXY_BIN` (or an equivalent config field) is set, exec that path instead. This is
  what keeps §6.6's "drop in a memory-safe reimplementation" promise real — document the flag/env
  contract (`--mode`, `--allow`, listener on fd 3, `proxy.log` on stdout/stderr) as the stable
  interface a replacement must honor.
- **Pass the listener on fd 3, do not pass a path.** The parent owns socket creation (correct mode,
  correct dir, fail-fast on bind), the child needs no filesystem access to the socket dir at all —
  which is what makes it sandboxable later.
  ```go
  f, err := lis.(*net.UnixListener).File()   // dups the fd
  cmd.ExtraFiles = []*os.File{f}             // becomes fd 3 in the child
  ```
  Child side: `net.FileListener(os.NewFile(3, "egress"))`.
  **Gotcha:** `(*net.UnixListener).Close()` unlinks the socket path by default. If the parent closes
  its copy after handing the fd over, call `lis.SetUnlinkOnClose(false)` first, or the guest's
  connections will fail with a vanished socket. Decide explicitly which side unlinks at teardown and
  write it in a comment.
- **No readiness handshake is needed** — because the parent created and bound the listener, guest
  connections queue in the kernel backlog even before the child calls `Accept`. Just verify the child
  has not already exited. (This is a concrete benefit of fd-passing over path-passing; note it.)
- **Lifecycle**: `exec.CommandContext(runCtx, …)` plus the reap goroutine and 2-second kill/drain
  pattern already written at `controller_linux.go:72-84`. That code is being deleted from the guest —
  move it to the host rather than reinventing it. The run supervisor owns the child exactly as it
  already owns the VM and (on Firecracker) the control-socket bridge (§6.2).
- **Policy in via flags** (`--mode`, `--allow` CSV, `--dns`) — all non-secret, mirroring today's
  contract. Step 2 adds a stdin-delivered config blob for secret material specifically; do not build
  that channel now, but do not design flags that would have to be removed for it either.
- A failure to spawn the child, or an early child exit, is a **fail-fast run error**. Never boot a VM
  whose only egress path is dead.

### 5. Provider seam: one new method

`internal/provider/provider.go`:

```go
// EgressPort is the fixed vsock port the guest's egress forwarder dials on the host (§6.6).
const EgressPort uint32 = 1025

// ListenEgress returns a listener accepting guest-initiated connections on the fixed egress
// vsock port. The provider absorbs the backend asymmetry: on vfkit it is the unix socket a
// second virtio-vsock device (listen mode) connects to; on Firecracker it is the unix socket
// at <uds_path>_<port>. Must be called before Start. Closing it stops accepting; the VM is
// unaffected.
ListenEgress(ctx context.Context, port uint32) (net.Listener, error)
```

- **vfkit** (`vfkit.go:236-241`): add `config.VirtioVsockNew(uint(provider.EgressPort), egressSock,
  true)` to `AddDevices`. `egressSock` lives in the same per-VM run dir as `ctrlSock` (and inherits
  the `0700` self-owned socket-root check from `harden-vfkit-socket-dir.md` — verify it does; if the
  check is path-specific, extend it). `ListenEgress` is `net.Listen("unix", egressSock)`.
- **Firecracker**: `ListenEgress` is `net.Listen("unix", v.vsockSock+"_"+strconv.FormatUint(uint64(port), 10))`.
  No bridge and no handshake — guest→host is the raw direction.
- **fake** (`internal/provider/fake/fake.go`): return a real unix listener in a `t.TempDir()`-style
  per-VM dir (not an in-memory pipe) so the fd-passing path in §4 is genuinely exercised by
  orchestrator tests.
- Socket file mode `0600`, inside the existing per-VM `0700` run dir.

### 6. Orchestrator wiring

In `internal/orchestrator/orchestrator.go`, after `Create` and **before** `vm.Start` (`:134`):

1. `lis, err := vm.ListenEgress(ctx, provider.EgressPort)`.
2. Build the policy from `spec.Network` — the host already has it in the `RunSpec`, so nothing waits
   on the `SetNetworkPolicy` RPC.
3. Spawn the `krayt __egress-proxy` child per §4, fd 3 = the listener, stdout/stderr → `proxy.log`
   (§9).
4. Tear both down in the run's existing teardown path, alongside the VM.

Keep `setNetworkPolicy` (`:224`, `:416-427`) — the guest still needs the mode to decide the firewall
and whether to start the forwarder at all.

### 7. Simplify the nftables lock

`firewall_linux.go`: for `allowlist` and `none` the ruleset becomes, in full:

```
table inet krayt_egress {
  chain output {
    type filter hook output priority 0; policy drop;
    oif "lo" accept
  }
}
```

- `meta skuid "proxyd"` is **gone**. Delete the entire `SAFETY INVARIANT` comment block
  (`:17-24`) — the invariant no longer exists. Keep the `SINGLE-NETNS ASSUMPTION` comment (`:26-29`);
  it is still load-bearing.
- `ct state established,related accept` is **gone**: with no outbound external flow permitted there
  is no established external flow to match, and the hook is `output` only.
- `oif "lo" accept` **stays** — it is how the container reaches the forwarder on `127.0.0.1:3128`.
- **`full` mode is unchanged**: still deletes the table, so raw egress over the NIC still works. It is
  an explicit escape hatch and its existing test asserts that behavior; do not fold it in here.
  (Unifying `full` onto the proxy path is a legitimate follow-up, but it changes `full`'s meaning from
  "any protocol" to "HTTP(S) only" and deserves its own task.)
- Update `TestEgressRulesetShape` to assert the new shape **and** to assert `skuid` no longer appears
  anywhere in the ruleset (a positive regression guard against reintroducing the uid dependency).

### 8. Guest controller: start the forwarder, not the proxy

`controller_linux.go`:

- Keep the `guest.Network` interface (`internal/guest/network.go:25`) and the
  `service.go:292-301` call site unchanged — the returned env map is still the contract.
- `Apply` now: exec `krayt-vsock-forward --listen 127.0.0.1:3128 --vsock-port <EgressPort>`, apply the
  (simplified) firewall, `waitListening`, return the same `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` env.
- **Keep running it as the `proxyd` uid.** It no longer needs to be a distinct uid for the firewall to
  work, but a non-root uid for the one guest process that touches container-controlled bytes is free
  defence in depth. Keep the `proxyd` user in `images/flake.nix:124-129` and say why in a comment, so
  a later reader does not delete it as vestigial.
- Update the doc comments in `controller_linux.go` and `internal/guest/network.go` — both currently
  describe starting "the allowlist proxy".

### 9. `proxy.log` — a new run artifact, host-side redacted

Today the proxy's server-side `log.Printf` (`proxy.go:223`, `:257`) lands in the guest-agent journal
and is collected with the run. §6.6 explains why it exists at all: `net/http`'s CONNECT-proxy client
discards the response body on a non-2xx CONNECT, so the *only* place the real reason for a denial
appears is the server side. Losing that would be a debuggability regression on the single most common
support question.

- The supervisor writes the child's stdout/stderr to **`.krayt/runs/<id>/proxy.log`**.
- Contents: timestamps, hostnames, allow/deny verdicts, dial errors. **Never** request/response
  bodies. (Step 2 adds header *names* only.)
- **Run it through a host-side `Redactor` built from the secrets file.** For plain-HTTP forwards the
  proxy sees full URLs, which can carry a token in a query string. This is the **first host-side
  redaction path in krayt** — §6.8 currently claims redaction is "all in the guest, so no secret value
  crosses the vsock un-redacted". Amend that sentence; the guest claim stays true, it is just no
  longer the whole story.
- Add `proxy.log` to the §8.4 artifact layout and to whatever enumerates run artifacts in
  `internal/orchestrator`.

### 10. DNS

The host proxy resolves through the **host's system resolver** by default (respects the user's
VPN/split-horizon/corporate DNS, which hardcoding `1.1.1.1` does not).

- `resolverVia` stays for the `--dns` override; the default becomes a nil `Resolver`.
- No new user-facing config surface in this task — `--dns` is a child-process flag only. A
  `network.dns` config field is a follow-up if anyone asks for it.
- Document the behavior change in §6.6: DNS now resolves in the user's network context, not the VM's.

### 11. Image lockstep (`[HUMAN]` handoff — you cannot finish this yourself)

`internal/vmimage/pinned.go:22` `PinnedRef` is a single hardcoded digest, so host and guest ship in
lockstep and there is no version-skew case to support — **do not build a compatibility shim for old
images.** But it does mean the change is not runnable until a new image exists:

1. The guest changes (`cmd/krayt-vsock-forward`, `images/flake.nix`, `internal/guest/**`) trigger the
   RC workflow from `automate-vmimage-releases.md` on push.
2. `PinnedRef` must then be bumped to the published digest — which you cannot know until CI publishes.

Append a `HUMAN_TODO.md` entry per §14 with the exact sequence (push → read the RC digest from the
workflow run → bump `PinnedRef` → run the hardware suite). **Do not invent a digest.**

## Non-goals (state them in the spec, don't do them here)

- **Removing the guest NIC entirely.** In `allowlist`/`none` the VM demonstrably no longer needs one,
  and that would additionally let Linux drop the `setcap cap_net_admin+ep` from
  `hack/linux-net-setup.sh` and the tap+masquerade from `krayt doctor`. But Firecracker's `allocSlot`
  bundles tap + `/30` + CID in one allocation (`internal/provider/firecracker/firecracker.go:137-141`)
  and `full` mode still needs the NIC, so making it conditional is its own task. Record it as a
  follow-up.
- Unifying `full` mode onto the proxy path (§7).
- A named-forward-target mechanism for host/LAN services (§2).
- Any TLS termination, CA, or header injection — that is step 2.

## Tests

**Offline (required):**

- `internal/proxy` — the moved `proxy_internal_test.go` passes with the `checkDialAddr` matrix
  collapsed per §2 (private ranges refused in every mode, no `mode` parameter).
- `internal/guest/proxy` — `TestEgressRulesetShape` asserts the two-line chain and the absence of
  `skuid`.
- Forwarder: with an injected dial function standing in for `vsock.Dial`, assert bytes splice both
  ways, that N concurrent TCP accepts produce N distinct upstream dials, and that closing one side
  tears down the other.
- Child process: an orchestrator-level test over the `fake` provider that spawns the real
  `krayt __egress-proxy` child with a real fd-3 unix listener, dials it, and reaches an allowlisted
  `httptest` server through it — and is refused 403 for a non-allowlisted one. This is the test that
  proves fd-passing, argv, and teardown all work; do not fake it with an in-process `proxy.Serve`.
- Teardown: killing the run closes the child and unlinks the socket exactly once (the
  `SetUnlinkOnClose` gotcha in §4).
- `proxy.log` is written, contains the denial reason for a blocked host, and a secret value planted in
  a plain-HTTP query string does **not** appear in it.

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

**On hardware (`[HUMAN]` — needs a real Apple-Silicon Mac and a Linux/KVM box, after §11):**

This task invalidates the Phase 3 §14 evidence. `TestEgressEnforcement` must be re-run on **both**
backends, and extended:

1. reach an allowlisted host — passes (now via host proxy),
2. blocked from a non-allowlisted host — 403 from the host proxy,
3. blocked from a raw (non-proxied) socket — `1.1.1.1:443` still times out under the new two-line
   chain,
4. **new:** `nft list ruleset` in the guest contains no `skuid` rule (keep the `setuid(proxyd)=EPERM`
   assertion in `TestContainerHardening`; it just stops being the thing egress depends on),
5. **new:** a container attempt to reach the *host* on a private address is refused by the guard.

Also re-run `TestConcurrentRealVMs` — two simultaneous VMs must each get their own egress socket and
child process, and must not be able to reach each other's. This is the concurrency property vsock buys
over a gateway-bound TCP proxy; assert it, don't assume it.

Write these as real test code, then append the `HUMAN_TODO.md` entry. **Do not fabricate a hardware
result.**

## Docs (required)

- **`KRAYT_SPEC.md` §6.6** — substantial rewrite. Host-side L7 proxy as a separate process; the L3
  lock is `policy drop` + `oif lo` and no longer depends on §6.10; the guest→host vsock channel; the
  hard-blocked address ranges and the deliberately unsupported host/LAN case; host-resolver DNS. Keep
  and strengthen the "swappable / memory-safety-critical" paragraph — the component now sits *outside*
  the VM boundary, so a memory-safe reimplementation matters more, and the
  `KRAYT_EGRESS_PROXY_BIN` + fd-3 contract is how it stays swappable.
- **§6.8** — amend the "all in the guest" redaction claim for `proxy.log` (§9).
- **§6.12** — `EgressPort = 1025`, the guest→host direction, `VM.ListenEgress`, both backends'
  mechanics (vfkit `listen=true` second device; Firecracker `<uds>_<port>`, no handshake).
- **§8.4** — `proxy.log` in the artifact layout.
- **§10** — update the "Network egress" trust-boundary row; replace the "single-layer (in-guest)"
  residual with an honest statement of the new trade: enforcement is now host-side, so a proxy
  compromise is a *host* compromise rather than an escape from a VM that was about to be destroyed.
  State that this is why the proxy is a separate process and why it parses nothing it does not have
  to. Add the concurrent-VM isolation property vsock provides.
- **§11.6** — the image now carries `krayt-vsock-forward`, not `krayt-proxy`.
- **§14** — new phase entry; mark the Phase 3 egress evidence superseded and pending re-verification.
- **`README.md`** — the `1.1.1.1` DNS default if documented, and the newly unsupported host/LAN case.
- **`docs/ai-tasks/README.md`** — status.

## Done when

- The container's egress path is `container → 127.0.0.1:3128 (guest forwarder) → vsock → host proxy
  child process → internet`, with identical allowlist semantics to today.
- The guest nftables chain is `policy drop` + `oif "lo" accept` and nothing else in
  `allowlist`/`none`; no rule anywhere keys on a uid.
- `checkDialAddr` refuses every private/special range in every mode and no longer takes a mode.
- The proxy runs in its own process, receives its listener on fd 3, and is replaceable via
  `KRAYT_EGRESS_PROXY_BIN`.
- `proxy.log` is a collected, host-redacted run artifact carrying denial reasons.
- Offline suite green on both `GOOS`; `TestEgressRulesetShape` guards against `skuid` returning.
- Spec §6.6/§6.8/§6.12/§8.4/§10/§11.6/§14 amended, with the reversed statements explicitly reversed
  rather than quietly dropped.
- The `PinnedRef` bump and hardware re-verification are either **done and reported honestly**, or
  written as runnable tests and handed off in `HUMAN_TODO.md`.

## Constraints

- No new dependencies. `github.com/mdlayher/vsock` is already in `go.mod`; everything else is stdlib.
- Keep `internal/proxy` OS-agnostic and build-tag-free; keep `internal/guest/*` `//go:build linux`.
- The guest forwarder parses nothing.
- Keep the `Factory` seam and `newHandler` test seam intact.
- Behavior-preserving for the container: same `HTTP_PROXY` env, same 403/502 semantics, same allowlist
  matching. The only intended user-visible changes are the DNS resolution context and the
  private-range tightening — both must be documented.
- Small, reviewable diffs. The package move, the provider seam, and the child-process spawn are
  separable commits; use that if it helps review.
