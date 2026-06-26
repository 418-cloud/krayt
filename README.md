# krayt

Run AI coding agents in disposable, isolated micro-VMs — they work on a copy of your repo
and hand back a reviewable git patch.

> A `krayt` is the sealed crate you drop an agent into: it works inside, you take the result
> out, and the crate is destroyed. No live filesystem share with the host; the only thing
> that flows back is a diff you review before applying.

**Full design:** see [`KRAYT_SPEC.md`](./KRAYT_SPEC.md). This README only covers *what you
need on your machine* and *how to get started*. Architecture and rationale live in the spec.

> ⚠️ **Version-sensitive lines below are marked _(verify current)_.** Tool versions and
> formula names drift — confirm against each tool's current docs (or just run
> `krayt doctor`, which is the source of truth) rather than trusting a pinned number here.

---

## Platform reality (read first)

krayt is a **macOS / Apple Silicon** project for v1. The macOS VM provider (vfkit) and **all
integration tests require real Apple-Silicon hardware** — they cannot run in a cloud agent
or CI, because Apple's Virtualization.framework only runs on Apple hardware. The Linux
backend (Firecracker) is designed for but not built in v1.

What this means in practice:
- The OS-agnostic core (most of the codebase) builds and unit-tests anywhere via a fake VM provider.
- Anything that boots a real VM — the vfkit provider, the image boot test, end-to-end runs —
  must happen on your Mac.

---

## Prerequisites

There are **three tiers**. Most contributors only need the first. Tiers 2 and 3 are
provided by a Nix dev shell, so in practice you install **Go, vfkit, and (optionally) Nix**
— everything else comes from `nix develop`.

### 1. Build & run krayt (everyone)
- **macOS 13+** on Apple Silicon — _(verify current; vfkit needs 12+, some features 13/14+)_
- **Go** — current stable _(verify current)_
- **vfkit** — `brew install vfkit` _(verify current formula name)_. Carries the
  virtualization entitlement, so **krayt itself needs no code-signing**.
- **git**
- **Claude Code** — if you're driving development with the agent (see below)

> No `protoc` here — generated protocol code is checked into the repo.

Run `krayt doctor` after setup; it verifies host prerequisites (vfkit present + runnable).

### 2. Regenerate protocol code (only when editing `internal/protocol/krayt.proto`)
Provided by the dev shell — no per-tool installs:
```bash
nix develop          # provides protoc, protoc-gen-go, protoc-gen-go-grpc, buf, oras (pinned)
make proto           # regenerate; commit the result alongside the .proto
```
If you'd rather not use Nix, install `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc`
(or `buf`) yourself — see links below — but the pinned dev shell is recommended so plugin
versions can't drift.

### 3. Build the VM image (CI / image maintainers only)
The minimal Linux micro-VM image is a Nix flake under `images/`, built and published as an
OCI artifact (see `KRAYT_SPEC.md` §11). This is **owned by CI (or a human), not by Claude
Code** — building/boot-testing needs a Linux builder and real hardware.
- **arm64 Linux runner** (GitHub Actions)
- **Nix** (CI uses the Determinate Systems action; see links)
- **`oras`** — provided by the dev shell
- **Registry credentials** for publishing the image artifact

> **You do NOT need** `containerd`, `runc`/`crun`, or `nftables` on your Mac — those live
> *inside* the Nix-built VM image, not on the dev machine. Don't `brew install` them.

### Installing the tools
Links are canonical landing pages (they rarely move); prefer the command where given.
All marked _(verify current)_ — confirm against the linked page, since names/versions drift.

| Tool | Install | Reference _(verify current)_ |
|---|---|---|
| Go | platform installer | https://go.dev/doc/install |
| vfkit | `brew install vfkit` | https://github.com/crc-org/vfkit |
| Nix | `curl -fsSL https://install.determinate.systems/nix \| sh -s -- install` | https://determinate.systems/nix-installer/ — or the community installer at https://nixos.org/download |
| Claude Code | per docs | https://docs.claude.com/en/docs/claude-code/overview |
| protoc | via `nix develop`, else manual | https://protobuf.dev |
| buf (alt to protoc) | via `nix develop`, else manual | https://buf.build |
| protoc-gen-go | `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest` | https://protobuf.dev/reference/go/go-generated/ |
| protoc-gen-go-grpc | `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest` | https://grpc.io/docs/languages/go/quickstart/ |
| oras | via `nix develop`, else manual | https://oras.land |

> CI Nix install uses the GitHub Action `DeterminateSystems/determinate-nix-action`
> (or `nix-installer-action`) — see https://github.com/DeterminateSystems/nix-installer.

---

## Quick start (development)

```bash
git clone <your-fork> krayt && cd krayt
# tier-1 prereqs installed? confirm:
go build ./...        # OS-agnostic core + macOS provider compile
go test ./...         # unit tests via the fake VM provider (no real VM needed)
go run ./cmd/krayt doctor

# only if you need to regenerate protocol code or build the image:
nix develop           # drops you into a shell with protoc/buf/oras pinned
```

To actually boot a VM you also need a published base image (`krayt image pull`) — see
`KRAYT_SPEC.md` §11. That artifact comes from the tier-3 CI build.

---

## Repo orientation

| File | What it is |
|---|---|
| `KRAYT_SPEC.md` | The complete implementation spec — architecture, protocol, phases, acceptance criteria. The source of truth. |
| `CLAUDE.md` | Working agreement Claude Code reads each session (rules, phase discipline, handoff protocol). |
| `HUMAN_TODO.md` | Handoff log the agent maintains for steps a human must do (created during development). |
| `images/` | Nix flake for the micro-VM image (CI-built). |
| `internal/` | The implementation (see §9 of the spec for package layout). |

### Steps a human is expected to own (the `[HUMAN]` handoffs)
Claude Code does everything it can, then logs these to `HUMAN_TODO.md` and pauses if blocked:
- **Install vfkit** (`brew install vfkit`) — trivial, scriptable.
- **Run CI to build/publish the VM image** + provide registry credentials.
- **Run the boot test** on your Mac (vfkit boots the image → `Hello` round-trips).
- **Provide live API keys** to exercise a real agent image.

---

## Driving development with Claude Code

Recommended: develop with **Claude Code in the terminal on your Mac** — the laptop is the
target platform, so it's the only place the macOS-specific code can be built and tested.
The OS-agnostic phases can optionally be offloaded to Claude Code on the web (it PRs to
GitHub), but all VM/integration work comes back to the Mac.

Work **one phase at a time**, using each phase's "Done when" criterion in the spec as the
gate. A good kickoff prompt:

```
Read KRAYT_SPEC.md in full, then implement Phase 0 only.
First give me a short plan (files, §9.1 deps, how you'll meet Phase 0's "Done when");
wait for my OK before writing code. Follow CLAUDE.md / §14 and maintain HUMAN_TODO.md.
Do not start Phase 1.
```

See `CLAUDE.md` for the full working agreement.

---

## Status

Built phase by phase per `KRAYT_SPEC.md` §14.

- **Phase 0 — Foundations:** done. Provider/protocol/types scaffold, `fakeProvider`
  loopback, `krayt doctor`; `Hello` round-trips over the fake provider.
- **Phase 1 — Boot a VM on macOS:** done. vfkit provider, host control client +
  boot-readiness, base-image pull + digest verify, `krayt-agent` vsock binary, Nix image
  flake, and CI. Verified on real Apple-Silicon hardware: `krayt` boots the published
  image and a `Hello` RPC round-trips host↔guest over the vfkit vsock socket.
