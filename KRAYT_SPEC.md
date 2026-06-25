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
- Host repo isolation: **no live shared folder**; input via tar snapshot, output via patch.
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
│   │     ├── vsock control server  ◄──── host control channel ─┼── tar in / logs+patch out
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
    RepoPath     string            // host repo to snapshot (default: cwd)
    IncludeDirty bool              // include uncommitted changes over HEAD baseline
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
- Receive the **image archive**, **repo tarball**, **task**, **secrets**, and **network policy**.
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

  rpc PushCode(stream Chunk) returns (Ack);         // repo tarball, client-streaming
  rpc PushTask(TaskSpec) returns (Ack);
  rpc PushSecrets(SecretsBundle) returns (Ack);     // held in memory only (§6.8)
  rpc SetNetworkPolicy(NetworkPolicy) returns (Ack);

  // Start the container and stream events until it exits. The final RunEvent carries
  // the terminal Status (exit code); the stream then closes.
  rpc Start(StartRequest) returns (stream RunEvent);

  rpc CollectArtifacts(CollectRequest) returns (stream Chunk); // patch+report tar
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
  `NO_PROXY=localhost,127.0.0.1`.
- **DNS:** the proxy resolves allowlisted names itself; the container needs no direct DNS
  egress (DNS goes through the proxy uid or a guest-local resolver also running as `proxyd`).
- **Policy modes:** `allowlist` (default) — proxy enforces the domain list, seeded with
  the AI endpoints (`api.anthropic.com`, `generativelanguage.googleapis.com`) + registries
  the task needs; `full` — nftables policy switched to accept (explicit opt-in); `none` —
  proxy denies everything (usable because image acquisition is off the VM net path, §6.11).

### 6.7 Code transfer & patch generation (`internal/patch`)
**Robust baseline approach (avoids host `.git`/worktree pitfalls):**
- **Host:** create a clean snapshot of the repo state to send. Default: `git archive HEAD`
  (clean tree). Optionally include uncommitted working changes as an overlay.
- **Guest:** extract into `/workspace`, then `git init -q && git add -A && git commit -m baseline`.
- **Agent works** in `/workspace`.
- **On finish:** `git add -A && git diff baseline > /output/changes.patch`.
- **Host:** writes `changes.patch` to the run dir. User applies with
  `git apply changes.patch` (or `git apply --3way`) onto the real worktree after review.

This yields a portable patch independent of how the host repo's git internals are laid out.

### 6.8 Secrets (`internal/secrets`)
- Read from a **per-task secrets file** (e.g. `secrets.env` or `secrets.yaml`).
- Transferred over the encrypted-by-isolation vsock channel.
- Mounted in the container on **tmpfs** at `/run/secrets/` (and/or injected as env).
- **Never** written to the VM's persistent disk image; **never** logged (redacted in logs).
- Destroyed with the VM.

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
- **Registration (per-agent adapter, Phase 5):** wiring the agent's config to the MCP
  server is agent-specific (Claude Code et al. each configure MCP differently), so it lives
  in the optional adapter — **not** the agnostic core. The adapter registers the MCP server
  / wires the CLI **only when `--on-question=wait`**.

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
- The human answers with `krayt answer <run-id> [<qid>] <response>` (or an interactive
  one-line prompt; `choices[]` → tap/select). Multiple pending questions are answered FIFO by id.
- Every Q&A pair is persisted to `.krayt/runs/<id>/questions/<qid>.json` and summarized in
  `report.md` / `meta.json`, so the patch review shows what the agent asked and what it was told.

**Concurrency & safety:**
- A `waiting` run still owns a live VM, so it counts against max-concurrency; the timeout
  prevents indefinite parking.
- Question text comes from untrusted agent code → sanitize on display (strip terminal
  escape sequences), label it clearly as agent-originated, and never auto-fill secrets into
  an answer.

---

## 7. Run Lifecycle (Step by Step)

1. **Resolve spec** — merge flags + config file into a `RunSpec` (image, task, repo,
   network policy, secrets file, resources, env).
2. **Snapshot code** — `git archive HEAD` (+ optional dirty overlay) → tarball.
3. **Acquire image (host)** — resolve + pull the user's OCI image into the host store and
   export an OCI archive; reuse the digest-keyed cache to skip if already present (§6.11).
4. **Provision VM** — `Provider.Create` makes a CoW copy of the base rootfs, assigns a CID;
   `VM.Start` boots it.
5. **Connect** — host dials the guest-agent over vsock; handshake.
6. **Push inputs** — image archive (incremental: only missing blobs), code tarball, task,
   secrets, network policy.
7. **Start** — guest imports the image into containerd, brings up firewall+proxy, runs the container with mounts/env.
8. **Stream** — logs flow to host (and disk). Wall-clock timeout armed.
9. **Complete** — container exits (or timeout kills it). Guest builds the patch + report.
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
repo: .                         # repo to snapshot (default: cwd)
include_dirty: true             # include uncommitted changes over HEAD baseline

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
- `/run/secrets/*` — secrets (tmpfs).
- `/output/` — agent/guest writes `changes.patch` + `report.md` here (or guest generates the patch).

Completion = container process exit. Exit code is surfaced in `meta.json`.

### 8.3 Flag/file precedence
CLI flags override config file values, which override built-in defaults.

### 8.4 Run output artifacts (`.krayt/runs/<id>/`)
Every run produces a self-contained directory the human reviews from:

```
.krayt/runs/<id>/
├── changes.patch     # git diff vs baseline (the deliverable; §6.7)
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
│   ├── patch/               # baseline snapshot + diff; host-side apply helpers
│   ├── imagestore/          # host pull + OCI export + digest-keyed cache (§6.11)
│   └── secrets/             # secrets loading + redaction
├── cmd/krayt-ask/main.go    # tiny in-container CLI front-end for ask_human (§6.13)
├── images/                  # Nix-based VM image definition (kernel + rootfs)
│   ├── flake.nix            # declarative base image; pins kernel, runtime, guest-agent
│   ├── flake.lock           # pinned inputs (the update surface)
│   └── microvm.nix          # Linux backend (firecracker/cloud-hypervisor)  ← later
├── configs/                 # example krayt.yaml, default allowlist
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
| Linux VM backend (Phase 6) | `github.com/firecracker-microvm/firecracker-go-sdk` | host `AF_VSOCK` to guest CID |
| gRPC | `google.golang.org/grpc` + `google.golang.org/protobuf` | control protocol (§6.5) |
| Proto codegen | `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` | or `buf`; run via Nix/CI |
| Container runtime client | `github.com/containerd/containerd/v2/client` | guest, drives containerd (§6.10) |
| OCI registry / image pull+export | `oras.land/oras-go/v2` | host imagestore (§6.11) |
| OCI types/layout | `github.com/opencontainers/image-spec` | media types, `oci-layout` |
| Egress proxy (optional) | `github.com/elazarl/goproxy` | or hand-rolled CONNECT proxy (§6.6) |
| CLI | `github.com/spf13/cobra` (+ `spf13/pflag`) | command surface (§13) |
| Config | `gopkg.in/yaml.v3` | task config file (§8.1) |

Build constraints: `internal/provider/vfkit` and `internal/provider/vz` are
`//go:build darwin` (vfkit is pure-Go host-side; the vz fallback adds cgo). `internal/guest`
and its children are `//go:build linux` and cross-compiled to `linux/arm64`. Keep the
OS-agnostic core (orchestrator, protocol, task, imagestore host side, patch) free of
build tags so it compiles on both. Runtime: the vfkit provider requires the `vfkit` binary
installed (brew); `krayt doctor` checks for it (§13).

### 9.2 Code generation
The `.proto` (§6.5) lives at `internal/protocol/krayt.proto`; generated Go lands in
`internal/protocol/pb`. Generate via a `nix run`/`Makefile` target (so toolchain versions
are pinned) and check the generated code in, so neither the host build nor Claude Code
needs `protoc` present to compile.

---

## 10. Security Model

**Trust boundary:** the VM (separate Linux kernel) is the primary isolation boundary
between untrusted agent code and the host. The host kernel and filesystem are never
exposed.

| Surface | Control |
|---|---|
| Host kernel | Not shared — full VM boundary |
| Host filesystem | No live mount; input via tar snapshot, output via reviewed patch |
| Network egress | Default-deny + allowlist proxy; per-task opt-in to widen |
| Secrets | tmpfs only, never on disk, redacted from logs, destroyed with VM |
| Persistence | CoW disk destroyed on teardown; fresh VM per run |
| Patch application | Always manual; human reviews diff before `git apply` |

**Residual considerations to document:**
- Proxy-bypass via raw sockets (mitigated by default-deny egress).
- Malicious patch content (e.g. `.git/hooks`, build scripts) — reviewing the diff
  before apply is the control; consider a `--strip-hooks` / lint pass on patches later.
- Resource exhaustion — bounded by per-VM CPU/mem/disk + wall-clock timeout.

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
  firecracker / cloud-hypervisor / qemu — nearly turnkey for the Phase 6 Linux provider.

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
  nftables, the static guest-agent binary, CA certificates, busybox-equivalent coreutils.
  No editors, no shells beyond what systemd needs, no package manager.
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

### Phase 0 — Foundations
- [ ] Repo scaffold, Go module, CI, lint, build tags (§9.1).
- [ ] Define `Provider`/`VM` interfaces and `RunSpec`/`VMSpec` types.
- [ ] Author `krayt.proto` (§6.5); generate + check in `internal/protocol/pb` (§9.2).
- [ ] `fakeProvider` + in-process gRPC loopback for tests.
- [ ] `krayt doctor` for host prereq checks.
- [ ] **Done when:** `go test ./...` passes on macOS and Linux; a `Hello` RPC round-trips over the fake provider.

### Phase 1 — Boot a VM on macOS
- [ ] `vfkit` provider: build VM config via vfkit `pkg/config`, launch the signed vfkit subprocess, control via its REST API; CoW-clone the raw rootfs (`clonefile`); NAT + vsock (`socketURL`) devices.
- [ ] No krayt code-signing needed (entitlement lives on vfkit, §12). `doctor` checks vfkit is installed + runnable; README documents `brew install vfkit`. **[HUMAN: install vfkit]** — trivial, scriptable; not a signing identity.
- [ ] `images/flake.nix`: NixOS + systemd image per §11.6 (raw `rootfs.img` + kernel + initrd); build in CI on arm64 Linux runner; publish OCI artifact (§11.5). **[HUMAN: Linux builder/CI + registry creds]** — agent writes the flake + CI workflow; human runs CI / provides registry credentials.
- [ ] `krayt image pull` + digest verification before first run.
- [ ] `DialControl` = `net.Dial("unix", socketURL)` to vfkit's vsock bridge + gRPC client wiring (§6.12).
- [ ] **Done when:** on a real Mac (with vfkit installed), `krayt` boots the published image and a `Hello` RPC round-trips host↔guest over the vfkit vsock socket. **[HUMAN: boot test on real hardware]**

### Phase 2 — End-to-end single run (happy path)
- [ ] Host: pull user OCI image + export OCI archive; digest-keyed cache (`imagestore`).
- [ ] `QueryImageBlobs` + `PushImage` (stream only missing blobs); guest imports into containerd.
- [ ] `PushCode` + `git archive HEAD` snapshot + baseline commit in guest.
- [ ] `PushTask` injection at `/task/prompt.md`; `Start` runs the container entrypoint (agent-agnostic).
- [ ] Patch generation (`git diff baseline`) + `CollectArtifacts` back to host.
- [ ] Guaranteed VM teardown (defer + signal handling).
- [ ] **Done when:** `krayt run` against a trivial image that edits one file yields a correct `changes.patch` that `krayt apply` cleanly applies to the host repo.

### Phase 3 — Security & capability controls
- [ ] Egress proxy (uid `proxyd`) + nftables default-deny ruleset (§6.6); per-task allowlist.
- [ ] Per-task secrets file → `SecretsBundle` → container tmpfs; log redaction.
- [ ] Resource limits (cpu/mem/disk) + wall-clock timeout → kills container then VM.
- [ ] Include-dirty overlay option for uncommitted changes.
- [ ] **Done when:** a container can reach an allowlisted host, is blocked from a non-allowlisted host and from a raw (non-proxied) socket, and secrets never appear in logs/artifacts (asserted by tests).

### Phase 4 — Concurrency & UX
- [ ] Orchestrator: multiple concurrent runs, per-VM socket device, state under `.krayt/`.
- [ ] `ls`, `attach`, `logs`, `stop`, `rm`.
- [ ] Live log streaming (`Start` stream) + headless detach.
- [ ] Agent-question channel (§6.13): `RunEvent.Question` + `Answer` RPC, in-VM bridge, `waiting` state, `krayt answer`, desktop notification, Q&A persisted to `questions/`. Default `mode: fail` so it's inert unless opted in.
- [ ] Config file + flag precedence; example configs.
- [ ] **Done when:** N runs execute concurrently with isolated patches/logs, `attach` shows live output, and (with `--on-question=wait`) a stubbed agent question drives a `waiting` state that `krayt answer` resolves.

### Phase 5 — Polish & optional orchestration
- [ ] Emit `report.md` + `meta.json` per the §8.4 schemas (exit code, timings, patch stats, questions; agent notes if the image writes `/output/report.md`).
- [ ] `ask_human` front-ends (§6.13): in-VM MCP server + `krayt-ask` CLI, both bridging to the Phase-4 question channel.
- [ ] Optional agent adapters (`claude-code`, `gemini-cli`) — incl. registering the MCP server / wiring `krayt-ask` when `--on-question=wait`.
- [ ] Warm-VM pool to amortize boot time (optional).
- [ ] Patch safety lint (flag hooks/suspicious changes).
- [ ] **Done when:** a real agent image completes a task and the run dir contains patch + report + meta; with the adapter + `--on-question=wait`, the agent's `ask_human` call round-trips to `krayt answer`. **[HUMAN: live API keys for the agent image]**

### Phase 6 — Linux backend (parity)
- [ ] `firecracker` provider behind the same `Provider` interface (`CID`-based vsock).
- [ ] `/dev/kvm` detection + graceful messaging in `doctor`.
- [ ] Reuse guest-agent, protocol, patch, secrets, orchestrator unchanged.
- [ ] **Done when:** the Phase 2 end-to-end test passes unmodified on a Linux host via the firecracker provider. **[HUMAN: Linux host with `/dev/kvm`]**

---

## 15. Open Questions / Future Work

- **VM boot time** on macOS — measure; consider a warm pool if cold boot hurts UX.
- **Container runtime choice** — *resolved:* **containerd** via its Go client (§6.10).
  `runc` vs `crun` left as a build-time toggle; either is acceptable.
- **Image distribution** — *resolved:* **host pulls + pre-loads over vsock** (§6.11). The
  VM never needs registry egress; the host is the only registry-facing component.
- **Dirty-tree fidelity** — exact semantics of including uncommitted/staged/untracked files.
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
- **Baseline commit** — the in-guest git commit of the injected snapshot, against which
  the agent's changes are diffed to produce the patch.
- **Adapter** — optional orchestration glue that knows how to invoke a specific AI CLI;
  not required thanks to the convention-based contract.
