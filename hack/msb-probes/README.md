# msb-probes — the feasibility gate for the microsandbox (B1) migration

**P1–P9 have all run — P1–P5 on msb 0.6.16, 2026-08-29/30; P6 and P7 on 2026-09-04; P8 on 2026-09-05; P9 on 2026-09-06, same machine.** The outcomes live in
`KRAYT_SPEC.md` §14 Phase 11's feasibility-gate item and `docs/ai-tasks/README.md`'s row 1, not
here; what follows is what each probe asks and how to re-run it. One thread is left open: P4 on
Linux/KVM — see its row below. **P1's 2026-09-02 re-runs found a real
defect**: msb 0.6.16's vsock
relay drops the reply when the host closes the bridged socket first — 21 of 75 round trips
completed that way, against 25 of 25 when the host waits for the guest. `internal/askbridge` now
waits (`lingerUntilPeerCloses`, `KRAYT_SPEC.md` §6.13), and P1 is the regression check that would
catch msb changing this back.

**P9 ran on 2026-09-06** (msb 0.6.16) — all 5 measurements confirm msb enforces `*.<suffix>`
exactly as `support-wildcard-network-hosts.md` read it out of 0.6.16's source. Getting the fifth,
label alignment, took three runs and is the probe worth reading about before writing another one:
the non-aligned-neighbour check first shipped with no positive control, so its PASS could not tell
"msb enforces alignment" from "nothing answers at that name". A same-day control (`ce58d4f`) came
back DEAD — the templated default neighbour `evil<suffix>` → `evilgithub.com` is NXDOMAIN, so the
original PASS had indeed proved nothing. Only with a verified-live literal neighbour
(`wwwgithub.com`, now the `$4` default) did the third run measure it. `KRAYT_SPEC.md` §6.6's
wildcard paragraph records the confirmed result. See its row below.

Seven scripts (P1–P7) answered the questions
[`docs/adr-microsandbox-sandbox-layer.md`](../../docs/adr-microsandbox-sandbox-layer.md) had left
unverified against real hardware; the feasibility gate they formed is closed. `p8` is a different
kind of probe living in the same directory for the same reasons (real `msb` on real hardware, not
CI-able): it hardware-checks `add-msb-extra-conf-escape-hatch.md`'s claims about `sandbox.extra_conf`
after that feature had already landed, not a pre-migration feasibility question. None of these are
part of `hack/run-integration-tests.sh` and none run in CI — microsandbox (`msb`) is not installed
on any CI runner, and these exercise a third-party binary against real hardware, not krayt code.
Each prints exactly one line, `PASS: <probe> — <finding>` or `FAIL: <probe> — <finding>`, and exits
0/1 to match — that line is the whole reporting protocol.

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
| `p6-credential-not-in-run.sh` | During a **real `krayt run`**, does the credential stay out of every live msb process's argv and environ, out of the guest's own environment, and out of every artifact including `changes.patch`? **✅ PASS 2026-09-04** on three authoritative readings; the environ reading is ⚪ inconclusive on darwin, same as p4. | No — needs a **live credential**; closed criterion 3 of `HUMAN_TODO.md`'s `run-tasks-on-microsandbox.md` hardware entry. |

| `p7-passthrough-semantics.sh` | Does `--on-secret-violation passthrough` forward the placeholder unchanged to an out-of-scope host, or substitute the real value there? Measured against a `block-and-log` control and an in-scope regression guard. **✅ PASS 2026-09-04**: the placeholder is forwarded unchanged out of scope, the identical request blocks under `block-and-log`, and in-scope substitution still works. | **Was** blocking the decision; krayt now emits `passthrough` on the strength of it. |
| `p8-extra-conf-precedence.sh` | Does `sandbox.extra_conf` (`add-msb-extra-conf-escape-hatch.md`) behave the way its decisions 1–3 say? **Two arms**: three synthetic sandboxes with controls ask what *msb* does with krayt-shaped argv; a fourth measurement drives a real `krayt run` carrying a real `sandbox.extra_conf:` and measures the sandbox *krayt's own argv* built — which is what §8.1 actually claims. **✅ PASS 2026-09-05** on msb 0.6.16, all four: krayt's flags fully replace an `extra_conf`'s network policy (control proves the file was live, merely outranked); the secret-scope-widening escalation is real and attributable to the hatch (control without it saw only the placeholder); and both reproduce through a real `krayt run`. Measurement 2 came back **REJECTED**, confirming this script's own source read and **overturning decision 2** — per-secret `on_violation` is unreachable from any `msb` CLI surface, and `deny_unknown_fields` makes msb refuse the sandbox outright rather than ignore the field. `KRAYT_SPEC.md` §8.1/§10 and the task doc are corrected. | No — sized `add-msb-extra-conf-escape-hatch.md`'s hardware bullet, which is now closed. |
| `p9-wildcard-suffix-rules.sh` | Does msb enforce `*.<suffix>` the way `support-wildcard-network-hosts.md` reads it out of 0.6.16's source? Five measurements in three sandboxes: a **subdomain** reachable under `--net-rule allow@*.<suffix>` (does the deferred DNS-cache binding fire for `DomainSuffix` as it does for `Domain`, or is a wildcard allow entry inert?), the **apex** reachable under the same rule (`hostname == suffix` — the branch a subdomain-only test misses and that krayt documents as covered), the **non-aligned neighbour** `evil<suffix>` still **denied** (label alignment — the security property; this one must fail to connect), paired with a positive control proving that neighbour is live under its own exact rule, `--tls-bypass *.<suffix>` serving the **real upstream chain** rather than msb's own CA (read off the certificate issuer, p7's method), and `msb create --net-rule allow@*.com` **rejected** by msb itself (pins krayt's floor as *aligned* with msb's `SuffixTooBroad` guard rather than merely additive). **✅ 5/5 confirmed 2026-09-06** on msb 0.6.16, run against the default `github.com` family: `api.github.com` REACHED and `github.com` REACHED under one `allow@*.github.com`, the non-aligned neighbour `wwwgithub.com` DENIED under that same rule while LIVE under its own exact `allow@wwwgithub.com` (the control that makes the denial mean label alignment rather than NXDOMAIN), `--tls-bypass *.github.com` serving GitHub's real chain (`C=GB, O=Sectigo Limited, CN=Sectigo Public Server Authentication CA DV E36`) in a sandbox that had a secret declared and interception otherwise on, and `allow@*.com` REJECTED by msb. **The alignment measurement took three runs**: it first passed with no positive control at all; a same-day fix (`ce58d4f`) added one and it came back DEAD, because the templated default neighbour `evil<suffix>` → `evilgithub.com` is NXDOMAIN — so that original DENIED was indistinguishable from "nothing on the internet answers at that name". Replacing the template with a verified-live literal (`wwwgithub.com`, now `$4`'s default, an unrelated registrant's host ending in `github.com` with no label boundary) produced the real measurement. Override `$4` alongside `$1` if you change suffix families; a DEAD control is a FAIL, not a caveat. | No — krayt's pre-flight and its cross-checks are enforced entirely offline and unit-tested; P9 closes `support-wildcard-network-hosts.md`'s hardware bullet by confirming msb's runtime behaviour matches the source read the design rests on. |

`p7` **passed on 2026-09-04** and `internal/task/netpolicy_msb.go` now emits
`--on-secret-violation passthrough` as a result. It is the only probe written to decide a change to
krayt's own emitted policy rather than to record how msb behaves, so keep it runnable: it is the
evidence for that flag's value, and the reasoning in `NetworkArgs` points back here. Its expected
answer was read out of msb's source at v0.6.16
(substitution is gated on the secret's own scope *before* the violation action is consulted, so no
value of the flag can leak a credential) — the run exists because msb is beta and a source read is
not an observation. It takes three measurements because any one alone is unreadable: without the
`block-and-log` control, "the request succeeded" is equally consistent with "passthrough forwards"
and "this probe never provoked a violation"; without the in-scope guard, a passthrough that
silently stopped substituting altogether would look like a pass and break every run.

`p1` and `p2` are the two that *shape the design* of downstream tasks rather than merely sizing a
residual — both must be answered before `run-tasks-on-microsandbox.md` and
`dial-ask-channel-over-vsock.md` are implemented.

`p8` has **two arms, and the second is the one that tests krayt**. Measurements 1–3 build `msb
create` argv by hand, which establishes only a conditional: *given* argv A, msb resolves `--conf`
against flags thus. That krayt actually emits argv A is asserted by `internal/sandbox/msb_test.go`
against the fake `msb`, whose expected argv is itself hand-written — so both sides of the join are
transcriptions with nothing mechanically comparing them, and they can drift together and still both
pass. They had: every `msb create` in this script once emitted `--conf` **last**, the opposite of
`CreateSpec.Args()`, on the very axis measurement 1 measures. §8.1's claim ("krayt's own flags
outrank an `extra_conf`, so it cannot relax the run's network policy") is a claim about *krayt*, and
a conditional plus a transcription does not test it. So measurement 4 drives a real `krayt run`
whose `krayt.yaml` carries a real `sandbox.extra_conf:`, parks it at `--on-question=wait` for an
observation window (p6's trick), asserts `meta.json` recorded the file's digest, and then reads that
sandbox — the one krayt's own argv built — with the same curl probes. It needs `KRAYT_SECRETS`
pointing at a live agent credential, for p6's reason (someone has to authenticate to hold the
sandbox open); the secret *under test* is still an invented canary, since krayt's msb-era secrets
are `network.inject[]` keys resolved from the secrets file. Without `KRAYT_SECRETS` the arm is
skipped and the run prints a `NOTE:` saying §8.1's claim was not closed.

The arms are complementary. The synthetic one answers a question about msb that is true regardless
of krayt, and is the only way to ask measurement 2 at all — a config msb *rejects* cannot be
delivered through a krayt run, because `msb create` fails and no sandbox survives to read. The krayt
one answers whether krayt's shipped argv gets that resolution. When they disagree the pair localises
the fault: synthetic PASS + krayt FAIL means krayt builds different argv than documented; both
failing means msb changed. Either alone leaves that ambiguous. The synthetic controls do double
duty — they use the same generated config content the krayt arm hands to `sandbox.extra_conf`, so
the krayt arm needs no controls of its own.

Within the synthetic arm, `p8` bundles three measurements, each with its own control, into one run (five sandboxes total) —
the same "any one measurement alone is unreadable" discipline `p7` established. Precedence: a
sandbox given krayt's own `--net-default deny`/`--net-rule` flags *and* a `--conf` whose
`network.allow` names a different host must still refuse that host, checked against a control
sandbox where the *same* `--conf` runs with none of krayt's flags present (so the file's own
directive is confirmed live, not silently ignored — otherwise a BLOCKED result proves nothing).
Widening: a krayt-declared secret's `allowed_hosts` widened by a `--conf` entry for the same
env-var name gets the **real credential**, not a placeholder, substituted at the newly-allowed
host — checked against an identical sandbox with no `--conf`, which must show only the placeholder
there (otherwise "the real value arrived" is equally consistent with msb ignoring per-secret scope
entirely once a host is network-allowed, a much larger and unrelated defect). Per-secret
`on_violation` tightening is the one measurement this script's own header comment predicts will
fail before the hardware run happens: reading `crates/cli/lib/sandbox_config.rs`'s `SecretInput`
(the type backing both a root `--conf`'s `secrets:` map and `--secret-conf`) shows no `on_violation`
field, `deny_unknown_fields` set, and `materialize_secrets` hard-coding `on_violation: None` on
every entry built from a config file — so the field the internal `SecretEntry` struct carries
(cited in `add-msb-extra-conf-escape-hatch.md` decision 2) looks unreachable from any `msb` CLI
surface, config file or flag. The script still attempts it and classifies whichever way it comes
out; if it's rejected, decision 2 and the matching KRAYT_SPEC.md §8.1 paragraph need correcting.

`p6` **passed on 2026-09-04** (`run_63b9a3bf`): the guest's own `$CLAUDE_CODE_OAUTH_TOKEN` held
`$MSB_CLAUDE_CODE_OAUTH_TOKEN` — msb's placeholder — and the real value appeared in no msb argv, no
run artifact and not in `changes.patch`. Its environ reading came back inconclusive for the same
reason p4's did, which is why **p4 and p6 both want the same Linux/KVM re-run**: that one run
closes the environ window for msb's runtime process and for krayt's invocation of it at once.

`p6` is the odd one out and deliberately so: every other probe builds a synthetic sandbox to ask a
question about **msb**, while p6 drives a real `krayt run` to ask a question about **krayt's own
invocation of msb**. It needs that because the claim under test — only `msb create` ever carries
the value — is about which of krayt's calls attaches `secretEnv`, which no synthetic sandbox can
exercise. It parks the run at `--on-question=wait` to get an unlimited observation window, and it
does not chase the short-lived `msb create` process: reaching `waiting` at all proves the
credential got there, since the agent had to authenticate to ask its question.

It inherits p4's control-process discipline for the environ reading, and adds the same guard one
level up: if the host will not let it list processes at all, it **fails** rather than reporting an
empty process list as a clean one. That is not hypothetical — a sandboxed shell answers
`pgrep: Cannot get process list` on stderr and nothing on stdout, which with stderr discarded is
indistinguishable from "no msb is running".

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
./hack/msb-probes/p8-extra-conf-precedence.sh         # optional $1/$2: two header-echoing HTTPS endpoints on different hosts
# …and with the krayt-driven arm, which needs a live agent credential and makes a real, billed call:
KRAYT_SECRETS=./secrets.env ./hack/msb-probes/p8-extra-conf-precedence.sh
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
