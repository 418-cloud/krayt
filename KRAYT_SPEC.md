# Krayt — Ephemeral VM Sandbox for AI Coding Agents

> **Status:** Draft spec / implementation plan
> **Name:** `krayt` (working name — changeable later)
> **Target language:** Go
> **Primary platform:** macOS (Apple Silicon)
> **Secondary platform:** Linux (architected for, not built in v1)

---

## 1. Overview

`krayt` gives an AI coding agent a disposable, isolated environment to work in.
The flow is intentionally simple and ephemeral:

1. Spin up a **fresh, minimal Linux micro-VM**.
2. **Tar-pipe** a snapshot of the target repo into it (no live shared folder).
3. Launch a **user-provided Docker image** inside the VM that already contains all
   tools plus the AI agent (Claude Code, Gemini CLI, etc.).
4. Hand the agent a **task** and a tightly-scoped set of capabilities (network
   allowlist, secrets).
5. The agent works **freely** inside the container.
6. On completion, produce a **`git diff` patch** plus a structured report.
7. **Extract** the patch + report out of the VM.
8. **Destroy** the VM.
9. The human reviews the patch and applies it with `git apply` on the host if happy.

The VM is a **strong isolation boundary** (separate kernel) so untrusted code never
touches the host kernel or filesystem. The host repo is never live-mounted; the only
thing that flows back is a reviewable text patch.

---

## 2. Goals & Non-Goals

### Goals
- One disposable VM **per agent run**, with **multiple concurrent runs** supported.
- Host repo isolation: **no live shared folder**; input via git bundle, output via patch.
- **User-supplied Docker image** is the unit of capability — `krayt` knows nothing
  about which AI or tools are inside.
- A **minimal VM** whose only job is to run a single container + a small guest agent. Nothing else.
- **Cross-platform-ready architecture**: macOS native today, Linux drop-in later,
  without rearchitecting.
- **Reviewable, auditable** output. Nothing auto-applies to the host.

### Non-Goals (v1)
- Live bidirectional filesystem sync.
- Building/maintaining the user's container images.
- Auto-applying patches or pushing branches.
- A GUI. CLI only.
- Running on Linux natively in v1 (designed for, deferred in implementation).
- gVisor / shared-kernel sandboxing (we deliberately use a full VM boundary).
- **Exposing a Docker socket / docker-in-docker inside the VM.** The VM runs one
  user-supplied OCI image as a single container; it does not provide a Docker API for
  the agent to spawn further containers. If an image needs that, krayt is the wrong
  tool for it.

---

## 3. Design Principles

1. **krayt consumes a sandbox rather than building one.** `internal/sandbox` (§6.15) drives the
   `msb` (microsandbox) CLI as a subprocess over argv/stdio — the only place in krayt that knows
   a sandbox runtime exists, and it has no build tags, since msb itself is what does the
   OS-specific work on each platform. Everything above it (orchestration, patch logic, secrets,
   CLI) is plain, OS-agnostic Go. *(Supersedes the original v1 principle — "the Provider interface
   is the only OS-specific seam," with a shared guest agent running inside a krayt-built VM on
   both vfkit/macOS and Firecracker/Linux — decided superseded 2026-08-29 by ADR option B1,
   `docs/adr-microsandbox-sandbox-layer.md`, and deleted by `run-tasks-on-microsandbox.md`. See §15
   for the full record.)*
2. **Agent-agnostic core, convention-driven contract.** The tool injects inputs at
   well-known paths and reads outputs from well-known paths. Optional adapters add
   convenience for specific agents but are never required.
3. **Ephemeral by default.** A run owns its VM start-to-finish, then the VM is destroyed.
4. **Default-deny.** Network egress, secrets, and host access are all opt-in per task.
5. **Plain text out.** The deliverable is a `git` patch — diffable, reviewable, atomic.

---

## 4. Decisions (Locked In)

| Area | Decision |
|---|---|
| Language | Go |
| Primary OS | macOS (Apple Silicon) |
| Sandbox backend | **microsandbox (`msb`)** — a libkrun-based microVM runtime, driven as a subprocess by `internal/sandbox` (§6.15). One backend for macOS and Linux alike; no `Provider` interface, no per-OS provider package. *Decided 2026-08-29, ADR option B1 (`docs/adr-microsandbox-sandbox-layer.md`); supersedes the two rows below — see §15.* |
| ~~macOS VM backend~~ *(superseded)* | ~~**vfkit** (`crc-org/vfkit`) for v1 — drives Virtualization.framework via a tested, pre-signed subprocess; direct `Code-Hex/vz` embedding is the documented swap-in fallback, both behind the `Provider` seam~~ — deleted by `run-tasks-on-microsandbox.md`; see §15 |
| ~~Linux VM backend (future)~~ *(superseded)* | ~~Firecracker or Cloud Hypervisor via the same `Provider` interface~~ — built in Phase 7 as a Firecracker provider behind `Provider`, deleted by `run-tasks-on-microsandbox.md`; see §15 |
| Tool ↔ agent | Convention-first contract + optional orchestration adapters |
| Networking | Per-task policy; **default allowlist**, translated to a fully explicit `msb create` policy (§6.6) |
| Interaction | **Headless default**, attachable live log streaming |
| Concurrency | **Multiple concurrent** agent VMs |
| Output | `git diff` patch only; **manual apply** on host |
| Secrets | Per-task **secrets file**; a declared secret's value travels only in the `msb create` child's env, never on argv, never persisted (§6.6.1, §6.8) |
| Task definition | **CLI flags + optional config file** (flags override file) |
| Resource limits | Sensible defaults (e.g. 2 vCPU / 4 GB / 20 GB / 30 min), **fully configurable** |
| Agent → human questions | Optional async `ask_human` via an MCP server + `krayt-ask` CLI over an agnostic question channel; **default `fail`** (autonomous), opt into `wait`; timeout → sentinel by default (§6.13) |
| Agent authentication | Credential injected via the per-task secrets file (§6.8); scoped **API key** is the default, `CLAUDE_CODE_OAUTH_TOKEN` for subscription auth; the per-agent adapter enforces **exactly-one** credential; API key recommended for untrusted/concurrent runs (§6.14) |

---

## 5. High-Level Architecture

> **Redrawn by `run-tasks-on-microsandbox.md` (the cut-over, §14 Phase 11).** krayt no longer
> builds the isolation boundary — the guest agent, the gRPC control protocol, and the in-VM/
> host-side egress proxy are all deleted (§6.3–§6.5, §6.6, §6.10–§6.12). It rents one from `msb`
> instead. See git history for the pre-msb diagram.

```
┌──────────────────────────────── HOST (macOS / Linux) ────────────────────────────────┐
│                                                                                        │
│   krayt CLI                                                                           │
│        │                                                                              │
│        ▼                                                                              │
│   Orchestrator ──────────── manages N concurrent Runs, IDs, state, cleanup (§6.2)     │
│        │                                                                              │
│        ▼                                                                              │
│   internal/sandbox (msb driver, §6.15) ── argv/stdio only, no network protocol of its own
│        │                                                                              │
│        │  msb create / msb copy / msb exec --stream / msb logs / msb stop / msb rm    │
│        ▼                                                                              │
│   msb (microsandbox CLI, subprocess) ── rents, does not build, the isolation boundary  │
│        │                                                                              │
│        │  boots (libkrun)                                                             │
│        ▼                                                                              │
│   ┌──────────────── microVM sandbox (msb-managed) ──────────────────┐                 │
│   │                                                                  │                 │
│   │   agentd (msb's own, PID 1)                                     │                 │
│   │     └── the user's OCI IMAGE, as the sandbox rootfs             │                 │
│   │            AI agent (claude code / gemini / …) + tools          │                 │
│   │            /workspace          ← repo snapshot (krayt-helper)   │                 │
│   │            /task/prompt.md     ← the task                      │                 │
│   │            /output/*           ← patch + report                │                 │
│   │            /usr/local/bin/krayt-ask  ← ask_human CLI front-end  │                 │
│   └──────────────────────────────────────────────────────────────────┘                │
│        ▲                                                                              │
│        │ krayt-ask dials AF_VSOCK → host CID 2 : AskPort, over msb's own --vsock route │
│        │ (guest-initiated; the only channel that isn't a krayt→msb subprocess call)    │
│        │                                                                              │
│   internal/askbridge ── host unix socket (runDir/ask/ask.sock), §6.13                 │
└────────────────────────────────────────────────────────────────────────────────────────┘
```

There is no protocol of krayt's own on the host↔sandbox path: `internal/sandbox` shells out to
`msb` and reads its `--format json`/`--stream` output (§6.15, §7) — no vsock, no gRPC, nothing
generated. The one exception is `ask_human` (§6.13): the sandbox's `krayt-ask` dials `AF_VSOCK` to
host CID 2 directly, which msb's own `--vsock HOST_PATH:PORT` route bridges to the host unix
socket `internal/askbridge` listens on. No guest daemon, and no control protocol on either side of
that channel either.

---

## 6. Components

### 6.1 CLI (`internal/cli`)
Cobra-style command surface (see §13). Parses flags, loads optional task config,
merges them (flags win), hands a fully-resolved `RunSpec` to the orchestrator.

`RunSpec` is the host-side, fully-resolved description of one run (config + flags +
defaults already merged). It lives in `internal/task`:

```go
type RunSpec struct {
    ID           string            // assigned by the orchestrator
    ImageRef     string            // user OCI image (tag or digest)
    RepoPath     string            // host repo to bundle (default: cwd)
    IncludeDirty bool              // include uncommitted changes via non-mutating capture (§6.7)
    BundleDepth  int               // forward-bundle shape (§6.7); default 1 = snapshot, 0 = full history
    TaskPrompt   []byte            // contents of the task (file or inline)
    Env          map[string]string // non-secret env for the container
    SecretsPath  string            // path to per-task secrets file (may be empty)
    Network      NetworkPolicy     // mode + allowlist (mirrors the proto enum, §6.5)
    Resources    Resources         // CPUs, MemoryMiB, DiskGiB, Timeout
    Questions    QuestionsPolicy   // mode + per-question timeout + on-timeout (§6.13)
    Detach       bool              // headless vs stream-to-terminal
}

type Resources struct {
    CPUs      int
    MemoryMiB uint64
    DiskGiB   uint64
    Timeout   time.Duration       // wall-clock; expiry kills container then VM
}

type QuestionsPolicy struct {
    Mode      string              // "fail" (default) | "wait"
    Timeout   time.Duration       // per-question wait limit
    OnTimeout string              // "sentinel" (default) | "abort"
}
```

Resolution order (§8.3): built-in defaults → config file → flags. The orchestrator
derives the `VMSpec` (§6.3) from `RunSpec.Resources` + the pinned base image.

**Host resource preflight.** Before booting a VM, `krayt run` compares live host free RAM and
free disk against `spec.Resources.MemoryMiB`/`DiskGiB` plus a fixed safety margin
(`memMarginMiB = 2048`, `diskMarginGiB = 5`) reserved for the host OS and other processes, and
refuses to start — no VM created — if it doesn't fit. This is a host-wide, live-measurement
check (macOS only, via `vm_stat` for free memory and `syscall.Statfs` on the cache directory for
free disk; a no-op elsewhere, since the only real backend today, vfkit, is macOS-only), distinct
from and unrelated to `--max-concurrency`'s per-`.krayt` run-count limit (§6.2): it catches
oversubscription across *all* runs on the host, from any repo, not just this repo's own runs.
`--skip-resource-check` bypasses it for a user who knows better. The error names the actual free
vs. needed vs. margin numbers so it's actionable without reading source. Added after a real
incident: two concurrent runs on a 16GB Mac exhausted host RAM/disk and one died ~11 minutes in
with an opaque gRPC EOF instead of failing fast at start (`docs/ai-tasks/preflight-host-resources.md`).

### 6.2 Orchestrator (`internal/orchestrator`)
- Owns the set of active runs (map keyed by run ID).
- Allocates a unique run ID per VM (and a vsock CID on the Firecracker backend only; §6.12).
- Enforces optional max-concurrency and per-run resource budgets.
- Drives the run lifecycle (§7) and guarantees VM teardown even on error/panic/signal.
- Tracks run state including a **`waiting`** state when the agent has asked a question and
  `mode: wait` is set (§6.13); waiting runs still own a live VM and count against concurrency.
- Persists run metadata + artifacts under the project's `.krayt/runs/<id>/` (§8.4).
- **`running` means the code snapshot is already captured.** The transition to `running` is
  written only after the code bundle (§6.7) has been built from the host repo and streamed to
  the guest — not merely once the VM has booted. Before that point the state is `starting`: the
  VM may be booting, or the image/code transfer may still be in progress, and the host repo must
  not be assumed to have been read yet. This makes `running`, as observed externally via
  `meta.json`/`krayt ls`, the reliable signal that it is safe to mutate the host repo (checkout a
  branch, commit, rebase) without affecting this run's snapshot.

**Run supervision — daemon-less, process-agnostic.** krayt has **no central daemon**. Each
run is driven by a self-contained supervision loop that writes *all* run state to
`.krayt/runs/<id>/` — live logs to `logs/`, the lifecycle state (`starting`→`running`→
`waiting`→`done`/`failed`/`timed_out`) to `meta.json`, and Q&A to `questions/` — so it is
independent of the invoking terminal. Every management command (`ls`, `attach`, `logs`,
`stop`, `answer`, `rm`) operates on that **on-disk state plus a direct dial to the run's
recorded guest control socket** (§6.12), never on an in-process handle. This is what lets
`krayt answer` reach a `waiting` run's guest from a different invocation without any
daemon: the guest's `Answer` RPC (§6.5) is the coordination point, and the socket path lives
in the run dir.

- **Foreground supervisor:** without `--detach`, the `krayt run` process itself supervises its
  run to completion, streaming logs to the terminal.
- **Detached supervisor — "park and walk away" (Phase 5):** `krayt run --detach` re-execs a
  **session-detached (`setsid`) per-run supervisor child** (still **no central daemon**) that
  owns the VM to completion, then returns immediately — so the human can start a run, close the
  terminal, get the `waiting` notification later, and `krayt answer` it. Go's runtime rules out
  a raw `fork()`, so the child is a re-exec of the same argv with the run id passed through the
  environment; `setsid` detaches it from the controlling terminal so it outlives the shell. It
  records its own pid, so `krayt stop` signals it like any foreground run. Max-concurrency is
  enforced **across every process** sharing one `.krayt` by a file-lock semaphore (`AcquireSlot`
  over `.krayt/slots/`, sized by `--max-concurrency`), so foreground and detached runs queue
  against the same limit and a crashed holder's slot is released by the OS. Because the state
  model and every management command are already daemon-less and process-agnostic, this is
  localized to the run entrypoint — the rest is unchanged.

### 6.3 Provider interface (`internal/provider`)
Deleted — superseded by `internal/sandbox` (§6.15) under ADR option B1
(`docs/adr-microsandbox-sandbox-layer.md`, `run-tasks-on-microsandbox.md`). See git history for
the pre-msb text.

### 6.4 Guest agent (`internal/guest`)
Deleted — superseded by `internal/sandbox` (§6.15) and `cmd/krayt-helper` (§6.7) under ADR option
B1 (`docs/adr-microsandbox-sandbox-layer.md`, `run-tasks-on-microsandbox.md`). See git history for
the pre-msb text.

### 6.5 Control protocol (`internal/protocol`)
Deleted — superseded by `internal/sandbox` (§6.15) under ADR option B1
(`docs/adr-microsandbox-sandbox-layer.md`, `run-tasks-on-microsandbox.md`). There is no gRPC, no
`.proto`, and no generated Go on either side of the sandbox boundary any more — krayt drives `msb`
over argv/stdio instead (§6.15, §7). See git history for the pre-msb text.

### 6.6 Networking & egress policy (`internal/task`, `internal/sandbox`)
> **Rewritten by `run-tasks-on-microsandbox.md` (the cut-over, §14 Phase 11).** krayt's own
> in-guest/host-side egress proxy — `internal/proxy`, `internal/guest/proxy`,
> `cmd/krayt-vsock-forward`, the guest-initiated `EgressPort` vsock channel, and the per-VM tap +
> `/30` + `cap_net_admin` Linux setup (`hack/linux-net-setup.sh`) — is **deleted**. msb (§2, §6.15)
> owns egress policy and TLS interception itself; krayt's job shrinks to translating `krayt.yaml`'s
> network vocabulary into a fully explicit `msb create` policy and never letting an implicit msb
> default govern a run. Everything below is the current, live design — see git history for the
> pre-cutover text; `docs/adr-microsandbox-sandbox-layer.md` ("Default posture: what a bare
> sandbox gets") is the strategic record of why this shape was chosen, and
> `translate-network-policy-to-msb.md` built the pure translator this section now describes as
> live rather than additive.

**The never-empty-policy rule.** msb's own default egress policy — when no
`--net`/`--no-net`/`--net-default*` flag is present at all — is `from_profiles([Public])`: an
implicit `allow@public` rule granting the *entire public internet*, and critically, `--net-rule`
alone does **not** suppress it; it only layers krayt's rules on top of that default. So
`task.NetworkArgs(spec.Network, hasSecrets)` (`internal/task/netpolicy_msb.go`), called from
`orchestrator.Run` (§7 step 1) on every `krayt run`, always emits one of `--no-net`,
`--net-default deny`, or the `--net-default-egress`/`--net-default-ingress` pair — computing "no
policy" is a translation error the function itself refuses to return, never a state a real `msb
create` call can reach:

- **`none`** → `--no-net` and **zero** `--net-rule` flags. `--net none` is deliberately avoided:
  msb still layers any supplied `--net-rule` on top of `NetworkPolicy::none()`, so even one stray
  rule would silently punch a hole through the mode that is supposed to mean no network at all.
- **`allowlist` (default)** → `--net-default deny`, `allow@dns`, explicit `deny@<group>` rules for
  every private destination group (below), then one `allow@<host>` per `network.allow`
  entry, in the order given — deterministic, so the same config always renders byte-identical
  argv (pinned by golden tests against this repo's own `krayt.yaml`).
- **`full`** → `--net-default-egress allow --net-default-ingress deny`, plus the *same*
  `allow@dns` and explicit `deny@<group>` rules: `full`'s allow must not mean "and also the
  host's LAN" — the
  identical design rule krayt's pre-msb resolved-IP dialer guard used to enforce, now expressed as
  explicit msb rules instead of a Go-side check, since msb — not krayt — is doing the dialing.
- **Private/loopback/link-local/metadata stay denied in every mode, including `full`** — an
  existing krayt property carried forward, not a new one. msb exposes this as destination groups
  (`private`, `loopback`, `link-local`, `meta`, `multicast`, `host`; `msbDenyGroups` in
  `netpolicy_msb.go`), denied explicitly and ordered before every allow rule **except
  `allow@dns`** — see the DNS paragraph below for why that one has to come first.
- **Ingress is denied explicitly, in every mode** (`--net-default-ingress deny` for `full`; the
  deny-default modes need no separate ingress flag). krayt publishes no ports today, so this is
  inert — but msb's own ingress default is `allow`, and closing it costs one flag now rather than
  becoming a live gap the moment krayt publishes anything.

**The guest regains DNS — a genuine capability gain, stated plainly.** Under the pre-msb design
the guest had no usable network at all in `allowlist`/`none` — everything rode vsock to a host
proxy. Under msb the guest has a real, policed network interface, so `allowlist` mode emits
`allow@dns` explicitly — msb's `--net-default*` path adds **no** implicit DNS rule the way its
profile-based default does, so a deny-default allowlist with no explicit `allow@dns` resolves
nothing — and DNS is policed by msb's own gateway, with DNS-rebind protection on by default.

**`allow@dns` is emitted before the `deny@<group>` rules, and that ordering is load-bearing.**
msb's `dns` is a *destination*, not a capability — its own CLI help defines it as "the semantic
`dns` target for gateway UDP/TCP port 53" — and that gateway is the guest's end of a /30 carved
out of `--net-ipv4-pool`, which defaults to `172.16.0.0/12`. `dns` and `private` therefore name
overlapping destinations, and msb evaluates rules first-match-wins within a direction, so
whichever krayt emits first decides. With the denies first the gateway matched `deny@private` and
the guest resolved nothing: every agent request failed `ENOTFOUND` inside a sandbox whose policy
was otherwise exactly right. `dns` is the single exception to "denies before allows", and a narrow
one — it opens the gateway's port 53, not the private groups those denies exist to close.

**`--tls-intercept` is emitted whenever any secret is declared, and only then.** This is
redundant with msb's own behavior — `SandboxBuilder::secret_entry` turns on interception
unconditionally the instant any `--secret` is declared (confirmed on hardware,
`hack/msb-probes/p3-secret-tls-intercept.sh`, 2026-08-29) — emitted anyway to pin that behavior
explicitly rather than depend on an undocumented builder side effect in a beta tool.

**`network.mitm` has no msb equivalent and is a hard pre-flight error**
(`task.ValidateNetworkPolicyForMsb`, the live validator since this cutover, called from
`internal/cli` before any sandbox is created): under msb there is no configuration in which a
secret is declared without TLS interception, so the opt-in the key used to represent has nothing
left to opt into. See §6.6.1 for what replaced `network.inject`'s header-shaped vocabulary.

**The un-stripped-header regression (§10).** msb never strips a pre-existing auth header the way
`internal/proxy` used to before setting its own — a credential the agent obtained elsewhere and
placed in a header addressed to an allowed host now goes out **untouched**. Bounded by the
allowlist (the agent can only send it somewhere already permitted), not eliminated; see §10's
threat table for the full statement.

**Container hardening follows the same "removed key, named replacement" shape.**
`internal/task/container_msb.go`'s `ValidateContainerPolicyForMsb` is the container-policy
counterpart to `ValidateNetworkPolicyForMsb` (§8.1, §6.10 stub): msb's `--security
default|restricted` replaces krayt's own OCI-spec capability/seccomp/rootfs knobs entirely
(`harden-container-oci-spec.md`'s work is superseded by this cutover), so `container.capabilities`,
`container.seccomp: unconfined`, and `container.readonly_rootfs` are removed keys that hard-error,
each naming `--security` as the replacement. Every krayt sandbox is created with `--security
restricted` (fixed, not user-configurable) — chosen because `probe-microsandbox-feasibility.md`'s
P2 (2026-08-30) confirmed `msb exec --user root` still works under `--security restricted` with a
root-owned path staying unreadable to an `--user agent` exec, so the guest helper (§6.7) keeps
BOTH the restricted profile AND its own privilege separation rather than trading one for the
other.

#### 6.6.1 Secret substitution at the host (`internal/task`, `internal/sandbox`)
> **Rewritten by `run-tasks-on-microsandbox.md` (the cut-over).** The host-side TLS-intercepting
> MITM proxy this section used to describe (`internal/proxy`'s CA/leaf-cache/injection machinery,
> `network.mitm`/`network.inject[].{strip,set,set_prefix,set_literal,refresh}`,
> `internal/adapter/anthropic_wire.go`) is **deleted**. msb owns credential substitution itself;
> krayt's job shrinks to naming which secrets-file key may reach which hosts. See git history for
> the pre-cutover text.

**Three channels, one carries a value** (`docs/adr-microsandbox-sandbox-layer.md`, "How secrets
actually reach msb"; §6.15, §8.1):

| krayt input | Channel | Carries a value? |
|---|---|---|
| `krayt.yaml` `network.inject[]` → `task.SecretSpec{Key, Hosts}` | `--secret NAME@host1,host2,...` — one per credential, rendered by `internal/sandbox.SecretArgs` | no — name + hosts only |
| `krayt.yaml` `network.allow`/`network.mode` | `--net-rule`/`--net-default*` (`task.NetworkArgs`, §6.6) | no |
| the secrets file's real values | `cmd.Env` on the spawned `msb create` process only (`internal/sandbox.SecretEnv`) | **yes — this only** |

msb itself enforces the argv half: `--secret` accepts only `NAME@HOST[,HOST...]` and rejects an
inline `NAME=VALUE@HOST` on both `create` and `modify`. The value channel is `exec.Cmd.Env`, never
`os.Environ()` — `internal/sandbox.Client`'s closed child-env allowlist (§6.15) plus the resolved
`KEY=VALUE` pairs for exactly the declared keys, appended only to the one `msb create` invocation
that starts the sandbox. `Client.Create` is the only method with a `secretEnv` parameter — every
other method, `Exec` included, has none — so the Timing rule below is structural, not
conventional.

**The narrowed contract (decided in the ADR, "The secret-handling contract").** krayt's pre-msb
rule was "never on argv, never in env" — the deleted proxy's stdin channel existed specifically to
meet it. Under msb the requirement narrows to **never on argv, never persisted**:
`/proc/<pid>/environ` is mode `0400` (same-uid only), unlike the world-readable
`/proc/<pid>/cmdline` that makes argv unacceptable, and the adversary the narrowed rule admits —
someone who can already read `secrets.env` (0600), ptrace krayt, or read msb's own heap, where the
value lives for the sandbox's lifetime un-zeroized regardless of how it arrived — has host
compromise already, which is out of scope for both threat models. This is a decision made once,
in the ADR, not a silent regression nobody noticed.

**Timing.** msb reads a secret's value "at start time", not at config-load time — the environment
must be set on whichever invocation actually starts the sandbox (`msb create`). A later `msb exec`
against the running sandbox needs no env at all; the per-sandbox host runtime holds the value for
the sandbox's lifetime.

**Every secret must be network-scoped — a capability loss, stated plainly (§6.8, §8.1).** Under
the pre-msb design a `secrets.env` key could be materialized inside the guest and used *there* —
an SSH key, a signing key, a local database password. msb has no equivalent channel: `--secret`
never puts a value in the guest (the guest gets msb's own placeholder instead), `--env KEY=VALUE`
puts the value on argv (disqualified), and `msb copy` into a tmpfs mount would require writing the
value to a host temp file first (rejected — it trades away "never persisted" for a weaker
property). `task.ValidateNetworkPolicyForMsb` therefore hard-errors a `secrets.env` key with no
`network.inject` entry naming it, pre-flight, rather than silently delivering something weaker
than the key's name promises.

**The mechanism difference from krayt's own, deleted proxy.** krayt used to do *shape
translation*: know the provider's wire format, strip a named header, set a different one
(`internal/adapter/anthropic_wire.go`, deleted by this cutover). msb does *placeholder
substitution*: it finds the placeholder string the workload already sent — under its own default,
`$MSB_<NAME>`, or a shaped placeholder if one is set — and swaps it in place, wherever it appears,
without needing to know which header it was in. `network.inject`'s schema shrank accordingly
(§8.1): a `krayt.yaml` entry now names a secrets-file **key** and the **hosts** it may be
substituted to, nothing else — no header name, no prefix, no literal, no refresh hook.
`task.SecretSpecsFromConfig` hard-errors, naming itself, on any of the old
`host`/`strip`/`set`/`set_prefix`/`set_literal`/`refresh` fields appearing on an entry, rather than
silently ignoring one — silently dropping `strip` specifically would reopen the regression below
without telling anyone.

**The un-stripped-header regression (§10, §6.6).** msb never strips a pre-existing auth header
before substituting its own placeholder's real value in — krayt's deleted proxy stripped
`authorization`/`x-api-key` first. A credential the agent obtained elsewhere and placed in that
header, addressed to an allowed host, now goes out **untouched**. Bounded by the allowlist (the
agent can only send it somewhere already permitted), not eliminated — see §10.

**Adapter-contributed secrets.** An adapter (§6.14) contributes its own selected credential as a
`task.SecretSpec` via `Plan.Secrets` rather than an `Inject` rule; `task.MergeSecretSpecs` unions
it under the user's own `network.inject`-derived specs, with the user's own entry winning on a key
collision — the same "user wins" precedence the deleted `MergeInjectRules` used, carried forward
onto the new shape.

### 6.7 Code transfer & patch generation (`internal/patch`)
The repo enters the VM as a **git bundle** — a single self-contained byte stream carrying
real git objects — and is **cloned** into `/workspace` as a real repository. Unlike a flat
`git archive` snapshot, a bundle imports a **real commit as HEAD** (tagged `krayt-baseline`), so
the guest never fabricates a baseline at apply time. *What* that imported HEAD is depends on
`bundle_depth` (below): full history (`0`) imports the repo's real HEAD, keeping real ancestry — a
commit-level merge-base and a host-fetchable reverse `commits.bundle`; a snapshot (`>= 1`, the
default) imports a synthetic **parentless** commit carrying HEAD's tree, so the net change still
round-trips via `changes.patch` (3-way apply matches at the **blob** level, since the tree's blobs
are the host's) but individual agent commits do not. Either way a bundle preserves git's object
model exactly — file modes, executable bits, symlinks — which the tar/`git archive` path did not
guarantee.

**The forward bundle must be self-contained (host → guest).** The guest clones into an
*empty* VM, so the inbound bundle must carry **no prerequisites**. A range bundle (e.g.
`HEAD~1..HEAD`) records prerequisite commit IDs in its header, and a clone into an empty repo
then fails with a "does not have … prerequisite commits" error — so the forward direction
**must not** use a range. `git bundle create` also has **no `--depth` flag**, and — critically —
it does **not** record a shallow clone's boundary: bundling a shallow clone produces a bundle
that references HEAD's parents without including them, so the guest clone fails with *"remote did
not send all necessary objects"* for any repo whose HEAD has parents. (A single-commit repo hides
this, since depth-1 cuts nothing.) krayt therefore makes the bundle self-contained *by shape*,
keyed on `bundle_depth`:

```
# bundle_depth >= 1 (default): a single-commit SNAPSHOT of the current state
git clone --depth 1 file://$REPO $TMP/src                    # just the tip's tree + blobs
TREE=$(git -C $TMP/src rev-parse 'HEAD^{tree}')
SNAP=$(git -C $TMP/src commit-tree $TREE -m "krayt: workspace snapshot")   # PARENTLESS root commit
git -C $TMP/src update-ref refs/heads/<branch> $SNAP
git -C $TMP/src bundle create $TMP/repo.bundle HEAD <branch> # no boundary → self-contained

# bundle_depth <= 0: full history — a full clone bundled as-is (real SHAs, all objects)
git clone file://$REPO $TMP/src
git -C $TMP/src bundle create $TMP/repo.bundle HEAD <branch>
```

A parentless snapshot has no shallow boundary, so it clones cleanly into the empty guest (the
bundle must name at least one ref for `git clone` to check out). So `bundle_depth` (§6.1/§8.1)
means **`0` = full history; `>= 1` = single-commit snapshot** (default `1`). Use `0` when the
agent needs history, or when you want the reverse `commits.bundle` to be host-fetchable — its
baseline is a real commit only with full history; a snapshot baseline is synthetic, so
`changes.patch` (always produced) is the deliverable.

**Non-mutating dirty capture (`include_dirty`).** A bundle carries only *committed* objects,
so uncommitted work needs explicit handling — and the capture **must never mutate the user's
repo** (no `git add`/`git commit` against their real index, worktree, or refs). krayt builds a
throwaway commit from a **temporary index**, honoring `.gitignore` so ignored junk
(`node_modules`, build output, secrets) is not shipped *(verify current)*:

```
export GIT_INDEX_FILE=$TMP/idx
git read-tree HEAD             # seed the temp index from HEAD (skip if unborn HEAD)
git add -A                     # overlay tracked + new (non-ignored) changes
TREE=$(git write-tree)
DIRTY=$(git commit-tree $TREE -p HEAD -m "krayt: dirty worktree")   # parentless for a snapshot or unborn HEAD
```

The user's index, working tree, and refs stay untouched; `$DIRTY` is bundled as the imported
HEAD and simply disappears when the final diff is computed against the recorded baseline.
(`git stash create` is a simpler alternative, but its untracked-file handling is
version-dependent — verify before relying on it.) The **no-commits-yet** repo (unborn HEAD)
is handled by skipping `read-tree`/`-p` and committing the temp-index tree as a root commit.

**Guest-side ingest (order matters):**
- You **cannot** `git clone` from a pipe, so the guest first streams the bundle bytes to a
  **temp file** (`/tmp/repo.bundle`), then clones from that file.
- `git bundle verify /tmp/repo.bundle` runs **before** cloning — it catches
  truncation/corruption and surfaces any unexpected prerequisites early with a clear error.
- Configure a **krayt bot git identity** (`user.name`/`user.email`) in the guest **before**
  any commit, or commits/stash fail in a fresh container.
- `git clone /tmp/repo.bundle /workspace`, then **record the baseline immediately** —
  `git -C /workspace rev-parse HEAD`, tagged `krayt-baseline` — *before* the agent runs. The
  final diff is computed against this recorded baseline, not `HEAD~1`.
- **Snapshot a root-only `patchgit`** — copy the pristine `/workspace/.git` (as just cloned,
  before the container runs) to a `patchgit` dir *outside* the workspace, root-owned `0700`,
  never bind-mounted into the container and never made container-writable. Patch generation
  (below) runs against `patchgit`, so the workspace `.git` — which is deliberately left
  container-writable so the agent can `git commit` — is never trusted by the root guest-agent's
  git (§10 finding #2).
- Optionally drop the `origin` remote (it points at the now-deleted temp bundle file).

**Patch out (primary) + optional commit bundle.** On completion the deliverable is, as
before, a reviewable patch against the *true* recorded baseline (cleaner apply via the real
merge-base), written to `/output/changes.patch`. The diff stages everything first and
compares against the baseline (`git add -A` then `git diff --cached krayt-baseline`) rather
than `krayt-baseline..HEAD`, so an agent that edits the working tree **without committing** —
the common case — still produces a non-empty patch; a `..HEAD` diff would miss those
uncommitted edits. The host saves it to the run dir and the human applies it with `git apply`
(or `git apply --3way`) after review (§8.4). **Additionally**, because the guest now has real history, it **may** emit
a **reverse range bundle** of just the new commits —
`git bundle create /output/commits.bundle krayt-baseline..HEAD` — so multi-commit work applies
faithfully on the host via `git fetch /output/commits.bundle`. A range bundle is correct here
(unlike the forward direction) because the host already has the baseline, so the
`krayt-baseline..HEAD` prerequisites are satisfiable. The commit bundle is **optional and
additive**: `changes.patch` stays the primary human-review artifact and the review ergonomics
are unchanged.

**Patch generation is isolated from the container-writable `.git` (§10 finding #2).** Because the
agent commits inside the container, `/workspace/.git` is left writable and is therefore
attacker-controlled — a container can overwrite `.git/config`, `.git/hooks/*`, or `.gitattributes`.
Since the guest-agent runs `git` **as root**, it must **never** trust that config: a
`[core] fsmonitor = …` or a diff-driver `textconv`/external `command` would otherwise execute as
root when the guest runs `git add`/`git diff` (a container→guest-root escape). So the guest
generates the patch entirely against the root-only **`patchgit`** dir (the pristine `.git`
snapshotted at ingest), with the workspace as a detached work tree, and force-clears the dangerous
knobs on every invocation:

```
GIT_DIR=$ROOT/patchgit  GIT_WORK_TREE=/workspace \
GIT_CONFIG_NOSYSTEM=1  GIT_CONFIG_GLOBAL=/dev/null  GIT_ATTR_NOSYSTEM=1 \
git -c core.fsmonitor= -c core.hooksPath=/dev/null read-tree krayt-baseline
git -c core.fsmonitor= -c core.hooksPath=/dev/null add -A
git -c core.fsmonitor= -c core.hooksPath=/dev/null diff --cached --binary --no-textconv krayt-baseline
```

The baseline is resolved from `patchgit` (root-only), so a container that moves the workspace
`krayt-baseline` tag cannot skew the diff; the command-line `-c` knobs win over any repo-local
config; `--no-textconv` and the empty `core.hooksPath`/`core.fsmonitor` neutralize any
diff-driver/hook/fsmonitor execution; and system/global config plus system attributes are all
disabled. `commits.bundle` reads the agent's commits from the untrusted workspace `.git` — safe
because `git bundle create` runs no hooks/fsmonitor/textconv — but its baseline boundary is the SHA
resolved from `patchgit`, and it stays best-effort (a corrupt workspace `.git` never affects the
security-critical `changes.patch`).

**Integrity.** The bundle is a single artifact, hashable and checkable (`git bundle verify`
plus a digest). The host digests the exact bundle bytes it streams to the guest — via
`opencontainers/go-digest` (the `sha256:<hex>` convention already used for the OCI artifact
(§6.11) and secrets (§6.8)) — and records it as `provenance.bundle_digest` in `meta.json`
alongside the run's commit provenance (§8.4).

**Provenance.** Because the imported HEAD is usually *not* the user's real HEAD (a snapshot's
baseline is synthetic, and `include_dirty` folds in a further synthetic commit — only full
history with no dirty changes bundles the real HEAD as-is), a run records **both** commits,
distinctly, in `meta.json` (§8.4): `head_sha`, the real `git rev-parse HEAD` at bundle time
(empty for an unborn HEAD) — permanent and checkoutable — and `bundle_sha`, the commit actually
imported as `krayt-baseline` and diffed against for `changes.patch`. They coincide only in the
full-history/no-dirty case; `bundle_depth` and `include_dirty` are recorded alongside so a reader
can tell whether that equality is expected.

**Known limitations (v1):**
- **git-LFS:** a bundle carries LFS *pointer* files, not the large objects (which live on an
  LFS server). LFS-tracked content is therefore **not** transferred; fetching it would need
  network egress to the LFS endpoint, conflicting with the isolation model. Out of scope for v1.
- **Submodules:** a superproject bundle includes the gitlink but **not** submodule contents,
  so repos with submodules won't have submodule working trees in the guest. Out of scope for v1.

**Under B1 (microsandbox, `docs/adr-microsandbox-sandbox-layer.md`), `cmd/krayt-helper` performs
this entire sequence — it is now the only implementation.** There is no more guest-agent: every
"the guest"/"guest-agent" reference above (ingest, baseline tagging, the root-only `patchgit`
snapshot, force-cleared git knobs, `changes.patch`/`commits.bundle` generation) describes
`internal/patch` functions that `cmd/krayt-helper` now calls directly, run as root via
`msb exec --user root` against a workspace the non-root `agent` user has already edited
(`add-krayt-guest-helper.md`) — the mechanics are unchanged byte-for-byte, only the process
calling them changed. The helper is **stateless, exec'd, argv in and JSON on stdout, exits**: no
gRPC, no control protocol, no long-running process, no listener of any kind — growing one would
re-create the guest agent inside someone else's sandbox. It takes no secrets argument and performs
no secret scan; that scan is host-side (§6.8, §8.4) because secret values never enter the sandbox
at all under B1. It exposes exactly two subcommands, run in this order per §7:

- `krayt-helper setup --bundle <path> --workspace <path> --patch-git <path> --agent-user <name>`
  runs `Ingest`, then `SetupPatchGit` **before** relaxing the tree, then
  `MakeContainerWritable` — in that order, non-negotiably: the pristine root-only patchgit
  snapshot must be taken before the tree becomes agent-writable, or the container→sandbox-root
  isolation `fix-guest-git-config-rce.md` bought is void. Prints `{"baseline", "workspace",
  "patch_git", "agent_user"}` on stdout.
- `krayt-helper finish --workspace <path> --patch-git <path> --baseline <ref> --out <dir>` runs
  `Diff` into `<out>/changes.patch` and `BundleCommits` into `<out>/commits.bundle` when the agent
  committed, against the same root-only `patchgit` and force-cleared git knobs described above.
  Prints `{"baseline", "commits_bundle", "diff_bytes"}` on stdout.

Both print human-readable errors on stderr and exit non-zero on failure. Distribution is
`go:embed` per architecture (`runtime.GOARCH`; the sandbox OS under msb is always Linux, so there
is no second dimension to select on) — no registry, no OCI artifact, no Nix, no boot test, since
the helper is neither a kernel nor a rootfs.

### 6.8 Secrets (`internal/secrets`, `internal/sandbox`, `internal/task`)

- Read from a **per-task secrets file** (e.g. `secrets.env`) on the host — the key set is
  read pre-flight (never the values) for adapter/exactly-one/pre-flight checks (§6.14, §8.1);
  the values themselves are loaded once, host-side, when a run actually starts (`internal/orchestrator.Run`).
- **Every secret must be network-scoped.** `network.inject[]` names a secrets-file key and the
  host(s) msb may substitute its value into (`key:`/`host:`|`hosts:`, §8.1); a key with no
  matching `inject` entry is a **pre-flight error** (`task.ValidateNetworkPolicyForMsb`), not a
  silently-dropped capability. There is no channel for a secret to be *used inside* the sandbox —
  unlike the pre-msb design, where every `secrets.env` key was materialized at `/run/secrets/<KEY>`
  regardless of how it was used, msb offers no equivalent: `--secret` never puts a value in the
  guest (the guest gets msb's own placeholder instead, §6.14), `--env KEY=VALUE` puts the value on
  argv (disqualified), and `msb copy` into a tmpfs mount would require writing the value to a host
  temp file first (rejected — it trades away "never persisted" for a weaker property). This is a
  real capability loss versus the pre-msb design and is documented as such, not smoothed over: a
  value that genuinely must be readable inside the sandbox belongs in plain `env:` instead, with
  the user's eyes open.
- **Three channels a secret's parts travel, only one carries a value.** `network.inject[]` becomes
  `--secret KEY@HOST[,HOST...]` argv (`internal/sandbox.SecretArgs`) — a name and an allow list,
  never a value; `network.allow`/`passthrough` become `--net-rule`/`--tls-bypass` argv
  (`task.NetworkArgs`, §6.6); the secrets-file **value** travels only in `cmd.Env` on the spawned
  `msb create` child (`internal/sandbox.SecretEnv`) — never on disk, never on argv, never on any
  other invocation of `msb`. msb itself enforces the argv half: `--secret` accepts only
  `KEY@HOST[,HOST...]`, and rejects an inline `KEY=VALUE@HOST` on both `create` and `modify`.
- **Timing.** msb reads a secret's value "at start time", not at config-load time, so the
  environment is set only on the invocation that actually starts the sandbox — `msb create`. A
  later `msb exec` against the running sandbox needs none, because the per-sandbox host runtime
  holds the value for the sandbox's lifetime; `internal/sandbox.Client` enforces this
  structurally, not by convention — `Exec` has no parameter through which a secret could reach it
  at all, and only `Create` accepts one (`secretEnv []string`).
- **The narrowed threat requirement.** `/proc/<pid>/environ` is mode `0400` — same-uid only,
  unlike the world-readable `/proc/<pid>/cmdline` that makes argv unacceptable. The adversary the
  env channel admits is one who can already read `secrets.env` (0600), ptrace krayt, and read
  msb's own heap, where the value lives for the sandbox's lifetime regardless of how it arrived,
  un-zeroized, by msb's own documentation. Host compromise is out of scope, so the requirement is
  **never on argv, never persisted** — a deliberate, recorded decision (`docs/adr-microsandbox-sandbox-layer.md`,
  "The secret-handling contract"), not a weakening nobody noticed.
- **Where the real value ends up.** msb substitutes it at its own TLS interception boundary, on
  the host, into requests the sandbox sends toward an allowed host for that key — the sandbox
  itself only ever holds msb's placeholder (§6.14). Declaring any secret turns on TLS interception
  for the whole sandbox automatically (a beta-tool behavior krayt pins explicitly with
  `--tls-intercept` anyway, §6.6).

**The host-side secret-key scanner (`internal/orchestrator.PatchSecretKeys`).** `changes.patch` is
scanned, never redacted: rewriting hunk bytes would corrupt the diff and break `git apply`, so the
patch is collected byte-exact and scanned afterward for the secrets file's real values. A hit
raises a Safety warning naming the matched **key only** (never the value) in `report.md`/
`meta.json`, for the human to review before applying. Under B1 this runs entirely host-side and is
best-effort defense in depth rather than a real leak path: no secret value can legitimately reach
the sandbox at all (it is never delivered as anything but msb's own placeholder), so a genuine hit
here would itself indicate something else has gone wrong. It reuses `secrets.ScanKeys`, the same
substring-over-the-whole-buffer matcher an artifact redactor would use.

**Other host-side artifacts.** The run's persisted `logs/console.log` (msb's `logs --source system
--json` output, §7) and `ask_human`'s persisted `questions/<id>.json` prompt/choices
(`internal/askbridge.Bridge`'s `push` callback, §6.13) are both redacted host-side against the same
secrets-file values before being written — the host already holds every value it needs to redact
against, so this costs nothing extra. Live agent stdout/stderr (`logs/agent.log`) is **not**
redacted line-by-line the way the pre-msb guest redacted it, since the agent process never
legitimately holds a value to leak into its own output in the first place.

Agent model-provider credentials (e.g. Claude Code's `ANTHROPIC_API_KEY` or
`CLAUDE_CODE_OAUTH_TOKEN`) ride this same `network.inject` mechanism — see agent authentication
(§6.14) for how a credential maps to a host and the exactly-one rule the adapter enforces.

### 6.9 Logging & streaming (`internal/orchestrator` + guest)
- Container stdout/stderr → guest → vsock `Logs` stream → host.
- Headless default: logs persisted to `.krayt/runs/<id>/logs/`.
- `krayt attach <id>` tails the live stream; `krayt logs <id>` reads persisted logs.

### 6.10 Container runtime — containerd (`internal/guest/runner`)
Deleted — superseded by `internal/sandbox` (§6.15) under ADR option B1
(`docs/adr-microsandbox-sandbox-layer.md`, `run-tasks-on-microsandbox.md`). msb runs the user's
OCI image itself (libkrun, `agentd` as PID 1); krayt drives no container runtime of its own any
more, and the container-hardening knobs this section used to specify are replaced by msb's
`--security default|restricted` (§6.6, §8.1). See git history for the pre-msb text.

### 6.11 Image acquisition — host pull + vsock pre-load (`internal/imagestore`)
Deleted — superseded by `internal/sandbox` (§6.15) under ADR option B1
(`docs/adr-microsandbox-sandbox-layer.md`, `run-tasks-on-microsandbox.md`). `msb create --image`
resolves and pulls the user's image itself; krayt no longer maintains its own digest-keyed image
cache or vsock pre-load path. See git history for the pre-msb text.

### 6.12 vsock transport & gRPC wiring (the host/guest asymmetry)
Deleted — superseded by `internal/sandbox` (§6.15) under ADR option B1
(`docs/adr-microsandbox-sandbox-layer.md`, `run-tasks-on-microsandbox.md`). krayt drives `msb` over
argv/stdio, not a network protocol of its own; the one remaining vsock use is `ask_human` dialing
straight from the sandbox to the host over msb's own `--vsock` route (§6.13), with no gRPC and no
host/guest asymmetry to hide behind an interface. See git history for the pre-msb text.

### 6.13 Agent → human questions (`ask_human`)
An **optional, asynchronous** way for the agent to pause and ask the human a question, get
an answer, and continue — without a terminal or attach session. Off by default, so batch
stays batch; enabled per run. The design keeps the agnostic core intact and puts the
agent-specific part in the adapter.

**Three layers:**
- **Question channel (agnostic core — `internal/askbridge` on the host, `internal/askclient` in
  the sandbox):** the stable contract, rewritten by `run-tasks-on-microsandbox.md` (the cut-over,
  §14 Phase 11). There is no guest daemon and no control protocol on either side: `krayt-ask` (and
  its `--mcp` front-end) inside the sandbox dials `AF_VSOCK` **straight to the host** — no listener
  inside the sandbox, ever — over msb's own `--vsock HOST_PATH:PORT` route, which bridges guest CID
  2 on `sandbox.AskPort` (`internal/sandbox`, §6.15) to a host unix socket. `internal/askbridge`
  (`Bridge`/`Serve`/`Listen`) listens on that socket and answers each connection with one
  question/answer exchange, a newline-delimited JSON wire protocol: the sandbox writes a question,
  blocks, and the host writes back an answer (or the no-answer sentinel on timeout) before the
  sandbox continues. Independent of which agent is running. This replaces the pre-msb design's
  in-VM bridge + guest-agent + gRPC `RunEvent.Question` push entirely — see git history for that
  text.
- **Two front-ends onto the channel:**
  - **`ask_human` MCP server:** a tiny MCP server krayt runs inside the sandbox exposing one
    tool — `ask_human{ question, choices?, context? }` — bridged to the question channel via
    `internal/askclient.OverSocket`. Idiomatic for MCP-speaking agents; the tool *description*
    steers *when* to ask ("only when genuinely blocked on a decision a human must make"). This is
    the premium path.
  - **`krayt-ask` CLI:** a small binary copied into the sandbox per run (`guestbin.AskName`,
    §6.15), that any agent can shell out to (`krayt-ask [--choices a,b] "question"` → answer on
    stdout). Universal lowest-common-denominator fallback. Same channel underneath.
- **Registration (per-agent adapter):** wiring the agent's config to the MCP server is
  agent-specific (Claude Code et al. each configure MCP differently), so it lives in the
  optional adapter — **not** the agnostic core. The adapter wires the CLI **only when
  `--on-question=wait`**; MCP-server registration lands with the MCP server itself.

**Transport — a direct vsock dial, no guest listener.** `internal/askbridge` is the moved,
hardened continuation of the pre-msb in-VM bridge; `internal/askclient` is the in-sandbox client
half split out of the pre-msb in-guest `ask` package (`OverSocket`, `parseDialAddr` supporting both
a bare unix path and a `vsock://cid:port` URL, and a build-tagged `dialVsock` confined to linux,
since the vsock transport is only ever exercised inside the linux sandbox). `KRAYT_ASK_SOCKET`
keeps its name and now always carries the URL form under msb: `vsock://2:1026`
(`sandbox.AskSocketEnv`). The `--vsock` route is created only under `--on-question=wait`
(`orchestrator.Run`, §7 step 3), matching the CLI/MCP wiring's own existing "only when wait" rule —
a `fail` run gets no guest→host channel at all, so `krayt-ask` inside the container simply fails to
dial and its CLI front-end maps that straight to the no-answer sentinel; there is no separate
in-process "fail mode" branch to maintain.

Because the host process reads bytes an arbitrary sandbox process wrote directly (no gRPC, no
generated protobuf, no framing library standing between the sandbox and krayt),
`internal/askbridge.Serve` carries three bounds the pre-msb in-VM bridge never needed under a
per-VM resource limit: a byte cap on one request (`maxAskRequestBytes`, 64 KiB), a read deadline
around decoding the request only — never around `Bridge.Ask`, which legitimately blocks for the
whole `--question-timeout` — and a cap on in-flight questions, past which a new question gets the
no-answer sentinel immediately rather than a queue slot. The host socket lives in the run's own
private state directory (`runDir/ask/ask.sock`) whenever that path fits, and in a per-uid root
(`<tmp>/krayt-<uid>/<run-id>/`) when it does not — `orchestrator.runSocketDir` chooses. The msb
path is *not* exempt from macOS's `sockaddr_un` length limit, as this design first assumed: the
krayt-controlled suffix alone is 43 bytes and the repo path in front of it is unbounded, so a
scratch repo under macOS's own `$TMPDIR` overflowed 104 bytes and failed every
`--on-question=wait` run with `bind: invalid argument`. The fallback root is the pre-msb vfkit
root and is not world-writable: it gets the same `0700`/owner/no-symlink treatment as the run
directory. Both are hardened
by `internal/sockroot.Ensure` (extracted from the pre-msb vfkit/Firecracker socket-root check so
there is one check, not several) and created `0700`, owned by the invoking user; the socket itself
is `0600` inside it — narrower than the pre-msb in-guest bridge's `0777`, which existed only so a
non-root container could reach a root-owned directory on the guest side. There is no non-root party
on the host side to widen it for. Secret redaction moved host-side with the boundary: the host
already holds every secret value, so `orchestrator.Run`'s push callback (§7 step 3) redacts the
prompt/choices against the secrets file's values in its own closure before a `QuestionRecord` is
persisted, at no extra cost. `cmd/krayt-vsock-forward` and the pre-msb in-guest `ask` package are
deleted by this cutover — nothing keeps the old transport alive as a fallback.

**The host writes its answer and then waits for the sandbox to close** — it never closes first.
This is a property of the channel, not an implementation detail: msb 0.6.16's vsock relay discards
a reply that is still in flight when the host end of the bridged unix socket closes, and it does so
most of the time. `hack/msb-probes/p1-vsock-nonroot.sh` measures 21 of 75 round trips completing
with the host closing immediately after writing, against 25 of 25 with the host waiting (2026-09-02,
Apple-Silicon Mac; §14 Phase 11's P1 bullet carries the per-shape rates, and the loss is
indistinguishable from the guest — EOF with zero bytes read — so nothing downstream could detect
it). `internal/askbridge.lingerUntilPeerCloses` therefore drains the connection until the sandbox
closes it, bounded by its own deadline and byte cap so a sandbox that goes silent, or talks instead
of closing, delays only itself. `krayt-ask` closes as soon as it has decoded its answer, which is
what makes that wait cost microseconds; a future guest-side client must keep doing so.

**Modes — `--on-question`, default `fail`:**
- `fail` (default): neither front-end is wired → `ask_human` is absent and `krayt-ask`
  returns a "no human" sentinel immediately → the agent proceeds autonomously. Unattended
  runs never block. (This is why earlier phases are unaffected by the feature.)
- `wait`: front-end(s) wired. A call pauses the agent; the run enters the **`waiting`**
  state; the question is surfaced to the human; the answer flows back and the agent continues.

**Timeout — `--question-timeout` (default e.g. 10m), `--on-question-timeout` = `sentinel`
(default) | `abort`:**
- Each question has a timeout. On expiry the default returns a **"no answer" sentinel**
  (`AnswerRequest.no_answer = true`) so the agent can fall back gracefully (proceed
  conservatively or abort itself); `abort` instead fails the whole run. The run's overall
  wall-clock timeout still applies on top. The timeout also bounds how long a `waiting` VM
  parks (it holds live resources).

**Host UX:**
- The run shows `waiting` in `krayt ls`, **and a system/desktop notification fires**
  ("run `<id>` is waiting for input").
- `krayt questions <run-id>` lists the run's questions — pending and answered — with the prompt
  (sanitized, labeled as agent-originated) and choices, so the human never reads
  `questions/*.json` by hand; each pending entry prints the exact `krayt answer` line to run.
- The human answers with `krayt answer <run-id> [<qid>] <response>` (or an interactive
  one-line prompt; `choices[]` → tap/select). Multiple pending questions are answered FIFO by id.
- Every Q&A pair is persisted to `.krayt/runs/<id>/questions/<qid>.json` and summarized in
  `report.md` / `meta.json`, so the patch review shows what the agent asked and what it was told.
- **State transitions.** A `Question` event moves the run to `waiting`. The reverse edge —
  `waiting`→`running` when the answer lands — must come from a **guest "question resolved"
  `RunEvent`** emitted when `bridge.Answer` delivers, *not* from inferring resumption off the
  log stream: an agent can (and does) keep logging while blocked in `ask_human`, and an answer
  may arrive from a different process (`krayt answer` dialing the guest directly), so the host
  cannot reliably detect resumption itself. The resolved event is a Phase-5 protocol addition;
  until then a run stays `waiting` until it reaches a terminal state (never wrongly showing
  `running` mid-wait). The per-question timeout is likewise self-correcting — it probes with a
  no-answer sentinel and acts only if `Ack.Ok` shows the question was still pending, so an
  already-answered question is never wrongly sentinel-echoed or aborted.

**Concurrency & safety:**
- A `waiting` run still owns a live VM, so it counts against max-concurrency; the timeout
  prevents indefinite parking.
- Question text comes from untrusted agent code → sanitize on display (strip terminal
  escape sequences), label it clearly as agent-originated, and never auto-fill secrets into
  an answer.

### 6.14 Agent authentication
An agent in the sandbox needs a credential to reach its model provider, and krayt treats
that credential as **just another secret**: it rides the per-task secrets file (§6.8), lands
on tmpfs at `/run/secrets`, is never written to the VM disk, and is redacted from logs. The
agnostic core needs **no** change to support agent auth — it only transports the secrets
bundle. Everything agent-specific (which env var a credential maps to, and enforcing that
exactly one is set) lives in the optional **per-agent adapter** — the same place the
`ask_human` MCP registration lives (§6.13, Phase 6), **not** the core. Claude Code is the
worked example; its specifics below track the official auth docs
(`code.claude.com/docs/en/authentication`).

**Two credential shapes, one delivery path.** Claude Code accepts either:
- `ANTHROPIC_API_KEY` — a Console API key, billed pay-per-token; scoped and independently
  revocable.
- `CLAUDE_CODE_OAUTH_TOKEN` — a ~1-year OAuth token produced by running `claude setup-token`
  on a machine with a browser (the command walks the OAuth flow, prints the token, and saves
  it nowhere). It authenticates against a Pro/Max/Team/Enterprise subscription and is scoped
  to inference only.

Either way the user lists one credential in the secrets file, the adapter scopes it to
`api.anthropic.com` (§6.8, §6.14), and msb substitutes the real value at its own TLS boundary; the
container only ever sees msb's placeholder under that credential's own env var. No core code
knows it is an auth credential rather than any other secret.

**Exactly-one rule.** Claude Code resolves credentials in a fixed precedence — cloud-provider
creds (`CLAUDE_CODE_USE_BEDROCK`/`_VERTEX`/`_FOUNDRY`) → `ANTHROPIC_AUTH_TOKEN` →
`ANTHROPIC_API_KEY` → `apiKeyHelper` → `CLAUDE_CODE_OAUTH_TOKEN` → interactive `/login`
(unusable headless). So when both `ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN` are
present the API key silently wins and the subscription is bypassed (billed as API usage); in
print mode (`claude -p`) the key is always used when present, with no prompt. To avoid
silently billing the wrong account, the **Claude Code adapter MUST enforce that exactly one
auth credential is set**, failing fast (or at minimum warning) when both appear. This is
adapter logic, not core logic.

**Caveats to weigh per task:**
- **Headless billing — ANSWERED 2026-08-17 (P5, `inject-claude-oauth-token-at-proxy.md`).** A
  headless `claude -p` on a `CLAUDE_CODE_OAUTH_TOKEN` is metered against the **subscription**, not
  API credits: its `/v1/messages` responses carry `anthropic-ratelimit-unified-5h-*` /
  `-7d-*` / `-overage-status` (the unified subscription windows) and **none** of the per-key
  `anthropic-ratelimit-requests-*` / `-tokens-*` headers that the same task with a real
  `ANTHROPIC_API_KEY` received in the mirror run. Evidence: `run_b408545b` vs. `run_99bd261c`,
  recorded in `HUMAN_TODO.md`. Metering follows the credential shape that reaches Anthropic — which
  under `mitm: true` shape translation is the shape the PROXY sends, not the one the container was
  configured with. Relatedly, Bare mode (`--bare`) does not read `CLAUDE_CODE_OAUTH_TOKEN` at
  all — a bare-mode invocation must use `ANTHROPIC_API_KEY` or an `apiKeyHelper`. It still
  applies in full when `mitm` is off. **Under shape mirroring (2026-08-18) it applies to a
  translated subscription token too** — the container really is OAuth-configured, by design. krayt
  never invokes `--bare`, so nothing in krayt is affected.
- **Concurrency tension** (touches the concurrent-runs model, §4): subscription auth suits
  roughly 1–3 steady agents; for many concurrent or overnight runs prefer an API key, since
  subscription plans carry weekly rate caps *(verify current)*.
- **Blast radius.** A subscription token is tied to a personal/seat plan; though scoped to
  inference, it is less granularly revocable than a scoped API key and exposes that seat's
  consumption and rate budget to whatever runs in the VM. For krayt's untrusted-codebase use
  case, prefer a scoped, independently-revocable API key (§10).
- **Lifetime / rotation.** The subscription token lasts ~1 year; regenerate it with
  `claude setup-token` on a browser machine, or supply an `apiKeyHelper` — a script that
  prints a token, re-invoked after 5 minutes or on HTTP 401 (interval via
  `CLAUDE_CODE_API_KEY_HELPER_TTL_MS`) — for short-lived or rotating credentials.
- **Non-root.** Run the agent as a **non-root** uid; Claude Code refuses uid 0 and any
  non-root uid satisfies it. This is part of the container contract (§8.2) *(verify current)*.
- **Egress.** The auth/refresh and inference endpoints must be on the allowlist (§6.6); an
  OAuth/refresh flow may touch more endpoints than a single static API key, so it can need a
  wider allow list.

**Recommended default.** krayt supports both shapes through the one secrets mechanism, and
the choice is **per task**. Untrusted code or many concurrent agents → **API key** (safer
blast radius, fits the concurrency model, predictable billing). Trusted, low-concurrency runs
where you want to spend your own seat → `CLAUDE_CODE_OAUTH_TOKEN`. The safe default — a scoped
API key — matches krayt's headline use case (an agent working over an untrusted codebase), so
the docs and examples lead with it.

**Delivery: msb secret scoping, not a header table (§6.8, §6.15).** Every adapter (`claude-code`,
`gemini-cli`, `opencode`) enforces the exactly-one rule above, then returns exactly one
`task.SecretSpec{Key, Hosts}` naming the selected credential and the host(s) it authenticates
against — for `claude-code`, `api.anthropic.com`. `internal/orchestrator` merges the adapter's
spec under the user's own `network.inject[]` (the user's own scope for that key wins if they wrote
one, `task.MergeSecretSpecs`) and passes the result to `internal/sandbox` as `--secret
KEY@HOST[,HOST...]`. msb does the rest: it turns on TLS interception for the sandbox, sets the
guest's OWN credential env var to its own default placeholder, and substitutes the real value at
its host-side TLS boundary into any request the sandbox sends toward an allowed host — the agent
CLI emits its normal `x-api-key`/`authorization` header unprompted (or whatever else it composes)
and msb matches the placeholder string wherever it lands, with no header-name table or wire-shape
knowledge on krayt's side at all. This retires the pre-msb design's `internal/adapter/anthropic_wire.go`
(a vendor-specific table of which header a credential rides, deleted at the run-tasks-on-microsandbox.md
cut-over) and its `network.mitm`/header-strip/set/prefix vocabulary (§6.6.1) — msb's placeholder
substitution needs none of it, since it matches a string rather than replacing a named header.

**Residual: no header stripping.** The pre-msb host proxy stripped whatever auth header a
container sent before attaching the real credential; msb does not — it substitutes a placeholder
string wherever it finds one but never removes a *different* header the agent set. A credential
the agent obtained some other way and placed in a header addressed to an allowed host goes out
untouched (§10). This is bounded by the allowlist — it can only reach a host the run already
permits — but is a real, accepted regression from the pre-msb design, not an oversight.

**Verified on hardware (2026-08-18, pre-msb design, referenced for provenance).** Before this
mechanism moved to msb, the equivalent property — a real credential never entering the sandboxed
VM, the container running on a placeholder — was verified end to end for both Anthropic credential
shapes (`run_df97fffa` OAuth, `run_c654e575` API key, with `mitm: false` control `run_10fc027d`),
including that Claude Code does not validate credential format client-side. The msb-era mechanism
inherits the same shape-agnostic design but has not yet had its own hardware confirmation run — see
`HUMAN_TODO.md`.

**Recommended default (unchanged in substance).** A compromised agent has unlimited *authenticated*
access to every allowlisted host, and can spend the credential's quota/rate budget, for the run's
duration regardless of delivery mechanism. Prefer a scoped, independently-revocable API key over a
subscription token for untrusted code either way (§10).

### 6.15 microsandbox driver (`internal/sandbox`)

Under ADR option B1 (`docs/adr-microsandbox-sandbox-layer.md`), krayt drives its sandbox layer
through [microsandbox](https://github.com/superradcompany/microsandbox) (`msb`), a libkrun-based
microVM runtime, as a subprocess over argv, stdio and its `--format json` / `--json` output
(§6.6, §12) — not the Go SDK, which is a cgo `dlopen` bridge that would cost `CGO_ENABLED=0`
without buying independence from the `msb` binary (the SDK downloads it too).

`internal/sandbox` is the *only* place in krayt that knows `msb` exists — nothing above it may
construct an `msb` argv directly; `internal/orchestrator` calls it exclusively through `Client`'s
methods and the pure `CreateSpec`/`SecretArgs`/`SecretEnv` argv builders below (§7). It is
OS-agnostic (no build tags) and has no cgo.

**`Client`** wraps one resolved `msb` binary path and is stateless — every method spawns its own
process, killed with the caller's `context.Context` like every other orchestrator-driven
subprocess (§6.2). `KRAYT_MSB_BIN` overrides the resolved path — the test seam, and the escape
hatch for a non-`PATH` install. Methods: `Version` (parses `msb --version` against `MinVersion`,
currently `0.6.16` — the version the ADR was verified against), `Context` (`msb context --format
json`, the backend assertion below), `Create` (`msb create`, argv rendered by `CreateSpec.Args()`,
a pure function so the whole surface is unit-testable without spawning anything), `Exec` (`msb exec
--stream`, see below), `Copy` (`msb copy`, docker-cp syntax), `Logs` (`msb logs --json`, JSON
Lines tagged by stream), `SystemLogs` (`msb logs --source system --json`, boot/system diagnostics —
§7 step 2), `Stop`/`Remove` (`msb stop` / `msb rm --force`), and `Pull` (`msb pull`).
`CreateSpec` and friends carry no `krayt.yaml` vocabulary and no lifecycle policy — which flags a
run deserves is decided above this package (network-policy translation §6.6, secret handling §6.8,
and the run's own order of operations §7 all belong to `internal/orchestrator`, not to the driver
itself).

**The child environment is a closed allowlist, never `os.Environ()`**: an unset `cmd.Env` would
hand the `msb` child whatever the operator happened to have exported (an API key, a stray
`MSB_PROFILE=prod`) when they ran `krayt run`. The allowlist forwards `PATH`, `HOME` (msb resolves
its own runtime under `$HOME/.microsandbox`), `MSB_HOME`, `SSL_CERT_FILE`, and `SSL_CERT_DIR` only
when the operator already has them set — none is fabricated.

**`MSB_BACKEND=local` is pinned on every invocation — always set, never forwarded from this
process's own environment.** This is a security requirement, not tidiness. msb resolves its
backend as *programmatic → `MSB_BACKEND` → `MSB_PROFILE` → `active_profile` in
`~/.microsandbox/config.json` → local*, so an operator who has ever `export MSB_BACKEND=cloud`, or
who has a cloud `active_profile` saved from an unrelated session, would otherwise have `krayt run`
silently execute the task — and hand it credentials — on microsandbox's hosted service. Pinning
`MSB_BACKEND=local` defeats both the environment and the saved profile, since `MSB_BACKEND`
outranks both. The pin is asserted, not just set: `Client.Context` runs `msb context --format
json` and callers must refuse to proceed unless it reports the local backend — a pin that is never
checked is a comment.

**Streaming.** `msb exec`'s default non-interactive mode buffers the entire command's output and
writes it only after the process exits, which is unusable for krayt's live log streaming (§6.9),
so `Exec` always passes `--stream`. Its one constraint is that stdin must be a real pipe, never a
terminal; `Exec` gives it an explicit pipe deliberately (defaulting to an empty reader when the
caller supplies none) rather than leaving it to inherit. `msb exec` propagates the guest command's
own exit code via `std::process::exit`, while msb's *own* failures also exit `1` via an `anyhow`
error — so exit `1` alone cannot distinguish "the agent returned 1" from "msb could not start the
command". `Exec` resolves this structurally: a non-zero exit with **no** output observed on either
stream is reported as `ErrMsbFailed`, a distinct error the orchestrator can branch on, rather than
guessed at as the agent's own exit code.

**Secret handling (§6.8).**
`Client.Create` is the only method that accepts a `secretEnv []string` parameter, appended to the
child's environment on top of the closed allowlist above; every other method (`Exec` included)
has no such parameter at all, so the Timing rule (§6.8: a secret's value is set once, on whichever
invocation starts the sandbox) is structural, not conventional. Two pure functions, next to
`CreateSpec.Args()`, render the two channels a secret actually travels: `SecretArgs([]task.SecretSpec)
[]string` renders one `--secret NAME@HOST[,HOST...]` flag per spec, deterministically ordered by
key; `SecretEnv([]task.SecretSpec, map[string]string) ([]string, error)` returns the `KEY=VALUE`
entries for `secretEnv`, for exactly the declared keys — erroring on a declared key the secrets
file lacks, so a misconfigured run refuses pre-flight rather than reaching msb unauthenticated.

**Version floor.** `MinVersion` (`0.6.16`) is enforced by `krayt doctor` (below) — msb is beta and
has shipped a breaking wire change in a patch release, so a silent version drift below the
verified floor must be surfaced, not discovered as an outage.

**`krayt doctor` checks.** `commonChecks()` (`internal/cli/doctor.go`) runs four checks, delegated
to `internal/sandbox.DoctorChecks`: msb found on `PATH` (or via `KRAYT_MSB_BIN`), its version
against `MinVersion`, `msb context --format json` resolving to the local backend under krayt's own
pinned child env (reporting the resolved backend either way, so an operator with a cloud profile
sees krayt overriding it rather than silently benefiting from it), and an `msb doctor` passthrough
— msb ships its own host-readiness command (hypervisor availability, KVM interrupt acceleration on
Linux, a clone probe inside `MSB_HOME`), and krayt surfaces its exit status as one check rather
than reimplementing any of it. All four are **mandatory** (`[FAIL]`, not `[warn]`): msb is krayt's
only sandbox backend, so a host without a healthy install cannot run anything. There are no more
vfkit/firecracker/`/dev/kvm`/tap/NAT checks — that whole host-network-setup surface (§6.3, §6.6
pre-msb) was deleted with the providers themselves.

---

## 7. Run Lifecycle (Step by Step)

> **Rewritten by `run-tasks-on-microsandbox.md` (the cut-over, §14 Phase 11).** `internal/
> orchestrator/orchestrator.go` was rewritten from scratch around `internal/sandbox` (§6.15); the
> previous 12-step list (provision a VM, dial the guest-agent, push inputs over vsock, stream a
> gRPC event, destroy the VM) described the deleted `internal/{provider,guest,protocol}` stack.
> `Deps` is now just `{Sandbox *sandbox.Client, LogOut io.Writer, OnClient func(runID string,
> answer AnswerFunc)}` — no `Provider`/`BaseVM`/`Image`. See git history for the pre-msb list.

1. **Resolve spec** — merge flags + config file into a `RunSpec` (image, task, repo, network
   policy, secrets file, resources, env), same as before, plus resolve `spec.Network.Secrets
   []task.SecretSpec` and load the secrets file's values.
2. **Name the sandbox and register teardown immediately** — `name := "krayt-" + spec.ID`
   (`sandboxName`, §6.15 decision 8). A single deferred function is registered right here, before
   `Create` is even attempted, so it runs on **every** exit path — success, agent failure, msb
   failure, wall-clock timeout, or ctx cancellation: it (a) calls `deps.Sandbox.SystemLogs(ctx,
   name)` and persists it, redacted against the secrets file's values, to `logs/console.log` (msb's
   replacement for the pre-msb guest serial console — `msb logs --source system --json` includes a
   reconstructed `boot-error.json` block when a sandbox never finished starting), then (b) calls
   `Stop` then `Remove`, both of which wrap `context.WithoutCancel` plus a 30s timeout internally
   (`sandbox.teardownTimeout`) so the caller need not.
3. **Wire the `ask_human` channel, only if `spec.Questions.Mode == task.QuestionWait`** —
   `askbridge.Listen(runDir/ask)` binds `ask.sock`; a `Bridge` is constructed whose push callback
   redacts the prompt/choices against the secrets file's values, persists a `QuestionRecord`, flips
   the run to `waiting`, and fires a desktop notification; a per-question timeout, if set, arms
   `armQuestionTimeout`, which calls `bridge.Answer(qid, "", true)` **directly in-process** — there
   is no RPC round trip, unlike the pre-msb design, because the bridge itself is the authority.
   `bridge.OnResolved` flips the state back to `running` once the last outstanding question clears.
   A second unix socket, `control.sock` (`internal/orchestrator/runctl.go`'s `serveRunControl`), is
   bound in the same directory so a *separate* `krayt answer` process — a different terminal, or a
   detached run — can still deliver an answer; this replaces the pre-msb design's trick of dialing
   the guest's vsock control socket directly, which has no msb equivalent (the sandbox never
   listens for anything; it only dials out). `RunRecord.CtrlSocket` now names this host-side
   control socket, not a guest one, and `RunRecord` gains `SandboxName`. Only in this mode does
   `CreateSpec.Vsock` get one entry (`{HostPath: runDir/ask/ask.sock, Port: sandbox.AskPort}`), and
   `KRAYT_ASK_SOCKET` is merged into `spec.Env` as `sandbox.AskSocketEnv`
   (`"vsock://2:1026"`) — in `fail` mode **no** `--vsock` route is created at all, so the sandbox's
   own `krayt-ask` simply fails to dial and its CLI front-end maps that to the no-answer sentinel;
   there is no separate host-side "fail mode" branch to maintain.
4. **`msb create`** — `sandbox.CreateSpec{Image: spec.ImageRef, Name: name, User: "agent",
   CPUs/MemoryMiB/DiskGiB: from spec.Resources, MaxDuration: spec.Resources.Timeout, Env:
   spec.Env (sorted), Vsock: (see step 3), Secrets: one `SecretRef` per `spec.Network.Secrets`
   entry, Security: "restricted" (fixed, not configurable, §6.6), ExtraArgs:
   task.NetworkArgs(spec.Network, hasSecrets)}`. `secretEnv` (the `KEY=VALUE` pairs, via
   `sandbox.SecretEnv`) is passed **only** to this one call — msb's Timing rule (§6.6.1): a
   secret's value is set once, at sandbox creation. Belt-and-braces alongside `--max-duration`:
   the whole `Run` call is also wrapped in `context.WithTimeout(ctx, spec.Resources.Timeout)` —
   the ctx is what makes teardown deterministic, `--max-duration` is what stops a wedged guest
   outliving it.
5. **Copy in** — the git bundle (`patch.CreateBundle`, unchanged, §6.7), `/task/prompt.md`, and
   the two embedded guest binaries (`guestbin.Binary(name, runtime.GOARCH)` — msb's guest arch
   always equals the host's, since libkrun runs same-arch). `krayt-helper` is copied to
   `guestbin.GuestPath("krayt-helper")` (`/.krayt/krayt-helper`); `krayt-ask` is copied straight
   to `/usr/local/bin/krayt-ask` (the fixed path §8.2's container contract already promises, so no
   agent image needed rebuilding). A defensive `chmod +x` runs as root afterward, since msb's
   mode-preservation on copy is not a pinned contract.
6. **`msb exec --user root`** — `krayt-helper setup --bundle /tmp/repo.bundle --workspace
   /workspace --patch-git /.krayt/patchgit --agent-user agent`, JSON stdout parsed for
   `baseline`. Only now — once the code snapshot is durably cloned into the sandbox — does
   `rec.State` flip to `running`, preserving the pre-msb invariant that `running` means "safe to
   mutate the host repo now" (§6.2).
7. **`msb exec --user agent --stream`** — runs the one fixed command every agent image exposes,
   `/usr/local/bin/krayt-agent-entrypoint` (uniform across every published agent image; no
   per-adapter command table needed), stdout+stderr both wired to `io.MultiWriter(logFile,
   LogOut-if-not-detached)`. A non-zero exit with **zero bytes observed on either stream** is
   `sandbox.ErrMsbFailed` and surfaces as a **failed run** naming the driver failure explicitly —
   never reported as "the agent exited N" (`internal/sandbox.Client.Exec`'s own structural
   heuristic, consumed via `errors.Is`). A wall-clock timeout (ctx deadline exceeded) produces the
   same `{TimedOut: true, ExitCode: -1}` result shape whether it fires here or during any earlier
   step — `isWallClockTimeout` is just a `ctx.Err()`/deadline comparison now, with no gRPC-specific
   checks left. On a timeout, helper `finish` and artifact collection are skipped (ctx is already
   dead), same as the pre-msb behavior.
8. **`msb exec --user root`, again** — `krayt-helper finish --workspace /workspace --patch-git
   /.krayt/patchgit --baseline <from step 6> --out /output`.
9. **Copy out** — `msb copy name:/output <local-tmp-under-runDir>`, tolerant of either a nested
   (`tmp/output/...`) or flat (`tmp/...`) copy shape, since msb's exact docker-cp semantics for a
   directory source aren't pinned by anything verifiable offline; moved into the run dir via
   `os.Rename` (staged under `runDir` itself so it's guaranteed same-filesystem).
10. **Host-side, no exec involved** (unchanged code, just re-wired inputs) — `patch.Stat`/
    `patch.Lint` on the collected `changes.patch`; `orchestrator.PatchSecretKeys(patchPath,
    secretValues)` (built unwired by a prior task, now actually called) scans the collected patch
    for the secrets file's real values and turns any hit into a Safety warning naming the KEY only,
    never the value — the **only** secret-scanning path now, since under msb no secret value ever
    legitimately enters the sandbox at all, so this is defense-in-depth, not a real leak path any
    more. `writeReport`/`meta.json` are otherwise byte-for-byte the pre-existing code (§8.4's
    schema is unchanged except `NetworkMeta.InjectedKeys`/`.MITM`, see §8.4).
11. **Teardown** fires via the deferred function from step 2 no matter which of the above steps
    errored, timed out, or the ctx was cancelled.
12. **Review & apply** — human inspects the patch; `git apply` if satisfied. Unchanged.

---

## 8. Configuration

### 8.1 Task config file (`krayt.yaml` — optional)
```yaml
image: my-agent:latest          # required (flag or file)
task: ./task.md                 # path to task prompt (or inline `task_text:`)
repo: .                         # repo to bundle (default: cwd)
include_dirty: true             # include uncommitted changes (non-mutating capture, §6.7)
bundle_depth: 1                 # 1 = single-commit snapshot; 0 = full history (§6.7)
transcript: false               # copy the agent's own session transcript out before teardown (§8.4)

network:
  mode: allowlist               # allowlist | full | none
  allow:
    - api.anthropic.com
    - generativelanguage.googleapis.com
    - registry.npmjs.org
  mitm: true                      # opt-in TLS termination + header injection at the host proxy;
                                   # default false — a run that doesn't set this is byte-identical
                                   # to one without the feature at all (§6.6.1)
  passthrough: [github.com]       # tunnel these, never MITM (subset of allow in mode: allowlist)
  inject:
    - host: api.anthropic.com     # exact host match, same matcher as `allow`
      strip: [x-api-key, authorization]   # remove these from the guest's request first
      set:                                # then set these
        x-api-key: ANTHROPIC_API_KEY      # header name -> secrets-file key, resolved host-side
      # set_literal:                      # header name -> fixed, non-secret value (optional)
      #   x-krayt-mitm: "1"

secrets: ./secrets.env          # per-task secrets file (tmpfs in container)

env:                            # non-secret env passed to the container
  LOG_LEVEL: info

resources:
  cpus: 2
  memory: 4GiB
  disk: 20GiB
  timeout: 30m

questions:                      # agent → human questions (§6.13)
  mode: fail                    # fail (default, autonomous) | wait (pause for input)
  timeout: 10m                  # per-question wait limit
  on_timeout: sentinel          # sentinel (default; agent decides) | abort (fail the run)

# optional orchestration adapter (otherwise the image entrypoint runs).
# The adapter also wires the ask_human MCP server / krayt-ask CLI when mode: wait (§6.13).
agent:
  adapter: none                 # none | claude-code | gemini-cli | opencode

# optional container hardening overrides (§6.10, §10). The defaults are the secure ones —
# all capabilities dropped, containerd's seccomp profile applied, writable rootfs — so an
# absent `container:` block already runs least-privilege. Config-file only (no CLI flags in v1).
container:
  capabilities:                 # opt-in caps re-granted on top of drop-all; CAP_ prefix optional
    - net_bind_service          # e.g. bind :80/:443 as non-root
  seccomp: default              # default (containerd profile) | unconfined (drop the filter)
  readonly_rootfs: false        # opt-in read-only rootfs (default false; see §8.2 caveat)
```

**Two tracked files, two purposes.** `configs/krayt.yaml` is the generic, fully-annotated
template above — copy it as a starting point for any task. The repo-root `krayt.yaml` is a
second, deliberately tracked file: krayt's own shared dev config for dogfooding krayt on this
repo (pins the `krayt-dev` image, Claude model/effort, this repo's network allowlist, and
host-side injection of both credentials its tasks use), so every contributor gets the same
starting point. It carries no secret material — `secrets:` only names a path, the file it points
at is itself gitignored, and the one credential-shaped value in its `env:` block is the non-secret
`GH_TOKEN` placeholder the proxy replaces — so tracking it is safe; a real credential must never
be inlined there.

**That file is not auto-loadable, by its own construction.** It sets `network.mitm`,
`network.passthrough`, and `network.inject`, which the §8.3 table refuses from an auto-loaded
`<repo>/krayt.yaml`; the repo's own config is held to that boundary exactly like any other repo's
would be. Runs pass it explicitly, from the repo root:

```sh
krayt run --config krayt.yaml --task <file>
```

A bare `krayt run --task <file>` stops with an error naming `network.mitm` — the opt-in, working
as designed. `TestApplyConfigDogfoodsThisRepo` (`internal/cli`) pins both halves: refused when
auto-discovered, accepted whole under `--config`.

Only `api.github.com` carries a hand-written `inject` rule there (`authorization: Bearer ` +
`GH_TOKEN`, `gh`'s documented env-var auth path supplying the placeholder the proxy replaces).
`api.anthropic.com` deliberately carries none: `agent.adapter: claude-code` plus `mitm: true`
makes the adapter emit one from the observed wire shape of whichever credential the secrets file
holds (§6.14), and a hand-written rule would win over it (§8.1's merge precedence) and pin that
shape away from the provenance notes that justify it. Every other allowlisted host is in
`passthrough`, so the go/nix/git toolchains keep verifying real upstream chains.

**`container.capabilities` denylist.** These are **never** grantable, even if named, and the
config is rejected at load if one appears: `CAP_SETUID`, `CAP_SETGID`, `CAP_SETPCAP`,
`CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_DAC_READ_SEARCH`, `CAP_BPF`,
`CAP_SYS_PTRACE`. The setuid/net classes would re-open the egress-allowlist bypass (§6.6, §10);
the rest are broad container-escape primitives. A task that needs open networking uses
`network.mode: full` — a deliberate, separately-reviewed opt-in — not a capability grant.

**`readonly_rootfs` caveat.** It is opt-in (default OFF) for two reasons: (1) **compatibility** —
the reference agent images run as `USER agent` and write into `$HOME` (nix profile, `~/.claude`,
Go caches); a read-only rootfs breaks them, and a tmpfs over `$HOME` would hide the image's
pre-installed tooling; (2) **marginal benefit** — krayt's isolation is the ephemeral VM +
single-use container (one run per VM, CoW disk destroyed on teardown, no host fs shared, the
trusted guest-agent runs *outside* the container), so read-only rootfs mainly buys
persistence/tamper resistance that has almost no blast radius here. When enabled it is paired
with writable ephemeral tmpfs for `/tmp` and `/run` only.

**`network.mitm`/`passthrough`/`inject` validation (§6.6.1).** All fail-fast at `krayt run`
pre-flight, before any VM or image work: `inject` requires `mitm: true`; every `inject[].host`
must not be in `passthrough`, and (in `mode: allowlist`) must also be in `allow` — in `mode:
full` there is no list to check a host against, so any host is accepted; every secrets-file key
named in `inject[].set` must exist (a typo must not silently produce an unauthenticated run that
fails opaquely 30s into the agent); `passthrough ⊆ allow` in `mode: allowlist`, free-form in
`mode: full`; header names must be valid HTTP tokens and not hop-by-hop. `full` + `mitm`
intercepts **every** TLS connection the agent makes except those listed in `passthrough` — that
is the point of `full`, stated plainly here so it isn't a surprise.

**Under msb — the target model, additive and not yet wired (§6.6, `translate-network-policy-to-msb.md`,
`hand-secrets-to-msb.md`).** `task.ValidateNetworkPolicyForMsb` exists beside `ValidateNetworkPolicy`
above but is not yet called from anywhere — the vfkit/Firecracker path this section describes is
still the one that executes, and it still requires `mitm: true` to inject anything, including for
this repo's own `krayt.yaml`. When `run-tasks-on-microsandbox.md` swaps the call site,
`network.mitm` becomes a hard pre-flight error naming itself and its replacement: under msb,
declaring any secret enables TLS interception automatically, so the key has nothing left to opt
into.

`network.inject` keeps its name — §8.3's containment table already refuses it from an
auto-loaded repo-local config by key name, and that protection carries over without re-deriving it
— but loses most of its schema. `host`/`strip`/`set`/`set_prefix`/`set_literal`/`refresh` are the
pre-msb, header-shaped vocabulary above; under msb an entry instead names a secrets-file **key**
and the **hosts** it may be substituted to, nothing else, because msb substitutes a placeholder
*string* wherever it appears rather than replacing a named header:

```yaml
network:
  inject:
    - key: GH_TOKEN              # secrets-file key name; never a value
      hosts: [api.github.com]    # hosts allowed to receive it
    - key: ANTHROPIC_API_KEY
      host: api.anthropic.com    # singular form accepted; `host` xor `hosts`
```

`task.ConfigInjectRule` carries both shapes' fields at once (a plain, unvalidated parse); which
shape a given entry uses is decided by which fields it sets. `task.SecretSpecsFromConfig` converts
the msb shape into `task.SecretSpec{Key, Hosts}` and hard-errors, naming itself, on `strip`, `set`,
`set_prefix`, `set_literal` or `refresh` appearing on an entry — silently ignoring one of these,
`strip` above all, would weaken the posture (an un-stripped pre-existing auth header reaching an
allowed host untouched, §10) without telling anyone. `task.ValidateNetworkPolicyForMsb` calls it,
then additionally enforces: every `network.inject` entry's key must exist in the task's secrets
file (the same typo protection the pre-msb shape already has) and, the new rule msb's model
requires (§6.8), every key that *does* exist in the secrets file must have a `network.inject`
entry — a secret with nowhere to be scoped can never be delivered under msb at all, so leaving one
unscoped is refused pre-flight rather than silently dropped.

`internal/sandbox.SecretArgs`/`SecretEnv` (§6.15) render the resulting specs into the two channels
a secret actually travels — argv (names and hosts only) and the msb child's env (the one channel
that carries a value, §6.8) — and `adapter.Plan.Secrets` (§6.14) is how an adapter contributes its
own selected credential to the same list.

**Adapter-supplied secret scoping and merge precedence.** An adapter's `Prepare` returns exactly
one `task.SecretSpec` for its selected credential (§6.14) alongside the `Env` additions it already
contributes. Before any sandbox work:
- The adapter's spec is merged into `network.inject`'s specs from the config file/flags —
  `task.MergeSecretSpecs` — by key: a credential the user never scoped gets the adapter's `Hosts`
  verbatim; a credential the user already scoped keeps the **user's own hosts entirely**, and the
  run logs that the adapter's own scope was overridden — a user who scoped a credential themselves
  is never silently second-guessed.
- The **merged** set is re-run through the exact same `ValidateNetworkPolicyForMsb` pre-flight
  described above. An adapter-scoped credential naming a host outside `network.allow` (in `mode:
  allowlist`) fails the run before any sandbox work, exactly as a typo'd hand-written scope
  would — an adapter's suggestion is never exempt from the check a human's config is held to.
- The merged set is what actually becomes `--secret KEY@HOST[,HOST...]` argv on `msb create`
  (§6.15) — adapter-supplied scopes travel the identical path hand-written ones do; there is no
  second, adapter-only channel into msb.

### 8.2 Container contract (convention)
Injected by the tool, regardless of adapter:
- `/workspace` — the repo snapshot (agent's working dir).
- `/task/prompt.md` — the task description.
- `/output/` — the agent (or `cmd/krayt-helper`, run as root via `msb exec`, for
  `changes.patch`/`commits.bundle`) writes here; the whole directory is collected (§6.7, §7).
- `/usr/local/bin/krayt-ask` — the `krayt-ask` CLI front-end (§6.13), copied in per run
  (`msb copy`) rather than baked into the image, so it needs no image rebuild; any agent can shell
  out to it. `KRAYT_ASK_SOCKET` names the bridge it connects to — always a `vsock://cid:port` URL
  under msb (`dial-ask-channel-over-vsock.md`), where `krayt-ask` dials the host directly and no
  guest process listens at all.
- **The model-provider credential env var** (e.g. `ANTHROPIC_API_KEY`), if the adapter selected
  one — but the entrypoint never finds it in a file. See below.

**There is no `/run/secrets` under msb.** Every credential krayt hands to a run is delivered by
msb's own placeholder-substitution mechanism (§6.8, §6.14): msb sets the sandbox's own credential
env var to its default placeholder itself, the moment `--secret` names it, and substitutes the
real value host-side into requests toward an allowed host. A compliant entrypoint must therefore
treat **an already-set, recognized credential env var** as sufficient — it must never require a
`/run/secrets/<key>` file, and must never overwrite a credential variable that arrives already
set. `hack/test-entrypoint-credentials.sh` guards this contract; every reference image satisfies
it already, since none needed rebuilding for the msb cutover (msb sets the guest environment
itself, and `krayt-ask` was already copied in rather than baked in).

The container **must** run as a **non-root** uid — this is **enforced, not just a convention**
(§10): an image whose `USER` is root (uid 0) or unset **fails the run** with a clear error and
never launches (`msb create --user`/`msb exec --user`, always a fixed non-root user, §7). Some
agents (Claude Code among them) also refuse uid 0 independently.

**The agent's own state directory is read, not injected.** `--transcript` copies the agent's
session transcript out of `$HOME` (the path is the adapter's, relative to whatever `$HOME` the
container user actually has — `/home/agent` for the claude-code images, `/home/node` for
gemini-cli, resolved in-guest rather than assumed). That is a *read* of image-owned state, outside
this contract's injected paths and outside `/output`; an image owes krayt nothing here, and an
image whose agent writes no transcript simply yields none.

An image that writes into its own rootfs — e.g. `$HOME` under `/home/agent` (nix profile,
`~/.claude`, Go caches) — needs a writable rootfs; msb's `--security` profile has no read-only-rootfs
knob to opt into anyway (`container.readonly_rootfs` is a hard pre-flight error under msb, §8.1).

Completion = the agent's exec exit code (§7). Exit code is surfaced in `meta.json`.

### 8.3 Flag/file precedence
CLI flags override config file values, which override built-in defaults.

**Auto-loaded vs. explicit configs.** A `krayt.yaml` reaches a run one of two ways, and they are
*not* equally trusted. An **explicit** `--config <path>` is the operator naming the file: it is
honored in full. An **auto-loaded** `<repo>/krayt.yaml` (no `--config` passed) is a file that ships
inside the repo the agent is about to work on — it is untrusted input (§10), so it may configure a
run but may not write the run's security policy:

This table is the **whole** boundary — every field of the config is in exactly one row, so a field's
treatment is stated in one place rather than discovered one field at a time. A field added to
`krayt.yaml` must be added here too.

| Field | Auto-loaded `<repo>/krayt.yaml` | Explicit `--config` | Why |
|---|---|---|---|
| `network.mitm: true` | **error** | honored | turns on TLS interception |
| `network.inject` (non-empty) | **error** | honored | names which credential is injected into which host's requests |
| `network.passthrough` (non-empty) | **error** | honored | exempts a host from interception |
| `network.mode: full` | **error** | honored | drops the egress allowlist entirely |
| `repo:` | **error** | honored | redirects **which host directory is bundled into the VM** — and is also the run-artifact root `.krayt/` is written under |
| `container.capabilities` (non-empty) | **error** | honored | re-grants Linux capabilities the run drops by default (§8.1) |
| `container.seccomp: unconfined` | **error** | honored | disables the seccomp profile (§8.1) |
| `secrets:` | contained: honored only if the resolved path stays inside the repo root | honored | host file read; its values are loaded host-side and, per key, substituted by msb into the sandbox's requests (§6.8) |
| `task:` | contained: honored only if the resolved path stays inside the repo root | honored | host file read, shipped into the guest as the run's prompt |
| everything else (`image`, `network.mode: allowlist\|none`, `network.allow`, `agent`, `env`, `resources`, `questions`, `include_dirty`, `bundle_depth`, `transcript`, `container.readonly_rootfs`) | honored | honored | configures the run without redirecting what krayt reads/writes on the host or relaxing the container's confinement |

A refused field is an **error, not a warning and not a silent ignore** — the run stops, naming the
field, the file, and the `krayt run --config <path>` opt-in. Silently dropping it would leave the
operator believing a policy that is not in force.

`repo:` is refused rather than contained: a repo's own config redirecting *which repo to bundle* is
self-referential nonsense with no legitimate use, so refusing is both simpler and stricter than a
containment check. Untreated, `repo: ../sibling` makes krayt bundle a **different, private repo's
git history** into the VM for an attacker-influenced agent to read, and writes that run's `.krayt/`
artifacts into whatever directory the poisoned file named, at the operator's uid.
`container.readonly_rootfs` is *not* refused — it only tightens, so a repo asking for it is harmless.

`secrets:` and `task:` are contained rather than refused, because a repo's own tracked config
legitimately names its gitignored secrets file and its checked-in task prompt. The value is resolved
against the repo root and `filepath.Clean`ed; an absolute path (`/Users/x/.env`, `/etc/hostname`) or
one that climbs out (`../../.env`) is rejected — otherwise a poisoned repo could ship an arbitrary
host file's contents into a run — as declared secrets or as its prompt (which the agent can then
echo into `report.md` or `changes.patch`). Containment is judged on the path that will actually be
**opened**, not on how it is spelled: every symlink in the value and in the repo root is resolved
first, so a repo shipping `secrets.env -> ~/.aws/credentials` is rejected too. A path that does not
exist is not an escape — there is nothing to follow, and the missing file is reported when the file
is read.

**Pre-boot policy summary.** Every run prints its resolved egress policy to stderr before the VM
boots — mode, allowlist, MITM on/off, passthrough list, and each inject rule's host and header
**names** (never a value, never a secrets-file key's contents). It is printed after adapter merging
and `ValidateNetworkPolicy`, so it is the final policy, and before any VM or image work, so it is
the operator's last chance to notice a host they did not choose. `meta.json`/`report.md` (§8.4)
only record the policy after the fact.

### 8.4 Run output artifacts (`.krayt/runs/<id>/`)
> **Amended by `run-tasks-on-microsandbox.md` (the cut-over, §14 Phase 11).** The artifact tree
> and `meta.json` schema below are otherwise byte-for-byte the pre-existing design — see the two
> call-outs after the tree and the `NetworkMeta` note below for exactly what changed.

Every run produces a self-contained directory the human reviews from:

```
.krayt/runs/<id>/
├── changes.patch     # git diff vs the recorded krayt-baseline (primary deliverable; §6.7)
├── commits.bundle    # optional: reverse range bundle of the agent's commits (§6.7), if returned
├── report.md         # human-readable summary (see below)
├── meta.json         # machine-readable run record (schema below)
├── ask/              # ask_human bridge state under --on-question=wait: ask.sock, control.sock (§6.13)
├── questions/        # one <qid>.json per agent question + its answer (§6.13), if any
└── logs/
    ├── agent.log     # sandbox stdout/stderr (merged, timestamped) — from `msb exec --stream`
    ├── console.log   # msb's boot/system diagnostics (`msb logs --source system --json`,
    │                  #   redacted), replacing the pre-msb guest serial console (§7 step 2)
    └── transcript/   # opt-in (`--transcript`): the agent's own session transcript, copied out
                       #   of the guest before teardown — redacted and size-capped. Absent by
                       #   default and whenever the adapter declares no path.
```

**`logs/transcript/` is opt-in and is the only artifact krayt reads out of the agent's own `$HOME`
rather than from `/output`.** It exists because `agent.log` is the agent's *stdout*, which for a
headless run is just the final message: a failed run leaves no record of which tool calls it made.
The transcript does record them. It is captured in `Run`'s teardown defer rather than alongside the
other artifacts, deliberately — `collectOutput` is skipped on a wall-clock timeout, an aborted
question and any msb driver failure, which are exactly the runs worth debugging.

Treat it as an **opaque diagnostic**: every agent CLI documents its transcript schema as internal
and changing between releases, so krayt copies, redacts and caps it but never parses it, and
neither should tooling. Its size cap keeps a head *and* a tail (`elideMiddle`) rather than
`console.log`'s tail-only truncation, because in a transcript the first appearance of a problem
matters as much as the failure it ends in.

Two artifacts from the pre-cutover design are gone, not renamed: **`proxy.log`** (there is no
host-side egress-proxy child any more — msb's own egress enforcement produces no comparable
per-run log krayt captures) and **`secret-scan.json`** — a patch-secret-value hit is now folded
directly into `meta.json`'s/`report.md`'s existing `Safety` list (`orchestrator.PatchSecretKeys`,
§6.6.1, §6.8), rather than written as its own file. **`events.jsonl`** is also gone: there is no
`RunEvent` stream any more (§6.5 stub) to serialize one JSON object per event from.

`meta.json` — written by the host on completion; the schema is fixed so tooling and the
`ls` command can rely on it:

```json
{
  "id": "run_2f9c1a",
  "image_ref": "my-agent@sha256:…",
  "repo_path": "/Users/me/proj",
  "task_summary": "first 200 chars of the task prompt",
  "network": { "mode": "allowlist", "allow": ["api.anthropic.com"] },
  "resources": { "cpus": 2, "memory_mib": 4096, "disk_gib": 20, "timeout_secs": 1800 },
  "questions_mode": "fail",
  "started_at": "2026-06-06T10:00:00Z",
  "ended_at":   "2026-06-06T10:07:42Z",
  "duration_secs": 462,
  "exit_code": 0,
  "timed_out": false,
  "patch": { "path": "changes.patch", "files_changed": 7, "insertions": 124, "deletions": 18 },
  "provenance": {
    "head_sha": "a1b2c3d4e5f6…",
    "bundle_sha": "9f8e7d6c5b4a…",
    "bundle_depth": 1,
    "include_dirty": false,
    "bundle_digest": "sha256:…"
  },
  "questions": [
    { "id": "q1", "prompt": "Target Postgres or SQLite?", "answer": "postgres", "answered_by": "human", "waited_secs": 35 }
  ],
  "sandbox_name": "krayt-run_2f9c1a",
  "ctrl_socket": "/path/to/.krayt/runs/run_2f9c1a/ask/control.sock",
  "error": ""
}
```

`provenance` records what source the run was based on (§6.7): `head_sha` is the real, checkoutable
`git rev-parse HEAD` at bundle time (empty for an unborn HEAD); `bundle_sha` is the commit actually
imported as `krayt-baseline` and diffed against for `changes.patch` — equal to `head_sha` only in
the full-history/no-dirty case, synthetic otherwise. `bundle_depth`/`include_dirty` are the request
flags that determine whether that equality is expected, and `bundle_digest` is a
`opencontainers/go-digest` hash of the exact bundle bytes streamed to the guest.

**`network.mitm`/`network.injected_keys` are reinterpreted, not renamed, under msb.** The JSON
field names (`NetworkMeta.MITM`/`.InjectedKeys`) are unchanged from the pre-cutover schema, but
what they mean shifted with the design: `mitm` now reports "any secret was declared for this run"
(msb turns on TLS interception automatically the instant a `--secret` is present, §6.6 — there is
no longer a separate opt-in to report on) and `injected_keys` now lists **every** declared secret's
key (§6.6.1) rather than only those an explicit `network.inject[].set` rule named, since under msb
every declared secret is substituted the same way. `sandbox_name` is the `msb` sandbox this run
created (`"krayt-<id>"`, §6.15 decision 8), useful for a manual `msb logs`/`msb exec` against a
stuck run. `ctrl_socket` now names the **host-side** run-control socket
(`internal/orchestrator/runctl.go`'s `serveRunControl`, §6.13, §7 step 3) that a separate `krayt
answer` invocation dials — not a guest-reachable socket, since under msb the sandbox never listens
for anything; it only dials out.

`report.md` — a short, fixed-section human summary (the guest may also write its own to
`/output/report.md`; if present, the host prefers that and appends the run facts):

```
# Run <id>
- Image: <image_ref>   Task: <task_summary>
- Result: <success|failed|timed out>   Exit: <code>   Duration: <hms>
- Network: <mode> (<allow…>)

## Changes
<files_changed> files, +<insertions>/-<deletions>. See changes.patch.

## Provenance
- Commit: <head_sha>  (bundle: <bundle_sha>, depth: <bundle_depth>, dirty: <yes|no>)
- Bundle digest: <bundle_digest>
- Metadata digest (consistency check, not a signature): <sha256 of meta.json>

## Notes
<agent-provided notes from /output/report.md, if any>
```

The `## Provenance` section appears only when the code bundle was built and streamed (§6.7). The
**metadata digest** is a `sha256` of the `meta.json` bytes written for this run — a **drift/
consistency check, not tamper-evidence**: `meta.json` and `report.md` are written back-to-back by
the same trusted host process, so it cannot detect a deliberate, consistent edit of both. What it
*does* do is let someone holding `report.md` apart from `meta.json` (e.g. pasted into a ticket)
confirm the two still match, or notice `meta.json` was later corrupted/overwritten.

Secret **values** never appear in `report.md`, `meta.json`, or the question records — the host
redacts the report and question text (§6.8, §6.13) against the secrets file's values. The one
exception is `changes.patch` itself: it is left byte-exact so `git apply` works, so a secret an
agent wrote into a tracked file *is* present there. When that happens the host's
`orchestrator.PatchSecretKeys` (§6.6.1, §6.8 — the only secret-scanning path now that no secret
value ever legitimately enters the sandbox) appends a **Safety** warning per matched key directly
to `rec.Safety`, naming the key only (never the value), e.g. *"changes.patch contains the value of
secret ANTHROPIC_API_KEY — review before applying"* — rendered into both `report.md`'s Safety
section and `meta.json`'s `safety` array, so the human catches it before applying. There is no
separate `secret-scan.json` file (§8.4's artifact-tree note above). `krayt
ls` reads `meta.json`; `krayt patch`/`apply` read `changes.patch`.

---

## 9. Project Structure

> **Amended by `run-tasks-on-microsandbox.md` (the cut-over, §14 Phase 11).** `internal/
> {provider,guest,protocol,proxy,controlclient,imagestore}` and `cmd/krayt-{agent,vsock-forward}`
> are deleted; `internal/sandbox` (+ `internal/sandbox/guestbin`), `internal/askbridge`,
> `internal/askclient`, `internal/sockroot`, and `cmd/krayt-helper` are the packages that replace
> them (§6.15, §6.13, §6.7). `internal/vmimage` and `images/` are unchanged by this task (kept for
> `retire-vm-image-pipeline.md`, §14) — listed here as-is.

```
krayt/
├── cmd/
│   ├── krayt/main.go
│   ├── krayt-ask/           # in-sandbox CLI front-end for ask_human, dials AF_VSOCK directly (§6.13)
│   └── krayt-helper/        # //go:build linux — stateless root-run guest binary: setup/finish (§6.7)
├── internal/
│   ├── cli/                 # cobra commands, flag/config merge (OS-agnostic; the run_{darwin,linux,other}.go
│   │                         #   split collapsed into one run.go — msb is the only backend now)
│   ├── orchestrator/        # run lifecycle, concurrency, teardown, state (§7); drives internal/sandbox only
│   ├── sandbox/             # the msb CLI driver: Client, CreateSpec.Args(), Secret{Args,Env}, DoctorChecks (§6.15)
│   │   └── guestbin/        # go:embed of the krayt-helper/krayt-ask static linux binaries; GuestRoot = "/.krayt"
│   ├── askbridge/           # host-side ask_human bridge: Bridge, Listen, Serve (§6.13)
│   ├── askclient/           # in-sandbox client half of the ask_human channel: OverSocket, vsock dialer (§6.13)
│   ├── sockroot/            # hardens a directory before binding a socket in it; shared by askbridge
│   ├── adapter/             # optional per-agent adapters (claude-code, gemini-cli, opencode); MCP/CLI wiring (§6.13, §6.14)
│   ├── task/                # config schema + parsing, incl. netpolicy_msb.go / secrets_msb.go / container_msb.go
│   ├── patch/               # git bundle create/verify/clone/diff (+ optional reverse bundle); non-mutating dirty capture; host-side apply helpers (§6.7)
│   ├── vmimage/             # unchanged by this task — kept for retire-vm-image-pipeline.md (§14)
│   └── secrets/             # secrets loading + redaction
├── images/                  # unchanged by this task — Nix-based VM image build, kept for retire-vm-image-pipeline.md (§14)
├── configs/                 # example krayt.yaml, default allowlist
├── flake.nix                # dev shell (oras pinned — still used by internal/vmimage; no protobuf/protoc-gen-go/buf pins any more)
├── Makefile                 # build, test, `make guest-bins` targets (no `make proto`)
├── docs/
└── README.md
```

### 9.1 Pinned dependencies
Use these exact modules so the agent doesn't guess. (Pin concrete versions in `go.mod`
at implementation time; major versions shown where they matter.)

> **Amended by `run-tasks-on-microsandbox.md`.** The macOS/Linux VM-backend rows, the gRPC/
> protobuf/proto-codegen rows, the guest vsock listener, the containerd client, and the
> hand-rolled/`goproxy` egress-proxy row are all **dropped** — nothing in krayt links any of them
> once `internal/{provider,guest,protocol,proxy}` are deleted. `oras.land/oras-go/v2` stays: it is
> still used by `internal/vmimage`, unchanged by this task. See git history for the dropped rows'
> original text.

| Concern | Module | Notes |
|---|---|---|
| Sandbox runtime | *none — `msb` is driven as a subprocess, not a Go dependency* | `internal/sandbox` shells out to the `msb` binary and parses its `--format json`/`--stream` output; no SDK, no cgo (§6.15) |
| OCI registry / image pull+export | `oras.land/oras-go/v2` | host-side `internal/vmimage` only (§11) — unrelated to `krayt run`'s sandbox path since this cutover |
| OCI types/layout | `github.com/opencontainers/image-spec` | media types, `oci-layout`, `internal/vmimage` |
| CLI | `github.com/spf13/cobra` (+ `spf13/pflag`) | command surface (§13) |
| Config | `gopkg.in/yaml.v3` | task config file (§8.1) |
| `ask_human` MCP server | `github.com/modelcontextprotocol/go-sdk` (v1.2.0, `/mcp`) | stdio MCP server for `krayt-ask --mcp` (§6.13); pulled only by `cmd/krayt-ask` |

Build constraints: `cmd/krayt-helper` is `//go:build linux` (the guest is always Linux under
libkrun); `cmd/krayt-ask`'s vsock dialer and `internal/cli/resources_*.go` are the only other
OS-tagged files in the repo (`run-tasks-on-microsandbox.md`'s Done-when explicitly checks this —
the OS-specific seam is gone because the OS-specific work is msb's now). Everything else,
including `internal/sandbox` itself, is OS-agnostic. Runtime: `krayt run` requires the `msb`
binary installed; `krayt doctor` checks for it and its version floor (§6.15, §12).

### 9.2 Code generation
Deleted — superseded by `internal/sandbox` (§6.15) under ADR option B1
(`docs/adr-microsandbox-sandbox-layer.md`, `run-tasks-on-microsandbox.md`). There is no `.proto`,
no generated Go, and no `make proto`/`protoc`/`buf` toolchain left in the repo — krayt drives `msb`
over argv/stdio, and there is nothing to codegen. See git history for the pre-msb text.

---

## 10. Security Model

> **Amended by `run-tasks-on-microsandbox.md` (the cut-over, §14 Phase 11).** krayt's own
> in-guest/host-side egress proxy, its container-hardening OCI-spec code, and
> `internal/adapter/anthropic_wire.go`'s credential shape translation are all **deleted**; msb
> owns egress enforcement, TLS interception, and container hardening itself. The table and
> residuals below are rewritten for that reality — see git history for the pre-cutover text,
> which described a design that no longer runs.

**Trust boundary:** the microVM (separate Linux kernel, libkrun via msb) is the primary
isolation boundary between untrusted agent code and the host. The host kernel and filesystem are
never exposed.

| Surface | Control |
|---|---|
| Host kernel | Not shared — full VM boundary (§2, §6.15) |
| Host filesystem | No live mount; input via git bundle, output via reviewed patch |
| Repo ingest | git bundle cloned in the sandbox by `cmd/krayt-helper` (§6.7) — source `.git/hooks` are never executed or imported, and the sandbox commits under a throwaway krayt bot identity. The workspace `.git` is left agent-writable (so the agent can commit) but is **never trusted by the root-run helper's git**: patch generation runs against a root-only `patchgit` snapshot with `core.fsmonitor`/`core.hooksPath` force-cleared and `--no-textconv`, so agent-written `.git/config`/hooks/attributes cannot execute as root (§6.7, finding #2) |
| Network egress | Default-deny, translated to a **fully explicit** `msb create` policy (`task.NetworkArgs`, §6.6) — enforced entirely by msb's own userspace network stack, not by anything krayt runs. The guest now has a real, policed network interface (including DNS in `allowlist` mode) rather than none at all — a genuine capability gain over the pre-msb design, policed by msb's own gateway with DNS-rebind protection on by default |
| `ask_human` bridge | A host-side process reading sandbox-authored input: `krayt-ask` dials the host directly over vsock — no guest listener, ever. `internal/askbridge.Serve` decodes the question with a byte cap, a decode-only read deadline, and a cap on in-flight questions (§6.13). Unauthenticated by construction — any sandbox process can dial it — but bounded to one question/answer exchange per connection (residual below) |
| Container privileges | msb's own `--security restricted` profile (§6.6, §8.1), fixed and not user-configurable. krayt's pre-msb OCI-spec hardening (dropped Linux capabilities, containerd seccomp, enforced non-root, opt-in read-only rootfs) is **superseded, not layered on top** — `container.capabilities`, `container.seccomp: unconfined`, and `container.readonly_rootfs` are removed keys that hard-error, naming `--security` as the only, coarser replacement (`task.ValidateContainerPolicyForMsb`) |
| Secrets | A declared secret's real value travels only in the `msb create` child's env — never on disk, never on argv (§6.6.1, §6.8). **Redacted host-side** (there is no guest process left to redact in — the sandbox never holds a value) from live logs, `report.md`, and `ask_human` prompt/choices. `changes.patch` is **scanned, not redacted**; a hit surfaces as a Safety warning naming the key only (§6.8, §8.4) |
| Secret substitution at the host | Declaring any secret **automatically** enables TLS interception (§6.6.1) — there is no "secret without MITM" under msb, unlike the pre-msb opt-in `network.mitm`. msb substitutes the placeholder string the workload already sent, wherever it appears, but **never strips a pre-existing auth header first** the way krayt's own deleted proxy did. **The one real regression against krayt's pre-msb design**: a credential the agent obtained elsewhere and placed in a header addressed to an allowed host goes out **untouched**. Bounded by the allowlist — the agent can only send it somewhere already permitted — not eliminated |
| Run configuration (`krayt.yaml`) | **Split by provenance** (§8.3, whose table is the full field-by-field boundary): an `--config <path>` the operator named is honored in full; a `<repo>/krayt.yaml` auto-loaded from the repo under test is untrusted input and may configure a run but **not write its security policy, redirect what krayt reads or writes on the host, or relax the container's confinement**. Refused with an error: `network.mitm` (now a hard error everywhere, not just here — §6.6), `network.inject`, `network.passthrough`, `network.mode: full`, `repo:`, `container.capabilities`, `container.seccomp: unconfined` (likewise hard errors everywhere — §6.6, §8.1). Contained to the repo root (no absolute path, no `..` escape, no symlink resolving out): `secrets:`, `task:`. Without this split a poisoned repo could name the operator's own secrets-file key as scoped to an attacker-controlled host (`network.inject`), bundle a *different*, private repo into the VM for the agent to read, or read an arbitrary host file in as the run's prompt — with every consistency check passing, because the file is only ever compared against itself |
| Persistence | msb sandbox stopped and removed on teardown; fresh sandbox per run |
| Patch application | Always manual; human reviews diff before `git apply` |

**Residual considerations to document:**
- **Egress enforcement is msb's, entirely — krayt owns none of it any more.** The pre-msb design's
  residual here was about a host-process proxy compromise; that proxy is deleted, and with it the
  whole class of "guest-root escape defeats the allowlist by flushing guest nftables" finding —
  there is no guest-side firewall for a guest-root escape to flush, because msb's policy engine
  runs entirely outside the sandbox, in msb's own userspace network stack. What krayt is
  responsible for is narrower and different: emitting a **complete, correct** `msb create` policy
  every time (the never-empty-policy rule, §6.6) — a translation bug there is a config error, not
  a runtime bypass a compromised agent can trigger.
- Container-runtime / guest-kernel bugs — blast radius minimized by libkrun's own VM boundary plus
  msb's `--security restricted` profile (§6.6), which is msb's to maintain, not krayt's.
- Malicious patch content (e.g. `.git/hooks`, build scripts) applied on the **host** — the
  source repo's hooks are already never run in the sandbox, and the root-run helper's own git no
  longer trusts agent-written `.git` config/hooks/attributes either (patch generation is isolated
  in the root-only `patchgit`, §6.7 / finding #2). What remains is that the emitted
  `changes.patch` could still add files like `.git/hooks/*` or build scripts that run on the
  **host** after apply; reviewing the diff before `git apply` is the control, and a
  `--strip-hooks` / lint pass on patches is a possible future addition.
- Secret redaction coverage — the host redacts every artifact it can safely rewrite (live logs,
  `report.md`, `ask_human` prompt/choices, §6.8) using the same secrets-file values it loaded to
  create the sandbox. Two known, accepted gaps: (1) live-log redaction is chunk-oriented, so a
  secret value split across two log chunks is not caught — it affects only the streamed logs,
  since `report.md` and the patch are scanned as whole buffers; (2) **redaction matches the secret
  value verbatim, so an agent that transforms it before emitting it is not caught** — masking the
  middle, chunking it across lines, or encoding it all defeat an exact-match `Redactor`. This is
  not hypothetical: a pre-msb regression run (`run_117d6f75`) hit it with a *cooperative* agent,
  which masked the middle of the API key on its own initiative before writing the report — no
  exact match existed, so the `Redactor` never fired at all, and a 19-character key prefix
  persisted in `report.md` and the agent log. This limit is inherent to output filtering: it
  defends against accidental verbatim disclosure, not against a sandbox that holds the secret and
  chooses how to emit it — an agent set on exfiltration can always encode it through an
  allowlisted channel. Under msb this is **narrower than it was**: since no secret value ever
  legitimately enters the sandbox at all (msb substitutes only at its own network boundary), the
  scenario requires the agent to have first obtained the value some other way.
- Resource exhaustion — bounded by per-sandbox CPU/mem/disk (`--cpus`/`--memory`/`--root-disk`)
  plus `--max-duration` and krayt's own `context.WithTimeout` (§7 step 4, belt and braces).
- **`ask_human` — any sandbox process can dial the bridge.** The channel is unauthenticated by
  construction: nothing about `krayt-ask` dialing straight to the host adds or removes an identity
  check. A hostile process in the same sandbox can ask a plausible-looking question and collect
  the human's answer meant for the agent. It does not reach the host beyond the one bounded
  question/answer exchange `internal/askbridge.Serve` performs, and the existing controls are
  unchanged: the prompt is labeled agent-originated on display, and a human is never expected to
  auto-fill a secret into an answer (§6.13).
- **`ask_human` — cross-run isolation is per-sandbox by construction.** vsock maps guest CID 2:port
  to whichever host path was named on *that* sandbox's `create`, so one fixed `sandbox.AskPort` is
  safe to reuse across every concurrent run — there is no shared host CID namespace for two runs
  to collide in (§6.15, §7 step 3).
- Auth-credential blast radius — a subscription token (`CLAUDE_CODE_OAUTH_TOKEN`) is tied to a
  personal/seat plan and is less granularly revocable than a scoped API key; exposing one to
  untrusted code risks that seat's consumption and rate budget. Prefer a scoped,
  independently-revocable API key for untrusted runs (§6.14). Under msb this is **fully closed for
  every declared secret, not only an explicitly injected one**: no secret value ever legitimately
  enters the sandbox (§6.6.1), so there is nothing there for a compromise to steal regardless of
  credential shape — the pre-msb distinction between "delivered via `SecretsBundle`" and
  "injected" no longer exists. What is unchanged: a compromised agent can still spend the
  credential's quota and rate budget against every allowed host for the run's duration, so
  "prefer a scoped API key for untrusted code" still stands.
- **Placeholder shape.** msb's own default placeholder is `$MSB_<NAME>`; krayt may supply a
  credential-shaped custom placeholder via msb's `placeholder` field instead (see §6.14, §6.15).
  P5 (`probe-microsandbox-feasibility.md`, 2026-08-29) confirmed Claude Code accepts msb's default
  placeholder unmodified — no client-side credential-format check is known to exist — so this is
  recorded as an available option, not a demonstrated requirement. If a future probe (or a
  different vendor) forces a stricter-looking placeholder, that requirement is itself a finding
  worth recording here, because a placeholder forced to look more like a real credential is more
  likely to be mistaken for one by a human reading a log.

---

## 11. The Minimal VM Image (Nix-based)

A small Linux image whose only job is to run the guest-agent + a container runtime.
The image is **defined declaratively with Nix** and built reproducibly. This is the
isolation boundary, so we want to know exactly what is in it and be able to rebuild it
bit-for-bit.

> Scope note: Nix governs **only** this base micro-VM image. The user's Docker image
> (the AI + tools) is supplied at run time and is explicitly **not** Nix-built. Keep the
> two separate.

> **Known-stale pending `retire-vm-image-pipeline.md`.** `run-tasks-on-microsandbox.md` deleted
> `cmd/krayt-agent` and `cmd/krayt-vsock-forward`, which `images/flake.nix`'s guest-agent
> `buildGoModule` target still references — this whole pipeline builds an artifact nothing
> consumes any more (§14 Phase 11), and its Nix build will fail as-is until the follow-up task
> removes it. Left unfixed here deliberately; not attempted in this task.

### 11.1 What the image contains
- **Kernel:** a minimal Linux kernel (pinned via nixpkgs) with virtio, vsock, overlayfs,
  and nftables enabled.
- **Userland:** minimal NixOS closure — **containerd** as the container runtime (driven
  by the guest-agent's Go client; see §6.10) with `runc` or `crun` as the OCI runtime,
  nftables, and the embedded **guest-agent** binary, started as a systemd service.
- **guest-agent build:** built with `buildGoModule` so the Go toolchain is pinned too —
  the whole artifact is reproducible end to end.
- **Boot:** vz supports Linux kernel boot and EFI on macOS 13+. Standardize on one
  (kernel + initrd/rootfs is the simpler path for vz).

### 11.2 Why Nix
- **Reproducible:** every input pinned via `flake.lock`; a given `krayt` version maps
  to a known image hash.
- **Declarative:** the entire system (packages, kernel version + config, services,
  nftables rules, runtime) lives in one expression — no imperative Dockerfile/rootfs drift.
- **Read-only by design:** the `/nix/store` is immutable, matching the "minimal,
  untampered VM" philosophy.
- **Cheap updates:** bumping the kernel or any package is a one-line input/lock change —
  important because the guest kernel is the security boundary and needs timely patching.
- **Linux backend bonus:** `microvm.nix` is purpose-built for minimal NixOS microVMs on
  firecracker / cloud-hypervisor / qemu — nearly turnkey for the Phase 7 Linux provider.

### 11.3 The macOS build caveat (settled: build in CI)
Apple's Virtualization.framework is **not** a `microvm.nix` backend, and building
Linux/NixOS images **on a Mac requires a Linux builder**. Resolution:
- On macOS, Nix is the **builder** that produces the `vmlinuz` + rootfs artifacts the
  `vz` provider boots — not an integrated hypervisor layer.
- **Canonical build path = GitHub Actions on an arm64 Linux runner** (see §11.5). On a
  Linux runner this is effectively a no-op, so the "Mac needs a Linux builder" caveat
  disappears for the build path.
- A local `nix-darwin` `linux-builder` VM is **optional** — only worth setting up if you
  want fast local image iteration without round-tripping through CI.

### 11.4 Specify, distribute, update
- **Specify:** the flake under `images/` is the single source of truth for the base image.
- **Build:** CI on an **arm64 Linux runner** builds the kernel + rootfs natively for
  `aarch64-linux` (no emulation) and emits a versioned, content-addressed artifact.
- **Distribute:** the artifact is packaged as a standard **OCI artifact** and pushed to
  an OCI registry (the `rootfs.img` layer is zstd-compressed in transit, to shrink the
  ~2 GiB cold-pull download — see below). The OCI **digest is the content address** —
  `krayt` pins its version → digest and **verifies the digest** on `krayt image pull`
  (and `doctor`) before first use. The registry is interchangeable (ghcr.io is the
  convenient default, but any OCI-compliant registry works — **no hard dependency on
  ghcr.io**).
- **Run:** each run gets a **copy-on-write clone** of the verified base image so runs
  never share state.
- **Update:** bump the flake input/lock → CI rebuilds → push new OCI artifact → bump the
  pinned digest in `krayt`. Fully auditable in git.
- **Reclaim:** the base image is cached per digest under `<user-cache-dir>/krayt/vmimage/<digest>/`
  (from `os.UserCacheDir()`; typically `~/.cache/krayt/...` on Linux and `~/Library/Caches/krayt/...` on macOS) and, like the user-image cache (§6.11), never cleaned up on its own. `krayt image ls/rm/prune`
  manage both caches together (§6.11); the base-image side of the retention policy is simply
  *keep the pinned digest, drop the rest* — `krayt run` only ever reads the pinned digest's
  directory, so any other vmimage entry (an old pin, or a stale sanitized-ref dir) is dead
  weight and pruned unconditionally.

**Retention policy (`krayt image prune`).** Removes everything outside these keeps, deleting
by default (no `--dry-run` needed to take effect):
- **base VM image:** keep **only** the entry matching the pinned digest; every other vmimage
  entry is removed unconditionally. `--all` never removes the pinned entry — use
  `krayt image rm --force <digest>` for that one specifically.
- **container image:** keep it if **either** (a) it was last used within `--older-than`
  (default `24h`), **or** (b) its digest matches the image of a **non-terminal** run under
  `--repo` (default `.`) whose recorded `image_ref` is *itself* a digest reference
  (`…@sha256:<hex>`, direct string match — a tag-based ref can't be resolved to a cache digest
  offline and relies on the age floor instead; a known, documented gap).
- `--all` bypasses **both** container-kind protections (age + in-use). `--dry-run` reports
  exactly what would be removed/kept and why, and the reclaimable total, without deleting.
`krayt image rm <digest>` accepts a full digest or an unambiguous hex prefix (docker-rmi
style) and errors — without deleting anything — on no match, an ambiguous prefix, or the
pinned base image without `--force`.

> Fallback if Nix ever becomes friction: `mkosi` (systemd's image builder) is the
> next-best declarative option — gentler, reasonably reproducible — at the cost of
> Nix's strict reproducibility and the `microvm.nix` integration. Not needed for a
> single-trusted-owner setup.

### 11.5 CI / build pipeline (GitHub Actions)
The canonical build path. Clean and simple: build natively on arm64, publish as an
OCI artifact.

- **Runner:** an **arm64 Linux runner** (e.g. `ubuntu-24.04-arm`). Building natively for
  `aarch64-linux` keeps the toolchain clean — no `binfmt`/QEMU cross-emulation, and the
  artifact arch matches the vz VM (arm64) exactly.
- **Nix:** install via `DeterminateSystems/nix-installer-action` (or
  `cachix/install-nix-action`); optional binary cache to speed rebuilds.
- **Build:** `nix build .#vmImage` → versioned kernel + rootfs artifacts.
- **Package & push:** wrap the artifacts as an **OCI artifact** (e.g. via `oras push`)
  with a descriptive media type; the registry returns/records the **digest**.
- **Pin:** the build records `version → digest`; `krayt` consumes that mapping and
  verifies the digest at pull time.
- **Trigger:** on tag / release (and on `images/flake.lock` changes, to catch kernel and
  package bumps automatically).

Sketch:
```yaml
jobs:
  build-image:
    runs-on: ubuntu-24.04-arm        # native aarch64-linux, no emulation
    steps:
      - uses: actions/checkout@v4
      - uses: DeterminateSystems/nix-installer-action@main
      - run: nix build .#vmImage      # -> ./result (kernel + rootfs)
      - run: |
          zstd -19 -T0 -o rootfs.img.zst ./result/rootfs.img
          oras push <registry>/krayt-vmimage:${GITHUB_REF_NAME} \
            ./result/vmlinuz:application/vnd.krayt.kernel \
            ./rootfs.img.zst:application/vnd.krayt.rootfs+zstd
      # capture the pushed digest -> record as the pinned image reference
```
`rootfs.img` is the only layer compressed (`vmlinuz`/`initrd` are too small for it to be worth
the complexity) — the client (`vmimage.Pull`) decompresses it back to plain raw bytes
immediately after download, so nothing past `Pull` ever sees the compressed form.

Consumer side: `krayt image pull` resolves its pinned digest, pulls the OCI artifact
from whichever registry is configured, verifies the digest, and caches the base image
locally for CoW cloning. Because it is a plain OCI artifact addressed by digest, the
registry is swappable and the image is portable across hosts.

### 11.6 Image internals & boot contract (sub-spec)
This is the riskiest deliverable and the one Claude Code cannot fully verify locally
(building/boot-testing needs a Linux builder — own it in CI; see §11.3). What the flake
must produce and guarantee:

- **Init:** NixOS with **systemd** (decision settled — consistent with `microvm.nix` on
  the Linux backend; systemd owns mounts, ordering, and network bring-up). No hand-rolled
  PID 1.
- **Services (systemd units), ordered:**
  1. `containerd.service` — containerd daemon, socket at `/run/containerd/containerd.sock`.
  2. `krayt-agent.service` — the guest-agent, `Type=notify`,
     `After=containerd.service network-online.target`, `Wants=network-online.target`.
- **Filesystems:** kernel built with `virtio`, `vsock` (`CONFIG_VSOCKETS`,
  `CONFIG_VIRTIO_VSOCKETS`), `overlayfs`, `nftables`. Rootfs as the boot disk vz mounts;
  `/run`, `/tmp` on tmpfs; containerd state under `/var/lib/containerd`.
- **Networking:** one NAT NIC up via `systemd-networkd`; nftables ruleset from §6.6 applied
  by the guest-agent at run start (not baked statically — it depends on per-task policy).
- **Closure contents (and nothing else):** kernel, systemd, containerd + `runc`/`crun`,
  nftables, the static guest-agent binary, CA certificates, busybox-equivalent coreutils, and
  the pieces the run pipeline shells out to: **`gitMinimal`** for the §6.7 bundle
  ingest/diff, **`e2fsprogs` + `util-linux`** to format + mount the per-run scratch disk
  (§6.10), and — since `move-egress-proxy-to-host.md` replaced the in-guest L7 proxy with a
  host process — the **`krayt-vsock-forward`** binary (a dumb TCP<->vsock pipe, not a proxy)
  run as the dedicated **`proxyd`** user, kept as defense in depth though no longer load-bearing
  for the L3 lock (§6.6). No editors, no shells beyond what systemd needs, no package manager.
- **Output artifacts:** `vmlinuz` + `initrd` + `rootfs.img` (**raw** format — neither backend
  takes qcow2), built for **both** `aarch64-linux` and `x86_64-linux` from one flake and one
  NixOS config, and published as a **single multi-arch OCI index** (§11.5). `rootfs.img` is
  compressed (`+zstd`) for the registry transfer only; `krayt image pull` decompresses it back
  to the same raw format before anything touches it, so the raw-on-disk / CoW-clone contract
  stated here is unchanged. krayt pins the
  *index* digest — one `PinnedRef`, one `PinnedDigest`, no architecture anywhere in the pin —
  and resolves it to the arch it can boot at pull time (`vmimage.selectPlatform`): arm64 for
  vfkit, amd64 for firecracker.
  - **The per-arch artifacts must carry an OCI image config declaring `{"architecture":…,
    "os":"linux"}`** (`oras push --config`). This is easy to miss and fails in an ugly way:
    these are *artifacts* with custom media types, not container images, so nothing else carries
    an architecture. Without the config, `oras manifest index create` cannot infer a platform,
    the index entries get a null `platform`, and selection then matches **nothing on any host** —
    a broken pull for everyone, from an index that published perfectly cleanly. CI asserts the
    platforms are present rather than trusting it.
- **The x86_64 (firecracker) boot contract differs from the aarch64 (vfkit) one** in three ways
  that are easy to get wrong and fail obscurely:
  1. **The kernel must be an uncompressed ELF `vmlinux`.** Firecracker's x86_64 loader cannot
     boot the `bzImage` that `system.boot.loader.kernelFile` names — upstream's own CI kernels
     are `vmlinux-*` ELF binaries for this reason. nixpkgs ships the ELF as `vmlinux` in the
     kernel's **`dev` output**; the flake strips it (379 MiB → ~55 MiB, debug info only) and
     publishes it as `vmlinuz`. The kernel has `CONFIG_PVH=y`, so Firecracker finds the PVH
     entry note.
  2. **virtio-MMIO, not virtio-PCI.** Firecracker has no PCI bus, so `virtio_mmio` must be in
     `boot.initrd.availableKernelModules` or root will not mount. (Both transports are listed;
     the unused one simply never matches a device.)
  3. **Console is `ttyS0`** (8250 serial), not vfkit's `hvc0` virtio-console. The provider
     normalises this itself, so a `VMSpec` written for either backend boots on both.
- **`systemd-network-generator` must be enabled** (it ships with systemd but is off by default).
  It is what turns the firecracker provider's cmdline `ip=`/`ifname=` into networkd config; see
  §6.6. Without it the guest boots fine and answers `Hello` with **no network address at all** —
  a silent failure, so it has its own on-hardware regression test.
- **Boot contract (what the host relies on):** within N seconds of `VM.Start` (vfkit
  process up + VM booted), the guest-agent is listening on vsock port `1024` (bridged to the
  host `socketURL`) and answers `Hello`. The host treats a successful `Hello` as "VM ready";
  failure within a timeout → abort + `Destroy`.

> Practical ownership: have Claude Code author `flake.nix` and the systemd units, but make
> the boot-test (vfkit boots the image → `Hello` round-trips) a human/CI checkpoint, since
> the agent's sandbox can't build or boot the Linux image.

---

## 12. macOS Specifics & Gotchas

> **Amended by `run-tasks-on-microsandbox.md` (the cut-over, §14 Phase 11).** `vfkit` is no
> longer a prerequisite — the vfkit provider is deleted along with the rest of
> `internal/provider`. **`msb`** is the one thing `krayt run` needs installed. See git history for
> the pre-cutover text.

- **Runtime dependency: `msb`.** `krayt run` needs the `msb` binary installed and resolvable via
  `PATH` (or `KRAYT_MSB_BIN`, §6.15). `krayt doctor`'s msb checks (§6.15) are now **mandatory, not
  optional** — msb is the only sandbox backend, so a host without a healthy msb install fails
  `krayt doctor` outright, and version drift below `sandbox.MinVersion` is reported as a failure,
  not a warning.
- **Signing / entitlements.** msb's own installer/runtime carries whatever signing its
  libkrun-based VM boundary needs; **krayt itself needs no virtualization entitlement or special
  code-signing of its own** — the same property the vfkit-carries-the-entitlement design had,
  just owned by a different vendored binary now.
- **Apple Silicon:** the user's OCI image runs same-arch under libkrun (arm64 on an
  Apple-Silicon host) — there is no separate "guest-agent architecture" to match any more, since
  krayt ships no guest-agent (§6.4 stub). `guestbin.Binary(name, runtime.GOARCH)` (§6.15, §7 step
  5) picks the matching `krayt-helper`/`krayt-ask` binary for the host's own arch.
- **vsock:** the one remaining vsock use is `ask_human` — `krayt-ask` inside the sandbox dials
  `AF_VSOCK` to host CID 2, which msb's own `--vsock HOST_PATH:PORT` route bridges to a host unix
  socket `internal/askbridge` listens on (§6.13). There is no host/guest asymmetry left for krayt
  to hide behind an interface (§6.12 stub) — msb owns the vsock plumbing on both sides.
- **Networking:** msb owns network policy entirely (§6.6); domain filtering, TLS interception, and
  the private/loopback/metadata denylist are all msb's userspace network stack, not anything
  krayt runs on the host or in the guest.

---

## 13. CLI Surface (initial)

```
krayt run     [--image] [--task] [--repo] [--config] [--secrets]
                 [--net allowlist|full|none] [--allow domain ...]
                 [--cpus] [--memory] [--disk] [--timeout] [--detach]
                 [--skip-resource-check]
                 [--on-question wait|fail] [--question-timeout DUR] [--on-question-timeout sentinel|abort]
krayt ls                       # list active/recent runs (shows `waiting` runs)
krayt attach  <run-id>         # live-stream a running agent's logs
krayt logs    <run-id>         # show persisted logs
krayt questions <run-id> [--pending-only] [--sort asked|pending-first|pending-last]   # list a run's questions + answers (§6.13)
krayt answer  <run-id> [<qid>] <response>   # answer a waiting agent question (§6.13); FIFO if qid omitted
krayt patch   <run-id>         # print/locate the run's changes.patch
krayt apply   <run-id>         # helper: git apply the patch onto the host (after review)
krayt stop    <run-id>         # stop + destroy a run's VM
krayt rm      <run-id>         # remove run artifacts
krayt image pull  [--ref] [--digest]                 # pull + verify the base VM image (§11.4)
krayt image ls                                       # list cached base-VM + container images with size/last-used
krayt image rm    <digest> [--force]                 # remove one cached image by digest/prefix (--force for the pinned base)
krayt image prune [--repo] [--older-than DUR] [--all] [--dry-run]   # bulk-reclaim cache disk under the retention policy
krayt doctor                   # check host prereqs (vfkit installed+runnable on macOS; /dev/kvm on linux)
krayt upgrade [--version vX.Y.Z] [--yes|-y] [--check]   # update krayt in place from a GitHub release
```

`run` is headless/detached-capable; default streams logs to the terminal but the VM
work is the same either way.

`upgrade` re-verifies the downloaded binary against the target release's published
`checksums.txt` before installing it — the same check as the manual install path (README's
"Prebuilt binaries"), automated — and never touches any other command's behavior: it is the only
command that ever talks to GitHub, and only on-demand when a user runs it.

`--task` takes a path to the task prompt file, or `-` to read the prompt from stdin (e.g.
`echo "…" | krayt run --task - …`), so a prompt can be supplied without a file on disk. Combined
with `--detach`, the already-read stdin bytes are spooled to a file in the run dir and handed to
the detached supervisor child, since its stdin is gone after it re-execs (§6.2).

**Shell completion.** cobra's built-in `krayt completion <bash|zsh|fish|powershell>` statically
completes command and flag names. On top of that, the run-scoped commands (`apply`, `logs`,
`attach`, `stop`, `rm`, `patch`, `questions`, `answer`) dynamically complete `<run-id>` from
`.krayt/` state under `--repo`, each filtered to the runs it can act on (`stop`/`attach` → live
runs, `rm` → finished unless `--force`, `answer` → `waiting`), and `answer` also completes the
run's pending `<question-id>`. `image rm` completes `<digest>` from the cached images in both
cache roots (offering the full digest so a pick is unambiguously removable), annotated with each
image's kind and size and `(pinned)` for the base image. `run`'s enum flags (`--net`, `--on-question`, `--on-question-timeout`,
`--agent`) and `questions --sort` complete their fixed value sets from the same constants that
validate them; `run`'s `--image`/`--allow` complete from this repo's run history. Untrusted
agent-originated text (question prompts) is sanitized (§6.13) before appearing in a completion
description.

---

## 14. Milestone Roadmap

**Test strategy (applies to every phase).** *(Rewritten by `run-tasks-on-microsandbox.md`, §14
Phase 11 — the sentence this replaces, "the `Provider` interface is the seam that makes the core
testable without a VM," is now **false**: `internal/provider` and its `fakeProvider` are deleted.
See git history for the pre-cutover text.)* The test seam is a **scriptable fake `msb` binary**
the orchestrator's own tests re-exec themselves as — the same idiom `internal/sandbox`'s own
tests already used one layer down (`add-msb-sandbox-driver.md`): a real `exec.Cmd` runs a
re-exec'd test binary that plays `msb`, detected by argv verb rather than an env flag (the child
env is a closed allowlist by design, §6.15, so a marker on the outer process can't reach it).
Unit-test the orchestrator, task, patch, and CLI against it on any OS. Real-sandbox behaviour
(msb boot, image pull, network policy enforcement) is covered by an integration harness gated
behind a build tag and run on a real Mac / in CI, with a real `msb` installed. Each phase below
lists a concrete **Done when** checkpoint — prefer wiring that as an automated test.

### Implementation protocol for the coding agent
Some steps cannot be completed by the coding agent alone — they need credentials, real
hardware, a Linux builder, or live secrets. Handle these explicitly; do **not** guess,
stub, or fabricate results for them.

**Maintain a handoff log.** Keep a `HUMAN_TODO.md` at the repo root — the single place
where work requiring a human is recorded.

**When a task needs a human:**
1. First do everything around it that you *can* — write the config, scripts, workflow
   YAML, entitlements file, exact commands, and the tests — so the human's part is reduced
   to running or providing only the thing that genuinely requires them.
2. Append a structured entry to `HUMAN_TODO.md` (template below).
3. Then decide based on dependency:
   - **Non-blocking** (no current task depends on it): log it and continue with other work.
   - **Blocking** (a downstream task can't proceed or be verified without it): stop and
     ask the human directly in the session, referencing the `HUMAN_TODO.md` entry.

Never fabricate a result for a human-only step — no fake code signatures, no invented
image digests, no "boot succeeded" without a real boot. An honestly-blocked step is
correct; a faked one is a defect.

**Categories that require a human:**
- Apple Developer signing identity / notarization — **only if** you switch to the direct
  `vz` provider; the v1 vfkit path needs no krayt signing (§12). vfkit install is trivial.
- A Linux builder or CI run to build/boot the Nix image (§11.3, §11.6).
- Registry or other credentials/secrets (publishing the OCI artifact, §11.5).
- Real-hardware checks: vz boot on a Mac, `/dev/kvm` on Linux (Phase 1 / 6 "Done when").
- Live API keys / secrets needed to exercise a real agent image (Phase 5).

**`HUMAN_TODO.md` entry template:**
```
## [<phase>] <short title>
- Needed: <what the human must do or provide>
- Why the agent can't: <credential / hardware / builder reason>
- Exact steps/commands: <copy-pasteable commands, or the file to fill in>
- Verify success by: <observable check, ideally a test or command output>
- Blocking: yes/no — <what is blocked if yes>
```

**Entry lifecycle — a done entry is deleted, not ticked.** `HUMAN_TODO.md` answers one question:
what still needs a human? An entry that has been verified is removed from the file entirely, so that
question stays cheap to answer as the project grows. Before removing it, record the outcome where it
belongs permanently — the relevant phase's `Done when (hardware)` checkbox below (with the run id
that proved it), `docs/ai-tasks/README.md`'s status table, and any code comment that carries the
vendor/provenance fact. `git log` preserves the entry's full text either way, so nothing is lost by
deleting; something *is* lost by deleting before the evidence has a durable home.

Tasks marked **[HUMAN]** below are the expected handoff points.

### Phase 0 — Foundations ✅
- [x] Repo scaffold, Go module, CI, lint, build tags (§9.1).
- [x] Root `flake.nix` dev shell (protoc/buf/oras pinned) + `Makefile` with `make proto` (§9.2).
- [x] Define `Provider`/`VM` interfaces and `RunSpec`/`VMSpec` types.
- [x] Author `krayt.proto` (§6.5); generate + check in `internal/protocol/pb` (§9.2).
- [x] `fakeProvider` + in-process gRPC loopback for tests.
- [x] `krayt doctor` for host prereq checks.
- [x] **Done when:** `go test ./...` passes on macOS and Linux; a `Hello` RPC round-trips over the fake provider.

### Phase 1 — Boot a VM on macOS ✅
- [x] `vfkit` provider: build VM config via vfkit `pkg/config`, launch the signed vfkit subprocess, control via its REST API; CoW-clone the raw rootfs (`clonefile`); NAT + vsock (`socketURL`) devices.
- [x] No krayt code-signing needed (entitlement lives on vfkit, §12). `doctor` checks vfkit is installed + runnable; README documents `brew install vfkit`. **[HUMAN: install vfkit]** — trivial, scriptable; not a signing identity.
- [x] `images/flake.nix`: NixOS + systemd image per §11.6 (raw `rootfs.img` + kernel + initrd); build in CI on arm64 Linux runner; publish OCI artifact (§11.5). **[HUMAN: Linux builder/CI + registry creds]** — agent writes the flake + CI workflow; human runs CI / provides registry credentials.
- [x] `krayt image pull` + digest verification before first run.
- [x] `DialControl` = `net.Dial("unix", socketURL)` to vfkit's vsock bridge + gRPC client wiring (§6.12).
- [x] **Done when — Historical, describes the pre-msb design, superseded by Phase 11.** on a real Mac (with vfkit installed), `krayt` boots the published image and a `Hello` RPC round-trips host↔guest over the vfkit vsock socket. **[HUMAN: boot test on real hardware]** *(The vfkit provider and its `Hello` RPC are deleted by `run-tasks-on-microsandbox.md`; this criterion described a backend that no longer exists in the design.)*

### Phase 2 — End-to-end single run (happy path) ✅
- [x] Host: pull user OCI image + export OCI archive; digest-keyed cache (`imagestore`).
- [x] `QueryImageBlobs` + `PushImage` (stream only missing blobs); guest imports into containerd.
- [x] Host: create a **self-contained git bundle** (parentless snapshot, or full history at `bundle_depth 0`) (§6.7). *(Non-mutating `include_dirty` capture is deferred to Phase 3.)*
- [x] `PushCode` streams the bundle → guest writes it to a temp file, `git bundle verify`s it (from a throwaway repo — verify needs a repo context), clones into `/workspace`, sets the krayt bot git identity, and **records the baseline** (`krayt-baseline`) before the agent runs (§6.7).
- [x] `PushTask` injection at `/task/prompt.md`; `Start` runs the container entrypoint (agent-agnostic).
- [x] Patch generation (`git diff` vs the recorded `krayt-baseline`, staging all so uncommitted edits are captured) + optional reverse range bundle (`commits.bundle`) + `CollectArtifacts` back to host (§6.7).
- [x] Guaranteed VM teardown (defer + signal handling).
- [x] **Done when:** `krayt run` against a trivial image that edits one file yields a correct `changes.patch` that `krayt apply` cleanly applies to the host repo. *(Met both via the automated `fakeProvider` proof and a real-VM run on Apple Silicon — see HUMAN_TODO.md.)*

### Phase 3 — Security & capability controls ✅
- [x] Egress proxy (uid `proxyd`) + nftables default-deny ruleset (§6.6); per-task allowlist. *(Hand-rolled proxy behind a swappable `Factory` seam; L7 allowlist unit-tested, L3 lock + raw-socket block confirmed on hardware. The proxy resolves DNS as `proxyd` so lookups pass the lock while the container stays DNS-blocked.)*
- [x] Per-task secrets file → `SecretsBundle` → container tmpfs; log redaction.
- [x] Resource limits (cpu/mem/disk) + wall-clock timeout → kills container then VM. *(Disk: the per-run scratch disk sized to `DiskGiB` landed early in the vfkit provider, Phase 2; cpu/mem are applied to the VM; wall-clock now kills the container task then tears down the VM.)*
- [x] Include-dirty: non-mutating temp-index capture (`GIT_INDEX_FILE` + `write-tree` + `commit-tree`) folded into the inbound bundle when `include_dirty` is set, leaving the user's repo untouched (§6.7). *(Moved here from Phase 2.)*
- [x] **Done when:** a container can reach an allowlisted host, is blocked from a non-allowlisted host and from a raw (non-proxied) socket, and secrets never appear in logs/artifacts (asserted by tests). *(Redaction + proxy L7 by the automated suite; the L3 raw-socket lock confirmed on Apple Silicon — `TestEgressEnforcement` green: PASS reach-allowlisted / block-non-allowlisted / block-raw-socket.)*

### Phase 4 — Concurrency & UX
- [x] Orchestrator → `Manager`: multiple concurrent runs, max-concurrency, per-VM socket device, state under `.krayt/` (§6.2). *(`RunRecord` state model + `Manager`; `TestConcurrentRuns`, `TestMaxConcurrency`.)*
- [x] `ls`, `attach`, `logs`, `stop`, `rm` — over on-disk state + a direct guest dial (daemon-less, process-agnostic; §6.2). *(Plus `patch`; `stop` signals the recorded supervisor PID.)*
- [x] Live log streaming (`Start` stream) + headless detach. *(v1: foreground supervisor; `--detach` = headless. `attach` follows the on-disk log — `TestAttachLive`. The detached "park and walk away" supervisor is specced in §6.2 and scheduled in Phase 5 below.)*
- [x] Agent-question channel (§6.13): `RunEvent.Question` + `Answer` RPC, in-VM bridge (`internal/guest/ask`), `waiting` state, `krayt answer`, desktop notification, Q&A persisted to `questions/`. Default `mode: fail` so it's inert unless opted in. *(Serialized `Start`-stream sends fixed a latent concurrent-`Send`. The container-facing ask socket + `krayt answer` cross-process dial are wired but exercised for real only with the Phase-5 front-ends / on hardware — see HUMAN_TODO.)*
- [x] Config file + flag precedence; example configs. *(`krayt.yaml` via yaml.v3; `configs/krayt.yaml`; `TestConfigPrecedence`.)*
- [x] **Done when:** N runs execute concurrently with isolated patches/logs, `attach` shows live output, and (with `--on-question=wait`) a stubbed agent question drives a `waiting` state that `krayt answer` resolves. ✅ *(`TestConcurrentRuns` + `TestAttachLive` + `TestQuestionWaitAnswer`, all against the fakeProvider; race-clean.)*

### Phase 5 — Polish & optional orchestration
- [x] Emit `report.md` + `meta.json` per the §8.4 schemas (exit code, timings, patch stats, questions; agent notes if the image writes `/output/report.md`). *(Host-side, fakeProvider-proven: `RunRecord` is the full §8.4 schema; `report.go` renders the fixed-section report and folds the agent's `/output/report.md` into Notes; patch diffstat via `patch.Stat` (`git apply --numstat`). `TestReportAndMeta` + `TestReportPrefersAgentNotes`. **Confirmed on Apple Silicon** — run_afbb910f wrote a full §8.4 `meta.json` (`questions[]` with `answered_by`/`waited_secs`, patch diffstat, timings) and a rendered `report.md`.)*
- [x] `krayt-ask` CLI front-end (§6.13): a small in-container binary any agent can shell out to (`krayt-ask [--choices a,b] "question"`), bridging to the Phase-4 question channel over the mounted unix socket; prints the answer on stdout (exit 0) or a no-answer sentinel (exit 2) so the agent falls back. *(`cmd/krayt-ask`, reusing `ask.OverSocket`; `TestRunSentinelWhenUnreachable`/`TestRunUsage`, round-trip `TestRunRoundTrip` skips under the sandbox's blocked `bind(2)` — HUMAN-verified on hardware. Built into the image via `flake.nix` and bind-mounted onto the container PATH at `/usr/local/bin/krayt-ask` (guest `RunConfig.AskBinary` + runner mount; `TestAskBinaryIn`). Exercising it on hardware — the last Done-when clause — awaits the base image rebuild (`hack/krayt-ask-probe`, HUMAN).)*
- [x] Optional agent adapters (`internal/adapter`: `none`/`claude-code`/`gemini-cli`/`opencode`) — host-side pre-flight (`--agent` flag + `agent.adapter`) that validates auth and wires `krayt-ask` (`KRAYT_ASK_SOCKET`) when `--on-question=wait`. *(In-container credential export + agent launch run in the image entrypoint (§8.2) and need live keys — HUMAN. MCP-server registration is Phase 6.)*
- [x] Claude Code adapter maps the provided credential to the correct env var (`ANTHROPIC_API_KEY` vs `CLAUDE_CODE_OAUTH_TOKEN`) and enforces exactly-one auth, failing fast if both are set (§6.14). *(`claude-code` adapter, exactly-one over the recognized keys; wired into `krayt run` before any VM boot. `TestClaudeCodeExactlyOne` + `TestApplyAdapterAuthGate`/`TestApplyAdapterWiresAsk`.)*
- [x] **Detached supervisor — "park and walk away" (§6.2):** `krayt run --detach` re-execs a session-detached (`setsid`) per-run supervisor (no central daemon) that owns the VM to completion; the launcher returns immediately. Cross-process max-concurrency via a file-lock semaphore (`AcquireSlot` over `.krayt/slots/`, `--max-concurrency`). Reuses the Phase-4 on-disk state + management commands unchanged; localized to the run entrypoint. *(`TestAcquireSlotLimits`/`TestAcquireSlotCrossProcess` (real subprocesses) + `TestSpawnDetached`; existing `TestMaxConcurrency` now backed by the file lock. End-to-end "close the terminal, answer after" **verified on Apple Silicon** via `hack/ask-probe`: `--detach` returned with the supervisor pid, `krayt ls` showed `starting`→`waiting`, and `krayt answer` from a separate shell resolved it to `done` — see HUMAN_TODO.)*
- [x] Patch safety lint (flag hooks/suspicious changes). *(`patch.Lint` flags changes that execute outside the workspace edit — git hooks, CI config, shell startup files, direnv, newly-executable files — surfaced in `meta.json` `safety`, report.md's Safety section, and a `krayt run` warning. `TestLint`.)*
- [x] **Done when:** a real agent image completes a task and the run dir contains patch + report + meta; with the adapter + `--on-question=wait`, an agent's `krayt-ask` call round-trips to `krayt answer`; and a `--detach`ed run survives its launching process — its `waiting` question is answerable from a separate invocation after the terminal closes. ✅ **All three clauses verified on Apple Silicon:** *(1) detach — `hack/ask-probe` (`--detach` returned, `waiting` answered from a separate shell); (2) real agent — `hack/claude-code` (Claude Code authenticated via `CLAUDE_CODE_OAUTH_TOKEN`, edited `/workspace`, exit 0, patch+report+meta, `krayt apply` clean); (3) `krayt-ask` binary — `hack/krayt-ask-probe` (non-root uid 1000 shelled out to `krayt-ask` on PATH → `krayt answer` resolved it). Base image v0.0.0-rc16. The premium MCP front-end and precise `waiting`→`running` resume are Phase 6.)*

### Phase 6 — `ask_human` MCP front-end & precise resume
Both items need a `.proto`/image change, so they share one guest image rebuild and one HUMAN gate — carved out of Phase 5 to keep that phase fully host-provable.
- [x] In-VM `ask_human` **MCP server** (§6.13): `krayt-ask --mcp` runs a stdio MCP server (official Go SDK) exposing one tool — `ask_human{ question, choices?, context? }` — bridged to the question channel via `ask.OverSocket`; its tool *description* steers *when* to ask, and a no-answer maps to a "proceed autonomously" sentinel. The `claude-code` entrypoint registers it (`.mcp.json` / `--mcp-config`) only when `--on-question=wait` (i.e. `KRAYT_ASK_SOCKET` set). *(Handler host-proven by `TestAskHumanHandler`; on-VM round-trip rides the shared Phase-6 rebuild. Decision resolved: official `github.com/modelcontextprotocol/go-sdk` v1.2.0 (§9.1), for maintainability over bespoke wire code.)*
- [x] Guest **"question resolved"** `RunEvent` (§6.13): emitted when `bridge.Answer` delivers, so the host flips `waiting`→`running` precisely on answer instead of holding `waiting` until the run ends. *(`RunEvent.Resolved` added to the proto + regenerated; `ask.Bridge.OnResolved` → guest emit; host tracks outstanding questions and resumes at zero — fires for every answer path (Answer RPC / cross-process `krayt answer` / timeout sentinel). Host-proven by `TestQuestionResolvedResumes` + `TestBridgeOnResolved`; existing waiting-state tests still pass. On-VM confirmation rides the shared Phase-6 image rebuild.)*
- [x] **Done when:** on a rebuilt image with the adapter + `--on-question=wait`, an agent's `ask_human` **MCP tool call** round-trips to `krayt answer`, and the run flips `waiting`→`running` precisely when the answer lands (not on the next log line). ✅ **Verified on Apple Silicon (base image v0.0.0-rc17):** Claude Code registered the MCP server, called `ask_human` (run → `waiting`, question "PostgreSQL or SQLite?" persisted), `krayt answer … PostgreSQL` round-tripped the answer, `krayt ls` **directly showed the run flip `waiting`→`running`** on the answer (the guest `Resolved` event), Claude implemented the chosen DB (`db.py` + `psycopg`), and finished `done` (exit 0) — the full §6.13 premium path. Host logic proven by `TestQuestionResolvedResumes` + `TestAskHumanHandler`.

### Phase 7 — Linux backend (parity) ✅
- [x] `firecracker` provider behind the same `Provider` interface. *(`internal/provider/firecracker`, `//go:build linux`. Subprocess + REST-over-unix-socket, hand-rolled — no new deps (§9.1). vsock is a unix dial + `CONNECT <port>` handshake, **not** `AF_VSOCK` — §6.12 corrected. CoW = `FICLONE` with a sparse-copy fallback (ext4 has no reflink). Per-VM tap + `/30` + flock'd slot allocation, since firecracker supplies no NAT/DHCP — §6.6.)*
- [x] `/dev/kvm` detection + graceful messaging in `doctor`. *(Checks read/write **access**, not just presence: being in the `kvm` group does nothing until a new login session, which is the failure everyone actually hits. Plus firecracker binary+version, `/dev/net/tun`, `CAP_NET_ADMIN`, and host NAT.)*
- [x] Reuse guest-agent, protocol, patch, secrets, orchestrator unchanged. *(Not one line changed in any of them. The only edit outside `internal/provider/firecracker` + `internal/cli` (per-OS wiring) is the x86_64 image in `images/flake.nix` and a build-tagged `newTestProvider()` seam in the integration tests.)*
- [x] x86_64 VM image. *(Same flake, both systems. ELF `vmlinux` + `virtio_mmio` + `systemd-network-generator` — see §11.6.)*
- [x] **Done when — Historical, describes the pre-msb design, superseded by Phase 11.** the Phase 2 end-to-end test passes unmodified on a Linux host via the firecracker provider. *(The firecracker provider is deleted by `run-tasks-on-microsandbox.md`; msb is one backend for both macOS and Linux, so this Linux-parity criterion no longer applies to the current design.)* ✅ **Verified on real hardware** (GCP VM, nested virt, Intel VT-x): `TestEndToEndRealVM` — the Phase 2 test, body and assertions byte-identical, with only the provider construction swapped — boots the x86_64 image under Firecracker v1.16.1, streams in the image + repo bundle, runs the agent container, and returns a `changes.patch` that `patch.Apply` lands cleanly on a fresh clone (exit 0). Also green: `TestBootHello` (`Hello` round-trips over the vsock handshake), `TestGuestNetwork`, and `TestConcurrentRealVMs` (3 simultaneous VMs, unique taps/CIDs, patches provably not crossed, every tap reaped on teardown).
- [x] **The Phase 3 security suite also re-verified on Linux** (not required by the "Done when", but the claim worth having before anyone runs untrusted code on this backend): `TestEgressEnforcement`, `TestContainerHardening`, `TestRootImageFailsClosed`, `TestGuestGitConfigInjectionInert`, `TestSecretConfinementInArtifacts` — all green against firecracker. The two that matter: a non-allowlisted host is refused by the proxy **and** a raw socket that ignores the proxy is dropped by nftables (`1.1.1.1:443` → timeout), while `setuid(proxyd)` fails `EPERM` — so the finding-#1 egress bypass is closed on this backend too. This required writing `hack/netprobe`, which the spec assumed existed but which had never been committed.

### Phase 8 — Host-side egress proxy, step 1 (`move-egress-proxy-to-host.md`) ✅
Step 1 of the three-step host-side-proxy arc (`docs/ai-tasks/README.md`). Moves the L7
allowlist proxy off the guest entirely, behind a new guest-initiated vsock channel — a
behavior-preserving, security-strictly-improving change for the container (§6.6).

> **Historical — describes the pre-msb design, superseded by Phase 11.** `internal/proxy`,
> `cmd/krayt-vsock-forward`, and the `EgressPort`/`ListenEgress` provider seam this phase built
> are all deleted by `run-tasks-on-microsandbox.md`; msb's own policy engine and userspace network
> stack replace the entire mechanism (§6.6). The record below stands as evidence of what was
> built and verified at the time, not as a description of the code that runs today.

> **Supersedes the Phase 3 "Done when" egress evidence.** `TestEgressEnforcement`,
> `TestContainerHardening`'s `setuid(proxyd)` clause, and `TestConcurrentRealVMs`'s isolation
> claim were all proven against the **in-guest** proxy design. That design no longer exists —
> the evidence is not wrong, it is **obsolete**, and must be re-collected against the new
> host-side design before this phase's own "Done when" is considered met on hardware. See
> `HUMAN_TODO.md`.

- [x] `internal/proxy` — the L7 allowlist handler + resolved-IP SSRF guard, moved out of
  `internal/guest/proxy` (git history preserved via `git mv`), OS-agnostic and
  build-tag-free. `checkDialAddr` no longer takes a `mode` parameter: every private/special
  address range is refused in **every** mode, including `full` — the `mode: full` carve-out
  that used to permit RFC1918/CGNAT/ULA is deleted outright, not widened, because the dialer
  now runs on the host rather than inside a VM (§6.6 §2). The `Factory` and `newHandler`
  test seams are unchanged.
- [x] `internal/guest/proxy` keeps only the simplified, loopback-only, uid-free nftables lock
  (`firewall_linux.go`) and the `Controller` that starts the new guest-side forwarder
  (`controller_linux.go`) as the `proxyd` uid — defense in depth, no longer load-bearing.
- [x] `cmd/krayt-vsock-forward` (new, `//go:build linux`) replaces `cmd/krayt-proxy` (deleted):
  a parse-nothing TCP<->vsock pipe, one vsock connection per accepted TCP connection.
- [x] Provider seam: `provider.EgressPort = 1025` + `VM.ListenEgress`, implemented on vfkit (a
  second `listen=true` virtio-vsock device), Firecracker (`<uds_path>_<port>`, no handshake —
  guest→host needs none), and the fake provider (a real unix-socket listener, not an
  in-memory pipe, so orchestrator tests genuinely exercise fd-passing).
- [x] `krayt __egress-proxy` — a hidden cobra subcommand, self-exec'd by the run supervisor
  (`KRAYT_EGRESS_PROXY_BIN` swap seam), receiving its listener on fd 3 (never a socket path).
  Verified excluded from shell completion (`TestEgressProxyCmdHidden`,
  `internal/cli/complete_test.go`).
- [x] Orchestrator wiring: `ListenEgress` + spawn, between `Create` and `Start`; teardown
  alongside the VM; `proxy.log` written host-redacted from the child's captured
  stdout/stderr — the first host-side redaction path in krayt (§6.8).
- [x] **Done when (offline):** `go build ./...` + `GOOS=linux GOARCH=arm64 go build ./...` +
  `go test -race ./...` + `golangci-lint run` all green, including a real (not mocked)
  `krayt __egress-proxy` child process spawned over the fake provider's fd-3 listener
  (`TestSpawnEgressProxyRealChildProcess`), its teardown (`TestSpawnEgressProxyTeardown`),
  the forwarder's splice/concurrency/teardown behavior
  (`cmd/krayt-vsock-forward/forward_test.go`), and `proxy.log` redaction
  (`TestEgressProxyWriteLogRedactsSecrets`). ✅
- [x] **Done when (hardware, `[HUMAN]`):** the guest image rebuilds with `krayt-vsock-forward`
  in place of `krayt-proxy` (§11 image lockstep — `PinnedRef` cannot be bumped until CI
  publishes the new digest), then the re-verification suite in `HUMAN_TODO.md` passes on
  both backends: allowlisted reach / non-allowlisted block / raw-socket block (now via the
  host proxy), `nft list ruleset` in the guest contains no `skuid` rule, a container attempt
  to reach the **host** on a private address is refused, and `TestConcurrentRealVMs` shows
  two simultaneous VMs each getting their own egress socket + child process with no
  cross-VM reachability. ✅ **Verified on both backends** against image
  `sha256:4fe2b0b78581d5194ded643fbe5b73c5d69372e70955a37ab716d680974f5014` — darwin/vfkit on
  an Apple-Silicon Mac and linux/firecracker under KVM, `hack/run-integration-tests.sh` green
  end to end on each. The ruleset clause is proven by the live `nft list ruleset` dump the
  guest publishes to `/dev/console` and `assertGuestRuleset` reads back: `table inet
  krayt_egress` with `policy drop` + `oif "lo" accept`, and no `skuid` in the whole ruleset
  (NixOS's own input-only `nixos-fw` table included). The private-address refusal is
  `hack/netprobe` check 4 → 403.
  - **One clause holds by construction, not by assertion:** `TestConcurrentRealVMs` sets no
    `NetworkPolicy`, so it never exercises egress. What its 3 concurrent runs do prove is that
    each simultaneous VM gets its own egress socket + child process — `spawnEgressProxy` runs
    unconditionally per run, and a shared or colliding socket would have failed them. Cross-VM
    *unreachability* is structural rather than tested: each VM dials `provider.EgressPort` on
    its own CID and the provider binds a distinct host socket per VM, so a guest has no way to
    name another run's proxy. This is the same reasoning the test already applies to patch
    isolation ("isolation is checked by construction, not by inspection"). An assertion would
    need per-run allowlists in the netprobe; logged here rather than claimed as tested.

### Phase 9 — TLS MITM & credential injection, step 2 (`add-tls-mitm-credential-injection.md`) ✅
Step 2 of the three-step host-side-proxy arc (`docs/ai-tasks/README.md`; depends on Phase 8,
step 1). Opt-in TLS termination at the host proxy so an HTTP-shaped agent credential never
enters the VM at all (§6.6.1, §6.8, §6.14).

> **Historical — describes the pre-msb design, superseded by Phase 11.** The opt-in
> `network.mitm` + host-proxy CA/injection machinery this phase built is deleted by
> `run-tasks-on-microsandbox.md`: under msb, declaring any secret enables TLS interception
> automatically (§6.6.1) — there is no separate opt-in left to test. The record below stands as
> evidence of what was built and verified at the time, not as a description of the code that runs
> today.

> **Complete, offline and on hardware.** Phase 8's gate cleared first (both backends, image
> `4fe2b0b7…`), then this phase was verified with live credentials across two providers: an
> Anthropic run proving a credential can authenticate without ever entering the VM, its
> `mitm: false` mirror proving nothing changes when you don't opt in, and a Gemini run proving
> Node trusts the ephemeral CA — each with the negative control that makes its positive mean
> something. One clause (the `opencode` image's `NODE_EXTRA_CA_CERTS` check) was re-homed to that
> image's own handoff entry; see the "Done when (hardware)" note below.

- [x] `task.NetworkPolicy` extended with `MITM`/`Passthrough`/`Inject` (+ `InjectRule`/
  `RefreshRule`); `task.ValidateNetworkPolicy` enforces every §8.1 pre-flight rule
  (`internal/task/network.go`, `TestValidateNetworkPolicyValid`/`Invalid`).
- [x] `NetworkPolicy.ca_cert` added to `krayt.proto` and regenerated (`make proto-direct`) —
  the only new field on the guest wire protocol; mitm/passthrough/inject stay entirely host-side.
- [x] `internal/proxy`'s ephemeral per-run CA + bounded (1024-entry) SNI leaf cache, ECDSA
  P-256, no exported private-key path (`mitm.go`, `TestCAChainsAndSNI`/`TestCALeafCacheEvicts`/
  `TestCAPrivateKeyNotExported`).
- [x] The MITM CONNECT path (`connect_mitm.go`): hijack → leaf for the CONNECT authority →
  `httputil.ReverseProxy` over the SAME SSRF-guarded transport as the tunnel/forward paths,
  `FlushInterval: -1`, strip-then-set injection, inner-Host/authority mismatch → 400, oversized
  headers → rejected, any MITM setup failure fails closed (never a silent tunnel fallback). The
  plain tunnel (`handler.tunnel`, renamed verbatim from the old `connect`) is untouched and stays
  the fallback for `passthrough`/mitm-off. A `RefreshFunc` seam (401 → one refresh → one retry)
  ships as plumbing only, nil until `inject-claude-oauth-token-at-proxy.md` (step 3) registers one.
- [x] `krayt __egress-proxy` gains `--mitm`/`--run-id`, a `StdinConfig` read from stdin
  (passthrough + resolved inject rules — the only channel secret material crosses into this
  process), and reports its CA's public cert back to the parent once over fd 4, then closes it.
  `internal/orchestrator`'s `spawnEgressProxy` builds and sends that stdin payload (resolving
  each `set` secrets-file key to its value) and reads the fd-4 CA cert back with a bounded wait.
- [x] Secrets partitioning (§6.8's load-bearing change): `pushSecrets` withholds every
  `network.inject[].set`-referenced key from `SecretsBundle`, asserted on the captured proto
  message (`TestSecretsBundleOmitsInjectedKeys`) — not on downstream container behavior.
  `meta.json`/`report.md` record which keys were injected (names only).
- [x] Guest: `NetworkPolicy.ca_cert` → `/run/krayt/ca.crt` (0644) + `KRAYT_CA_CERT`/
  `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`/`NODE_EXTRA_CA_CERTS` in the container env
  (`controller_linux.go`'s `applyCACert`, `TestApplyCACertNoop`/`WritesFileAndEnv`); a no-op when
  `network.mitm` is false. All three reference agent-image entrypoints concatenate their distro
  CA bundle with `$KRAYT_CA_CERT` for `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE` (§8.2) — required, not
  optional, since all three are node-based and Node does not read the system trust store at all.
- [x] **Done when (offline):** `go build ./...` + `GOOS=linux GOARCH=arm64 go build ./...` +
  `go test -race ./...` + `golangci-lint run` all green, including the full offline test list
  from `add-tls-mitm-credential-injection.md` (CA/leaf, child stdin/fd-4 secrecy, `mode: full` +
  `mitm`, injection replace-not-append, passthrough via a real TLS-terminating upstream, the SSRF
  guard on the MITM path, SSE + chunked-NDJSON streaming, secrets partitioning,
  `mitm: false` byte-identical, hostile-input rejection, and every pre-flight validation rule). ✅
- [x] **Done when (hardware, `[HUMAN]`):** a real `claude-code` image run with `mitm: true` +
  `inject:` for `ANTHROPIC_API_KEY` completes a task while `env`/`/run/secrets` inside the
  container hold **no** credential (absence asserted, not just success); the same run with
  `mitm: false` is unchanged; `npm install` (or an equivalent TLS-heavy fetch) succeeds through
  the MITM path in each of the three agent images (the `NODE_EXTRA_CA_CERTS` check — the most
  likely thing to break); the full Phase 3 security suite re-run on both backends. **Mostly run.**
  The central claim is verified on darwin/vfkit with a live credential (run `run_c654e575`): the
  agent authenticated while `/run/secrets` did not exist in the container at all, the env held
  only the entrypoint's placeholder, a `curl` sending no auth header got 200 with a real body,
  and the presented leaf was issued by `krayt ephemeral MITM CA (run_c654e575)` — interception
  and injection both shown directly. The `mitm: false` regression (`run_117d6f75`) mirrors it
  exactly — credential present in `/run/secrets`, CA vars unset, and the same no-auth call
  answered 401 `x-api-key header is required`, which pins the injected run's 200 to the injection
  rather than to any ambient auth. The Phase 3 suite re-ran green on both backends against the
  same image (`4fe2b0b7…`). The `NODE_EXTRA_CA_CERTS` exercise is done for `krayt-agent-gemini-cli`
  (`run_e19488dd`): a real `npm install` through the MITM proxy succeeded with `strict-ssl` on,
  the same install with only `NODE_EXTRA_CA_CERTS` removed failed `SELF_SIGNED_CERT_IN_CHAIN`
  (the negative control that makes the positive mean something), and the registry's presented
  issuer was `krayt ephemeral MITM CA (run_e19488dd)`. `krayt-agent-claude-code` ships no
  `node`/`npm` at all, so it satisfies the "equivalent TLS-heavy fetch" reading of this clause
  (curl through the MITM path, done).
  - **Scope decision, not an oversight:** the third image, `krayt-agent-opencode`, has not had
    this check — it is not published yet. Rather than hold this phase open on an image build that
    involves none of this phase's code, the requirement was **moved into that image's own
    `[tooling]` entry in `HUMAN_TODO.md`** (with the full method: cache-cleared install, the
    `SELF_SIGNED_CERT_IN_CHAIN` negative control, and the issuer check). It is recorded there as
    required rather than optional, because Node reads no system trust store — broken CA plumbing
    in that entrypoint would fail every TLS call it makes under `network.mitm`.

### Phase 10 — Credential shape translation, step 3 (`inject-claude-oauth-token-at-proxy.md`)
Step 3 of the host-side-proxy arc (`docs/ai-tasks/README.md`; depends on Phase 9). Keeps the real
credential out of the VM entirely: with `network.mitm` on, the container is configured with a
non-secret placeholder under the credential's own variable, and the proxy attaches the real value in
the shape the provider wants (§6.14, shape mirroring).

> **Mechanism complete, both vendor shapes observed, implemented, and verified end to end.**
> The plumbing — adapter-produced `InjectRule`s, `task.MergeInjectRules` (user config wins on
> conflict, §8.1), re-running pre-flight validation over the merged set, the non-secret placeholder
> contract, `InjectRule.SetPrefix` for an auth scheme — is generic and fully implemented; it will
> translate any credential shape `internal/adapter/anthropic_wire.go`'s table has an entry for. Both
> Anthropic shapes are now in that table, each from a live observation (`run_c654e575`;
> `run_b408545b` + `run_99bd261c`, 2026-08-17), and the implemented `CLAUDE_CODE_OAUTH_TOKEN` rule
> has an end-to-end hardware run confirming it (`run_df97fffa`, 2026-08-18) — see `HUMAN_TODO.md`.

- [x] `internal/adapter/anthropic_wire.go`: the one file in the repo allowed to encode an
  Anthropic header/endpoint — a declarative `map[string]anthropicWireRule` plus the
  per-shape `Placeholder` values, headed by a dated PROVENANCE comment naming which probe observed
  which entry and recording the 2026-08-18 shape-mirroring decision. Pinned by `TestAnthropicWireRulesGolden`; `internal/proxy` verified to
  contain no vendor identifiers by `TestProxyPackageHasNoAnthropicIdentifiers` (scoped to
  non-test files — internal/proxy's pre-existing tests already used "api.anthropic.com" as
  generic example data before this task, unrelated to it).
- [x] `adapter.Plan` extended with `Inject []task.InjectRule` (reusing step 2's own domain type
  rather than a parallel one — `internal/task` has no dependency on `internal/adapter`, so this
  has no import-cycle cost) and `Placeholders map[string]string`; `adapter.Input` extended with
  `MITM bool`. `claudeCode.Prepare` emits a translation rule + placeholder only when `in.MITM` is
  true AND the selected credential has a table entry; otherwise unchanged
  (`TestApplyAdapterMITMOff`, `TestApplyAdapterNoMITMSubscriptionTokenUnchanged`).
- [x] `task.MergeInjectRules` (§8.1): unions adapter-produced rules into the user's own
  `network.inject`, per-host, header-by-header, user wins on conflict, override logged
  (`TestMergeInjectRules*`, `internal/task/network_test.go`). `krayt run`'s pre-flight now runs
  the adapter first, merges, THEN validates the merged set once — an adapter rule is held to
  exactly the same `ValidateNetworkPolicy` standard a hand-written one is
  (`TestApplyAdapterUnallowlistedHostFailsPreflight`).
- [x] Shape-mirroring proof: `TestMITMShapeTranslationPlaceholderMirrorsTheCredential` asserts every
  observed credential shape produces its placeholder under its OWN env var and configures no other
  credential variable. Written to iterate every *recognized* credential, not just the observed ones,
  so a future table entry is covered for free.
- [x] Container-side contract (§8.2): every agent entrypoint accepts an already-set credential env
  var as satisfying its "a credential is configured" check, keeping `KRAYT_INJECTED_CREDENTIAL` as
  the compatibility fallback. `hack/test-entrypoint-credentials.sh` exercises all three branches
  offline against the real scripts — the seam that let every shape-translated run exit 78 until
  2026-08-18 while every Go test passed.
- [x] **Done when (offline):** `go build ./...` + `GOOS=linux GOARCH=arm64 go build ./...` +
  `go test -race ./...` + `golangci-lint run` all green, including every offline test this task's
  file lists that doesn't require the fallback design (unused — P3 found headers-only differences
  and selected the primary design).
- [x] **Done when (hardware, `[HUMAN]`):** ✅ **2026-08-18, `run_df97fffa`** (with `mitm: false`
  control `run_10fc027d`). A live `CLAUDE_CODE_OAUTH_TOKEN` run under `mitm: true`: `200` on
  `POST /v1/messages`, the real 108-byte token attached host-side (`sent=[…
  authorization=<scheme="Bearer" credential_len=108>]`), the container's own request carrying a
  28-byte placeholder, `/run/secrets` absent entirely, `secret-scan.json` clean, and the
  subscription's `anthropic-ratelimit-unified-*` headers on the response — so the injected token
  bills the subscription, not API credits. The control run, same task and image, found the real
  token in `/run/secrets` as expected, which is what makes the first run's absence mean something.
  Two facts observed for the first time here: Claude Code accepts a **placeholder** on its OAuth
  path (a prefix-less 28-byte string, so it validates credential format on neither path), and it
  **scrubs its credential from child-process environments** — `env` inside the agent cannot show it,
  which is why the entrypoint's own line and the proxy log are the evidence.

### Phase 11 — Microsandbox migration (ADR option B1)

Replaces krayt's own sandbox layer — `internal/{provider,guest,protocol,proxy,vmimage,
controlclient}` and the Nix VM image — with [microsandbox](https://github.com/superradcompany/microsandbox)
(`msb`), driven as a subprocess. Decided in `docs/adr-microsandbox-sandbox-layer.md`; split into
eleven tasks under `docs/ai-tasks/README.md`'s "Microsandbox migration (ADR option B1)" section.
Tasks 2–6 are additive (the vfkit/Firecracker path keeps working byte-for-byte until task 7 flips
the switch and deletes it in the same change), so this phase's checkboxes below track real,
independently-landed progress rather than a single big-bang "Done when".

- [x] **Feasibility gate** (`probe-microsandbox-feasibility.md`) — five hardware-probe scripts
  under `hack/msb-probes/`, **all run on msb 0.6.16, 2026-08-29/30, on an Apple-Silicon Mac**. The
  two design-shaping questions are answered and nothing below is gated on them any more:
  - **P1 — the dial works; the host must not close first.** A non-root guest process (`agent`,
    uid 1000) opens `AF_VSOCK` to host CID 2 in the real agent image unmodified, and msb bridges
    that dial to the host socket **as the invoking user** — `peer uid` is the caller's on every
    accepted connection, against a `0600` socket inside a `0700` directory. `krayt-ask` dials the
    host directly, the root-owned in-guest forwarder is dead (`dial-ask-channel-over-vsock.md`),
    and decision 10's socket hardening stands as written.
    **The reply, though, is dropped unless the host waits for the guest to close.** Measured
    2026-09-02 on msb 0.6.16 (Apple-Silicon Mac), 25 round trips per shape against one sandbox:

    | shape | who closes first | completed |
    |---|---|---|
    | bare `$TMPDIR` socket, as `agent` | host | 7/25 |
    | `0600` in a `0700` dir, as `agent` | host | 5/25 |
    | `0600` in a `0700` dir, as **root** | host | 9/25 |
    | `0600` in a `0700` dir, as `agent` | **guest** | **25/25** |

    Every loss is identical and silent from inside: the host logs the bytes it read *and* the echo
    it wrote, and the guest's read returns EOF having received nothing. It is not a privilege
    problem (root loses too), not the private directory (the bare shape loses at the same rate),
    and not the peer uid. msb's relay discards the reply still in flight when the host end closes.
    §6.13's channel therefore requires the host to wait for the sandbox to close
    (`internal/askbridge.lingerUntilPeerCloses`, covered by
    `TestServeDoesNotCloseBeforeTheSandboxDoes`). `hack/msb-probes/p1-vsock-nonroot.sh` passes or
    fails on that shape — the one krayt ships — and reports the close-first rates beside it
    without failing on them, so a re-run says whether msb still drops replies (expected) or has
    fixed it (which would make the wait belt-and-braces rather than load-bearing).
  - **P2** — `msb exec --user root` works under `--security restricted`: uid 0, and a root-created
    0700 path stays unreadable to an `--user agent` exec. The guest helper takes the restricted
    profile *and* its privilege separation (`add-krayt-guest-helper.md`); it is not a trade-off.
  - **P3** — **answered against expectation**: `--secret` alone *does* enable TLS interception
    (`SandboxBuilder::secret_entry` sets `network.tls.enabled = true`), so the ADR's correction 1
    is withdrawn and `--tls-intercept` emission is explicit-over-implicit rather than required.
    The consequence that follows: **under msb a secret cannot be declared without MITM**, so
    §6.6.1's opt-in `network.mitm: false` has no B1 equivalent once any secret exists.
  - **P4** — inconclusive on darwin *by construction*: a positive control shows `ps -Eww` cannot
    read any same-uid child's environment on macOS, so the reading is uninformative rather than
    negative. msb's source says the value reaches the long-lived runtime on an anonymous
    `--config-fd`, never its environ, making the environ window only krayt's own `msb create`
    call — **a Linux/KVM re-run would confirm that and is the one loose thread here**, though it
    sizes an already-accepted residual and blocks nothing.
  - **P5** — Claude Code accepts msb's default `$MSB_ANTHROPIC_API_KEY` placeholder: `claude -p`
    reached the real API from a deny-default sandbox holding only the placeholder. This is also
    the first end-to-end proof of the B1 credential path on hardware. `hand-secrets-to-msb.md`'s
    `--secret-conf` contingency is dead and must not be built.
- [ ] `add-msb-sandbox-driver.md` — new `internal/sandbox`, the `msb` CLI driver, plus `krayt
  doctor` checks that delegate to `msb doctor`.
- [ ] `translate-network-policy-to-msb.md` — pure-function translation of `krayt.yaml`'s network
  vocabulary into a fully explicit msb policy, never an empty one.
- [ ] `hand-secrets-to-msb.md` — the secret hand-off: `--secret NAME@HOST` on argv (names only),
  values in the msb child's `cmd.Env`, msb's default placeholder.
- [ ] `add-krayt-guest-helper.md` — `cmd/krayt-helper`, stateless, argv-in/JSON-out, run as root
  via `msb exec --user`, a thin wrapper over the `internal/patch` functions krayt keeps.
- [x] `dial-ask-channel-over-vsock.md` — `krayt-ask` dials `AF_VSOCK` to host CID 2 directly over
  msb's `--vsock` route; retires `cmd/krayt-vsock-forward` at the cut-over. Additive:
  `internal/askbridge` + `internal/sockroot` + the `vsock://cid:port` dialer are built and tested,
  nothing calls them from `krayt run` yet. P1's measurement is baked into the channel — the host
  waits for the sandbox to close (§6.13).
- [x] `run-tasks-on-microsandbox.md` — **the cut-over.** `orchestrator.Run` was rewritten from
  scratch to drive the msb lifecycle end to end (§7): `internal/{provider,guest,protocol,proxy,
  controlclient,imagestore}`, `cmd/krayt-{agent,vsock-forward}`,
  `internal/adapter/anthropic_wire.go` (+ golden tests), the `internal/cli/run_{darwin,linux,
  other}.go`/`doctor_{darwin,linux,other}.go` OS-specific splits, `hack/linux-net-setup.sh`, `make
  proto`/the protobuf devShell pins, and the CI `integration-linux` job are all **deleted** in the
  same change. `internal/sandbox`, `internal/askbridge`, `internal/askclient`, `internal/sockroot`,
  `cmd/krayt-helper`, and `internal/task/{netpolicy,secrets,container}_msb.go` — built additively by
  tasks 2–6 — are now actually wired into `krayt run`; `krayt doctor`'s msb checks became mandatory
  (§6.15); the test seam moved from `fakeProvider` to a scriptable fake `msb` binary (this section's
  "Test strategy" paragraph, above, is rewritten accordingly). `internal/vmimage`/`images/`/`krayt
  image` are **knowingly kept** for one more task (`retire-vm-image-pipeline.md`) — they build and
  publish an artifact nothing consumes any more, dead weight rather than a second execution path;
  `images/flake.nix` still references the now-deleted `cmd/krayt-agent`/`cmd/krayt-vsock-forward` in
  its Nix build and will fail to build as-is until that follow-up lands. §3, §5, §6.3–§6.6,
  §6.10–§6.12, §6.13, §7, §8.4, §9.1/§9.2, §10, §12, and §15 are amended accordingly.
  **Done when (offline)**: met — `go build ./...` on darwin and linux, `go test -race ./...`, and
  `golangci-lint run` are green; no `internal/provider`/`internal/guest`/`internal/protocol`/
  `internal/proxy`/`internal/controlclient` import remains anywhere; teardown-on-every-path,
  no-secret-on-argv, and orchestrator coverage are all asserted against the fake `msb` with no
  regression from the pre-cutover suite. **Done when (hardware, `[HUMAN]`)**: met 2026-09-04 on an
  Apple-Silicon Mac (msb 0.6.16, `ghcr.io/418-cloud/krayt-agent-claude-code:latest`, live
  credential). `run_d25279fb` — plain run, `done`, `exit 0`, non-empty `changes.patch`, rendered
  `report.md`. `run_aa23143e` — `--on-question=wait`, reached `waiting` with a real
  `questions/q1.json`, resolved by `krayt answer` over the run control socket, resumed, and
  finished `done` with the edit it had asked about; that is the `ask_human` round trip through
  `krayt-ask` → `AF_VSOCK` → msb's `--vsock` bridge → `internal/askbridge`, plus the cross-process
  `krayt answer` dial. **The pass found five defects the offline suite could not**, each fatal to
  a real run: msb rejecting Go's composite duration on `--max-duration`; `msb copy` not creating
  guest parent directories; `deny@private` shadowing `allow@dns` under first-match-wins (§6.6);
  the ask socket overflowing macOS's `sun_path` limit (§6.13); and `on_timeout: abort` losing a
  race with the agent's own exit. Two of the five were masked by test doubles more forgiving than
  the real `msb`, one by a test that passed only because another bug failed every run — recorded
  in `HUMAN_TODO.md` so the lesson outlives the fixes. Criterion 3 — the credential reaching nothing but `msb
  create` — is met too (`run_63b9a3bf`, `hack/msb-probes/p6-credential-not-in-run.sh`): the guest
  held msb's `$MSB_CLAUDE_CODE_OAUTH_TOKEN` placeholder, and the real value appeared in no msb
  process's argv, no run artifact and not in `changes.patch`. That probe's environ reading is
  inconclusive on darwin — macOS will not show another process's environment even at the same uid,
  the limitation `hack/msb-probes/p4-environ-exposure-window.sh` already records — so the environ
  window closes on the Linux/KVM re-run P4 is already waiting for, not here. `krayt apply` closes the loop: `run_b18ad67e`'s patch
  applied cleanly to the scratch repo's working tree. **Every criterion of this phase's hardware
  re-verification is met; the `HUMAN_TODO.md` entry is deleted accordingly**, and
  `retire-vm-image-pipeline.md` is unblocked.
- [ ] `retire-vm-image-pipeline.md` — deletes `internal/vmimage`, `images/`, the three image
  workflows, and the Linux-builder requirement.
- [ ] `add-msb-extra-conf-escape-hatch.md` — opt-in `sandbox.extra_conf: <path>`, explicitly
  unvalidated, subject to §8.3 containment.
- [ ] `expand-platforms-under-msb.md` — linux/arm64 in the release matrix, plus a real Windows
  port.
- [ ] `warm-start-msb-sandboxes.md` — flat OCI rootfs + `--materialize` pre-pull, opt-in and
  defaulting off until measured.
- **Done when:** the gate's two blocking probes (P1, P2) have a real-hardware finding recorded in
  `HUMAN_TODO.md` and folded back into this checklist (met); each subsequent task's own "Done when"
  is met in turn as it lands (met through task 7's offline half); and task 7
  (`run-tasks-on-microsandbox.md`) — the one that deletes the code Phases 0–10 verified — has its
  own hardware re-verification pass, the same discipline Phase 8 applied when it superseded Phase
  3's egress evidence. **The hardware half of task 7 is the one open item in this phase** — see its
  entry above and `HUMAN_TODO.md`.

---

## 15. Open Questions / Future Work

- **microsandbox as a fourth `Provider` — evaluated and rejected, 2026-08-29.** Forcing msb
  *beneath* krayt's `Provider` interface (§6.3) failed on cgo (msb's Go SDK is a `dlopen` bridge
  to an embedded Rust library — paying cgo would cost `CGO_ENABLED=0` and the single-Linux-runner
  cross-build), on the absence of a host→guest channel matching `VM.DialControl` (msb offers no
  such thing — the sandbox never listens for anything, it only dials out), on a `VMSpec` whose
  kernel/initrd/cmdline fields msb cannot honour (msb has no kernel/rootfs of its own to point at),
  and on running two overlapping security models at once. **This is a narrower question than
  `docs/adr-microsandbox-sandbox-layer.md` asked, and does not mean msb was rejected outright** —
  the ADR asks whether krayt should stop building a sandbox and consume one instead, dissolves
  three of the four objections above by removing the constraint that msb sit under `Provider`
  (cgo is avoidable by driving the `msb` CLI directly rather than its SDK; `DialControl` stops
  mattering because krayt uses msb's own exec/copy API instead of a protocol of its own; there is
  no third VM image because there is no krayt-built VM image at all), and answers **yes** — ADR
  option B1, decided 2026-08-29 and implemented by `run-tasks-on-microsandbox.md`. See the ADR in
  full for the reasoning the four-objection framing above doesn't capture.
- **The `Provider` interface — superseded, 2026-08-29 (ADR option B1).** §3's original design
  principle 1 ("the Provider interface is the only OS-specific seam") and §4's `macOS VM backend`
  (vfkit v1, `Code-Hex/vz` fallback) / `Linux VM backend` (Firecracker) decisions are superseded:
  `internal/sandbox` (§6.15) replaces `internal/provider` entirely, and `internal/provider`,
  `internal/guest`, and `internal/protocol` are deleted by `run-tasks-on-microsandbox.md`. See §3,
  §4, §5, and the §6.3–§6.5/§6.10–§6.12 stubs for the current text.
- **VM boot time / warm-VM pool** on macOS — measure cold-boot latency first; if it hurts UX,
  add an optional **warm-VM pool** that pre-boots and parks idle VMs to amortize boot time.
  Deferred deliberately: it's a boot-time optimization that should be driven by real-world
  measurements, not built speculatively. It shares the detached supervisor's cross-process
  coordination — the `.krayt/` file-lock semaphore (`orchestrator.AcquireSlot`, §6.2) — so a
  pooled VM counts against the same max-concurrency limit as an in-flight run.
- **Container runtime choice** — *resolved:* **containerd** via its Go client (§6.10).
  `runc` vs `crun` left as a build-time toggle; either is acceptable.
- **Image distribution** — *resolved:* **host pulls + pre-loads over vsock** (§6.11). The
  VM never needs registry egress; the host is the only registry-facing component.
- **Dirty-tree fidelity** — *resolved:* non-mutating temp-index capture folds uncommitted
  (non-ignored) changes into the inbound bundle, leaving the user's index/worktree/refs
  untouched (§6.7).
- **Mid-run human input** — *resolved:* async `ask_human` question channel (§6.13), not a
  terminal. Full interactive/attached pairing remains intentionally out of scope.
- **Artifact signing / provenance** — optionally sign run outputs for auditability.
- **Removing the guest NIC entirely in `allowlist`/`none`** — since `move-egress-proxy-to-host.md`,
  the VM no longer needs one in those modes (no DNS, no registry egress, no bundle egress), which
  would additionally let Linux drop `setcap cap_net_admin+ep` (`hack/linux-net-setup.sh`) and the
  tap+masquerade setup from `krayt doctor`. Not done: Firecracker's `allocSlot` currently bundles
  tap + `/30` + CID into one allocation, and `full` mode still needs the NIC, so making it
  conditional is its own task.
- **Unifying `full` mode onto the host proxy path** — today `full` still opens the guest NIC
  directly (nftables table deleted outright) rather than routing through the host proxy at HTTP
  granularity. Doing so would change `full`'s meaning from "any protocol, unfiltered" to "HTTP(S)
  only, unfiltered by host string" — a real semantic change deserving its own task, not folded into
  `move-egress-proxy-to-host.md`.
- **A named-forward-target mechanism for host/LAN services** — `move-egress-proxy-to-host.md`
  hard-blocks loopback/private/LAN ranges from the egress proxy in every mode (§6.6 §2), so a
  local Ollama/LM Studio on `127.0.0.1:11434` or a LAN package mirror is no longer reachable from
  the sandbox at all. If wanted, that needs a purpose-built, explicitly-named forward target (not
  a blanket range unblock, which would let the sandbox reach the user's entire LAN).
- **Host-side proxy steps 2 and 3** (`add-tls-mitm-credential-injection.md`,
  `inject-claude-oauth-token-at-proxy.md`) — TLS MITM + host-side credential injection, then
  OAuth-token support specifically for Claude Code. Both strictly depend on step 1
  (`move-egress-proxy-to-host.md`) landing and passing its hardware re-verification first.

---

## 16. Glossary

- **vsock** — virtio sockets; host↔guest comms channel that works under both
  Virtualization.framework (macOS) and KVM/Firecracker (Linux).
- **vfkit** — `crc-org/vfkit`, a macOS CLI/REST hypervisor over Virtualization.framework
  (itself built on `Code-Hex/vz`), used by podman/minikube/crc. krayt's v1 macOS provider
  drives it as a subprocess; the entitlement lives on vfkit, not krayt.
- **CoW rootfs** — copy-on-write clone of the base VM disk so each run is isolated and disposable.
- **Egress proxy** — in-guest forward proxy enforcing the per-task domain allowlist.
- **`ask_human` / question channel** — optional async path for the agent to pause and ask
  the human a question mid-run (§6.13), exposed as an MCP tool + `krayt-ask` CLI over an
  agent-agnostic channel; gated by `--on-question=wait`.
- **git bundle** — a single file packaging real git objects + refs; krayt ships the repo
  into the VM as a bundle and clones a real repository from it (§6.7).
- **Self-contained vs. range bundle** — a *self-contained* bundle has no prerequisites and
  clones into an empty repo (used host→guest, produced as a parentless snapshot or a full-history
  clone); a *range* bundle (`<base>..HEAD`) records prerequisites and only unbundles where the base
  already exists (used guest→host for the optional commits bundle) (§6.7).
- **Baseline (`krayt-baseline`)** — the imported HEAD of the cloned bundle, recorded and
  tagged in the guest before the agent runs; the agent's changes are diffed against it to
  produce the patch (§6.7). No synthetic commit is fabricated.
- **Adapter** — optional orchestration glue that knows how to invoke a specific AI CLI;
  not required thanks to the convention-based contract.
- **`ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN`** — the two credential shapes Claude Code
  accepts, both delivered through the per-task secrets file (§6.8, §6.14). The API key is a
  scoped, pay-per-token Console key; `CLAUDE_CODE_OAUTH_TOKEN` is a ~1-year, inference-scoped
  subscription token minted by **`claude setup-token`** on a browser machine. The adapter
  enforces exactly one of them; krayt defaults to the API key for untrusted/concurrent runs.
