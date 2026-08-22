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
The single OS-specific seam.

```go
type VMSpec struct {
    ID        string
    Kernel    string // path to vmlinuz (or EFI image)
    RootFS    string // path to the BASE rootfs image; provider makes a CoW clone per run
    CID       uint32 // vsock guest CID — Firecracker only; ignored by the vfkit/vz providers (§6.12)
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
- **`internal/provider/firecracker`** (v1, Linux): same interface over Firecracker/KVM. `Create`
  clones the raw rootfs, allocates the VM's tap device + vsock CID, and `Start` launches the
  `firecracker` binary as a subprocess configured over its **REST API on a unix socket** — the
  same subprocess+REST idiom as the vfkit provider, hand-rolled rather than pulled from
  `firecracker-go-sdk` (§9.1). Three things differ materially from vfkit:
  - **CoW clone:** there is no `clonefile(2)`. `Create` uses the `FICLONE` ioctl (reflink) where
    the filesystem supports it — Btrfs, or XFS with `reflink=1` — and falls back to a
    sparse-aware copy where it does not. **ext4 has no reflink support at all**, so on the
    common Linux setup each VM costs a real copy of the base rootfs (~2 GiB); putting krayt's
    run dir on XFS/Btrfs makes clones O(1) with no code change. Firecracker takes raw block
    devices only, so a qcow2 backing file is not an option.
  - **vsock:** a unix socket plus the `CONNECT` handshake, not `AF_VSOCK` — see §6.12.
  - **Networking:** Firecracker has **no built-in NAT device and no DHCP server** (vfkit has
    both). The provider creates a tap device per VM, gives it its own `/30`, and passes the
    guest its address on the kernel command line; the host needs one-time setup for this. See
    §6.6.

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

### 6.6 Networking & egress proxy (`internal/proxy`, `internal/guest/proxy`, `internal/orchestrator`)
> **Amended by `move-egress-proxy-to-host.md` (step 1 of a three-step arc, §14).** The L7
> allowlist proxy moved from an in-guest process to a separate **host** process, reached over a
> new guest→host vsock channel. This is a **behavior-preserving, security-strictly-improving**
> move for the container: identical allowlist semantics, identical `HTTP_PROXY` contract — the
> only user-visible changes are DNS now resolving in the host's network context (was: a
> hardcoded `1.1.1.1`) and the SSRF guard's private-range carve-out being deleted outright (was:
> permitted under `mode: full`). Everything below reflects the current (post-move) design;
> superseded statements are not preserved inline — see git history for the pre-move text.

The container runs in the **VM's own network namespace** (no CNI bridge) — there is one
container per VM, so the VM boundary *is* the network boundary and host-networking-in-VM
is the simplest correct choice. Enforcement layers:

- **VM interface:** one NAT interface (vz NAT device / Firecracker tap), brought up by
  the NixOS network config. **This interface applies no filtering of its own** — vfkit/the
  Virtualization.framework NAT device forwards whatever the guest sends; the hypervisor is not a
  firewall.
- **Host side of the wire — the one real asymmetry between backends (Phase 7).** vfkit hands the
  VM a NAT'd NIC with a DHCP server built in, so a macOS host needs no setup. **Firecracker
  provides neither**: it gives the VM a bare tap device and nothing else. So on Linux the
  *provider* owns the host end:
  - **tap + subnet per VM.** Each VM gets its own tap device and its own `/30` out of
    `172.16.0.0/16` (host `.1`, guest `.2`), rather than a shared bridge — so two concurrent
    runs share no L2 segment and cannot see each other's traffic (§10). Allocation is guarded by
    an flock'd slot file, because `krayt run --detach` means several krayt *processes* can be
    booting VMs at once (§6.2).
  - **Address delivery.** With no DHCP server, the guest's address travels on the kernel command
    line (`ifname=`/`ip=`/`nameserver=`, dracut syntax). Note that the *kernel's* `ip=`
    autoconfiguration does **not** read it — that needs `CONFIG_IP_PNP`, which the nixpkgs kernel
    does not set, so the kernel ignores the parameter silently. It is consumed in userspace by
    **`systemd-network-generator`**, which the image enables (§11.6); it writes the corresponding
    `.network`/`.link` files before networkd starts. The vfkit path is untouched: with no `ip=`
    on the cmdline nothing is generated and the image's DHCP unit applies as before.
  - **One-time host setup, not per-run privilege.** Creating a tap needs `CAP_NET_ADMIN`, and
    routing guests out needs IP forwarding + a NAT masquerade rule. These are granted once
    (`hack/linux-net-setup.sh`: a `setcap cap_net_admin+ep` file capability on the krayt binary,
    so krayt does **not** run as root, plus the forwarding/masquerade rules) and checked by
    `krayt doctor`. The tap is handed to the invoking uid (`TUNSETOWNER`) so the *firecracker*
    process itself needs no capabilities at all. This NIC still exists (`full` mode still needs
    it — see below), but in `allowlist`/`none` the guest no longer strictly needs it; making it
    conditional is a follow-up (§15), not done here.
  - **None of this weakens the guest's egress policy.** What a container may reach is still
    decided by the allowlist proxy (now host-side, below) + the nftables loopback-only lock,
    identically on both backends. The host network setup only provides the `full`-mode wire.
- **The L7 allowlist proxy runs on the HOST, as a separate process (`internal/proxy`).** A
  small **HTTP/HTTPS CONNECT forward proxy** (hand-rolled) checks the `CONNECT` host and
  plain-HTTP `Host` against the per-task allowlist — exactly as before, just relocated.
  Concretely: the run supervisor spawns `krayt __egress-proxy` (a hidden cobra subcommand,
  `internal/cli`) via **self-exec** (`os.Executable()`, overridable with
  `KRAYT_EGRESS_PROXY_BIN`) as its own OS process — it must not share an address space with the
  process that (from step 2 of this task's arc onward) holds the user's real credentials,
  writes their repo, and runs the run supervisor. The parent creates and binds the listener
  socket and hands it to the child on **fd 3** (`cmd.ExtraFiles`), so the child needs no
  filesystem access to the socket directory at all — a socket **path** is never passed. Policy
  arrives as flags (`--mode`, `--allow`, `--dns`), matching the old in-guest contract shape.
  A failure to spawn the child, or an early child exit, is a fail-fast run error: the VM never
  boots with its only egress path already dead.
- **The guest→host vsock channel (`EgressPort = 1025`, `internal/provider`).** This is the one
  genuinely new primitive: every other vsock channel in krayt is host-initiated
  (`DialControl`/`ControlPort = 1024`); this one is **guest-initiated**. `VM.ListenEgress(ctx,
  port)` returns a listener accepting guest connections, called by the orchestrator after
  `Create` and before `Start` (the socket must exist before the backend process launches, so a
  guest connection racing boot never finds it missing):
  - **vfkit:** a *second* virtio-vsock device, `VirtioVsockNew(EgressPort, egressSock, true)` —
    `listen=true` means connections are guest→host, and (unintuitively) the **host** is the one
    that binds and listens on `egressSock`; vfkit itself is the *client*, dialing in each time
    the guest connects out. `ListenEgress` is a plain `net.Listen("unix", egressSock)`, called
    before vfkit's subprocess starts. `egressSock` lives in the same short, `0700` per-VM socket
    dir as `ctrlSock` (§6.12's socket-root hardening applies identically).
  - **Firecracker:** no device to add and no `CONNECT <port>\n` handshake (that dance is
    host→guest only, §6.12) — a guest connection to `(VMADDR_CID_HOST, port)` is bridged by
    Firecracker to a host unix socket at `<uds_path>_<port>`, which Firecracker dials as a
    *client*, symmetric with the vfkit case: the host must already be listening there.
    `ListenEgress` is `net.Listen("unix", vsockSock+"_"+port)`.
  - **fake provider:** a REAL unix-socket listener in a per-VM temp dir (not an in-memory
    pipe), so orchestrator-level tests genuinely exercise the fd-passing path in §4 of the task,
    not a mock of it.
  - **Why vsock and not a gateway-bound TCP proxy.** The tempting shortcut — point
    `HTTP_PROXY` at the VM's gateway IP and run the proxy there — is wrong. §6.6's tap+`/30`
    isolation (above) exists precisely so two concurrent VMs share no L2 segment; vfkit/vmnet's
    NAT gives no such isolation, so a gateway-bound proxy would be reachable by *every other
    run's VM on the host*, and a `0.0.0.0` bind mistake would expose it to the LAN — a
    materially worse blast radius once step 2 makes this process hold the user's real
    credentials. vsock has neither problem: on vfkit each VM gets its own host unix socket, on
    Firecracker its own `uds_path` — the channel is authenticated by construction and is not
    routable. Concurrent-VM isolation over this channel is asserted on hardware by
    `TestConcurrentRealVMs` (§14).
- **The guest side is now a dumb pipe (`cmd/krayt-vsock-forward`, `internal/guest/proxy`).**
  `krayt-vsock-forward --listen 127.0.0.1:3128 --vsock-port 1025` accepts TCP on `--listen`
  (the container's `HTTP_PROXY` target) and, for each accepted connection, dials the host over
  vsock and splices the two byte streams — **one vsock connection per accepted TCP connection**
  (no multiplexing; `HTTP_PROXY` keep-alive means the container opens several concurrently). It
  **parses nothing**: no HTTP, no TLS, no allowlist. That is the whole point — the
  adversarially-exposed parser moved off the guest entirely, so nothing may follow it back onto
  the guest side; if a future change makes this binary want to look at a byte, the design has
  regressed. It still runs as the dedicated `proxyd` uid (`internal/guest/proxy/controller_linux.go`)
  as defense in depth — a non-root uid for the one guest process touching container-controlled
  bytes is free — but this is **no longer load-bearing for the L3 lock** (below); do not delete
  it as vestigial.
- **L3 enforcement, simplified to loopback-only.** With the L7 proxy off the guest entirely, the
  guest needs no DNS (the host proxy resolves), no registry egress (§6.11 pre-loads images over
  vsock), and no bundle egress (§6.7 rides vsock) — so in `allowlist`/`none` there is nothing for
  the container to legitimately reach except the forwarder on loopback:

  ```
  table inet krayt_egress {
    chain output {
      type filter hook output priority 0; policy drop;
      oif "lo" accept
    }
  }
  ```
  The lock is in the **`inet` family** (IPv4 + IPv6 alike). **No rule keys on a uid anymore** —
  the `meta skuid "proxyd"` accept and its `ct state established,related` companion are both
  gone, because there is no longer an external flow for the guest to legitimately originate at
  all. This **deletes the cross-module invariant** the old design depended on (§10): previously
  the lock's correctness lived partly outside `firewall_linux.go`, in the container's dropped
  `CAP_SETUID`/`CAP_SETGID` and enforced non-root (§6.10) — a future OCI-spec regression there
  could silently reopen the egress bypass (finding #1). Moving the proxy off-box **deletes that
  invariant rather than defending it**: the guest chain no longer keys on identity at all, so
  there is nothing for a capability regression to unlock. `full` mode is **unchanged** — it
  still deletes the table outright, so raw egress over the NIC still works as an explicit
  escape hatch; unifying `full` onto the proxy path is a legitimate follow-up (§15), not done
  here.

  **Single-netns assumption** (unchanged). This `output`-hook rule is correct only while the
  container shares the **VM's** network namespace (`oci.WithHostNamespace`, one container per
  VM) — the VM boundary is the network boundary, so the container's sockets traverse this hook.
  A future change that gives the container its own netns would move its traffic out of
  `output`'s view and require a `forward` chain instead.
- **Container env:** launched with `HTTP_PROXY` / `HTTPS_PROXY` pointing at the forwarder
  (`http://127.0.0.1:3128`) and `NO_PROXY=localhost,127.0.0.1` (the lowercase
  `http_proxy` / `https_proxy` / `no_proxy` forms are set too, for tools that only read those)
  — byte-for-byte the same contract the container saw before this task; only what is on the
  other end of `127.0.0.1:3128` changed.
- **DNS resolves in the HOST's network context now, not the VM's.** The host-side proxy
  resolves through the **host's system resolver** by default — respecting the user's
  VPN/split-horizon/corporate DNS, which a hardcoded server cannot. `--dns` (a child-process
  flag, no `network.dns` user-facing config surface yet) still overrides it, mirroring the old
  `krayt-proxy --dns` contract shape. This is a documented, intended behavior change: DNS used
  to resolve as the VM would see it; now it resolves as the host does.
- **Policy modes:** `allowlist` (default) — the proxy permits only the hosts the task lists
  (`--allow` / `network.allow`); with none listed it is **deny-all**, so a task that needs the
  AI endpoints (`api.anthropic.com`, `generativelanguage.googleapis.com`) or a package
  registry must allow them explicitly — krayt does **not** auto-seed them. `full` — nftables
  policy switched to accept (explicit opt-in, guest-side, unchanged by this task); `none` —
  proxy denies everything (usable because image acquisition is off the VM net path, §6.11). The
  agent's **auth/refresh** endpoints must be allowlisted alongside the inference endpoint
  (§6.14); an OAuth/`apiKeyHelper` refresh flow may touch more hosts than a static API key, so
  it can need a wider list.
- **The allowlist is per-*host*, not per-host:port — one entry authorizes every TCP port on that
  host.** The policy decision is made on the bare hostname (the CONNECT authority's port is
  stripped before the allowlist lookup), but the **full authority is what gets dialed**. So
  `allow: [api.example.com]` permits `CONNECT api.example.com:443` *and* `:22`, `:11434`, `:1` —
  each dialed at the port the guest named, and under `mitm` each receiving the injected
  credential at `https://<host>:<that port>`. This is intended: §6.6 speaks of hosts throughout,
  and `passthrough`'s stated purpose (git+ssh on 443) depends on the port being the guest's
  choice. But "exact-host allowlist" reads narrower than it is — an allowlisted host is reachable
  on **any** port, subject only to the resolved-IP guard below. If a host must be reachable on one
  port only, krayt has no mechanism for that today.
- **What a `passthrough` host gives up (§6.6.1).** From `200 Connection established` onward the
  proxy is a **byte pipe**. Still enforced for such a host: the allowlist decision on the CONNECT
  authority, `checkDialAddr` on every resolved IP, the fact that the CONNECT authority is what
  gets dialed, and a single `observe CONNECT … via=tunnel` line. Given up **entirely**: the
  request line, path and query; every request and response header; status codes; bodies; whether
  the traffic is even HTTP; any injection or stripping (§6.6.1); the per-request observation log;
  and any notion of how much data moved — on **any** port (see the bullet above). Operationally
  this is a one-word cliff: adding a host to `passthrough` silently converts it from "inspected
  and injectable" to "opaque", and the only place that conversion shows up is that one log line,
  emitted only when request observation is on — `KRAYT_PROXY_LOG_REQUESTS=1`, or
  `KRAYT_PROXY_LOG_HEADER_VALUES=<names>` (which implies it).
- **Resolved-IP guard (SSRF / DNS-rebinding) — now a HARD block on every proxy-mediated dial, no
  carve-out.** The host-string allowlist is not enough on its own: an allowlisted name (or, in
  `full`, any name) could resolve to an internal address. After the proxy resolves an upstream
  name, it checks the **resolved IP** — on *every* A/AAAA answer and every connection attempt,
  via the dialer's `Control` hook, covering both the CONNECT tunnel dial and the plain-HTTP
  transport dial — and refuses, **unconditionally, in every policy mode including `full`**:
  loopback (`127.0.0.0/8`, `::1`), link-local (`169.254.0.0/16`, `fe80::/10`), the cloud
  **metadata** IP `169.254.169.254`, the unspecified address (`0.0.0.0`, `::`), multicast, and
  (since this task) **private / ULA ranges** — `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
  the `100.64.0.0/10` CGNAT range, and `fc00::/7` — **with no `mode: full` exception anymore**.
  Public addresses are allowed (still subject to the host allowlist above). The check is
  **fail-closed**: an address that cannot be parsed is refused. A refusal returns a `403`. **This
  guard is proxy-side, so it only covers traffic that is actually proxy-mediated** — see the
  `full`-mode caveat immediately below.

  **Why the carve-out was deleted rather than kept.** The old `mode: full` exception existed
  because the dialer lived *inside* the VM: letting it reach `192.168.0.0/16` meant "the VM's
  own NAT segment is reachable" — a contained, low-stakes trade. With the dialer now on the
  **host**, the identical carve-out would mean "the sandbox can reach the user's real LAN and
  loopback services from a trusted host process" — a materially worse trade, so it is refused
  outright rather than widened. **This is a deliberate, documented casualty for proxy-mediated
  traffic:** a well-behaved agent that honors `HTTP_PROXY` — a local Ollama/LM Studio on
  `127.0.0.1:11434`, a LAN package mirror — cannot reach it through the proxy, in any policy
  mode. That use case, if wanted, needs a purpose-built mechanism (an explicitly named forward
  target), not a range unblock; it is recorded as a possible follow-up (§15), not built here.
  **It is not a hard guarantee in `mode: full`**, though: `full` also deletes the guest's
  nftables table outright (above), so software that ignores `HTTP_PROXY` — or opens a raw socket
  — bypasses the proxy (and this guard) entirely and can reach any routable private/LAN address
  directly over the NIC, identically to before this task. Only the proxy path gained the
  unconditional block; `full`'s raw-NIC escape hatch is unchanged and was never proxy-mediated.

  Because the *resolved* IP is what's checked (not the requested name), this also mitigates
  **DNS-rebinding** to internal addresses.
- **Isolated as a swappable, memory-safety-critical component — more so now that it is
  off-VM.** The proxy is a **standalone host process** (`krayt __egress-proxy`, spawned by
  self-exec or `KRAYT_EGRESS_PROXY_BIN`) sitting behind a stable contract: fixed flags in
  (`--mode` / `--allow` / `--dns` / `--mitm` / `--run-id`, §6.6.1), a listener on fd 3, a
  JSON `StdinConfig` (passthrough + resolved inject rules — the only place secret material
  reaches this process, §6.6.1) on stdin, the ephemeral MITM CA's public cert (or nothing)
  written once to fd 4, logs on stdout/stderr (`internal/orchestrator` redirects them into
  `proxy.log`, §9). Nothing else in krayt depends on *how* it is implemented —
  `internal/proxy`'s `Factory`/`newHandler` seam (unchanged since before
  `add-tls-mitm-credential-injection.md`; MITM is a mode of the existing handler, not a fork
  of it) still lets the allowlist/MITM handler itself be swapped (e.g. for `elazarl/goproxy`)
  without touching the process wiring. Because it is the component most directly exposed to
  **untrusted, adversarial network input**, and now sits *outside* the VM boundary rather than
  inside a disposable one, a memory-safe reimplementation (e.g. Rust/Zig) matters *more* here
  than it did in-guest — drop in a binary honoring the same flags/fd-3/stdin/fd-4/log contract
  via `KRAYT_EGRESS_PROXY_BIN`, and neither the orchestrator nor the guest changes.
- **`proxy.log` — a new, host-redacted run artifact (§9).** `net/http`'s CONNECT-proxy client
  discards the response body on a non-2xx CONNECT, so the *only* place a denial's real reason
  (DNS failure, connection refused, blocked-address guard, …) appears is the proxy's own
  server-side log. The run supervisor captures the child's stdout/stderr and, on teardown,
  redacts it against the task's secrets (the first HOST-side redaction path in krayt — §6.8) and
  writes it to `.krayt/runs/<id>/proxy.log`. What lands there is **failures and policy denials
  only** — a run in which every request succeeded correctly leaves the file **empty**.
- **`KRAYT_PROXY_LOG_REQUESTS=1` — the opt-in request-observation mode
  (`internal/proxy/observe.go`).** Set on the `krayt run` invocation (the proxy child inherits the
  environment), it adds one `proxy.log` line per handled request: the request line, the
  already-approved host, header **names**, query-parameter **names**, and the response status —
  never a header value, never a query value, never a body, the same rule §6.6.1 sets for
  everything else this process logs. This is what makes the proxy an *instrument* — the only way
  to answer "which host, path, and auth header did this agent actually use", which a wire-format
  probe (§6.14, `inject-claude-oauth-token-at-proxy.md` P1–P4) must observe before an injection
  rule may be written for a credential shape. Off by default, because an always-on version would
  persist the hosts and paths every ordinary run visited. It is an env var rather than a flag on
  purpose: the flag set is the `KRAYT_EGRESS_PROXY_BIN` swap contract above, and a replacement
  binary must be able to ignore a diagnostics request instead of dying on an unknown flag.
- **`KRAYT_PROXY_LOG_HEADER_VALUES=<names>` — the one narrow relaxation of "never a value".** A
  comma-separated list of header names whose values the observation log may record in full (implies
  the mode above). It exists because a header *name* is not always enough: an API's required opt-in
  flags (a beta or version header) are non-secret facts a probe must record **exactly**, and guessing
  them is precisely what the probe protocol forbids. A **credential-bearing** name never yields its
  value — `authorization`, `proxy-authorization`, `cookie`, `set-cookie`, `x-api-key`, `api-key`, and
  any header this run's own `network.inject[]` rules touch are reduced to
  `<scheme="Bearer" credential_len=N>`: the public RFC 7235 scheme plus the length of the material
  after it. That shape is what answers a probe's two remaining questions — "Bearer-prefixed or raw
  token?" and "is the credential forwarded verbatim, or did the client exchange it first?" (compare
  `credential_len` against the secrets-file value's own length) — without a credential ever entering
  the artifact. Disclosing a high-entropy token's *length* into an already-redacted, per-run,
  explicitly-opted-into log is the deliberate trade being made here.

#### 6.6.1 TLS MITM & credential injection (`internal/proxy`, `add-tls-mitm-credential-injection.md`)
> **Amended by `add-tls-mitm-credential-injection.md` (step 2 of the three-step host-side-proxy
> arc, §14; depends on `move-egress-proxy-to-host.md`, step 1, above).** Everything below is
> **opt-in** (`network.mitm: false` by default, §8.1) — a user who does not set it observes
> **zero behavior change** from step 1: same tunnel path, no CA in the guest's env map, byte-
> identical `internal/proxy` behavior.

**What it buys.** Today an agent credential rides `SecretsBundle` → guest memory → container
tmpfs at `/run/secrets` (§6.8, §6.14): the agent process can read it, and so can anything that
compromises it, and a stolen credential **outlives the run** — the one thing the ephemeral-VM
model otherwise prevents. With `network.mitm: true` plus `network.inject[]` naming a host, the
proxy terminates that host's TLS on the host and attaches the credential itself; the named
secrets-file key is **withheld from `SecretsBundle` entirely** (§6.8) — the container never
holds it, so there is nothing in the VM for a compromise to steal.

**What it does *not* buy — be honest about this, it is the main way overselling it goes wrong:**
- **It removes credential *theft*, not credential *use*.** The proxy cannot distinguish an
  agent-initiated request from a legitimate one: a compromised agent still has unlimited
  *authenticated* access to every allowlisted host for the run's duration. This converts
  exfiltration into a confused deputy — a real improvement (a confused deputy dies with the VM;
  a stolen key does not) but not "no risk".
- **It only covers HTTP-shaped credentials.** An SSH key, a signing key, or anything a tool
  computes over cannot move to the proxy; those still ride `SecretsBundle` unchanged.
- **It does not stop the credential being *reflected back* to the container.** The injected header
  goes to an allowlisted host; if that host has any endpoint that echoes request headers — a
  `/headers` or `/debug` route, an error page that quotes the request, a verbose 4xx — the
  credential comes back **in the response body, in plaintext**, and the proxy streams that body to
  the guest untouched. Nothing could catch it without reading response bodies, which the
  hostile-input rules below forbid outright. So the honest statement is conditional: **if** every
  host in `network.allow` that also has an `inject` rule is one the operator trusts not to reflect
  request headers, **then** a compromised agent can *use* the credential for the run's duration but
  cannot learn its bytes. That conditional is doing all the work here, and **krayt enforces no part
  of it** — it is an operator assumption about the allowlisted hosts, checked by nothing.
- **It moves the adversarial parser outside the blast-radius boundary a second time.** §6.6
  already names the proxy "the component most directly exposed to untrusted, adversarial network
  input" post step-1: a proxy compromise there bought unrestricted egress from a VM about to be
  destroyed. After this task, it buys code execution in the one host process holding the user's
  real credentials. Go's memory safety helps; request-smuggling and header-confusion bugs do not
  care — the hostile-input rules below (and §10's residual) are the mitigation, not optional
  garnish.

**Design decisions:**
- **`mitm` is allowed in every mode, `full` included.** In `mode: full` + `mitm`, every TLS
  connection not in `passthrough` is intercepted — including hosts on no allowlist at all. That
  is the point of `full`, but it makes leaf-cert generation unbounded (the guest, not the
  operator, picks every SNI), so the SNI leaf cache **must** be capped (below) rather than
  growing for the run's lifetime.
- **Ephemeral per-run CA, in memory, never written to host disk.** A persistent krayt CA on the
  user's disk that VMs trust would be a worse artifact than the one step 1 removed. `internal/proxy`
  generates one ECDSA P-256 CA per proxy-process lifetime (one process per run) and discards it at
  teardown; there is no exported API path that can return its private key (only `CACertPEM()`,
  the public certificate).
- **ECDSA P-256** for the CA and every leaf — per-connection RSA keygen is visibly slow. Leaves
  are cached by SNI, bounded at 1024 entries with eviction on overflow (a performance cache, not
  a security boundary, so eviction policy needn't be precise LRU).
- **Secret material reaches the proxy child on stdin, never argv or env.** Flags land in the
  process table; env is readable from `/proc/<pid>/environ`. The run supervisor writes one JSON
  document — the passthrough list and every inject rule, with `set`'s secrets-file key names
  already resolved to values — to the child's stdin at startup, then closes it.
- **`http/1.1` only in ALPN.** A hijacked `CONNECT` does not get `net/http`'s automatic h2
  upgrade; advertising `h2` and then serving 1.1 breaks clients. The *upstream* leg (the shared,
  SSRF-guarded transport) keeps `ForceAttemptHTTP2` as before.
- **`FlushInterval: -1`** on the `httputil.ReverseProxy` that serves the decrypted request:
  `ReverseProxy` only auto-flushes `text/event-stream` by default, and streaming NDJSON/long-poll
  would otherwise buffer and stutter the agent's token stream.
- **Per-host `passthrough` (tunnel, no MITM) list.** Pinned TLS clients and non-HTTP-over-TLS
  (git+ssh on 443) must survive; those hosts get the plain step-1 tunnel, byte-for-byte, by
  definition — never intercepted, never injected into.
- **Never log request or response bodies.** Every byte is now cleartext in a process that writes
  logs; headers may be logged name-only, same rule as step 1's `proxy.log`.
- **`net/http/httputil.ReverseProxy`, stdlib only.** No new dependency — `internal/proxy` was
  already hand-rolled (the `elazarl/goproxy` option in §6.6 was never adopted), so this removes
  no third-party framework either.

**Config (`network.mitm` / `network.passthrough` / `network.inject[]`, §8.1).** `inject[].strip`
and `.set` are separate lists on purpose: the header the container sends is not necessarily the
header that goes upstream (`inject-claude-oauth-token-at-proxy.md`, step 3, removes one auth
header and sets a different one). `set` values are secrets-file **key names**, resolved
host-side; `set_literal` values are fixed, non-secret strings — kept syntactically distinct so a
literal can never be mistaken for a resolved secret. One further key exists because a credential's
wire format is not always "this header = this value" (added by
`inject-claude-oauth-token-at-proxy.md` from the 2026-08-17 subscription-token observation):
- **`set_prefix`** — a literal prefix on a `set` header's resolved value, i.e. an auth **scheme**
  (`authorization: Bearer <token>`). Folded in host-side while the secrets-file key is resolved, so
  `internal/proxy`'s contract stays "set this header to this exact string" and no scheme knowledge
  crosses that boundary. Skipped when the resolved value is empty, so the proxy's fail-closed
  unresolved-credential check still fires instead of seeing a plausible bare `Bearer `.

Every rule is validated at `krayt run`
pre-flight, before any VM or image work: `inject` requires `mitm: true`; a rule's host must not
be in `passthrough`, and (in `mode: allowlist`) must be in `allow`; every `set` key must exist in
the secrets file; header names must be valid HTTP tokens and not hop-by-hop; `passthrough ⊆
allow` in `mode: allowlist`; every `set_prefix` must name a header that `set` also names
(case-insensitively) and carry a non-empty, CR/LF-free value. Injection targets **HTTPS only** — a plain-HTTP request to a host
with an inject rule is refused outright (400) rather than forwarded unauthenticated or given the
credential in cleartext; the MITM path is structurally the only place injection can fire.

**The MITM path (`internal/proxy`'s `handler.connect` → `connectMITM`).** After the existing
allowlist check: a `passthrough` host, or MITM being off, gets the unmodified step-1 tunnel —
that fallback must stay reachable no matter what MITM does, so it is never modified to depend on
MITM state. Otherwise: hijack the client connection, write `200 Connection established`, wrap it
in `tls.Server` with a leaf for the CONNECT authority (never a guest-supplied `Host` header — the
allowlist already approved the CONNECT authority, not whatever the decrypted request claims), and
serve HTTP/1.1 over it via `httputil.ReverseProxy`. The `Rewrite` hook sets the outbound URL from
the CONNECT authority; `Transport` is the **same, already SSRF-guarded** transport the tunnel and
plain-HTTP paths use, so `checkDialAddr`'s resolved-IP guard (§6.6) runs on every MITM upstream
dial too — proxy-mediated traffic gets the identical guarantee regardless of path. Injection
applies **after** `Rewrite`, in order: delete every header named in `strip`, then set every header
in `set`/`set_literal` — stripping before setting is what makes a guest header unable to smuggle
a second value past an injected one.

**Treating guest input as hostile (§10).** The proxy now parses attacker-controlled HTTP inside
attacker-controlled TLS, on the host, holding real credentials — non-negotiable rules: an inner
request's `Host` that disagrees with the CONNECT authority is a smuggling signal, refused with
400, never forwarded; the inner HTTP server bounds `MaxHeaderBytes` (1 MiB) and
`ReadHeaderTimeout` on top of step 1's existing timeouts; a CONNECT authority that isn't a valid
`host[:port]` is refused with 400; an injected value that resolves empty at request time (a
config error the pre-flight check didn't catch, e.g. an empty secrets-file value) is a 500, never
sent upstream unauthenticated; any MITM setup failure — leaf generation, TLS handshake — **fails
the connection outright**, never silently degrades to the plain tunnel (a silent fallback would
drop injection and send the agent out unauthenticated, a confusing failure far from its cause).

**Optional per-rule `refresh` (plumbing only).** A rule may declare `refresh: {host, path_prefix,
response_token_fields}` naming an upstream credential-refresh endpoint. `internal/proxy` ships
only the generic mechanism — a `RefreshFunc` seam wired into the MITM upstream transport that,
on a `401` for a rule with `refresh` configured **and** a `RefreshFunc` actually registered,
performs exactly one refresh and retries the original request exactly once (buffering the
request body up to 4 MiB to make the retry correct; a larger body skips the retry rather than
buffer without bound). A second `401` is always surfaced as-is — never a loop. The proxy stays
generic on purpose: it has no idea what Anthropic (or anyone) is. Constructing the actual refresh
request and parsing its response is vendor-specific knowledge that belongs in a per-agent adapter
(§6.14), not the core — `inject-claude-oauth-token-at-proxy.md` (step 3, Phase 10) is the intended
first consumer of this seam, but only if its P3 probe forces the fallback design (§6.14); the
primary design (the one currently implemented) needs no `RefreshFunc` at all, since a
translated-to-API-key credential never refreshes. With no `RefreshFunc` registered — true for
every run today — this remains a zero-overhead no-op and a `401` behaves exactly as it would with
no `refresh` block at all.

**Delivering the CA to the container (§8.2).** The credential never enters the VM; the run's
ephemeral CA's **public** certificate does, over the channel that already exists:
`NetworkPolicy.ca_cert` (`internal/protocol/krayt.proto`, empty when `network.mitm` is false).
The guest (`internal/guest/proxy/controller_linux.go`) writes it to `/run/krayt/ca.crt` (0644 —
it is public) and sets `KRAYT_CA_CERT` plus best-effort `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`/
`NODE_EXTRA_CA_CERTS` pointing at it; the **distro-specific** part — concatenating that with the
container's own system bundle so `passthrough` hosts still verify — is the container entrypoint's
job (§8.2), not the guest's.

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

### 6.8 Secrets (`internal/secrets`)
- Read from a **per-task secrets file** (e.g. `secrets.env` or `secrets.yaml`).
- Transferred over the encrypted-by-isolation vsock channel.
- Mounted in the container on **tmpfs** at `/run/secrets/` (and/or injected as env).
- **Never** written to the VM's persistent disk image.
- Destroyed with the VM.

**Redaction scope.** Until `move-egress-proxy-to-host.md` this was "all in the guest, so no
secret value crosses the vsock un-redacted" — that guest-side claim is still true for every
artifact below, but it is no longer the *whole* story: `proxy.log` (§6.6, §9) is the **first
host-side** redaction path, because the egress proxy that produces it now runs on the host, not
in the guest. The guest builds one `Redactor` per run from the secret values (§6.5) and applies
it to everything the agent controls that the host will keep:
- **Live container logs** — each stdout/stderr line is redacted before it is streamed as a
  `RunEvent`. This is line/chunk-oriented, so a value split across two chunks is a known,
  accepted miss (see §10 residuals); it only affects live logs.
- **`report.md`** — the agent-written `/output/report.md` is redacted in place after the run,
  before it is collected, so the notes the host folds into the final report carry no value.
- **`ask_human` prompt + choices** — redacted at the bridge boundary before the question leaves
  the VM, covering both the live display and the persisted `questions/<id>.json`. Answers come
  from the human (host side), not the agent, so they are not a leak path.
- **`changes.patch` is scanned, NOT redacted.** Rewriting hunk bytes would corrupt the diff and
  break `git apply`, so the patch is left byte-exact. Instead the guest scans it for secret
  values and, on a hit, writes a `secret-scan.json` marker naming the matched **keys only**
  (never the values, §8.4); the host raises a Safety warning per key in `report.md`/`meta.json`
  so the human reviews before applying. Whole-buffer scan, so unlike live logs there is no
  split-chunk gap here.
- **`proxy.log` — HOST-side, built from a `Redactor` constructed the same way (§6.5's secret
  values, loaded host-side from the same secrets file), applied once, whole-buffer, when the
  run supervisor persists the egress proxy child's captured stdout/stderr (§6.6, §9).** This
  exists because, for plain-HTTP forwards, the proxy sees full request URLs, which can carry a
  token in a query string — the same class of risk `changes.patch`'s scan-not-redact already
  documents, just on a different artifact and (since the source is a log, not a git diff)
  redactable in place like the other logs above, not merely scanned. Fail-closed like
  `writeConsoleLog`: if the secret values can't be loaded to redact against, the file is
  dropped rather than risked in the clear.

Agent model-provider credentials (e.g. Claude Code's `ANTHROPIC_API_KEY` or
`CLAUDE_CODE_OAUTH_TOKEN`) ride this same mechanism — see agent authentication (§6.14) for
how a credential maps to the right env var and the exactly-one rule the adapter enforces.

**Secrets partitioning (`network.mitm` + `network.inject`, §6.6.1).** When a secrets-file key is
named in any `inject[].set` rule, it is **withheld from `SecretsBundle` entirely** — the load-
bearing change of `add-tls-mitm-credential-injection.md`. The above bullet list ("read from a
per-task secrets file... mounted on tmpfs... transferred over vsock") is no longer true for an
injected key specifically: it is loaded host-side, attached to the matching request by the MITM
proxy, and never crosses into `SecretsBundle`, guest memory, or `/run/secrets` at all — the
container that would otherwise hold it runs credential-free for that key. It **remains** in the
host `Redactor` set used for run logs, `report.md`, and `proxy.log` (above), since a value the
proxy attaches can still appear in `proxy.log` the same way any other secret can. `meta.json`/
`report.md` record **which keys were injected** (names only) so the human reviewing a run can see
the container ran without them — the user-visible payoff of this whole mechanism. Everything else
in this section is unchanged: a non-injected secret still rides `SecretsBundle` exactly as before.

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
- **Least-privilege OCI spec (hardened, §10).** The container runs the untrusted agent, so
  the guest builds its OCI spec fail-closed rather than inheriting containerd's permissive
  defaults:
  - **All Linux capabilities dropped by default** (bounding/effective/permitted/inheritable
    all empty, ambient explicitly cleared). A task may re-grant a specific few via
    `container.capabilities` (§8.1); the setuid class (`CAP_SETUID`/`SETGID`/`SETPCAP`),
    the network-admin class (`CAP_NET_ADMIN`/`NET_RAW`), and broad escape primitives
    (`CAP_SYS_ADMIN`/`SYS_PTRACE`/`DAC_READ_SEARCH`/`BPF`) are **never** grantable — they
    would re-open the VM escape surface. Dropping `CAP_SETUID`/`SETGID` is no longer what
    the egress lock's correctness depends on — since `move-egress-proxy-to-host.md` the
    guest's nftables chain is loopback-only and keys on no uid at all (§6.6) — but keeping
    the caps dropped remains real defense-in-depth against everything else this OCI spec
    guards against.
  - **Enforced non-root, fail-closed.** An image that would run as uid 0 (explicit `USER root`
    or an unset `USER`) **fails the run** with a clear error — krayt never silently forces a
    uid. Non-root is load-bearing for secret confinement (§8.2); it is no longer load-bearing
    for egress the way it was before `move-egress-proxy-to-host.md` (§6.6, §10).
  - **Containerd's default seccomp profile** is applied by default; a task opts out with
    `container.seccomp: unconfined` (§8.1).
  - **`NoNewPrivileges=true`** (containerd default) is kept.
  - **Read-only rootfs is a per-task opt-in** (`container.readonly_rootfs: true`, default
    OFF), paired with writable ephemeral tmpfs for `/tmp` and `/run` only (never a blanket
    tmpfs over a populated dir). Default-off is deliberate — see §8.2 for the two reasons
    (image compatibility + marginal benefit in the ephemeral-VM model).

  The container-policy inputs travel host→guest in `TaskSpec` (§6.5); the host validates and
  normalizes the capability list before pushing, so a typo or a denylisted cap fails fast at
  config load, before any VM boots.

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

**Cache management (`krayt image ls/rm/prune`).** The digest-keyed host cache grows
unbounded — a multi-GB agent image rebuilt every commit leaves a directory per digest, and
nothing reclaims them. `krayt image ls` lists both host caches (this one plus the base VM
image, §11.4) in one table — kind, short digest, best-effort ref, recursive size, and
**last used**; `krayt image rm <digest>` removes one by full digest or unambiguous hex prefix;
`krayt image prune` bulk-reclaims under a retention policy (§11.4). Each cache entry carries a
`.krayt-last-used` sentinel file whose mtime is the last-used signal: `Acquire` refreshes it
on **both** the fresh-pull and cache-hit paths (best-effort — a touch failure never fails
acquisition, it only makes the `ls` timestamp stale), and `ls`/`prune` fall back to the
directory mtime when the sentinel is absent (an image cached before this existed).

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
- **Host side — Firecracker (Linux):** the host does **not** use `AF_VSOCK`. Firecracker
  deliberately bypasses the host's vhost stack and mediates between an **`AF_UNIX` socket on
  the host** and `AF_VSOCK` in the guest, so a host→guest connection is a unix dial to the
  device's `uds_path` followed by a text **handshake**: send `CONNECT <port>\n`, read back
  `OK <assigned_hostside_port>\n` (or the connection is closed, if nothing is listening on
  that port in the guest yet). `DialControl` performs the handshake and hands the caller the
  post-ack `net.Conn`, so everything above it still sees a plain gRPC transport. *(Verified
  against Firecracker v1.16.1, `docs/vsock.md`. Earlier drafts of this spec claimed an
  `AF_VSOCK` connect to the guest CID; that was wrong.)*
- **Why no CID management anywhere:** with vfkit each VM has its own `socketURL`, with direct
  vz each VM owns its own `VZVirtioSocketDevice`, and with Firecracker each VM owns its own
  `uds_path` — **on none of the three backends is there a shared host CID namespace to
  allocate**, and CIDs cannot collide between VMs. `VMSpec.CID` is the guest's context ID and
  is meaningful only to Firecracker (which requires one in its vsock config); the firecracker
  provider still hands out a unique CID per VM, but for traceability, not isolation. What
  actually isolates two concurrent VMs is the per-VM unix socket.
- **Host→guest bridging (Firecracker):** `krayt answer`/`stop` reach a *running* VM from a
  separate process by dialing the socket path recorded from `VM.ControlSocket()` with a bare
  `net.Dial("unix", …)` (§6.2, §6.13) — no handshake. So the firecracker provider does not
  expose firecracker's raw `uds_path` as its `ControlSocket()`; it runs a small in-provider
  listener that accepts plain connections and splices each to a freshly handshaken one. The
  handshake stays inside the provider, which is the point of the seam.
- **Security note:** the channel needs no TLS — a vsock link reaches exactly one VM and is
  not on any network. `insecure` transport credentials are correct here, not a shortcut.
- **Socket-root hardening (vfkit, macOS):** the host unix sockets that bridge the vsock
  control channel and vfkit's REST lifecycle API live under a short base directory
  (`/tmp/krayt-<uid>`, per-user) — short because `sockaddr_un.sun_path` is capped at 104
  bytes and `$TMPDIR` is too long. Because `/tmp` is shared, the provider **verifies or
  creates** this root on every run: if it does not exist it is created with `os.Mkdir`
  (0700; fails, rather than following, on a symlink pre-placed at the path); if it already
  exists it must be a real directory owned by the current uid with mode exactly `0700`, or
  krayt **fails closed** with a clear error rather than placing control sockets under a
  directory another local user controls. krayt never chmod/chowns a directory it does not
  own. The per-VM socket dir inside it is an atomic `0700` `MkdirTemp`. The egress socket
  below lives in this same per-VM dir and inherits the same hardening.

- **The guest→host direction (`EgressPort = 1025`, added by `move-egress-proxy-to-host.md`).**
  Every channel above is **host-initiated** — the host dials, the guest listens. `EgressPort`
  is the opposite: the **guest** initiates, over `VM.ListenEgress(ctx, port) (net.Listener,
  error)`, called by the orchestrator after `Create` and before `Start` (§6.6). Both backends
  turn out to support this the same shape as the host→guest direction, just mirrored — the
  host binds and listens, and the backend's own process is the *client* dialing in once the
  guest connects out:
  - **vfkit:** a *second* `virtio-vsock` device with `listen=true` —
    `config.VirtioVsockNew(EgressPort, egressSock, true)`. Per vfkit's `pkg/config/virtio.go`,
    `Listen=true` means *"vsock connections will have to be done from guest to host"*; per
    `pkg/vf/vsock.go`'s `listenVsock`, vfkit "proxies connections from a vsock port to a host
    unix socket" — i.e. vfkit is the client of `egressSock`, not its server. Because vfkit
    fixes its device list at `Create` time, this device is added unconditionally there, even
    though the socket itself is not bound until `ListenEgress` runs (right before `Start`).
    vfkit's multiple-`--device virtio-vsock` support is per-port, not per-device: "there will
    only be a single virtio-vsock device added to the VM regardless of the number of
    occurrences" — the two `VirtioVsockNew` calls (control port 1024, egress port 1025) map
    onto one underlying vsock device with two port forwards, not two devices.
  - **Firecracker:** no device to add, and — unlike `DialControl` — **no `CONNECT <port>\n`
    handshake**; that dance is host→guest only. A guest connection to `(VMADDR_CID_HOST,
    port)` is bridged by Firecracker to a host unix socket at `<uds_path>_<port>`, which
    Firecracker dials as a client the moment the guest connects, symmetric with the vfkit case
    above. `ListenEgress` is a bare `net.Listen("unix", vsockSock+"_"+port)` — no bridge type
    is needed the way `DialControl`'s host→guest direction needed one, because there is no
    handshake to hide from callers.
  - **fake provider:** returns a REAL unix-socket listener in a per-VM temp dir (not an
    in-memory `bufconn` pipe like `DialControl`'s loopback), specifically so
    orchestrator-level tests can fd-pass it to a real spawned `krayt __egress-proxy` child and
    exercise the whole path for real (§6.6).
  - Closing the returned listener stops accepting; the VM itself is unaffected. The listener's
    underlying socket **file** is not explicitly unlinked by the process that closes it —
    `(*net.UnixListener).SetUnlinkOnClose(false)` is set first, so a guest connection racing the
    handoff to the spawned proxy child never finds the path gone — cleanup happens once, when
    the provider tears down the VM's per-run socket dir (`Destroy`), not via any listener
    `Close` in between.

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

**Injection as the preferred delivery for HTTP-shaped credentials (§6.6.1).** Everything above
describes the credential riding `SecretsBundle` into the container, which is still the only
option for anything that isn't a bare HTTP header (an `apiKeyHelper` script, an SSH/signing key,
anything a tool computes over) and remains the default. For a credential that is *just* an HTTP
header on requests to one host — `ANTHROPIC_API_KEY` on `api.anthropic.com` is the canonical
case — `network.mitm: true` + `network.inject[]` (§6.6.1, §8.1) is the **preferred** delivery
where the trust-model trade is acceptable: the key never enters the VM at all, closing the
"Auth-credential blast radius" residual below for that credential specifically, at the cost of
concentrating more trust in the host proxy process (§10). It composes with everything above:
which credential shape to use is unaffected, and the adapter's exactly-one rule still applies to
whatever ends up in the secrets file — injection only changes *where* the chosen credential is
attached, not which one is chosen.

**Credential shape translation — hiding OAuth entirely (`inject-claude-oauth-token-at-proxy.md`,
step 3 of the host-side-proxy arc).** Plain header injection above still delivers whichever
credential the user actually configured, in its own shape — an OAuth-configured container is
still OAuth-configured, just without the token in `/run/secrets`. Shape translation goes one step
further: **when `network.mitm` is on and the selected credential's wire format has been observed**
(`internal/adapter/anthropic_wire.go`), the container is configured with a **non-secret placeholder
under the credential's own variable**, and the real value is attached entirely host-side in whatever
shape the provider actually wants:

| User's secrets file has | Container gets | Proxy sends upstream |
|---|---|---|
| `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use` | `x-api-key: <real key>` |
| `CLAUDE_CODE_OAUTH_TOKEN` | `CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-krayt-placeholder-do-not-use` | `authorization: Bearer <real token>` |

**Shape mirroring (owner decision, 2026-08-18).** The placeholder deliberately mirrors the *kind* of
credential the user supplied, rather than presenting every credential to the container as an API key.
The agent then runs its own code path for that credential — composing the `anthropic-beta` opt-in
list, the request line, and every other shape-specific detail itself — and krayt substitutes exactly
one header value, synthesizing nothing. The alternative (always configure `ANTHROPIC_API_KEY` and
have the proxy build the OAuth request shape) requires krayt to guess which of the API-key path's
beta flags an OAuth credential will accept, and to re-guess whenever either path changes.

What mirroring gives up is the claim that the container cannot tell which credential kind is in use.
That was never actually true: a subscription's own responses carry
`anthropic-ratelimit-unified-5h-*`/`-7d-*` headers where an API key's carry
`anthropic-ratelimit-requests-*`/`-tokens-*`, and the container reads the response either way.
Preserving the illusion would mean fabricating response headers, which krayt does not do. Two
consequences follow:
- **The exactly-one rule still matters inside the container**, though it is not at risk: the
  container is configured with exactly one credential variable, so the entrypoint's own
  first-match-wins selection (§8.2) can never see two and pick the wrong one.
- **Bare mode's `CLAUDE_CODE_OAUTH_TOKEN` caveat above still applies** to a translated subscription
  token, since the container really is OAuth-configured. krayt never invokes `--bare`, so this
  affects nobody today; it is recorded because the earlier design did obsolete this caveat and the
  change is easy to miss.
- **The container's `network.allow` stays minimal**: any refresh/token endpoint the real credential
  needs is dialed by the *proxy*, upstream of the guest's own allowlist entirely, so it never needs
  to appear in the guest's `network.allow`. The refresh case remains hypothetical for env-var
  delivery — a CLI configured this way holds an access token and no refresh token, which is why
  neither probe run contacted a token endpoint at all.

**What's actually implemented vs. observed (2026-08-18).** The mechanism — adapter-produced
`InjectRule`s, merge-with-user-config precedence (§8.1), pre-flight re-validation, the placeholder
contract — is fully implemented and generic; it will translate ANY credential shape the vendor
table has an entry for. Both Anthropic shapes are now in it, each backed by a live observation
recorded in that file's PROVENANCE comment and pinned by a golden test:
- **`ANTHROPIC_API_KEY` on `api.anthropic.com`** (strip `x-api-key`/`authorization`, set
  `x-api-key`): observed live via `add-tls-mitm-credential-injection.md`'s hardware verification
  (`run_c654e575`), reused rather than re-run.
- **`CLAUDE_CODE_OAUTH_TOKEN` on `api.anthropic.com`** (strip both, set `authorization` to
  `Bearer ` + the secret): observed live on 2026-08-17 from two like-for-like MITM runs of the same
  task — `run_b408545b` with a genuine subscription token, `run_99bd261c` with a genuine API key.
  Same host, same `POST /v1/messages` path in both; the token goes on the wire **verbatim**
  (`credential_len` matched the secrets-file value's own length), so no exchange or refresh is
  involved. Metering follows the credential, which settles this section's earlier
  `(verify current)` on headless billing.

**Verified on hardware:** Claude Code accepts a *placeholder* on its OAuth path at startup, and the
whole run works end to end (`run_df97fffa`, 2026-08-18, with `mitm: false` control `run_10fc027d`).
It also does for `ANTHROPIC_API_KEY` (`run_c654e575` authenticated with a prefix-less placeholder,
which is also evidence the CLI does not validate credential format). See `HUMAN_TODO.md` for both
runs and their `proxy.log` lines.

**Recommended default, updated.** Translation means a subscription token no longer outlives the
run (it never enters the VM at all, and the proxy discards it at run teardown), so the
blast-radius argument for preferring a scoped API key over a subscription token (below) **softens**
under `mitm: true` with an observed shape — but it does **not disappear**: a compromised agent
still has unlimited *authenticated* access to every allowlisted host, and can spend the seat's
quota and rate budget, for the run's duration regardless of how the credential got attached. Prefer
a scoped API key for untrusted code either way; translation narrows the cost of a subscription
token's exposure, it doesn't remove the reason to be careful with one.

---

## 7. Run Lifecycle (Step by Step)

1. **Resolve spec** — merge flags + config file into a `RunSpec` (image, task, repo,
   network policy, secrets file, resources, env).
2. **Bundle code** — create a self-contained git bundle (parentless snapshot, or full history at
   `bundle_depth 0`; non-mutating temp-index capture if `include_dirty`) → byte stream (§6.7).
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
bundle_depth: 1                 # 1 = single-commit snapshot; 0 = full history (§6.7)

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
repo (pins the `krayt-dev` image, Claude model/effort, and this repo's network allowlist); with
no explicit `--config`, a run auto-loads `<repo>/krayt.yaml` if present (§8.3), so every
contributor gets the same starting point. It carries no secret material — `secrets:` only names a
path, and the file it points at is itself gitignored — so tracking it is safe; a real credential
must never be inlined into its `env:` block.

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

**Adapter-supplied injection and merge precedence (`inject-claude-oauth-token-at-proxy.md` §2/§4).**
An adapter's `Prepare` can return `Inject` rules of its own (§6.14's credential shape translation
is the first and, so far, only user of this) alongside the `Env` additions it already contributed.
Before any VM or image work:
- The adapter's `Inject` rules are **unioned** into `network.inject` from the config file/flags —
  `task.MergeInjectRules` — per host: a host the user never wrote a rule for gets the adapter's
  rule verbatim; a host the user already has a rule for is merged header-by-header. On a
  **conflict** (the same host AND the same header, matched case-insensitively, in either `set` or
  `set_literal`) **the user's explicit config wins** and the run logs which adapter-supplied
  header was overridden — a user who wrote their own `network.inject` rule for a host an adapter
  also manages is never silently second-guessed.
  `Strip` lists are unioned rather than conflict-checked (there's no "value" to disagree about); a
  user-supplied `refresh` block for a host likewise always wins over an adapter-supplied one.
- The **merged** set — adapter-supplied and hand-written rules alike — is re-run through the exact
  same `ValidateNetworkPolicy` pre-flight described above. An adapter rule naming a host outside
  `network.allow` (in `mode: allowlist`) fails the run before any VM boots, exactly as a typo'd
  hand-written rule would — an adapter's suggestion is never exempt from the check a human's
  config is held to.
- The merged set is what actually gets serialized into the egress-proxy child's stdin config
  (§6.6, §6.6.1) — adapter-supplied rules travel the identical path hand-written ones do; there is
  no second, adapter-only channel into the proxy.

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

The container **must** run as a **non-root** uid — this is now **enforced, not just a
convention** (§6.10, §10): an image whose `USER` is root (uid 0) or unset **fails the run**
with a clear error and never launches. Non-root is load-bearing, not cosmetic: dropped
`CAP_SETUID`/`SETGID` plus a non-root uid are jointly what stop the container from becoming
proxyd's uid and bypassing the egress allowlist (§6.6). Set a non-root `USER` in the image
(the reference images use `USER agent`, uid 1000). Some agents (Claude Code among them) also
refuse uid 0 independently.

An image that writes into its own rootfs — e.g. `$HOME` under `/home/agent` (nix profile,
`~/.claude`, Go caches) — is **incompatible with `container.readonly_rootfs: true`** (§8.1);
read-only rootfs is opt-in (default OFF) partly for this reason. When enabled, only `/tmp` and
`/run` are writable (ephemeral tmpfs); a writable tmpfs is never mounted over a populated dir.

**The `KRAYT_CA_CERT` contract (§6.6.1, only when `network.mitm: true`).** The guest writes the
run's ephemeral MITM CA's **public** certificate to `/run/krayt/ca.crt` (0644 — it is public,
never the private key), bind-mounts it read-only at that SAME path inside the container (so
`KRAYT_CA_CERT` resolves on both sides of the mount namespace, not just the guest's own), and
sets `KRAYT_CA_CERT=/run/krayt/ca.crt` plus best-effort `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`/
`NODE_EXTRA_CA_CERTS` pointing at it. This is a **no-op when `network.mitm` is false** — no file,
no mount, no env var, byte-identical to a run without the feature.

**The `KRAYT_INJECTED_CREDENTIAL` contract (§6.14, only when the adapter's selected credential is
named in `network.inject[].set`).** That credential is withheld from `SecretsBundle` entirely
(§6.6.1) — no `/run/secrets/<key>` file ever arrives for it — so a compliant entrypoint must not
require the file before starting. It decides it has a credential, in this order:

1. a `/run/secrets/<key>` file (ordinary delivery);
2. **a recognized credential env var that is already set** — krayt configures the container with a
   placeholder under the credential's own name (shape mirroring, §6.14), and the entrypoint must
   accept that value **as-is**, never overwriting it; this is the shape-translation path;
3. `KRAYT_INJECTED_CREDENTIAL` naming the (non-secret) key, for a krayt that set the name but no
   value — the pre-shape-translation contract, kept for compatibility.

The real value is attached to outgoing requests by the host proxy regardless of what the container
sends, so a placeholder only needs to satisfy the agent's own "a credential is configured" check.
Rule 2 is not optional: an entrypoint implementing only 1 and 3 exits `EX_CONFIG` (78) on every
shape-translated run — which is exactly what every krayt agent image did until 2026-08-18, and what
`hack/test-entrypoint-credentials.sh` now guards against.
A compliant entrypoint that wants MITM'd hosts to verify, **and** wants `passthrough` hosts (which
see the real upstream, not krayt's CA) to keep verifying too, must:
- Check `KRAYT_CA_CERT` is set and non-empty before doing anything distro-specific.
- For Go/OpenSSL-based tools, `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE` **replace** the system trust
  store rather than appending to it — pointing them at `KRAYT_CA_CERT` alone would break
  verification for every `passthrough` host. Concatenate the image's own distro CA bundle (e.g.
  `/etc/ssl/certs/ca-certificates.crt` on the Debian-based reference images) with `$KRAYT_CA_CERT`
  into one file and point both vars at **that** instead.
- `NODE_EXTRA_CA_CERTS` is genuinely additive, so it can point at `$KRAYT_CA_CERT` directly with
  no concatenation. **Node does not read the system trust store at all**, which is why this is
  required, not optional, for every one of the three current reference agent images
  (`claude-code`, `gemini-cli`, `opencode`) — all node-based.
- Do all of this only when `KRAYT_CA_CERT` is set, so a `mitm: false` run's entrypoint behavior
  is unchanged.

Completion = container process exit. Exit code is surfaced in `meta.json`.

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
| `secrets:` | contained: honored only if the resolved path stays inside the repo root | honored | host file read, shipped into the guest as the run's `SecretsBundle` |
| `task:` | contained: honored only if the resolved path stays inside the repo root | honored | host file read, shipped into the guest as the run's prompt |
| everything else (`image`, `network.mode: allowlist\|none`, `network.allow`, `agent`, `env`, `resources`, `questions`, `include_dirty`, `bundle_depth`, `container.readonly_rootfs`) | honored | honored | configures the run without redirecting what krayt reads/writes on the host or relaxing the container's confinement |

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
host file into the guest, as the run's `SecretsBundle` or as its prompt (which the agent can then
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
Every run produces a self-contained directory the human reviews from:

```
.krayt/runs/<id>/
├── changes.patch     # git diff vs the recorded krayt-baseline (primary deliverable; §6.7)
├── commits.bundle    # optional: reverse range bundle of the agent's commits (§6.7), if returned
├── report.md         # human-readable summary (see below)
├── meta.json         # machine-readable run record (schema below)
├── secret-scan.json  # optional: present only if a secret value appears in changes.patch (§6.8)
├── proxy.log         # host-side egress proxy child's redacted stdout/stderr (§6.6, §6.8)
├── questions/        # one <qid>.json per agent question + its answer (§6.13), if any
└── logs/
    ├── agent.log     # container stdout/stderr (merged, timestamped)
    ├── console.log   # guest-agent's own stdout/stderr, incl. krayt-vsock-forward (§6.6)
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
  "error": ""
}
```

`provenance` records what source the run was based on (§6.7): `head_sha` is the real, checkoutable
`git rev-parse HEAD` at bundle time (empty for an unborn HEAD); `bundle_sha` is the commit actually
imported as `krayt-baseline` and diffed against for `changes.patch` — equal to `head_sha` only in
the full-history/no-dirty case, synthetic otherwise. `bundle_depth`/`include_dirty` are the request
flags that determine whether that equality is expected, and `bundle_digest` is a
`opencontainers/go-digest` hash of the exact bundle bytes streamed to the guest.

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

Secret **values** never appear in `report.md`, `meta.json`, the question records, or
`secret-scan.json` — the guest redacts the report and question text and scans (not redacts) the
patch (§6.8). The one exception is `changes.patch` itself: it is left byte-exact so `git apply`
works, so a secret an agent wrote into a tracked file *is* present there. When that happens the
guest emits `secret-scan.json`:

```json
{ "patch_contains_secret_keys": ["ANTHROPIC_API_KEY"] }
```

— naming the matched secret **keys only** (never the values) — and the host adds a **Safety**
warning per key to `report.md`/`meta.json` (e.g. *"changes.patch contains the value of secret
ANTHROPIC_API_KEY — review before applying"*), so the human catches it before applying. `krayt
ls` reads `meta.json`; `krayt patch`/`apply` read `changes.patch`.

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
│   │   ├── proxy/           # simplified L3 lock (loopback-only) + the guest forwarder's Controller (§6.6)
│   │   ├── ask/             # in-VM question bridge + ask_human MCP server (§6.13)
│   │   └── runner/          # containerd Go client (single container per VM)
│   ├── proxy/               # host-side L7 egress allowlist proxy (`krayt __egress-proxy`, §6.6)
│   ├── adapter/             # optional per-agent adapters (claude-code, gemini-cli, opencode); MCP/CLI wiring (§6.13)
│   ├── task/                # config schema + parsing
│   ├── patch/               # git bundle create/verify/clone/diff (+ optional reverse bundle); non-mutating dirty capture; host-side apply helpers (§6.7)
│   ├── imagestore/          # host pull + OCI export + digest-keyed cache (§6.11)
│   └── secrets/             # secrets loading + redaction
├── cmd/krayt-ask/main.go    # tiny in-container CLI front-end for ask_human (§6.13)
├── cmd/krayt-vsock-forward/ # guest-side parse-nothing TCP<->vsock pipe to the host proxy (§6.6)
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
| Linux VM backend (Phase 7) | *none — hand-rolled REST client over Firecracker's API unix socket* | **decided in Phase 7, superseding `firecracker-go-sdk`.** The SDK's last tagged release is v1.0.0 (Aug 2022) — using it means pinning a `main` pseudo-version — and it drags `go-openapi` + CNI/containernetworking into krayt's `go.mod`, which `buildGoModule` then vendors into the guest image (§11.1) for no runtime benefit. The API surface krayt needs is six `PUT`s (`/machine-config`, `/boot-source`, `/drives/{id}`, `/network-interfaces/{id}`, `/vsock`, `/actions`); driving it directly mirrors what the vfkit provider already does with vfkit's REST API and **adds no new dependencies at all**, so the guest image's `vendorHash` is unchanged. Verified against the Firecracker v1.16.1 API spec. |
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
| Repo ingest | git bundle cloned in-guest — source `.git/hooks` are never executed or imported, and the guest commits under a throwaway krayt bot identity. The workspace `.git` is left container-writable (so the agent can commit) but is **never trusted by the root guest-agent's git**: patch generation runs against a root-only `patchgit` snapshot with `core.fsmonitor`/`core.hooksPath` force-cleared and `--no-textconv`, so container-written `.git/config`/hooks/attributes cannot execute as root (§6.7, finding #2) |
| Network egress | Default-deny + allowlist proxy, per-task opt-in to widen — **enforced host-side** since `move-egress-proxy-to-host.md`. The L7 allowlist proxy is a separate HOST process reached over a guest-initiated vsock channel; the guest's L3 lock is loopback-only and keys on **no uid at all**, so there is no container-hardening dependency left for it to bypass (§6.6) |
| Container privileges | **All Linux capabilities dropped** by default (validated, denylisted opt-in only); **enforced non-root** (uid-0 image fails the run); containerd **seccomp** profile applied; `NoNewPrivileges=true`; read-only rootfs available as a per-task opt-in (§6.10, §8.1) |
| Secrets | tmpfs only, never on disk, destroyed with VM; **redacted in the guest** from live logs, `report.md`, and `ask_human` prompt/choices. `changes.patch` is **scanned, not redacted** (redacting hunks would break `git apply`); a hit surfaces as a Safety warning naming the key only (§6.8, §8.4) |
| TLS MITM / credential injection | **Opt-in, default off** (`network.mitm`, §6.6.1). An injected secrets-file key is **withheld from `SecretsBundle` entirely** — the container never holds it, closing "Auth-credential blast radius" (below) for that credential. Ephemeral per-run CA, in memory only, private key never exported. Trades a HOST-process compromise for a *stronger* claim than plain egress enforcement: the proxy process now also holds real user credentials, not just a policy decision (residual below) |
| Run configuration (`krayt.yaml`) | **Split by provenance** (§8.3, whose table is the full field-by-field boundary): an `--config <path>` the operator named is honored in full; a `<repo>/krayt.yaml` auto-loaded from the repo under test is untrusted input and may configure a run but **not write its security policy, redirect what krayt reads or writes on the host, or relax the container's confinement**. Refused with an error: `network.mitm`, `network.inject`, `network.passthrough`, `network.mode: full`, `repo:`, `container.capabilities`, `container.seccomp: unconfined`. Contained to the repo root (no absolute path, no `..` escape, no symlink resolving out): `secrets:`, `task:`. Without this split a poisoned repo could turn on MITM and name the operator's own secrets-file key as the credential injected into an attacker-controlled host, bundle a *different*, private repo into the VM for the agent to read, read an arbitrary host file in as the run's prompt, or hand the container back the capabilities and seccomp profile §8.1 takes away — with every consistency check passing, because the file is only ever compared against itself |
| Persistence | CoW disk destroyed on teardown; fresh VM per run |
| Patch application | Always manual; human reviews diff before `git apply` |

**Residual considerations to document:**
- Proxy-bypass via raw sockets is caught by the default-deny, loopback-only L3 lock (§6.6). The
  original historical gap (finding #1) was **uid assumption**: with the L7 proxy in-guest and
  the L3 lock keyed on `skuid "proxyd"`, a container that kept `CAP_SETUID` could `setuid()` to
  proxyd (uid learned from `/proc/net/tcp` in the shared netns) and satisfy the `skuid` accept —
  bypassing the allowlist entirely. That was closed once (hardened OCI spec dropping
  `CAP_SETUID`/`SETGID` and enforcing non-root, §6.10) and is now closed a second, independent
  way by `move-egress-proxy-to-host.md`: the guest chain no longer has a `skuid` rule *at all* —
  egress-worthy content the container could send is either loopback (permitted, reaches only the
  parse-nothing forwarder) or dropped, full stop, with no identity for a capability regression to
  exploit. Regression-guarded by the `egressRuleset`-shape unit test (now also asserting `skuid`
  is *absent*) and, on hardware, by the `setuid(proxyd)=EPERM` + allowlist-enforcement
  integration tests — the `EPERM` assertion is kept as a still-useful non-root regression check,
  it just no longer doubles as the egress-bypass's load-bearing proof.
- Container-runtime / guest-kernel bugs — blast radius minimized by the least-privilege OCI
  spec (dropped caps, seccomp, no-new-privs, non-root) inside the already-isolated VM (§6.10).
- Malicious patch content (e.g. `.git/hooks`, build scripts) applied on the **host** — the
  source repo's hooks are already never run in-guest, and now the *guest's own* root git no
  longer trusts container-written `.git` config/hooks/attributes either (patch generation is
  isolated in the root-only `patchgit`, §6.7 / finding #2). What remains is that the emitted
  `changes.patch` could still add files like `.git/hooks/*` or build scripts that run on the
  **host** after apply; reviewing the diff before `git apply` is the control, and a
  `--strip-hooks` / lint pass on patches is a possible future addition.
- **Egress enforcement is host-side now — a different trade, not a strictly safer one.**
  `move-egress-proxy-to-host.md` moved the L7 allowlist proxy out of the VM entirely; this
  supersedes the old "single-layer, in-guest only" residual (the allowlist was previously
  enforced *entirely inside* the VM, with the host applying no filtering at all — that is no
  longer true). The honest statement of the new trade: a **guest-root escape can no longer
  defeat the allowlist** by flushing guest nftables or assuming a guest uid, because the L7
  decision does not live in the guest anymore — but a **compromise of the proxy process itself
  is now a HOST compromise**, not an escape from a disposable VM that was about to be destroyed
  anyway. This is why the proxy is a **separate process** (self-exec, not linked into the run
  supervisor) that **parses nothing it does not have to** — the guest's own forwarder
  (`krayt-vsock-forward`) is a byte-for-byte pipe with zero parsing surface, so the entire
  adversarial-input attack surface concentrates in one host process running the hand-rolled (or
  swapped-in, `KRAYT_EGRESS_PROXY_BIN`) `internal/proxy` package, rather than being spread across
  a guest process *and* a uid-keyed firewall rule whose correctness depended on container
  capabilities. The vsock channel this process is reached over additionally gives **concurrent-VM
  isolation by construction**: each VM gets its own host unix socket (vfkit) or `uds_path`
  (Firecracker), so one run's egress channel is not reachable from another run's VM — unlike a
  gateway-bound TCP proxy would be on vfkit/vmnet's shared-segment NAT (§6.6); asserted on
  hardware by `TestConcurrentRealVMs`. The container-hardening controls (dropped
  `CAP_SETUID`/`CAP_SETGID`, enforced non-root, seccomp, isolated `patchgit` patch generation —
  §6.10, §6.7) remain real defense-in-depth for everything else in this table, but are no longer
  what the egress allowlist's correctness depends on.
- Secret redaction coverage — the guest redacts every artifact it can safely rewrite (live
  logs, `report.md`, `ask_human` prompt/choices, §6.8). Three known, accepted gaps: (1) live-log
  redaction is chunk-oriented, so a secret value split across two log chunks is not caught — it
  affects only the streamed logs, since `report.md` and the patch are scanned as whole buffers;
  (2) `changes.patch` is left byte-exact (redacting hunks would break `git apply`), so a secret
  an agent wrote into a tracked file *is* present there — this is surfaced, not hidden, via the
  `secret-scan.json` marker and a per-key Safety warning for the human's pre-apply review (§8.4);
  (3) **redaction matches the secret value verbatim, so an agent that transforms it before
  emitting it is not caught** — masking the middle, chunking it across lines, or encoding it all
  defeat an exact-match `Redactor`. This is not hypothetical: Phase 9's `mitm: false` regression
  run (`run_117d6f75`) hit it with a *cooperative* agent, which masked the middle of the API key
  on its own initiative before writing the report. No exact match existed, so the `Redactor`
  never fired at all, and a 19-character key prefix persisted in `report.md` and the agent log.
  This limit is inherent to output filtering: it defends against accidental verbatim disclosure,
  not against a container that holds the secret and chooses how to emit it — an agent set on
  exfiltration can always encode it through an allowlisted channel. The structural fix for a
  credential is to never give it to the container: `network.mitm` + `network.inject` (§6.6.1),
  whose companion run (`run_c654e575`) had no credential in the VM to leak in any form.
- Resource exhaustion — bounded by per-VM CPU/mem/disk + wall-clock timeout.
- Auth-credential blast radius — a subscription token (`CLAUDE_CODE_OAUTH_TOKEN`) is tied to
  a personal/seat plan and is less granularly revocable than a scoped API key; exposing one
  to untrusted code risks that seat's consumption and rate budget. Prefer a scoped,
  independently-revocable API key for untrusted runs (§6.14). **Fully closed for an *injected*
  credential** (`network.mitm` + `network.inject`, §6.6.1) — it never enters the VM, so there is
  nothing there for a compromise to steal; this residual is otherwise unchanged for any
  credential still delivered via `SecretsBundle` (the default, and the only option for anything
  that isn't a bare HTTP header). **Softened further, not eliminated, by credential shape
  translation** (`inject-claude-oauth-token-at-proxy.md`, §6.14): when `mitm` is on and the
  selected shape is observed, a subscription token no longer outlives the run at all (it's
  discarded with the proxy process at teardown, same as an injected API key) — but a compromised
  agent can still spend that seat's quota and rate budget for the run's duration either way, so
  "prefer a scoped API key for untrusted code" still stands; translation narrows the exposure
  window, it doesn't remove the reason to be careful with a subscription token.
- **Placeholder shape, one per credential kind** (`sk-ant-krayt-placeholder-do-not-use`,
  `sk-ant-oat01-krayt-placeholder-do-not-use`). No client-side format check is known to exist:
  `run_c654e575` authenticated fine with the entrypoint's prefix-less
  `krayt-injected-at-host-proxy`, so the real thing's prefix is carried as cheap insurance, not to
  satisfy a demonstrated requirement — the rest of each string is deliberately
  human-legible-as-fake. The OAuth path validates neither path's credential format either: the
  `run_df97fffa` hardware run authenticated with the entrypoint's own prefix-less
  `krayt-injected-at-host-proxy` placeholder, same as the API-key path did. If a future probe (or a
  different vendor) forces a
  stricter-looking placeholder, that requirement is itself a finding worth recording here, because
  a placeholder forced to look more like a real credential is more likely to be mistaken for one by
  a human reading a log.
- **Accepted maintenance dependency on Anthropic's wire format** (`inject-claude-oauth-token-at-proxy.md`).
  Credential shape translation makes krayt responsible for tracking exactly what headers/endpoints
  Claude Code's API-key and subscription paths use — a fact that can change without notice on
  Anthropic's side and silently break every translated run until caught. The golden test
  (`TestAnthropicWireRulesGolden`) is offline and cannot detect that change itself — it only pins
  `internal/adapter/anthropic_wire.go`'s table against its own literal, so it fails if the two
  drift apart, not if Anthropic's live behavior does. A live failure or a re-probe is what surfaces
  the actual change; this is accepted, not incidental: the mitigation is confining every such fact
  to one dated, golden-tested file so that once a change is found, fixing it is "update one table,
  update the golden literal alongside it — the diff IS the changelog" — not "re-understand the
  proxy". `network.mitm: false` and non-translated credentials are entirely unaffected by a
  wire-format change; the dependency is scoped to the opt-in translation path only.
- **TLS MITM / credential injection — an honest trade, not a strict improvement** (§6.6.1,
  `add-tls-mitm-credential-injection.md`). Opt-in and off by default, but when on:
  - **It removes credential *theft*, not credential *use*.** The proxy cannot distinguish an
    agent-initiated request from a legitimate one — a compromised agent still has unlimited
    *authenticated* access to every allowlisted host for the run's duration. This converts
    exfiltration into a confused deputy (dies with the VM, unlike a stolen key) but is not "no
    risk".
  - **It only covers HTTP-shaped credentials.** An SSH key, a signing key, or anything a tool
    computes over cannot move to the proxy; those still ride `SecretsBundle` unchanged.
  - **It moves the adversarial parser outside the blast-radius boundary a second time.** A proxy
    compromise before this task bought unrestricted egress from a VM about to be destroyed; after
    it, the same compromise buys code execution in the one host process holding the user's real
    credentials. Go's memory safety helps; request-smuggling and header-confusion bugs do not
    care — mitigated, not eliminated, by the hostile-input rules in §6.6.1 (inner-Host/authority
    match, bounded headers, fail-closed on any MITM setup failure, never a silent tunnel
    fallback) and by keeping the CA private key in memory only, behind an accessor that returns
    only the public certificate.

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
- [x] **Done when:** on a real Mac (with vfkit installed), `krayt` boots the published image and a `Hello` RPC round-trips host↔guest over the vfkit vsock socket. **[HUMAN: boot test on real hardware]**

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
- [x] **Done when:** the Phase 2 end-to-end test passes unmodified on a Linux host via the firecracker provider. ✅ **Verified on real hardware** (GCP VM, nested virt, Intel VT-x): `TestEndToEndRealVM` — the Phase 2 test, body and assertions byte-identical, with only the provider construction swapped — boots the x86_64 image under Firecracker v1.16.1, streams in the image + repo bundle, runs the agent container, and returns a `changes.patch` that `patch.Apply` lands cleanly on a fresh clone (exit 0). Also green: `TestBootHello` (`Hello` round-trips over the vsock handshake), `TestGuestNetwork`, and `TestConcurrentRealVMs` (3 simultaneous VMs, unique taps/CIDs, patches provably not crossed, every tap reaped on teardown).
- [x] **The Phase 3 security suite also re-verified on Linux** (not required by the "Done when", but the claim worth having before anyone runs untrusted code on this backend): `TestEgressEnforcement`, `TestContainerHardening`, `TestRootImageFailsClosed`, `TestGuestGitConfigInjectionInert`, `TestSecretConfinementInArtifacts` — all green against firecracker. The two that matter: a non-allowlisted host is refused by the proxy **and** a raw socket that ignores the proxy is dropped by nftables (`1.1.1.1:443` → timeout), while `setuid(proxyd)` fails `EPERM` — so the finding-#1 egress bypass is closed on this backend too. This required writing `hack/netprobe`, which the spec assumed existed but which had never been committed.

### Phase 8 — Host-side egress proxy, step 1 (`move-egress-proxy-to-host.md`) ✅
Step 1 of the three-step host-side-proxy arc (`docs/ai-tasks/README.md`). Moves the L7
allowlist proxy off the guest entirely, behind a new guest-initiated vsock channel — a
behavior-preserving, security-strictly-improving change for the container (§6.6).

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
