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

krayt no longer builds its own VM. It drives [**msb** (microsandbox)](https://github.com/superradcompany/microsandbox)
as a subprocess to rent a sandboxed micro-VM per run — the same `msb` binary, the same driver
(`internal/sandbox`), on **macOS and Linux alike**. There is no more per-OS `Provider` split:
orchestrator, patch generation, secrets, and egress policy are one code path on both platforms.
See [`docs/adr-microsandbox-sandbox-layer.md`](./docs/adr-microsandbox-sandbox-layer.md) for why.

What this means in practice:
- The whole codebase builds and unit-tests anywhere via a scriptable fake `msb` binary — no real
  sandbox needed.
- **Hardware verification still needs a real host with `msb` installed** — msb's own sandboxes are
  libkrun-based micro-VMs, so on macOS that means Apple Silicon and on Linux it means a host with
  KVM available (msb's prerequisite, not krayt's — see "Prerequisites" below). This can't run in an
  ordinary CI runner or a cloud agent without nested virtualization.

**Prebuilt binaries.** Each release (see `RELEASING.md`) publishes `krayt` for **darwin/arm64** and
**linux/amd64** — the two tested targets — and **darwin/amd64**, which compiles and *should* run on
Intel Macs but is **not tested**. There is no **linux/arm64** build yet (tracked separately from
this migration). Verify a download against the release's `checksums.txt`.

**Upgrading.** Once krayt is installed, `krayt upgrade` updates it in place: it finds the latest
GitHub release (or a pinned one), downloads the right platform tarball, verifies it against that
release's `checksums.txt` (the same check described above, automated), and atomically swaps it
in — leaving a `.bak` of the previous binary next to it. `krayt upgrade --check` reports whether
an upgrade is available without changing anything; `krayt upgrade --version vX.Y.Z` pins,
downgrades, or reinstalls a specific release instead of latest.

---

## Prerequisites

There are **two tiers**. Most contributors only need the first. Tier 2 is
provided by a Nix dev shell, so in practice you install **Go, msb, and (optionally) Nix**
— everything else comes from `nix develop`.

### 1. Build & run krayt (everyone)

Common to both platforms — **no more per-OS split**: krayt needs the same tools on macOS and
Linux alike.

- **Go** — current stable _(verify current)_
- **git**
- **msb** (microsandbox) — the sandbox runtime krayt drives as a subprocess. Install it — see
  <https://github.com/superradcompany/microsandbox> — then run `krayt doctor` to confirm it's
  found and healthy. As of writing, the upstream installer is:
  ```sh
  curl -fsSL https://install.microsandbox.dev | sh
  ```
  _(verify current — check the linked repo for the latest install method)._ msb itself needs
  **Apple Silicon** on macOS or a **KVM-capable** host on Linux (its own prerequisite, not a
  separate krayt setup step — there is no more `/dev/kvm` group wrangling, tap device, or NAT
  script for krayt to own; msb manages its own sandbox networking).
- **Claude Code** — if you're driving development with the agent (see below)

Run **`krayt doctor`** after installing msb; it checks msb is on `PATH` (or `KRAYT_MSB_BIN`),
its version meets krayt's minimum, it resolves to the local (not a cloud) backend, and passes
`msb doctor` itself — all **mandatory**: a host without a healthy msb fails `krayt doctor`
outright, since `krayt run` cannot do anything without one.

> No `protoc`/`buf` here any more — krayt's own gRPC-on-vsock control protocol
> (`internal/protocol/krayt.proto`) was deleted along with the rest of the sandbox layer it drove;
> msb is driven as a CLI subprocess instead, so there is nothing left to codegen.

> **Guest helper binaries.** `make build`/`make test` cross-compile the embedded `linux/amd64` +
> `linux/arm64` `krayt-helper`/`krayt-ask` binaries first (`make guest-bins`) and embed them via
> `go:embed` — pure Go, `CGO_ENABLED=0`, so this needs no toolchain beyond Go itself and works the
> same on macOS or Linux. A plain `go build ./...` still compiles on a fresh clone (the embed
> directory ships a `.gitkeep`), it just won't have real guest binaries to copy into a sandbox.

### 2. Build the VM image (legacy — CI / image maintainers only)
`krayt run` no longer needs this: msb pulls the agent's own OCI image directly, so there is no
krayt-built base VM image in the loop any more. What's left is legacy, kept only for the
`krayt image` cache subcommands until it's retired: the minimal Linux micro-VM image is a Nix
flake under `images/`, built and published as an OCI artifact (see `KRAYT_SPEC.md` §11). This is
**owned by CI (or a human), not by Claude Code** — building/boot-testing needs a Linux builder and
real hardware.
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
| msb (microsandbox) | `curl -fsSL https://install.microsandbox.dev \| sh` | https://github.com/superradcompany/microsandbox |
| Nix | `curl -fsSL https://install.determinate.systems/nix \| sh -s -- install` | https://determinate.systems/nix-installer/ — or the community installer at https://nixos.org/download |
| Claude Code | per docs | https://docs.claude.com/en/docs/claude-code/overview |
| oras (legacy VM image pipeline only) | via `nix develop`, else manual | https://oras.land |

> CI Nix install uses the GitHub Action `DeterminateSystems/determinate-nix-action`
> (or `nix-installer-action`) — see https://github.com/DeterminateSystems/nix-installer.

---

## Quick start (development)

```bash
git clone <your-fork> krayt && cd krayt
# tier-1 prereqs installed? confirm:
make build             # cross-builds the embedded guest binaries, then builds krayt — same on macOS and Linux
make test              # unit tests via a scriptable fake msb binary (no real sandbox needed)
go run ./cmd/krayt doctor

# only if you need the legacy VM image pipeline (tier 2, CI/image maintainers only):
nix develop             # drops you into a shell with oras pinned
```

That's it — no base VM image to pull. `krayt run` hands your agent image straight to `msb create`,
which pulls it itself.

---

## Running an agent

No image building is required — pull one of krayt's published, ready-to-run
[agent images](#agent-images), grab a task file and a credential, and run:

```bash
# the agent works on a copy of the repo, returns a patch you review
krayt run --image ghcr.io/418-cloud/krayt-agent-claude-code --agent claude-code \
  --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com

# or pipe the prompt in headlessly (--task -) instead of a file:
echo "fix the flaky test in internal/foo" | \
  krayt run --image ghcr.io/418-cloud/krayt-agent-claude-code --agent claude-code --task - --repo .

krayt ls                      # states: starting → running → (waiting) → done
krayt patch <run-id>          # inspect the diff …
krayt apply <run-id>          # … then apply it to your repo if you're satisfied
```

`--image` also accepts your own container (see [Agent images](#agent-images) for the extension
pattern, or `hack/claude-code/` for a from-scratch build-it-yourself example) — krayt knows
nothing about which AI or tools are inside.

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
- **Disk cache.** Agent images are pulled and cached by **msb itself** now — krayt hands `msb
  create` the image reference and lets it manage that cache, rather than pulling and storing it
  itself. `krayt image ls/rm/prune` still exist and still see krayt's own legacy `vmimage/` cache
  under `<user-cache-dir>/krayt/` (a leftover of the pre-msb base VM image, no longer used by
  `krayt run`); `imagestore/` is no longer populated for the same reason. Sandboxes themselves are
  fully ephemeral either way.

Reproducible, ready-to-run examples live under `hack/` — most notably `hack/claude-code/`
(a real Claude Code agent, build-it-yourself version of the published image below) and
`hack/krayt-ask-probe/` (the question channel).

### Egress control

`--net allowlist` (default) — only hosts in `--allow`/`network.allow` are reachable; `--net full`
opens egress to the whole public internet (explicit opt-in); `--net none` denies all network
access outright. krayt still enforces default-deny/allowlist egress, but the mechanism is now
msb's own: krayt translates `krayt.yaml`'s `network:` block into a fully explicit
`--net-rule`/`--net-default`/`--tls-intercept`/`--tls-bypass` policy handed to `msb create` —
there is no more krayt-run egress-proxy subprocess or in-guest firewall table. Two behavior notes
worth knowing:

- **DNS resolves through msb's own gateway**, not your host's resolver — msb polices it with
  DNS-rebind protection. This is a change from krayt's own former proxy, which used your host's
  system resolver directly.
- **Loopback, link-local, private/LAN ranges (RFC 1918, CGNAT, ULA), the metadata address, and
  multicast are explicitly denied in every policy mode, including `full`.** krayt always emits
  these deny rules itself rather than relying on msb's own defaults (a bare sandbox's implicit
  default is to *allow* the public internet the moment no explicit policy is given, which is the
  opposite of what `--net none` needs to mean) — so `krayt run` refuses to create a sandbox at all
  unless it has computed a complete, explicit network policy. A local Ollama/LM Studio on
  `127.0.0.1:11434` or a LAN package mirror is still **not reachable** from inside the sandbox.

### Credential injection

**On the moment you declare a secret — there is no separate opt-in any more.** Substitution is
now msb's own: krayt passes each `network.inject[]` entry to `msb create` as `--secret
NAME@HOST[,HOST...]` (the secrets-file key name and the hosts it may be substituted into — never
the value, which travels only in the environment of the `msb` process krayt spawns). msb sets the
sandbox's own `NAME` environment variable to a placeholder itself, and swaps in the real value
only on egress to one of the named hosts. Declaring any secret this way turns on TLS interception
for the whole sandbox automatically — under msb there is no such thing as a secret without it, so
`network.mitm` is **gone as a config key** (it's a hard error if set) and there is nothing left to
opt into by hand.

This is **not** a strict improvement over the default: it removes credential *theft*, not *use* (a
compromised agent can still make authenticated requests for the run's duration), and it
concentrates trust in msb's own process, which holds your real credential in memory for the
sandbox's lifetime. It also does not strip a pre-existing `authorization`/similar header the agent
may have set itself — msb substitutes a placeholder string wherever it finds it, it does not
remove headers the workload sent — so a credential the agent obtained elsewhere and placed in that
header, addressed to an allowed host, goes out untouched. See
[`docs/adr-microsandbox-sandbox-layer.md`](./docs/adr-microsandbox-sandbox-layer.md) for the full
reasoning.

```yaml
# krayt.yaml
secrets: ./secrets.env          # still holds GH_TOKEN — msb substitutes it at the TLS boundary;
                                 # the container never receives the real value, only a placeholder
network:
  mode: allowlist
  allow: [api.github.com]
  inject:
    - key: GH_TOKEN              # secrets-file key name
      host: api.github.com       # or `hosts: [...]` for more than one
```

No header name, no strip list, no literal prefix — the tool inside the sandbox is expected to emit
its own placeholder-bearing header (`gh` does this itself), and msb matches the placeholder string
wherever it appears rather than krayt naming a header to rewrite. Run it exactly like any other
task — `krayt run --config krayt.yaml --task ./task.md`. The run's `report.md`/`meta.json` still
show which keys were injected (names only, never values) so you can confirm the container ran
without them.

**Claude Code needs no `network.inject[]` at all.** With `--agent claude-code`, the adapter scopes
whichever credential your secrets file holds (`ANTHROPIC_API_KEY` xor
`CLAUDE_CODE_OAUTH_TOKEN`) to `api.anthropic.com` itself:

```yaml
# krayt.yaml
secrets: ./secrets.env          # ANTHROPIC_API_KEY OR CLAUDE_CODE_OAUTH_TOKEN — exactly one
network:
  mode: allowlist
  allow: [api.anthropic.com]    # no network.inject[] to write — the adapter scopes it
agent:
  adapter: claude-code
```

```sh
krayt run --config krayt.yaml --agent claude-code --task ./task.md --repo .
```

Claude Code accepts msb's own default placeholder shape (`$MSB_<NAME>`) for either credential —
confirmed on hardware — so there is no krayt-maintained table of wire-format placeholders to keep
in sync with Anthropic's API any more; that whole translation layer
(`internal/adapter/anthropic_wire.go`) went with the old host-side proxy. The container holds no
real credential either way; it can still tell it is on a subscription, because Anthropic's own
responses say so in their rate-limit headers.

One capability this loses: krayt's old proxy could log intercepted request/response metadata
(header **names**, never values) to `.krayt/runs/<id>/proxy.log` for debugging what an agent
actually sent. There is no krayt-side equivalent under msb today — that observability went with
`internal/proxy`.

### Agent images

krayt publishes official, minimal, version-pinned container images with an agent already
installed — no `docker build` required to try krayt. Each is `debian:trixie-slim` (or the
agent's required base, e.g. `node:24-trixie-slim` for gemini-cli) plus a non-root user, the
CLI, [`rtk`](https://github.com/rtk-ai/rtk) (wired into the agent's own pre-tool hook so common
command output is compressed automatically — opt out per run with `KRAYT_RTK=off`, needs no
egress and no secret), and nothing else; extend one with `FROM` + `apt-get` for your own tools
(see each image's README). Built by [`agent-images.yml`](./.github/workflows/agent-images.yml),
tagged `:latest`, `:sha-<short>`, and `:<cli-version>`.

| Image | `--agent` | Credential (exactly one) | Required `--allow` |
|---|---|---|---|
| [`ghcr.io/418-cloud/krayt-agent-claude-code`](./images/agents/claude-code/) | `claude-code` | `ANTHROPIC_API_KEY` xor `CLAUDE_CODE_OAUTH_TOKEN` | `api.anthropic.com` |
| [`ghcr.io/418-cloud/krayt-agent-gemini-cli`](./images/agents/gemini-cli/) | `gemini-cli` | `GEMINI_API_KEY` xor `GOOGLE_API_KEY` | `generativelanguage.googleapis.com` (`GEMINI_API_KEY`) or `aiplatform.googleapis.com` (`GOOGLE_API_KEY`, Vertex AI) |
| [`ghcr.io/418-cloud/krayt-agent-opencode`](./images/agents/opencode/) | `opencode` | `ANTHROPIC_API_KEY` xor `OPENAI_API_KEY` xor `OPENROUTER_API_KEY` | `api.anthropic.com`, `api.openai.com`, or `openrouter.ai` (matching the credential) |

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

### Running the integration tests

There is no more `//go:build integration` Go suite — it drove the now-deleted vfkit/Firecracker
providers, and msb is a real subprocess with no in-process fake worth booting a sandbox to test
twice (`internal/sandbox`'s and `internal/orchestrator`'s own unit tests already exercise the
driver and the run lifecycle against a scriptable fake `msb`). What's left to verify only on real
hardware is msb itself: does a real sandbox boot, does a real agent image run in it, does
`ask_human` round-trip over the guest's vsock dial to the host. `hack/run-integration-tests.sh` is
the runnable form of that recipe — it needs a host with `msb` installed (an Apple-Silicon Mac, or
any host with KVM) and a real model credential; it does not run in CI:

```bash
export KRAYT_IMAGE=ghcr.io/418-cloud/krayt-agent-claude-code:latest
export KRAYT_SECRETS=./secrets.env   # a real model credential
hack/run-integration-tests.sh
```

It builds `krayt`, runs `krayt doctor` as a preflight (fails fast if msb isn't installed and
healthy), then launches a plain run and a `--on-question=wait` run against the real image — verify
the results with `krayt ls`/`attach`/`answer` as the script's own output describes.

---

## Repo orientation

| File | What it is |
|---|---|
| `KRAYT_SPEC.md` | The complete implementation spec — architecture, protocol, phases, acceptance criteria. The source of truth. |
| `CLAUDE.md` | Working agreement Claude Code reads each session (rules, phase discipline, handoff protocol). |
| `HUMAN_TODO.md` | Handoff log the agent maintains for steps a human must do (created during development). |
| `SECURITY.md` | Threat model pointer + how to privately report a vulnerability. |
| `CONTRIBUTING.md` | How to get set up, code/commit conventions, and what a PR should include. |
| `images/` | Legacy Nix flake for krayt's old base micro-VM image (no longer used by `krayt run`, kept only for the `krayt image` cache subcommands until retired); `images/agents/` holds the published, ready-to-run agent images (CI-built) and is very much still live. |
| `internal/` | The implementation (see §9 of the spec for package layout). |
| `cmd/` | Binaries: `krayt` (the CLI), `krayt-helper` (stateless, linux-only, root-run guest binary invoked via `msb exec` that builds the patch — clones the bundle, tags a baseline, diffs, bundles commits), `krayt-ask` (question front-end + MCP server; dials `AF_VSOCK` to the host directly). |
| `configs/` | Example `krayt.yaml` + default allowlist. |
| `hack/` | Reproducible demo/probe images used to verify features on hardware (`claude-code` agent, `ask-probe`, `krayt-ask-probe`, `msb-probes/`). |

### Steps a human is expected to own (the `[HUMAN]` handoffs)
Claude Code does everything it can, then logs these to `HUMAN_TODO.md` and pauses if blocked:
- **Install msb** — see <https://github.com/superradcompany/microsandbox> — trivial, scriptable.
- **Run a real `krayt run` against real msb** (`hack/run-integration-tests.sh`) on hardware with
  msb installed.
- **Provide live API keys** to exercise a real agent image.

---

## Driving development with Claude Code

There is no more macOS-specific code path to build and test — msb is driven the same way on macOS
and Linux, so the whole codebase builds and unit-tests anywhere. Real-sandbox hardware
verification (a real `msb`-backed `krayt run`) still needs a host with msb installed — an
Apple-Silicon Mac, or any Linux host with KVM — and can be offloaded there or run locally; it just
can't run in an ordinary CI runner or a cloud agent without nested virtualization.

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

Built phase by phase per `KRAYT_SPEC.md` §14. **Phases 0–7 were complete and verified on real
hardware on both backends**, released as
[`v0.5.0`](https://github.com/418-cloud/krayt/releases/tag/v0.5.0) — krayt ran a real coding
agent (Claude Code) in an isolated micro-VM over an untrusted repo and handed back a reviewable
patch, with egress control, secrets, concurrency, park-and-walk-away, and an agent↔human question
channel, on **both** macOS/vfkit and Linux/firecracker behind the same `Provider` interface.
**That sandbox layer has since been replaced** (Phase 11, below): krayt now drives
[msb](https://github.com/superradcompany/microsandbox) instead of owning vfkit/Firecracker itself.
See `CHANGELOG.md` for the full release history.

| Phase | What | State |
|---|---|---|
| 0 — Foundations | provider/protocol scaffold, `fakeProvider`, `doctor` | ✅ |
| 1 — Boot a VM on macOS | vfkit provider, vsock guest-agent, image pull; `Hello` round-trips | ✅ hardware (superseded, Phase 11) |
| 2 — End-to-end run | bundle → clone → agent edit → `changes.patch` that applies cleanly | ✅ hardware |
| 3 — Security & limits | egress allowlist proxy + nftables lock, secrets + redaction, resource/timeout | ✅ hardware (superseded, Phase 11) |
| 4 — Concurrency & UX | `Manager`, `ls`/`attach`/`logs`/`stop`/`rm`, config file, question channel | ✅ |
| 5 — Polish & orchestration | `report.md`/`meta.json`, patch lint, agent adapters + auth, `krayt-ask`, detached "park & walk away" | ✅ hardware |
| 6 — `ask_human` MCP + precise resume | in-VM MCP server, `waiting`→`running` on answer | ✅ hardware |
| 7 — Linux backend (parity) | `firecracker` provider behind the same interface | ✅ hardware (superseded, Phase 11) |
| 8 — Host-side egress proxy, step 1 | L7 allowlist proxy moved off the guest to a separate host process over a new guest-initiated vsock channel (`move-egress-proxy-to-host.md`) | ✅ offline (superseded, Phase 11) |
| 11 — Microsandbox migration (ADR option B1) | Replace krayt's own vfkit/Firecracker/guest-agent/proxy stack with a driver for [msb](https://github.com/superradcompany/microsandbox); msb now owns the sandbox and credential substitution (`run-tasks-on-microsandbox.md`, the cut-over) | ✅ cut-over landed — a real end-to-end `krayt run` against real msb on hardware is still outstanding |

The showcase: a real agent, blocked mid-task on a decision only a human could make, paused,
asked over MCP, got the answer, and continued with it — all inside the sandbox with a live
credential. See `hack/claude-code/` for the reproducible example.
