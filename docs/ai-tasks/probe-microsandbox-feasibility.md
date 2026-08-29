# Task: probe microsandbox on real hardware — the feasibility gate for the B1 migration

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md`, and `KRAYT_SPEC.md` §14
(implementation protocol) first.** This is the **gate task** for the microsandbox arc: two of its
probes answer questions that *shape the design* of every task after it, so it lands first and its
blocking probes must be answered before `run-tasks-on-microsandbox.md` and
`dial-ask-channel-over-vsock.md` are implemented.

You cannot run these probes yourself — they need an Apple-Silicon Mac with microsandbox installed,
and one needs a live Anthropic credential (`KRAYT_SPEC.md` §14, "Categories that require a human").
**Your deliverable is therefore the probe harness, not the results**: scripts a human runs
unattended, each printing an unambiguous PASS/FAIL line, plus the `HUMAN_TODO.md` entry that hands
them over. Do not fabricate an outcome — an honestly-blocked probe is the correct end state.

## Background

The ADR (`docs/adr-microsandbox-sandbox-layer.md`) settles that krayt stops building a sandbox and
starts consuming one: microsandbox (`msb`) replaces `internal/{provider,guest,protocol,proxy,
vmimage,controlclient}` and the Nix VM image. Option **B1** was chosen — msb owns credential
substitution too.

Almost everything in the ADR was verified against msb's source. Five things were not, and two of
them change the design rather than merely sizing a residual:

- If a **non-root** container process cannot open `AF_VSOCK`, `krayt-ask` cannot dial the host
  directly and `dial-ask-channel-over-vsock.md` needs a different design (a root-owned forwarder
  inside the guest — which is the guest daemon B1 exists to delete).
- If `msb exec --user root` does **not** work under `--security restricted`, krayt must choose
  between the restricted profile and the guest helper's privilege separation
  (`fix-guest-git-config-rce.md`'s property). It cannot have both, and
  `add-krayt-guest-helper.md` needs to know which.

## Decisions already made (do not re-litigate)

1. **B1 is the chosen option.** Not A, not B2, not C. Do not re-open the strategic question, and do
   not write a probe that "compares" msb against the vfkit stack — the comparison is over.
2. **The probes are scripts, not Go tests.** They exercise a third-party binary against real
   hardware; a `-tags integration` Go test would imply they run in `hack/run-integration-tests.sh`,
   which they must not (msb is not installed on CI runners). Put them in `hack/msb-probes/`, POSIX
   `sh`, one file per probe, executable, no arguments required.
3. **Every probe prints exactly one terminal line**, `PASS: <probe> — <one-line finding>` or
   `FAIL: <probe> — <one-line finding>`, and exits 0/1 to match. A human pasting five lines into a
   `HUMAN_TODO.md` entry is the whole reporting protocol; nothing parses them.
4. **Probes clean up after themselves** — `msb rm --force` in a trap, on every exit path. A leaked
   sandbox on the reviewer's machine is a defect.
5. **`msb` is assumed already installed** (`curl -fsSL https://install.microsandbox.dev | sh`).
   Each probe checks `command -v msb` and exits with a clear message if absent; installing it is
   the human's job, documented in the `HUMAN_TODO.md` entry, not something a probe does.
6. **Pin the msb version under test.** Each probe prints `msb --version` before its finding. msb is
   beta and has already shipped a breaking wire change as a patch release (ADR, "The two questions
   that are not technical"); a finding without a version attached is not re-checkable. The version
   the ADR was written against is **0.6.16**.

## The probes

### P1 — `AF_VSOCK` from a non-root guest process  **(blocking)**

`msb run --vsock HOST_PATH:PORT` exposes a host unix socket at guest CID 2 on `PORT`
(`docs/networking/host-sockets.mdx`). krayt's agent images run as the non-root user `agent`
(uid 1000, `images/agents/claude-code/Dockerfile:26,58`). Whether that user may open an `AF_VSOCK`
socket in msb's guest depends on the guest's device-node permissions, which nothing in msb's docs
states.

- Host side: a listener on a unix socket that echoes one line. `socat` is not assumed present —
  write the listener as a small Go program under `hack/msb-probes/vsock-echo-host/` and have the
  probe `go run` it in the background.
- Guest side: a client that dials `AF_VSOCK` CID 2 on the probe port, writes a line, reads it back.
  Write it as `hack/msb-probes/vsock-probe-guest/`, cross-compiled by the probe script with
  `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build`, and `msb copy`'d into the sandbox. Use
  `github.com/mdlayher/vsock` — already a pinned dependency (`KRAYT_SPEC.md` §9.1), already used by
  `cmd/krayt-vsock-forward`.
- Create the sandbox from the **real agent image** (`ghcr.io/418-cloud/krayt-agent-claude-code`)
  with `--user agent`, so this doubles as the ADR's open question 1 ("does a krayt agent image run
  unmodified under msb"). Run the guest probe with `msb exec --user agent`.
- **PASS** means the echo round-trips as `agent`. **FAIL** — including "works as root, fails as
  `agent`" — must say which, because those two failures have different fixes.

### P2 — exec-as-root under the `restricted` security profile  **(blocking)**

`--security restricted` sets `no_new_privs`, drops the mount-admin capability, and forces
`nosuid,nodev` on user mounts (`docs/security/hardening.mdx:57`). `agentd` runs as PID 1 as root
and spawns each exec, so `msb exec --user root` should still work — but `no_new_privs` is exactly
the kind of flag that breaks a privilege *raise*, and the guest helper's whole value is running as
root against a git dir the agent (running as `agent`) cannot write.

- `msb create --security restricted --user agent <agent-image> --name krayt-probe-p2`
- `msb exec --user agent krayt-probe-p2 -- id -u` → expect `1000`
- `msb exec --user root krayt-probe-p2 -- id -u` → expect `0`
- Also assert the property that matters, not just the uid: as root, create `/probe-root-only` with
  mode `0700`, then confirm `msb exec --user agent … -- cat /probe-root-only/x` fails.
- **PASS** requires all three. On FAIL, the finding line must say whether root exec was refused
  outright or succeeded-but-unprivileged, and the `HUMAN_TODO.md` entry must state plainly that
  `add-krayt-guest-helper.md` then has to choose between `--security restricted` and the helper's
  privilege separation.

### P3 — does `--secret` alone enable TLS interception?  **(non-blocking; confirms a source finding)**

**This one is already answered by source reading; the probe is confirmation, not discovery.** The
ADR's config table says `network.mitm` is "gone — declaring any secret enables TLS interception
automatically", and msb's own `docs/cli/configuration.mdx:320` says the same. **Both are wrong for
the CLI flag path.** In msb 0.6.16:

- `TlsConfig.enabled` is `#[serde(default)] pub enabled: bool` — i.e. **false**
  (`packages/microsandbox-types/rust/lib/domain.rs:2408-2411`).
- The CLI's `has_tls` predicate, which is the only thing that sets `t.enabled(true)`, lists every
  `--tls-*` and `--trust-host-cas` flag and **does not include `opts.secret`**
  (`crates/cli/lib/commands/common.rs:2198-2208`).
- `SecretEntry.require_tls_identity` defaults **true** (`domain.rs:2130-2134`), and the handler
  skips any such secret on a non-intercepted connection (`crates/network/lib/secrets/handler.rs:876`).

So `--secret NAME@host` **without** `--tls-intercept` means the placeholder is silently never
substituted: the request goes out over an unbroken TLS session carrying a meaningless string, and
the API rejects it as a bad credential. That is a silent-failure trap, which is why
`translate-network-policy-to-msb.md` makes emitting `--tls-intercept` mandatory whenever any secret
is declared.

The probe: send a request carrying the placeholder to any header-echoing HTTPS endpoint the
operator allows (the script takes the endpoint as `$1`, defaulting to `https://postman-echo.com/get`,
and the `HUMAN_TODO.md` entry says to substitute one they trust), once **with** `--tls-intercept`
and once **without**, and compare which of the placeholder and the real value arrives. Expect
substitution only in the first. If both substitute, msb's behaviour differs from its source as read
here and `translate-network-policy-to-msb.md` should be simplified accordingly — say so in the
finding rather than quietly moving on.

### P4 — how long the real value sits in an environment  **(non-blocking; sizes an accepted residual)**

The ADR's secret-handling contract *accepts* environ exposure (`msb` reads the value from krayt's
`cmd.Env`); what is unknown is whether it lives in the short-lived `msb create` process or in the
long-lived per-sandbox runtime for the whole run. This decides nothing — it sizes a residual krayt
has already accepted — but it is cheap and belongs in the spec's security section.

The ADR gives the Linux form:

```sh
KRAYT_CANARY=sk-canary msb create python --name t --secret 'KRAYT_CANARY@api.example.com'
tr '\0' '\n' < /proc/$(pgrep -f 'msb sandbox')/environ | grep KRAYT_CANARY
```

**macOS has no `/proc`.** Write the probe to use `ps -Eww -p <pid>` there and state in its output
that macOS may refuse to show another process's environment even at the same uid, in which case the
finding is "inconclusive on darwin" — not PASS and not FAIL. The Linux form is the authoritative
one; note in the `HUMAN_TODO.md` entry that running it on a Linux/KVM host answers it for both,
since it is a property of msb's process structure, not the host OS.

### P5 — does Claude Code accept msb's default placeholder?  **(non-blocking; needs a live credential)**

msb's default guest placeholder is `$MSB_<ENV_VAR>` (`docs/sandboxes/secrets.mdx`). krayt is taking
that default rather than supplying a shaped one (see `hand-secrets-to-msb.md`). The residual is not
msb mangling the placeholder — headers are the default `inject` target and the three documented
substitution exclusions are all body-shaped — but whether **Claude Code itself** rejects
`$MSB_ANTHROPIC_API_KEY` client-side, before any request leaves the container, on a length or
`sk-ant-` prefix check.

One live run against `api.anthropic.com` settles it: create a sandbox from the Claude Code agent
image with `--tls-intercept`, `--net-default-egress deny`, `--net-rule "allow@api.anthropic.com"`,
and `--secret 'ANTHROPIC_API_KEY@api.anthropic.com'` (value in the invoking shell's environment),
then exec a one-shot `claude -p 'reply with the single word ok'`.

**If it FAILS**, the fix is known and must be recorded in the finding rather than improvised: msb
exposes a per-secret `placeholder` field, but **only through a config file, not argv** — there is no
`--secret-placeholder` flag. `--secret-conf PATH` takes an unwrapped map of secret definitions
(`docs/cli/configuration.mdx:72`), and `--secret` does *not* carry clap's `conflicts_with = "net_conf"`
that `--net-rule` does, so the two may be combined. `hand-secrets-to-msb.md` documents that
contingency; this probe decides whether it has to be built.

## What to build

- `hack/msb-probes/` — `p1-vsock-nonroot.sh` … `p5-placeholder-accepted.sh`, plus `README.md`
  explaining what each answers, what installs `msb`, and which two are blocking.
- `hack/msb-probes/vsock-echo-host/` and `hack/msb-probes/vsock-probe-guest/` — the two small Go
  programs P1 needs. Keep them `package main` under `hack/`, matching `hack/netprobe` and
  `hack/edit-probe`; they are probe fixtures, not shipped code.
- One `HUMAN_TODO.md` entry per `KRAYT_SPEC.md` §14's template, covering all five probes as a single
  handoff (they share an install step and a machine). `Blocking: yes` — name P1 and P2 as the
  blocking pair and say which tasks each blocks.
- A new **Phase 11** stub in `KRAYT_SPEC.md` §14 for the microsandbox migration, whose first
  unchecked item is this gate, so the results have a durable home when the entry is deleted (§14's
  entry lifecycle).
- A `docs/ai-tasks/README.md` row in a new "Microsandbox migration (B1)" section — see that file's
  own note about the Status column being a durable record.

## Done when

- `sh -n hack/msb-probes/*.sh` is clean and every script is executable.
- `go build ./hack/msb-probes/...` succeeds for both host and guest programs, and
  `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./hack/msb-probes/vsock-probe-guest` succeeds from
  macOS (the cross-build the probe itself performs).
- `go build ./...`, `go test -race ./...` and `golangci-lint run` stay green — nothing here touches
  shipped packages.
- The `HUMAN_TODO.md` entry exists and names the exact commands.
- **The probes have not been run.** Say so plainly in your report.

## Out of scope

- Installing msb, running any probe, or recording any result.
- The `krayt doctor` msb check — that is `add-msb-sandbox-driver.md`.
- Deleting or modifying any existing krayt package. This task adds files under `hack/` and `docs/`,
  edits `HUMAN_TODO.md`, `KRAYT_SPEC.md` §14, and `docs/ai-tasks/README.md`, and nothing else.
