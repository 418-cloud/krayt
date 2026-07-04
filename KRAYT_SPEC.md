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

1. **The Provider interface is the only OS-specific seam.** Everything above it
   (orchestration, protocol, patch logic, secrets, CLI) is OS-agnostic Go. The guest
   agent runs inside Linux on both platforms, so it is shared too.
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
| macOS VM backend | **vfkit** (`crc-org/vfkit`) for v1 — drives Virtualization.framework via a tested, pre-signed subprocess; direct `Code-Hex/vz` embedding is the documented swap-in fallback, both behind the `Provider` seam (§6.3) |
| Linux VM backend (future) | Firecracker or Cloud Hypervisor via the same `Provider` interface |
| Tool ↔ agent | Convention-first contract + optional orchestration adapters |
| Networking | Per-task policy; **default allowlist** enforced by an in-guest egress proxy |
| Interaction | **Headless default**, attachable live log streaming |
| Concurrency | **Multiple concurrent** agent VMs |
| Output | `git diff` patch only; **manual apply** on host |
| Secrets | Per-task **secrets file**, transferred over the control channel, never persisted to VM disk |
| Task definition | **CLI flags + optional config file** (flags override file) |
| Resource limits | Sensible defaults (e.g. 2 vCPU / 4 GB / 20 GB / 30 min), **fully configurable** |
| Agent → human questions | Optional async `ask_human` via an MCP server + `krayt-ask` CLI over an agnostic question channel; **default `fail`** (autonomous), opt into `wait`; timeout → sentinel by default (§6.13) |
| Agent authentication | Credential injected via the per-task secrets file (§6.8); scoped **API key** is the default, `CLAUDE_CODE_OAUTH_TOKEN` for subscription auth; the per-agent adapter enforces **exactly-one** credential; API key recommended for untrusted/concurrent runs (§6.14) |

---

## 5. High-Level Architecture

```
┌──────────────────────────────── HOST (macOS / Linux) ────────────────────────────────┐
│                                                                                        │
│   krayt CLI                                                                         │
│        │                                                                               │
│        ▼                                                                               │
│   Orchestrator ──────────── manages N concurrent Runs, IDs, state, cleanup            │
│        │                                                                               │
│        ▼                                                                               │
│   Provider (interface)                                                                 │
│     ├── vfkit provider       (macOS, Virtualization.framework)   ← v1                 │
│     ├── vz provider          (macOS, direct Code-Hex/vz)         ← fallback           │
│     └── firecracker provider (Linux, KVM)                        ← later              │
│        │                                                                               │
│        │  boots                                                                        │
│        ▼                                                                               │
│   ┌──────────────── MICRO-VM (minimal Linux) ─────────────────┐                        │
│   │                                                            │                        │
│   │   guest-agent (Go, static linux binary)                   │                        │
│   │     ├── vsock control server  ◄──── host control channel ─┼── bundle in / logs+patch out
│   │     ├── egress proxy (allowlist) + default-deny firewall  │                        │
│   │     └── containerd (Go client) + egress proxy + nftables       │                        │
│   │            │                                              │                        │
│   │            ▼                                              │                        │
│   │   ┌──────── USER OCI IMAGE ───────────┐                   │                        │
│   │   │  AI agent (claude code / gemini)  │                   │                        │
│   │   │  + tools                          │                   │                        │
│   │   │  /workspace   ← repo snapshot     │                   │                        │
│   │   │  /task/prompt.md ← the task       │                   │                        │
│   │   │  /run/secrets/*  ← tmpfs secrets  │                   │                        │
│   │   │  /output/*    ← patch + report    │                   │                        │
│   │   └───────────────────────────────────┘                   │                        │
│   └────────────────────────────────────────────────────────────┘                      │
└────────────────────────────────────────────────────────────────────────────────────────┘
```

The **control channel** is `vsock` (virtio sockets) — supported by Virtualization.framework
on macOS and by KVM/Firecracker on Linux — so the same protocol works on both platforms.

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
    BundleDepth  int               // forward-bundle shallow depth (§6.7); default 1, 0 = full history
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

### 6.2 Orchestrator (`internal/orchestrator`)
- Owns the set of active runs (map keyed by run ID).
- Allocates a unique run ID per VM (and a vsock CID on the Firecracker backend only; §6.12).
- Enforces optional max-concurrency and per-run resource budgets.
- Drives the run lifecycle (§7) and guarantees VM teardown even on error/panic/signal.
- Tracks run state including a **`waiting`** state when the agent has asked a question and
  `mode: wait` is set (§6.13); waiting runs still own a live VM and count against concurrency.
- Persists run metadata + artifacts under the project's `.krayt/runs/<id>/` (§8.4).

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
The single OS-specific seam.

```go
type VMSpec struct {
    ID        string
    Kernel    string // path to vmlinuz (or EFI image)
    RootFS    string // path to the BASE rootfs image; provider makes a CoW clone per run
    CID       uint32 // vsock guest CID — Firecracker only; ignored by the vz provider (see §6.12)
    CPUs      int
    MemoryMiB uint64
    DiskGiB   uint64
}

type Provider interface {
    Create(ctx context.Context, spec VMSpec) (VM, error)
}

type VM interface {
    Start(ctx context.Context) error
    // DialControl opens the control channel to the guest-agent (guest listens, host
    // connects). On vz this goes through the per-VM VZVirtioSocketDevice; on Firecracker
    // it is an AF_VSOCK connect to the guest CID. Returns a net.Conn usable as a gRPC
    // transport (see §6.12). `port` is the guest vsock port (fixed; see §6.12).
    DialControl(ctx context.Context, port uint32) (net.Conn, error)
    Stop(ctx context.Context) error
    Destroy(ctx context.Context) error // also removes the CoW clone
    ID() string
}
```

- **`internal/provider/vfkit`** (v1): drives `crc-org/vfkit`, which itself wraps
  `Code-Hex/vz/v3`. `Create` builds the VM config via vfkit's `pkg/config` Go API and
  launches the signed vfkit binary as a subprocess; lifecycle is controlled over vfkit's
  REST API (unix socket). vfkit is used in production by podman/minikube/crc.
  - **CoW clone:** `Create` clones the base **raw** rootfs image with APFS `clonefile(2)`
    (vfkit needs raw/ISO, not qcow2); vfkit boots from the clone via its Linux bootloader
    (kernel + initrd + rootfs) or EFI. `Destroy` kills the vfkit process and deletes the clone.
  - **vsock:** vfkit exposes the guest vsock port as a **host unix socket**
    (`--device virtio-vsock,port=1024,socketURL=…`); `DialControl` is a plain unix-socket
    dial (see §6.12). No `CID` needed.
  - **Signing:** the entitlement lives on the vfkit binary, not krayt — see §12.
- **`internal/provider/vz`** (fallback, not built in v1): embeds `Code-Hex/vz/v3` directly
  in-process for a zero-runtime-dependency, fully-controllable path. Swap target if vfkit's
  API ever becomes a control ceiling. Same `Provider`/`VM` interface — no other code changes.
- **`internal/provider/firecracker`** (later): same interface over `firecracker-go-sdk`;
  here `CID` is meaningful and the host connects via `AF_VSOCK` to that CID.

> Everything outside this package is platform-agnostic.

### 6.4 Guest agent (`internal/guest`, built for `linux/arm64` + `linux/amd64`)
A small static Go binary baked into the VM rootfs and run as a **systemd service**
(`Type=notify`, ordered `After=containerd.service` and the network target). The VM uses
NixOS + systemd (see §11.1/§11.6); systemd owns init, mounts, and service ordering, so the
guest-agent stays a plain service rather than a hand-rolled PID 1.
Responsibilities:
- Run the **gRPC control server** on a fixed vsock port (§6.5, §6.12).
- Receive the **image archive**, **repo bundle**, **task**, **secrets**, and **network policy**.
- Bring up the **egress proxy + nftables firewall** (default-deny except the proxy; §6.6).
- Drive **containerd** (via its native Go client) to import + run the user's OCI image as a
  single container with the right mounts/env (see §6.10).
- **Stream container logs** back over the control channel.
- On container exit, **generate the patch** (§6.7) and **stream the artifact bundle** back.
- Signal completion / exit code, then idle for teardown.

### 6.5 Control protocol (`internal/protocol`, shared host+guest)
**Decision: gRPC over vsock.** Typed messages + first-class streaming for logs, tar, and
the image archive. The **guest is the gRPC server** (listens on a fixed vsock port); the
**host is the client** (connects through the provider's `DialControl`, see §6.12). The
`.proto` is the single source of truth, compiled to Go for both sides.

```proto
syntax = "proto3";
package krayt.v1;
option go_package = "github.com/<you>/krayt/internal/protocol/pb";

service GuestAgent {
  // Handshake + version negotiation.
  rpc Hello(HelloRequest) returns (HelloResponse);

  // Incremental image transfer (§6.11): host asks which blobs the guest already has,
  // then streams only the missing ones.
  rpc QueryImageBlobs(BlobQuery) returns (BlobPresence);
  rpc PushImage(stream Chunk) returns (Ack);        // OCI archive, client-streaming

  rpc PushCode(stream Chunk) returns (Ack);         // git bundle stream, client-streaming (§6.7)
  rpc PushTask(TaskSpec) returns (Ack);
  rpc PushSecrets(SecretsBundle) returns (Ack);     // held in memory only (§6.8)
  rpc SetNetworkPolicy(NetworkPolicy) returns (Ack);

  // Start the container and stream events until it exits. The final RunEvent carries
  // the terminal Status (exit code); the stream then closes.
  rpc Start(StartRequest) returns (stream RunEvent);

  rpc CollectArtifacts(CollectRequest) returns (stream Chunk); // patch+report tar (+ optional commits.bundle, §6.7)
  rpc Answer(AnswerRequest) returns (Ack);          // host answers an agent question (§6.13)
  rpc Shutdown(ShutdownRequest) returns (Ack);
}

message HelloRequest  { string client_version = 1; }
message HelloResponse { string agent_version = 1; string containerd_version = 2; }

message BlobQuery     { repeated string digests = 1; } // sha256: of image layers/config
message BlobPresence  { repeated string missing_digests = 1; }

message Chunk { bytes data = 1; string digest = 2; bool last = 3; } // digest set on blob/stream boundaries

message TaskSpec      { bytes prompt = 1; map<string,string> env = 2; } // env = non-secret
message SecretsBundle { map<string,string> values = 1; }               // tmpfs at /run/secrets
message NetworkPolicy { enum Mode { ALLOWLIST = 0; FULL = 1; NONE = 2; }
                        Mode mode = 1; repeated string allow = 2; }

message StartRequest  { string image_ref = 1; uint32 timeout_secs = 2; }

message RunEvent {
  oneof kind {
    LogLine  log = 1;
    Status   status = 2;          // terminal; last message on the stream
    Question question = 3;        // agent paused to ask the human (§6.13); not terminal
  }
}
message LogLine { enum Stream { STDOUT = 0; STDERR = 1; } Stream stream = 1; bytes line = 2; int64 ts_unix_ms = 3; }
message Status  { int32 exit_code = 1; bool timed_out = 2; string error = 3; }

// Agent → human question (§6.13). Pushed on the Start stream; host replies via Answer().
message Question      { string id = 1; string prompt = 2; repeated string choices = 3; uint32 timeout_secs = 4; }
message AnswerRequest { string question_id = 1; string response = 2; bool no_answer = 3; } // no_answer = timeout/declined

message CollectRequest  {}
message Ack             { bool ok = 1; string error = 2; }
message ShutdownRequest {}
```

Notes for implementers:
- `Chunk` is the shared streaming primitive (image, code, artifacts). Keep chunk size
  ~1–4 MiB. Never buffer a whole stream in memory on either side.
- `Start` is the spine: one server-stream that multiplexes log lines and ends with a
  single `Status`. The host writes logs to disk and (if attached) to the terminal.
- All secret material lives only in `SecretsBundle` → guest memory → container tmpfs;
  it is never written to the RunEvent stream or any artifact.

### 6.6 Networking & egress proxy (`internal/guest/proxy`)
The container runs in the **VM's own network namespace** (no CNI bridge) — there is one
container per VM, so the VM boundary *is* the network boundary and host-networking-in-VM
is the simplest correct choice. Enforcement layers:

- **VM interface:** one NAT interface (vz NAT device / Firecracker tap), brought up by
  the NixOS network config.
- **L7 allowlist:** a small **HTTP/HTTPS CONNECT forward proxy** (hand-rolled, or
  `elazarl/goproxy`) runs in the guest as a dedicated uid (e.g. `proxyd`). It checks the
  `CONNECT` host and plain-HTTP `Host` against the per-task allowlist.
- **L3 enforcement (the real lock):** nftables makes the proxy *unbypassable* by dropping
  all egress except loopback and the proxy's own uid:

  ```
  table inet egress {
    chain output {
      type filter hook output priority 0; policy drop;
      oif "lo" accept
      meta skuid "proxyd" accept          # only the proxy may leave the box
      ct state established,related accept
    }
  }
  ```
  Because the container does *not* run as `proxyd`, its only path out is via the proxy
  (set through `HTTP_PROXY`/`HTTPS_PROXY`); direct sockets are dropped. This closes the
  raw-socket bypass that a pure proxy-env approach would leave open.
- **Container env:** launched with `HTTP_PROXY` / `HTTPS_PROXY` pointing at the proxy and
  `NO_PROXY=localhost,127.0.0.1` (the lowercase `http_proxy` / `https_proxy` / `no_proxy`
  forms are set too, for tools that only read those).
- **DNS:** the proxy resolves names itself, dialing a fixed resolver (`1.1.1.1:53` by
  default; overridable via `krayt-proxy --dns`) **as `proxyd`**, so the lookup is permitted by
  the L3 lock. The container does no DNS of its own — its direct egress, port 53 included, is
  dropped. A system stub resolver (e.g. `systemd-resolved`) is deliberately bypassed: it runs
  as a different uid and its upstream query would be dropped by the `skuid "proxyd"` rule.
- **Policy modes:** `allowlist` (default) — the proxy permits only the hosts the task lists
  (`--allow` / `network.allow`); with none listed it is **deny-all**, so a task that needs the
  AI endpoints (`api.anthropic.com`, `generativelanguage.googleapis.com`) or a package
  registry must allow them explicitly — krayt does **not** auto-seed them. `full` — nftables
  policy switched to accept (explicit opt-in); `none` — proxy denies everything (usable
  because image acquisition is off the VM net path, §6.11). The agent's **auth/refresh**
  endpoints must be allowlisted alongside the inference endpoint (§6.14); an
  OAuth/`apiKeyHelper` refresh flow may touch more hosts than a static API key, so it can need
  a wider list.
- **Isolated as a swappable, memory-safety-critical component.** The proxy is a **standalone
  in-guest process** (`krayt-proxy`, run as its own `proxyd` uid) — a separate binary, not
  linked into the guest-agent — sitting behind a stable contract on both ends: the guest-agent
  selects it through the `Factory` seam (`internal/guest/proxy`) and drives it purely by
  process interface (fixed flags in — `--listen` / `--mode` / `--allow` / `--dns`; the proxy
  env out; the nftables `skuid` lock around it). Nothing else in krayt depends on *how* it is
  implemented. Because it is also the component most directly exposed to **untrusted,
  adversarial network input**, that isolation makes it the natural candidate for a future
  **memory-safe reimplementation** (e.g. Rust/Zig): drop in a binary that honors the same
  flags + env + uid contract and neither the agnostic core nor the guest-agent changes.

### 6.7 Code transfer & patch generation (`internal/patch`)
The repo enters the VM as a **git bundle** — a single self-contained byte stream carrying
real git objects — and is **cloned** into `/workspace` as a real repository. Unlike a flat
`git archive` snapshot, a bundle gives the guest a genuine HEAD and history, so there is **no
synthetic baseline commit**: the baseline is simply the imported HEAD. This yields cleaner
3-way patch application (a real merge-base) and lets multi-commit agent output survive the
round-trip. A bundle also preserves git's object model exactly — file modes, executable bits,
symlinks — which the tar/`git archive` path did not guarantee.

**The forward bundle must be self-contained (host → guest).** The guest clones into an
*empty* VM, so the inbound bundle must carry **no prerequisites**. A range bundle (e.g.
`HEAD~1..HEAD`) records prerequisite commit IDs in its header, and a clone into an empty repo
then fails with a "does not have … prerequisite commits" error — so the forward direction
**must not** use a range. `git bundle create` has **no `--depth` flag**, so there is no native
way to emit a self-contained shallow slice in one step. To stay lean *and* self-contained,
krayt **shallow-clones-then-bundles** on the host *(verify current)*:

```
git clone --depth <bundle_depth> file://$REPO $TMP/src         # shallow working copy
git -C $TMP/src bundle create $TMP/repo.bundle HEAD <branch>   # inherits the shallow boundary
```

The bundle inherits the shallow clone's boundary, so it has no prerequisites and clones
cleanly into the empty guest (it must name at least one ref so `git clone` has something to
check out). `bundle_depth` is configurable (§6.1/§8.1), default shallow (`1`); raise it — or
take full history — when the agent needs deeper context.

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
DIRTY=$(git commit-tree $TREE -p HEAD -m "krayt: dirty worktree")   # drop -p if unborn HEAD
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

**Integrity.** The bundle is a single artifact, hashable and checkable (`git bundle verify`
plus a digest), consistent with the digest discipline already used for the OCI artifact
(§6.11) and secrets (§6.8).

**Known limitations (v1):**
- **git-LFS:** a bundle carries LFS *pointer* files, not the large objects (which live on an
  LFS server). LFS-tracked content is therefore **not** transferred; fetching it would need
  network egress to the LFS endpoint, conflicting with the isolation model. Out of scope for v1.
- **Submodules:** a superproject bundle includes the gitlink but **not** submodule contents,
  so repos with submodules won't have submodule working trees in the guest. Out of scope for v1.

### 6.8 Secrets (`internal/secrets`)
- Read from a **per-task secrets file** (e.g. `secrets.env` or `secrets.yaml`).
- Transferred over the encrypted-by-isolation vsock channel.
- Mounted in the container on **tmpfs** at `/run/secrets/` (and/or injected as env).
- **Never** written to the VM's persistent disk image; **never** logged (redacted in logs).
- Destroyed with the VM.

Agent model-provider credentials (e.g. Claude Code's `ANTHROPIC_API_KEY` or
`CLAUDE_CODE_OAUTH_TOKEN`) ride this same mechanism — see agent authentication (§6.14) for
how a credential maps to the right env var and the exactly-one rule the adapter enforces.

### 6.9 Logging & streaming (`internal/orchestrator` + guest)
- Container stdout/stderr → guest → vsock `Logs` stream → host.
- Headless default: logs persisted to `.krayt/runs/<id>/logs/`.
- `krayt attach <id>` tails the live stream; `krayt logs <id>` reads persisted logs.

### 6.10 Container runtime — containerd (`internal/guest/runner`)
The guest runs the user's OCI image with **containerd**, driven from the Go guest-agent
via containerd's **native Go client** over its local gRPC socket.

- **Why containerd over podman here:** the guest-agent is a Go program controlling the
  runtime programmatically, one container per VM, with no human at a CLI. containerd is
  designed to be embedded/driven by another program and exposes a typed Go client for
  pull/import, create, start, stdio attach, wait, and delete. Podman's strengths
  (Docker-CLI compatibility, first-class rootless) don't apply: there is no human CLI,
  and the **VM is already the isolation boundary**, so rootless-in-VM is not a
  differentiator. Driving podman over an API would also require running
  `podman system service` — reintroducing a daemon and negating its daemonless selling
  point.
- **Image loading:** prefer importing the image as an **OCI archive into containerd's
  content store** (matches the "pre-load over vsock, no registry egress" model in §15).
  Falls back to a registry pull only if the network policy allows it.
- **Single-container model:** exactly one container per VM. **No Docker socket is
  exposed** and docker-in-docker is unsupported (see Non-Goals §2).
- **Low-level OCI runtime:** `runc` (default) or `crun` (lighter, faster start) — either
  is acceptable; selectable in the Nix image. Startup difference is not significant here.
- **Mounts/env per the container contract (§8.2):** `/workspace`, `/task/prompt.md`,
  `/run/secrets/*` (tmpfs), `/output/`, plus `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` for
  the egress proxy.

### 6.11 Image acquisition — host pull + vsock pre-load (`internal/imagestore`)
The **host** is the only component that touches a registry. The user's image is
acquired on the host and streamed into the VM over the same vsock control channel used
for code and task — the VM itself never needs registry egress.

Flow:
1. **Resolve + pull (host):** the host resolves the user's image (tag or digest) and
   pulls it into a local OCI store, reusing the same OCI plumbing as the base VM image
   (`oras-go` / a containerd content store on the host).
2. **Export (host):** export the image as a standard **OCI archive** (`oci-layout` tar).
3. **Stream (vsock):** send it as another protocol message — `PushImage{oci archive
   stream}` (§6.5) — structurally identical to `PushCode`, just larger. Streamed, never
   fully buffered in RAM on either side.
4. **Import + run (guest):** the guest imports the archive into containerd's content
   store via `client.Import(...)`, then creates and runs the container (§6.10).

Key properties:
- **Digest-keyed host cache:** the exported archive is cached on the host keyed by image
  **digest**. Repeat runs of the same image skip pull + export entirely.
- **Incremental transfer:** because OCI layers are content-addressed, the host streams
  only the blobs the guest's content store is missing — important when spinning up many
  ephemeral VMs, otherwise each run pays a multi-GB vsock copy.
- **Integrity for free:** containerd's content store verifies blob digests on `Import`,
  giving the same digest-verification guarantee the base image already has.
- **Network consequence:** image acquisition is fully off the VM's network path, so the
  per-task network policy governs **only** the agent's runtime traffic. `mode: none`
  becomes genuinely usable for tasks needing no runtime network, and there is no "VM
  needs registry egress just to start" caveat anywhere.

### 6.12 vsock transport & gRPC wiring (the host/guest asymmetry)
This is the subtlest cross-platform detail and the easiest to get wrong. vsock is **not**
symmetric across the two backends, so the `Provider` hides the difference behind
`DialControl` (§6.3) and everything above it speaks plain gRPC.

- **Guest side (identical on both backends):** the guest-agent listens on a **fixed vsock
  port** (e.g. `1024`) using `github.com/mdlayher/vsock` — `vsock.Listen(1024, nil)`
  returns a `net.Listener`, which is handed straight to `grpc.NewServer().Serve(lis)`.
- **Host side — vfkit (macOS, v1):** there is **no `AF_VSOCK` on a macOS host**, so vfkit
  bridges the guest vsock port to a **host unix socket** (started with
  `--device virtio-vsock,port=1024,socketURL=/…/ctrl.sock`). `Provider.DialControl` is then
  a plain `net.Dial("unix", socketURL)`, and the gRPC client uses it via
  `grpc.WithContextDialer(...)` + `grpc.WithTransportCredentials(insecure.NewCredentials())`
  (the link is isolated to this VM). This is simpler than the direct-vz path below.
- **Host side — direct vz (macOS fallback):** if embedding `Code-Hex/vz/v3`, the host
  connects through the per-VM `VZVirtioSocketDevice` (`device.Connect(1024)` → `net.Conn`)
  instead of a unix socket. Same `DialControl` contract, different innards.
- **Host side — Firecracker (Linux):** the host *does* use `AF_VSOCK`, connecting to the
  guest `CID` from `VMSpec`. Same `DialControl` signature again.
- **Why no CID management on macOS:** with vfkit each VM has its own `socketURL`, and with
  direct vz each VM owns its own `VZVirtioSocketDevice` — either way there is no shared CID
  namespace to allocate. The `CID` field in `VMSpec` is Firecracker-only.
- **Security note:** the channel needs no TLS — a vsock link reaches exactly one VM and is
  not on any network. `insecure` transport credentials are correct here, not a shortcut.

### 6.13 Agent → human questions (`ask_human`)
An **optional, asynchronous** way for the agent to pause and ask the human a question, get
an answer, and continue — without a terminal or attach session. Off by default, so batch
stays batch; enabled per run. The design keeps the agnostic core intact and puts the
agent-specific part in the adapter.

**Three layers:**
- **Question channel (agnostic core — `internal/guest/ask` + host):** the stable contract.
  A small in-VM bridge accepts a question from inside the container (over a local unix
  socket to the guest-agent), the guest-agent pushes it to the host as a
  `RunEvent.Question` on the `Start` stream (§6.5), blocks until the host calls
  `Answer(question_id, response)` (or the timeout fires), then returns the answer into the
  container. Independent of which agent is running.
- **Two front-ends onto the channel:**
  - **`ask_human` MCP server:** a tiny MCP server krayt runs inside the VM exposing one
    tool — `ask_human{ question, choices?, context? }` — bridged to the question channel.
    Idiomatic for MCP-speaking agents; the tool *description* steers *when* to ask
    ("only when genuinely blocked on a decision a human must make"). This is the premium path.
  - **`krayt-ask` CLI:** a small binary in the base image, mounted into the container, that
    any agent can shell out to (`krayt-ask [--choices a,b] "question"` → answer on stdout).
    Universal lowest-common-denominator fallback. Same channel underneath.
- **Registration (per-agent adapter):** wiring the agent's config to the MCP server is
  agent-specific (Claude Code et al. each configure MCP differently), so it lives in the
  optional adapter — **not** the agnostic core. The adapter wires the CLI **only when
  `--on-question=wait`** (Phase 5); MCP-server registration lands with the MCP server itself
  (Phase 6).

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

Either way the user lists one credential in the secrets file, krayt streams it in
`SecretsBundle` (§6.5), and the adapter exports it into the container environment. No core
code knows it is an auth credential rather than any other secret.

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
- **Headless billing.** Reports suggest `claude -p` with a subscription OAuth token may still
  draw API credits in some versions; confirm before assuming a sandboxed run is covered by
  the plan *(verify current)*. Relatedly, Bare mode (`--bare`) does not read
  `CLAUDE_CODE_OAUTH_TOKEN` at all — a bare-mode invocation must use `ANTHROPIC_API_KEY` or an
  `apiKeyHelper`.
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

---

## 7. Run Lifecycle (Step by Step)

1. **Resolve spec** — merge flags + config file into a `RunSpec` (image, task, repo,
   network policy, secrets file, resources, env).
2. **Bundle code** — create a self-contained git bundle (shallow-clone-then-bundle at
   `bundle_depth`; non-mutating temp-index capture if `include_dirty`) → byte stream (§6.7).
3. **Acquire image (host)** — resolve + pull the user's OCI image into the host store and
   export an OCI archive; reuse the digest-keyed cache to skip if already present (§6.11).
4. **Provision VM** — `Provider.Create` makes a CoW copy of the base rootfs, assigns a CID;
   `VM.Start` boots it.
5. **Connect** — host dials the guest-agent over vsock; handshake.
6. **Push inputs** — image archive (incremental: only missing blobs), code bundle, task,
   secrets, network policy.
7. **Start** — guest imports the image into containerd, brings up firewall+proxy, runs the container with mounts/env.
8. **Stream** — logs flow to host (and disk). Wall-clock timeout armed.
9. **Complete** — container exits (or timeout kills it). Guest diffs against the recorded
   `krayt-baseline` for `changes.patch` (+ optional `commits.bundle`) and writes the report (§6.7).
10. **Collect** — host pulls the artifact bundle → `.krayt/runs/<id>/`
    (`changes.patch`, `report.md`, `logs/`, `meta.json`).
11. **Destroy** — `VM.Destroy` tears down the VM and deletes the CoW disk. Guaranteed via defer/signal handling.
12. **Review & apply** — human inspects the patch; `git apply` if satisfied.

---

## 8. Configuration

### 8.1 Task config file (`krayt.yaml` — optional)
```yaml
image: my-agent:latest          # required (flag or file)
task: ./task.md                 # path to task prompt (or inline `task_text:`)
repo: .                         # repo to bundle (default: cwd)
include_dirty: true             # include uncommitted changes (non-mutating capture, §6.7)
bundle_depth: 1                 # forward-bundle shallow depth; 0 = full history (§6.7)

network:
  mode: allowlist               # allowlist | full | none
  allow:
    - api.anthropic.com
    - generativelanguage.googleapis.com
    - registry.npmjs.org

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
  adapter: none                 # none | claude-code | gemini-cli
```

### 8.2 Container contract (convention)
Injected by the tool, regardless of adapter:
- `/workspace` — the repo snapshot (agent's working dir).
- `/task/prompt.md` — the task description.
- `/run/secrets/*` — secrets (tmpfs), **including any agent auth credential** (e.g.
  `ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN`); the adapter exports it into the
  environment from there (§6.14).
- `/output/` — agent/guest writes `changes.patch` + `report.md` here (or guest generates the patch).
- `/usr/local/bin/krayt-ask` — the `krayt-ask` CLI front-end (§6.13), bind-mounted on the PATH so
  any agent can shell out to it; `/run/krayt/ask.sock` is the bridge it connects to.

Because the container runs **non-root** (below), the tool makes these usable by any non-root uid:
`/run/secrets` is world-readable, `/workspace` and `/output` are writable, and the ask socket is
connectable (§8.2 was root-only before Phase 5 — fixed in the guest).

Run the container as a **non-root** uid: some agents (Claude Code among them) refuse to run
as uid 0, and any non-root uid satisfies them *(verify current)*.

Completion = container process exit. Exit code is surfaced in `meta.json`.

### 8.3 Flag/file precedence
CLI flags override config file values, which override built-in defaults.

### 8.4 Run output artifacts (`.krayt/runs/<id>/`)
Every run produces a self-contained directory the human reviews from:

```
.krayt/runs/<id>/
├── changes.patch     # git diff vs the recorded krayt-baseline (primary deliverable; §6.7)
├── commits.bundle    # optional: reverse range bundle of the agent's commits (§6.7), if returned
├── report.md         # human-readable summary (see below)
├── meta.json         # machine-readable run record (schema below)
├── questions/        # one <qid>.json per agent question + its answer (§6.13), if any
└── logs/
    ├── agent.log     # container stdout/stderr (merged, timestamped)
    └── events.jsonl  # one JSON object per RunEvent (optional, for tooling)
```

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
  "questions": [
    { "id": "q1", "prompt": "Target Postgres or SQLite?", "answer": "postgres", "answered_by": "human", "waited_secs": 35 }
  ],
  "error": ""
}
```

`report.md` — a short, fixed-section human summary (the guest may also write its own to
`/output/report.md`; if present, the host prefers that and appends the run facts):

```
# Run <id>
- Image: <image_ref>   Task: <task_summary>
- Result: <success|failed|timed out>   Exit: <code>   Duration: <hms>
- Network: <mode> (<allow…>)

## Changes
<files_changed> files, +<insertions>/-<deletions>. See changes.patch.

## Notes
<agent-provided notes from /output/report.md, if any>
```

Secrets never appear in any of these files (redaction per §6.8). `krayt ls` reads
`meta.json`; `krayt patch`/`apply` read `changes.patch`.

---

## 9. Project Structure

```
krayt/
├── cmd/krayt/main.go
├── internal/
│   ├── cli/                 # cobra commands, flag/config merge
│   ├── orchestrator/        # run lifecycle, concurrency, teardown, state
│   ├── provider/
│   │   ├── provider.go      # Provider/VM interfaces (OS-agnostic)
│   │   ├── vfkit/           # macOS via crc-org/vfkit subprocess   ← v1
│   │   ├── vz/              # macOS via direct Code-Hex/vz          ← fallback
│   │   └── firecracker/     # Linux (firecracker-go-sdk)           ← later
│   ├── protocol/            # vsock control protocol (shared host+guest)
│   ├── guest/               # guest-agent (compiled to linux)
│   │   ├── agent.go         # init/control server
│   │   ├── proxy/           # egress allowlist proxy + firewall
│   │   ├── ask/             # in-VM question bridge + ask_human MCP server (§6.13)
│   │   └── runner/          # containerd Go client (single container per VM)
│   ├── adapter/             # optional per-agent adapters (claude-code, gemini-cli); MCP/CLI wiring (§6.13)
│   ├── task/                # config schema + parsing
│   ├── patch/               # git bundle create/verify/clone/diff (+ optional reverse bundle); non-mutating dirty capture; host-side apply helpers (§6.7)
│   ├── imagestore/          # host pull + OCI export + digest-keyed cache (§6.11)
│   └── secrets/             # secrets loading + redaction
├── cmd/krayt-ask/main.go    # tiny in-container CLI front-end for ask_human (§6.13)
├── images/                  # Nix-based VM image definition (kernel + rootfs)
│   ├── flake.nix            # declarative base image; pins kernel, runtime, guest-agent
│   ├── flake.lock           # pinned inputs (the update surface)
│   └── microvm.nix          # Linux backend (firecracker/cloud-hypervisor)  ← later
├── configs/                 # example krayt.yaml, default allowlist
├── flake.nix                # dev shell (protoc/buf/oras pinned) + codegen target (§9.2)
├── Makefile                 # `make proto`, build, test targets
├── docs/
└── README.md
```

### 9.1 Pinned dependencies
Use these exact modules so the agent doesn't guess. (Pin concrete versions in `go.mod`
at implementation time; major versions shown where they matter.)

| Concern | Module | Notes |
|---|---|---|
| macOS VM backend (v1) | `github.com/crc-org/vfkit` (`pkg/config` + REST) | drives a signed vfkit subprocess; pure-Go host (no cgo); pin version |
| macOS VM backend (fallback) | `github.com/Code-Hex/vz/v3` | direct in-process embedding; cgo + macOS SDK; used only if the vz provider is built |
| Guest vsock listener | `github.com/mdlayher/vsock` | `vsock.Listen` → `net.Listener` for gRPC (guest, linux) |
| Linux VM backend (Phase 7) | `github.com/firecracker-microvm/firecracker-go-sdk` | host `AF_VSOCK` to guest CID |
| gRPC | `google.golang.org/grpc` + `google.golang.org/protobuf` | control protocol (§6.5) |
| Proto codegen | `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` | or `buf`; run via Nix/CI |
| Container runtime client | `github.com/containerd/containerd/v2/client` | guest, drives containerd (§6.10) |
| OCI registry / image pull+export | `oras.land/oras-go/v2` | host imagestore (§6.11) |
| OCI types/layout | `github.com/opencontainers/image-spec` | media types, `oci-layout` |
| Egress proxy (optional) | `github.com/elazarl/goproxy` | or hand-rolled CONNECT proxy (§6.6) |
| CLI | `github.com/spf13/cobra` (+ `spf13/pflag`) | command surface (§13) |
| Config | `gopkg.in/yaml.v3` | task config file (§8.1) |
| `ask_human` MCP server | `github.com/modelcontextprotocol/go-sdk` (v1.2.0, `/mcp`) | stdio MCP server for `krayt-ask --mcp` (§6.13, Phase 6); pulled only by `cmd/krayt-ask`, so it vendors into the guest-agent image → regenerate `flake.nix` `vendorHash` |

Build constraints: `internal/provider/vfkit` and `internal/provider/vz` are
`//go:build darwin` (vfkit is pure-Go host-side; the vz fallback adds cgo). `internal/guest`
and its children are `//go:build linux` and cross-compiled to `linux/arm64`. Keep the
OS-agnostic core (orchestrator, protocol, task, imagestore host side, patch) free of
build tags so it compiles on both. Runtime: the vfkit provider requires the `vfkit` binary
installed (brew); `krayt doctor` checks for it (§13).

### 9.2 Code generation
The `.proto` (§6.5) lives at `internal/protocol/krayt.proto`; generated Go lands in
`internal/protocol/pb`. **The generated code is checked into the repo**, so building or
running krayt — and Claude Code compiling it — needs **no `protoc`**. Only *regenerating*
after editing the `.proto` needs the codegen toolchain.

Regeneration runs behind a single pinned target so plugin/version skew never produces noisy
diffs:

```
make proto        # wraps `nix run .#proto` (or buf); pins protoc + protoc-gen-go + protoc-gen-go-grpc
```

This gives three prerequisite tiers (mirrored in the README):
- **Build/run krayt:** Go + vfkit + git. No protoc (generated code is committed).
- **Regenerate protocol:** Nix (or `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc`, or
  `buf`) — only when the `.proto` changes.
- **Build the VM image (CI):** arm64 Linux runner + Nix + `oras` + registry creds (§11.5).

> Guest-side runtime deps (`containerd`, `runc`/`crun`, `nftables`) live **inside** the
> Nix-built VM image, not on the dev machine — the flake owns them (§11.1/§11.6).

A root `flake.nix` `devShell` provides the codegen + image tools (`protoc`,
`protoc-gen-go`, `protoc-gen-go-grpc`, `buf`, `oras`) at pinned versions, so `nix develop`
is all a contributor needs for tiers 2–3 — no per-tool installs. `make proto` runs inside it.

---

## 10. Security Model

**Trust boundary:** the VM (separate Linux kernel) is the primary isolation boundary
between untrusted agent code and the host. The host kernel and filesystem are never
exposed.

| Surface | Control |
|---|---|
| Host kernel | Not shared — full VM boundary |
| Host filesystem | No live mount; input via git bundle, output via reviewed patch |
| Repo ingest | git bundle cloned in-guest — source `.git/hooks` are never executed or imported, and the guest commits under a throwaway krayt bot identity (§6.7) |
| Network egress | Default-deny + allowlist proxy; per-task opt-in to widen |
| Secrets | tmpfs only, never on disk, redacted from logs, destroyed with VM |
| Persistence | CoW disk destroyed on teardown; fresh VM per run |
| Patch application | Always manual; human reviews diff before `git apply` |

**Residual considerations to document:**
- Proxy-bypass via raw sockets (mitigated by default-deny egress).
- Malicious patch content (e.g. `.git/hooks`, build scripts) — reviewing the diff
  before apply is the control; consider a `--strip-hooks` / lint pass on patches later.
- Resource exhaustion — bounded by per-VM CPU/mem/disk + wall-clock timeout.
- Auth-credential blast radius — a subscription token (`CLAUDE_CODE_OAUTH_TOKEN`) is tied to
  a personal/seat plan and is less granularly revocable than a scoped API key; exposing one
  to untrusted code risks that seat's consumption and rate budget. Prefer a scoped,
  independently-revocable API key for untrusted runs (§6.14).

---

## 11. The Minimal VM Image (Nix-based)

A small Linux image whose only job is to run the guest-agent + a container runtime.
The image is **defined declaratively with Nix** and built reproducibly. This is the
isolation boundary, so we want to know exactly what is in it and be able to rebuild it
bit-for-bit.

> Scope note: Nix governs **only** this base micro-VM image. The user's Docker image
> (the AI + tools) is supplied at run time and is explicitly **not** Nix-built. Keep the
> two separate.

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
  an OCI registry. The OCI **digest is the content address** — `krayt` pins its
  version → digest and **verifies the digest** on `krayt image pull` (and `doctor`)
  before first use. The registry is interchangeable (ghcr.io is the convenient default,
  but any OCI-compliant registry works — **no hard dependency on ghcr.io**).
- **Run:** each run gets a **copy-on-write clone** of the verified base image so runs
  never share state.
- **Update:** bump the flake input/lock → CI rebuilds → push new OCI artifact → bump the
  pinned digest in `krayt`. Fully auditable in git.

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
          oras push <registry>/krayt-vmimage:${GITHUB_REF_NAME} \
            ./result/vmlinuz:application/vnd.krayt.kernel \
            ./result/rootfs.img:application/vnd.krayt.rootfs
      # capture the pushed digest -> record as the pinned image reference
```

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
  (§6.10), and the **`krayt-proxy`** binary run as the dedicated **`proxyd`** user for the
  egress proxy (§6.6). No editors, no shells beyond what systemd needs, no package manager.
- **Output artifacts:** `vmlinuz` + `initrd` + `rootfs.img` (**raw** format — vfkit boots
  raw/ISO, not qcow2), all `aarch64-linux`, packaged as the OCI artifact in §11.5. vfkit
  boots them via its Linux bootloader (kernel + initrd + cmdline) or EFI.
- **Boot contract (what the host relies on):** within N seconds of `VM.Start` (vfkit
  process up + VM booted), the guest-agent is listening on vsock port `1024` (bridged to the
  host `socketURL`) and answers `Hello`. The host treats a successful `Hello` as "VM ready";
  failure within a timeout → abort + `Destroy`.

> Practical ownership: have Claude Code author `flake.nix` and the systemd units, but make
> the boot-test (vfkit boots the image → `Hello` round-trips) a human/CI checkpoint, since
> the agent's sandbox can't build or boot the Linux image.

---

## 12. macOS Specifics & Gotchas

- **Entitlement / signing — handled by vfkit (v1):** the `com.apple.security.virtualization`
  entitlement is carried by the **vfkit** binary, which ships signed (installed via brew),
  so **krayt itself does not need the virtualization entitlement or special code-signing**.
  This removes the signing handoff that the direct-vz path would require. `krayt doctor`
  verifies vfkit is installed and runnable. *(If you ever switch to the direct `vz`
  provider, the entitlement + signing requirement moves onto the krayt binary — that becomes
  a `[HUMAN: signing identity]` step again.)*
- **Runtime dependency:** the vfkit provider needs the `vfkit` binary present (brew, pinned
  version). `doctor` checks presence + version; document the install in the README.
- **Image format:** vfkit boots **raw**/ISO images only (no qcow2). Keep `rootfs.img` raw;
  CoW clone via APFS `clonefile` works on raw images.
- **Apple Silicon:** ensure kernel/rootfs are `arm64`. Guest-agent and user images must
  match the VM architecture (arm64) unless emulating (avoid).
- **vsock:** no host `AF_VSOCK` on macOS — vfkit bridges the guest vsock port to a host
  unix socket (`socketURL`); the control channel dials that socket (§6.12).
- **NAT networking:** vfkit provides NAT; domain filtering is *our* responsibility (the
  in-guest egress proxy, §6.6), as neither vfkit nor the framework filters by domain.

---

## 13. CLI Surface (initial)

```
krayt run     [--image] [--task] [--repo] [--config] [--secrets]
                 [--net allowlist|full|none] [--allow domain ...]
                 [--cpus] [--memory] [--disk] [--timeout] [--detach]
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
krayt doctor                   # check host prereqs (vfkit installed+runnable on macOS; /dev/kvm on linux)
```

`run` is headless/detached-capable; default streams logs to the terminal but the VM
work is the same either way.

---

## 14. Milestone Roadmap

**Test strategy (applies to every phase).** The `Provider` interface is the seam that
makes the core testable without a VM: implement a `fakeProvider` whose VM loops back the
gRPC server in-process, and unit-test the orchestrator, protocol, imagestore (host side),
patch, and CLI against it on any OS. Real-VM behaviour (vz boot, image import, networking)
is covered by an integration harness gated behind a build tag and run on a real Mac / in
CI. Each phase below lists a concrete **Done when** checkpoint — prefer wiring that as an
automated test.

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
- [x] **Done when:** on a real Mac (with vfkit installed), `krayt` boots the published image and a `Hello` RPC round-trips host↔guest over the vfkit vsock socket. **[HUMAN: boot test on real hardware]**

### Phase 2 — End-to-end single run (happy path) ✅
- [x] Host: pull user OCI image + export OCI archive; digest-keyed cache (`imagestore`).
- [x] `QueryImageBlobs` + `PushImage` (stream only missing blobs); guest imports into containerd.
- [x] Host: create a **self-contained git bundle** (shallow-clone-then-bundle at `bundle_depth`) (§6.7). *(Non-mutating `include_dirty` capture is deferred to Phase 3.)*
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
- [x] Optional agent adapters (`internal/adapter`: `none`/`claude-code`/`gemini-cli`) — host-side pre-flight (`--agent` flag + `agent.adapter`) that validates auth and wires `krayt-ask` (`KRAYT_ASK_SOCKET`) when `--on-question=wait`. *(In-container credential export + agent launch run in the image entrypoint (§8.2) and need live keys — HUMAN. MCP-server registration is Phase 6.)*
- [x] Claude Code adapter maps the provided credential to the correct env var (`ANTHROPIC_API_KEY` vs `CLAUDE_CODE_OAUTH_TOKEN`) and enforces exactly-one auth, failing fast if both are set (§6.14). *(`claude-code` adapter, exactly-one over the recognized keys; wired into `krayt run` before any VM boot. `TestClaudeCodeExactlyOne` + `TestApplyAdapterAuthGate`/`TestApplyAdapterWiresAsk`.)*
- [x] **Detached supervisor — "park and walk away" (§6.2):** `krayt run --detach` re-execs a session-detached (`setsid`) per-run supervisor (no central daemon) that owns the VM to completion; the launcher returns immediately. Cross-process max-concurrency via a file-lock semaphore (`AcquireSlot` over `.krayt/slots/`, `--max-concurrency`). Reuses the Phase-4 on-disk state + management commands unchanged; localized to the run entrypoint. *(`TestAcquireSlotLimits`/`TestAcquireSlotCrossProcess` (real subprocesses) + `TestSpawnDetached`; existing `TestMaxConcurrency` now backed by the file lock. End-to-end "close the terminal, answer after" **verified on Apple Silicon** via `hack/ask-probe`: `--detach` returned with the supervisor pid, `krayt ls` showed `starting`→`waiting`, and `krayt answer` from a separate shell resolved it to `done` — see HUMAN_TODO.)*
- [x] Patch safety lint (flag hooks/suspicious changes). *(`patch.Lint` flags changes that execute outside the workspace edit — git hooks, CI config, shell startup files, direnv, newly-executable files — surfaced in `meta.json` `safety`, report.md's Safety section, and a `krayt run` warning. `TestLint`.)*
- [x] **Done when:** a real agent image completes a task and the run dir contains patch + report + meta; with the adapter + `--on-question=wait`, an agent's `krayt-ask` call round-trips to `krayt answer`; and a `--detach`ed run survives its launching process — its `waiting` question is answerable from a separate invocation after the terminal closes. ✅ **All three clauses verified on Apple Silicon:** *(1) detach — `hack/ask-probe` (`--detach` returned, `waiting` answered from a separate shell); (2) real agent — `hack/claude-code` (Claude Code authenticated via `CLAUDE_CODE_OAUTH_TOKEN`, edited `/workspace`, exit 0, patch+report+meta, `krayt apply` clean); (3) `krayt-ask` binary — `hack/krayt-ask-probe` (non-root uid 1000 shelled out to `krayt-ask` on PATH → `krayt answer` resolved it). Base image v0.0.0-rc16. The premium MCP front-end and precise `waiting`→`running` resume are Phase 6.)*

### Phase 6 — `ask_human` MCP front-end & precise resume
Both items need a `.proto`/image change, so they share one guest image rebuild and one HUMAN gate — carved out of Phase 5 to keep that phase fully host-provable.
- [x] In-VM `ask_human` **MCP server** (§6.13): `krayt-ask --mcp` runs a stdio MCP server (official Go SDK) exposing one tool — `ask_human{ question, choices?, context? }` — bridged to the question channel via `ask.OverSocket`; its tool *description* steers *when* to ask, and a no-answer maps to a "proceed autonomously" sentinel. The `claude-code` entrypoint registers it (`.mcp.json` / `--mcp-config`) only when `--on-question=wait` (i.e. `KRAYT_ASK_SOCKET` set). *(Handler host-proven by `TestAskHumanHandler`; on-VM round-trip rides the shared Phase-6 rebuild. Decision resolved: official `github.com/modelcontextprotocol/go-sdk` v1.2.0 (§9.1), for maintainability over bespoke wire code.)*
- [x] Guest **"question resolved"** `RunEvent` (§6.13): emitted when `bridge.Answer` delivers, so the host flips `waiting`→`running` precisely on answer instead of holding `waiting` until the run ends. *(`RunEvent.Resolved` added to the proto + regenerated; `ask.Bridge.OnResolved` → guest emit; host tracks outstanding questions and resumes at zero — fires for every answer path (Answer RPC / cross-process `krayt answer` / timeout sentinel). Host-proven by `TestQuestionResolvedResumes` + `TestBridgeOnResolved`; existing waiting-state tests still pass. On-VM confirmation rides the shared Phase-6 image rebuild.)*
- [x] **Done when:** on a rebuilt image with the adapter + `--on-question=wait`, an agent's `ask_human` **MCP tool call** round-trips to `krayt answer`, and the run flips `waiting`→`running` precisely when the answer lands (not on the next log line). ✅ **Verified on Apple Silicon (base image v0.0.0-rc17):** Claude Code registered the MCP server, called `ask_human` (run → `waiting`, question "PostgreSQL or SQLite?" persisted), `krayt answer … PostgreSQL` round-tripped the answer, `krayt ls` **directly showed the run flip `waiting`→`running`** on the answer (the guest `Resolved` event), Claude implemented the chosen DB (`db.py` + `psycopg`), and finished `done` (exit 0) — the full §6.13 premium path. Host logic proven by `TestQuestionResolvedResumes` + `TestAskHumanHandler`.

### Phase 7 — Linux backend (parity)
- [ ] `firecracker` provider behind the same `Provider` interface (`CID`-based vsock).
- [ ] `/dev/kvm` detection + graceful messaging in `doctor`.
- [ ] Reuse guest-agent, protocol, patch, secrets, orchestrator unchanged.
- [ ] **Done when:** the Phase 2 end-to-end test passes unmodified on a Linux host via the firecracker provider. **[HUMAN: Linux host with `/dev/kvm`]**

---

## 15. Open Questions / Future Work

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
  clones into an empty repo (used host→guest, produced via shallow-clone-then-bundle); a
  *range* bundle (`<base>..HEAD`) records prerequisites and only unbundles where the base
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
