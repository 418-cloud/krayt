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

krayt runs on **macOS / Apple Silicon** (vfkit) and on **Linux / KVM** (Firecracker), behind one
`Provider` interface. Everything above that seam — orchestrator, control protocol, guest agent,
patch generation, secrets, egress policy — is the same code on both.

What this means in practice:
- The OS-agnostic core (most of the codebase) builds and unit-tests anywhere via a fake VM provider.
- **macOS integration tests need real Apple-Silicon hardware.** They cannot run in CI or a cloud
  agent, because Apple's Virtualization.framework only runs on Apple hardware.
- **Linux integration tests need any host with `/dev/kvm`** — a bare-metal box, or a cloud VM with
  nested virtualization enabled. They *can* run in CI.

**Prebuilt binaries.** Each release (see `RELEASING.md`) publishes `krayt` for **darwin/arm64**
and **linux/amd64** — the two tested targets (Apple Silicon/vfkit and Linux-KVM/Firecracker,
matching what's actually verified on hardware) — and **darwin/amd64**, which compiles and
*should* run on Intel Macs via Virtualization.framework but is **not tested**. There is no
**linux/arm64** build yet: the base VM image is backend-tagged, not just arch-tagged (vfkit needs
a PE `Image`, Firecracker an uncompressed ELF `vmlinux`), and only the vfkit-formatted
`linux/arm64` variant is published today — a `linux/arm64` `krayt` would resolve to it and fail to
boot under Firecracker, so it's left unshipped rather than shipped broken. Verify a download
against the release's `checksums.txt`.

---

## Prerequisites

There are **three tiers**. Most contributors only need the first. Tiers 2 and 3 are
provided by a Nix dev shell, so in practice you install **Go, vfkit, and (optionally) Nix**
— everything else comes from `nix develop`.

### 1. Build & run krayt (everyone)

Common to both platforms:
- **Go** — current stable _(verify current)_
- **git**
- **Claude Code** — if you're driving development with the agent (see below)

**On macOS:**
- **macOS 13+** on Apple Silicon — _(verify current; vfkit needs 12+, some features 13/14+)_
- **vfkit** — `brew install vfkit` _(verify current formula name)_. Carries the
  virtualization entitlement, so **krayt itself needs no code-signing**.

**On Linux:**
- **KVM** — `/dev/kvm` must exist *and be writable by you*. Add yourself to the `kvm` group
  (`sudo usermod -aG kvm $USER`) and then **start a new login session** — group membership only
  takes effect at login, so an existing shell keeps getting "permission denied". On a cloud VM,
  make sure nested virtualization is enabled.
- **firecracker** — download a release from
  [firecracker-microvm/firecracker](https://github.com/firecracker-microvm/firecracker/releases)
  and put it on your `PATH`.
- **One-time host setup:** `sudo hack/linux-net-setup.sh`. Firecracker, unlike vfkit, has no
  built-in NAT device or DHCP server, so krayt has to create and address a tap device per VM.
  The script grants krayt `CAP_NET_ADMIN` as a **file capability** (so krayt does *not* run as
  root), enables IP forwarding, and installs krayt's NAT/forward rules as `krayt-nat.service` so
  they survive a reboot. It does not loosen the guest's egress policy — what a container may
  reach is still enforced inside the VM.

Run **`krayt doctor`** after setup on either platform; it checks each of the above and tells you
exactly what to do about anything missing.

> No `protoc` here — generated protocol code is checked into the repo.

> **Filesystem tip (Linux):** krayt gives each VM a copy-on-write clone of the base rootfs. On
> **XFS or Btrfs** that is a reflink — instant, and it costs no disk. **ext4 has no reflink
> support**, so the clone falls back to a full ~2 GiB copy per VM. Everything works either way,
> but putting `~/.cache/krayt` on XFS/Btrfs makes runs start faster and use far less disk.

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
go build ./...        # OS-agnostic core + this host's VM provider (vfkit on macOS, firecracker on Linux)
go test ./...         # unit tests via the fake VM provider (no real VM needed)
go run ./cmd/krayt doctor

# only if you need to regenerate protocol code or build the image:
nix develop           # drops you into a shell with protoc/buf/oras pinned
```

To actually boot a VM you also need a published base image (`krayt image pull`) — see
`KRAYT_SPEC.md` §11. That artifact comes from the tier-3 CI build.

---

## Running an agent

With a booted base image (`krayt image pull`) and an agent container image, a run looks like:

```bash
# the agent works on a copy of the repo, returns a patch you review
krayt run --image <agent-image> --task ./task.md --repo . \
  --secrets ./secrets.env --allow api.anthropic.com

# or pipe the prompt in headlessly (--task -) instead of a file:
echo "fix the flaky test in internal/foo" | krayt run --image <agent-image> --task - --repo .

krayt ls                      # states: starting → running → (waiting) → done
krayt patch <run-id>          # inspect the diff …
krayt apply <run-id>          # … then apply it to your repo if you're satisfied
```

- **Agent auth** rides the per-task secrets file (`--secrets`), lands on tmpfs at
  `/run/secrets`, and is redacted from logs. With `--agent claude-code` the adapter enforces
  exactly-one credential (`ANTHROPIC_API_KEY` xor `CLAUDE_CODE_OAUTH_TOKEN`) and fails fast
  before booting (§6.14).
- **Ask-the-human:** add `--on-question=wait` and the agent can pause to ask you a question
  (via the `ask_human` MCP tool or the `krayt-ask` CLI); resolve it with
  `krayt answer <run-id> <response>` from any terminal. Default `--on-question=fail` keeps
  unattended runs non-blocking.
- **Park & walk away:** add `--detach` and the run survives the terminal closing — track it
  with `krayt ls`/`attach` and answer questions later.
- **Resource preflight (macOS):** before booting, `krayt run` checks live host free RAM/disk
  against `--memory`/`--disk` plus a safety margin and refuses to start (no VM created) if the
  host can't afford it — so an oversubscribed host fails fast instead of dying opaquely mid-run.
  Pass `--skip-resource-check` to bypass.
- Flags can live in a `krayt.yaml` instead (see `configs/`); each run leaves a self-contained
  `.krayt/runs/<id>/` with `changes.patch`, `report.md`, `meta.json`, and logs.
- **Disk cache.** Base VM images and agent images are cached on the host under `<user-cache-dir>/krayt/`
  (`~/.cache/krayt/` on many Linux distros; `~/Library/Caches/krayt/` on macOS), in `vmimage/` and `imagestore/`, keyed by digest — a multi-GB agent image
  rebuilt on every commit accumulates there.
  `krayt image rm <digest>` drops one, and `krayt image prune` bulk-reclaims (keeping the pinned
  base image and anything a running run still needs). VMs themselves are fully ephemeral, so this
  host cache is the only thing that grows.

Reproducible, ready-to-run examples live under `hack/` — most notably `hack/claude-code/`
(a real Claude Code agent) and `hack/krayt-ask-probe/` (the question channel).

### Shell completion

krayt ships tab-completion for your shell. Load it once:

```sh
# bash (needs Homebrew bash-completion@2, or source it from ~/.bashrc)
krayt completion bash > "$(brew --prefix)/etc/bash_completion.d/krayt"
# zsh (macOS default shell)
krayt completion zsh > "${fpath[1]}/_krayt"
# fish
krayt completion fish > ~/.config/fish/completions/krayt.fish
```

Completion covers command and flag names (static), plus **dynamic** values read from the
host on demand:

- **`<run-id>`** for `apply`/`logs`/`attach`/`stop`/`rm`/`patch`/`questions`/`answer`, each
  filtered to the runs that command can act on (e.g. `stop` offers only live runs, `rm` only
  finished ones unless `--force` is set) and annotated with the run's state and image.
- **`<question-id>`** for `answer`, from the run's pending questions.
- **`<digest>`** for `image rm`, from the cached images in both cache roots (full digest as the
  completion value), annotated with each image's kind and size (and `(pinned)` for the base image).
- **`--net`/`--on-question`/`--on-question-timeout`/`--agent`/`questions --sort`** — their exact
  fixed value sets.
- **`--image`/`--allow`** for `run`, drawn from this repo's own run history (merged with a small
  set of well-known egress domains for `--allow`).

Repo-scoped completions read the same `.krayt/` state the commands do, so they honor `--repo`
(default `.`).

---

## Repo orientation

| File | What it is |
|---|---|
| `KRAYT_SPEC.md` | The complete implementation spec — architecture, protocol, phases, acceptance criteria. The source of truth. |
| `CLAUDE.md` | Working agreement Claude Code reads each session (rules, phase discipline, handoff protocol). |
| `HUMAN_TODO.md` | Handoff log the agent maintains for steps a human must do (created during development). |
| `SECURITY.md` | Threat model pointer + how to privately report a vulnerability. |
| `CONTRIBUTING.md` | How to get set up, code/commit conventions, and what a PR should include. |
| `images/` | Nix flake for the micro-VM image (CI-built). |
| `internal/` | The implementation (see §9 of the spec for package layout). |
| `cmd/` | Binaries: `krayt` (CLI), `krayt-agent` (guest), `krayt-proxy` (egress), `krayt-ask` (question front-end + MCP server). |
| `configs/` | Example `krayt.yaml` + default allowlist. |
| `hack/` | Reproducible demo/probe images used to verify features on hardware (`claude-code` agent, `ask-probe`, `krayt-ask-probe`). |

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

Built phase by phase per `KRAYT_SPEC.md` §14. **All eight phases (0–7) are complete and verified
on real hardware on both backends**, released as
[`v0.5.0`](https://github.com/418-cloud/krayt/releases/tag/v0.5.0) — krayt runs a real coding
agent (Claude Code) in an isolated micro-VM over an untrusted repo and hands back a reviewable
patch, with egress control, secrets, concurrency, park-and-walk-away, and an agent↔human question
channel, on **both** macOS/vfkit and Linux/firecracker behind the same `Provider` interface. See
`CHANGELOG.md` for the full release history.

| Phase | What | State |
|---|---|---|
| 0 — Foundations | provider/protocol scaffold, `fakeProvider`, `doctor` | ✅ |
| 1 — Boot a VM on macOS | vfkit provider, vsock guest-agent, image pull; `Hello` round-trips | ✅ hardware |
| 2 — End-to-end run | bundle → clone → agent edit → `changes.patch` that applies cleanly | ✅ hardware |
| 3 — Security & limits | egress allowlist proxy + nftables lock, secrets + redaction, resource/timeout | ✅ hardware |
| 4 — Concurrency & UX | `Manager`, `ls`/`attach`/`logs`/`stop`/`rm`, config file, question channel | ✅ |
| 5 — Polish & orchestration | `report.md`/`meta.json`, patch lint, agent adapters + auth, `krayt-ask`, detached "park & walk away" | ✅ hardware |
| 6 — `ask_human` MCP + precise resume | in-VM MCP server, `waiting`→`running` on answer | ✅ hardware |
| 7 — Linux backend (parity) | `firecracker` provider behind the same interface | ✅ hardware |

The showcase: a real agent, blocked mid-task on a decision only a human could make, paused,
asked over MCP, got the answer, and continued with it — all inside the VM with a live
credential. See `hack/claude-code/` for the reproducible example.
