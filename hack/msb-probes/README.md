# msb-probes — the feasibility gate for the microsandbox (B1) migration

**All five have run — msb 0.6.16, 2026-08-29/30, on an Apple-Silicon Mac.** The outcomes live in
`KRAYT_SPEC.md` §14 Phase 11's feasibility-gate item and `docs/ai-tasks/README.md`'s row 1, not
here; what follows is what each probe asks and how to re-run it. The one thread left is P4 on
Linux/KVM — see its row below. **P1's 2026-09-02 re-runs found a real defect**: msb 0.6.16's vsock
relay drops the reply when the host closes the bridged socket first — 21 of 75 round trips
completed that way, against 25 of 25 when the host waits for the guest. `internal/askbridge` now
waits (`lingerUntilPeerCloses`, `KRAYT_SPEC.md` §6.13), and P1 is the regression check that would
catch msb changing this back.

Five scripts that answered the questions
[`docs/adr-microsandbox-sandbox-layer.md`](../../docs/adr-microsandbox-sandbox-layer.md) had left
unverified against real hardware. They are **not** part of `hack/run-integration-tests.sh` and do
not run in CI — microsandbox (`msb`) is not installed on any CI runner, and these exercise a
third-party binary against real hardware, not krayt code. Each prints exactly one line,
`PASS: <probe> — <finding>` or `FAIL: <probe> — <finding>`, and exits 0/1 to match — that line is
the whole reporting protocol.

See `docs/ai-tasks/probe-microsandbox-feasibility.md` for the full background on each question and
why it matters.

## Install msb first

```sh
curl -fsSL https://install.microsandbox.dev | sh
```

Every probe checks `command -v msb` itself and fails with that same command if it's missing.
Every probe also prints `msb --version` before its finding — msb is beta and has already shipped
a breaking wire change as a patch release, so a finding without a version attached is not
re-checkable. These were written against **0.6.16**.

## The probes

| Script | Answers | Blocking? |
|---|---|---|
| `p1-vsock-nonroot.sh` | Can a **non-root** guest process (`agent`, uid 1000) open `AF_VSOCK` to reach the host, and does the round trip complete? Also doubles as "does a krayt agent image run unmodified under msb", and reports the peer uid msb's local backend connects to the host socket as (decision 10). | **Yes** — decides `dial-ask-channel-over-vsock.md`'s whole design (a direct `krayt-ask` dial vs. a root-owned in-guest forwarder). |
| `p2-exec-root-restricted.sh` | Does `msb exec --user root` still work — and actually hold root's privilege, not just report uid 0 — under `--security restricted`'s `no_new_privs`? | **Yes** — decides whether `add-krayt-guest-helper.md` can use `--security restricted` at all, or must give it up for the helper's privilege separation. |
| `p3-secret-tls-intercept.sh` | Does declaring `--secret` alone enable TLS interception, or is `--tls-intercept` required separately? | No — confirms a finding already made from msb's source; sizes whether `translate-network-policy-to-msb.md`'s mandatory `--tls-intercept` emission is necessary or can be dropped. |
| `p4-environ-exposure-window.sh` | Does the real secret value live only in the short-lived `msb create` process's environment, or in the long-lived per-sandbox `msb sandbox` runtime for the whole run? **⚪ Inconclusive on darwin by construction — the one probe worth re-running, on Linux/KVM.** | No — sizes an already-accepted residual (§ "The secret-handling contract" in the ADR); doesn't change the decision either way. |
| `p5-placeholder-accepted.sh` | Does Claude Code reject msb's default `$MSB_ANTHROPIC_API_KEY` placeholder client-side (length/prefix check) before any request leaves the container? | No — needs a **live Anthropic credential**; decides whether `hand-secrets-to-msb.md` must build the `--secret-conf`-shaped-placeholder contingency. |

`p1` and `p2` are the two that *shape the design* of downstream tasks rather than merely sizing a
residual — both must be answered before `run-tasks-on-microsandbox.md` and
`dial-ask-channel-over-vsock.md` are implemented.

`p3` is **answered, against the ADR's expectation**: on msb 0.6.16 substitution happened with and
without `--tls-intercept`, because declaring a secret enables interception by itself
(`SandboxBuilder::secret_entry` sets `network.tls.enabled = true`, `sdk/rust/lib/sandbox/builder.rs:834-843`;
the `has_tls` predicate the ADR read governs only the network overlay). It is kept as the
regression that would catch msb changing that back, with its verdict inverted to match: **PASS
means msb still substitutes without the flag**, and a FAIL naming `--tls-intercept` as required
means msb changed under us. The ADR's withdrawn correction 1 carries the full finding.

It takes two readings per sandbox rather than one, because "the real value arrived at the endpoint"
is ambiguous on its own — it fits both msb substituting a placeholder the guest sent and a guest
that held the real value all along, which are opposite findings — so it reads the guest's
`$KRAYT_P3_CANARY` too, prints who signed the certificate the guest was served (msb's intercept CA
means MITM), and creates a third no-secret control sandbox when the guest turns out to hold a real
value. It also exercises the plain sandbox **before** any `--tls-intercept` sandbox exists in the
msb server's lifetime, so leaked interception state cannot fake a positive.

### Every `msb exec` here passes `--no-tty`

msb allocates a PTY whenever the caller's stdin is a terminal
(`crates/cli/lib/commands/exec.rs`, `use_interactive_tty = stdin_is_terminal && !no_tty`), and
command substitution does not redirect stdin — so running a probe by hand took the PTY path, and a
PTY re-introduces echo and CRLF (msb's own words, `exec.rs`'s `--stream` doc comment). `id -u` came
back as `1000\r`, which fails an exact compare against `1000` and, echoed inside a longer message,
returns the cursor to column 0 so a FAIL line overwrites its own beginning. Keep `--no-tty` on any
exec you add, and strip CRs before comparing.

### Fixtures P1 needs

`p1-vsock-nonroot.sh` builds and drives two small Go programs (`package main`, probe fixtures
only — not shipped krayt code, matching `hack/netprobe` and `hack/edit-probe`):

- **`vsock-echo-host/`** — the host side. A plain unix-socket line echo server (`go build`/`go
  run`, OS-agnostic). msb's `--vsock HOST_PATH:PORT` maps this socket onto the guest's `AF_VSOCK`
  CID 2. It logs every step with a timestamp and its `-label`: the connecting peer's uid, the
  exact bytes read (`%q`, partial reads included), the echo, and — under `-linger` — whether the
  peer closed first. `-linger` is the variable, not a convenience: without it the host closes as
  soon as it has echoed, which is what a relay that discards in-flight data on close would punish.
- **`vsock-probe-guest/`** — the guest side (`//go:build linux`). Dials `AF_VSOCK` CID 2 via
  `github.com/mdlayher/vsock` (already pinned, `KRAYT_SPEC.md` §9.1; already used by
  `cmd/krayt-vsock-forward`), writes one line, reads it back. `p1-vsock-nonroot.sh`
  cross-compiles it (`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build`) and `msb copy`'s it in.
  Each leg exits with its own code — 3 dial, 4 write, 5 read, 6 mismatch — and the script
  classifies from the guest's own stderr rather than trusting msb to propagate the exact code.

### P1 runs three socket shapes, many iterations each

`--vsock` is repeatable, so one `msb create` exposes three host sockets and the probe dials each
in turn: `bare` (a path straight in `$TMPDIR` — the shape that passed on 2026-08-29), `priv`
(`ask.sock`, mode `0600`, inside a `0700` directory) and `linger` (`priv`, but the host waits for
the guest to close — **production's shape**, since `internal/askbridge` now waits). Each shape
differs from a neighbour in exactly one property, so a difference in outcome attributes itself:
`bare` vs `priv` isolates the private directory, `priv` vs `linger` isolates who closes first.

Each shape runs `$KRAYT_P1_ITERATIONS` round trips (default 25) inside a single `msb exec`, and
the verdict reads **rates**, not one sample — `bare=25/25 priv=24/25:read:1 linger=25/25`. That
is not caution for its own sake: the failure being chased is intermittent, and the 2026-09-02
13:11 run concluded "only the lingering host works" from one sample per shape while the bare
non-lingering host had passed in that very run.

The 2026-09-02 measurement, 25 iterations per shape on one sandbox: `bare=7/25`, `priv=5/25`,
`priv` as root `=9/25`, `linger=25/25`. Every loss was `read after 0 byte(s) "": EOF` on the guest
side *after* the host had logged both the bytes it read and the echo it wrote. Note what the rates
rule out — root loses too (not privilege), the bare shape loses at the same rate (not the private
directory), and the peer uid is the invoking user's throughout.

**The PASS criterion is `linger`** — completing every iteration as `agent` — because that
measurement moved production: `internal/askbridge` now waits for the sandbox to close
(`lingerUntilPeerCloses`, `KRAYT_SPEC.md` §6.13), so `linger` is the shape krayt ships and `bare`
and `priv` are shapes it deliberately no longer uses. Those two are still run and still reported,
in a `NOTE:` line of their own, but the probe does not fail on them: they characterise msb's
defect, and a probe that fails on a known, worked-around defect is noise. The note reads both
ways — if they start completing every iteration, msb has fixed the drop, and §6.13's wait becomes
belt-and-braces rather than load-bearing.

Everything a run prints — including the three background listeners' output, which is half the
evidence and the half that scrolls away first — is teed to a transcript file whose path is
printed at the end. On any failure the probe also dumps `msb logs` for the sandbox before
removing it: msb's relay logs the host side of `--vsock`, and that account does not depend on
anything the probe itself observed.

## Running

Each script takes no required arguments and cleans up its own sandboxes (`msb rm --force` in a
trap) on every exit path — a leaked sandbox on your machine is a defect, please report it.

```sh
./hack/msb-probes/p1-vsock-nonroot.sh
./hack/msb-probes/p2-exec-root-restricted.sh
./hack/msb-probes/p3-secret-tls-intercept.sh          # optional $1: a header-echoing HTTPS endpoint you trust
./hack/msb-probes/p4-environ-exposure-window.sh       # authoritative on Linux/KVM; best-effort on macOS
ANTHROPIC_API_KEY=sk-ant-... ./hack/msb-probes/p5-placeholder-accepted.sh
```

`p1`, `p2`, and `p5` default to pulling `ghcr.io/418-cloud/krayt-agent-claude-code` — override with
`KRAYT_MSB_PROBE_IMAGE` if you'd rather use a different one, but running the real agent image is
the point for `p1` (it doubles as the ADR's "does a krayt agent image run unmodified under msb"
question) and a hard requirement for `p2` (it needs the non-root `agent` user the image ships) and
`p5` (it needs `claude` on PATH).

A re-run's `PASS`/`FAIL` line belongs in the durable homes — `KRAYT_SPEC.md` §14 Phase 11 and
`docs/ai-tasks/README.md`'s row 1 — with the msb version beside it. There is no `HUMAN_TODO.md`
entry for these any more: it was deleted when the last probe landed, per `CLAUDE.md`'s rule that the
file is a queue of outstanding work rather than an archive.
