# Task: build a multi-arch `krayt-dev` agent image + a GHCR publish workflow

**Read `KRAYT_SPEC.md` (especially §6.13 question channel, §6.14 agent auth, §8.2 container
contract) and `CLAUDE.md` first. Give me a short plan and WAIT for my OK before writing code.**

## Background

krayt runs an AI coding agent inside a disposable micro-VM over a repo snapshot and returns a
reviewable git patch. I want to **dogfood** it: an agent image whose job is developing *krayt
itself*. So this image is passed as `krayt run --image <this> --agent claude-code`, the krayt repo
is injected at `/workspace`, and the agent must be able to **build, test, lint, and regenerate
protocol code** for krayt inside the sandbox before returning its patch.

It is the existing `hack/claude-code/` image **plus** the krayt dev toolchain — study
`hack/claude-code/{Dockerfile,entrypoint.sh,README.md}` and reuse that pattern.

## Deliverables

- `hack/krayt-dev/Dockerfile`, `hack/krayt-dev/entrypoint.sh`, `hack/krayt-dev/README.md`
- `.github/workflows/dev-image.yml` (multi-arch build + push to GHCR)
- If needed, a **no-Nix** proto path (see below) — a `hack/krayt-dev`-local script or a Makefile
  addition; do **not** break the existing Nix `make proto`.
- A `HUMAN_TODO.md` entry for the parts you can't do (the Docker build/push needs a real builder +
  registry; write everything around it, and **never fabricate a build/push result**).

## Container contract (must follow — §8.2)

Runs **non-root** (Claude Code refuses uid 0; krayt requires non-root). Consumes `/workspace` (the
krayt repo, injected by krayt — do **not** COPY krayt source into the image), `/task/prompt.md`,
`/run/secrets/*` (the agent credential), `/output/` (write `report.md`). When `--on-question=wait`,
`KRAYT_ASK_SOCKET` is set — register the `ask_human` MCP server exactly as
`hack/claude-code/entrypoint.sh` does (`krayt-ask --mcp` via `.mcp.json`). Reuse that entrypoint's
credential export + `git config --global --add safe.directory` logic.

## Toolchain (install directly — NO Nix; image must stay glibc for the native `claude` binary)

- Base: a glibc image with **Go 1.26.3** (e.g. `golang:1.26-bookworm`, which is multi-arch), plus
  `git`, `ca-certificates`, `curl`, `bash`.
- `golangci-lint`, `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` + `buf`, `oras`.
- **Claude Code CLI** via `curl -fsSL https://claude.ai/install.sh | bash` (as in `hack/claude-code`).
- Pin versions and let **Renovate** manage them (the repo already has `renovate.json` with
  digest-pinning for Dockerfiles).

## Non-root caches

The agent runs as a non-root uid and must be able to build/lint. Set writable, agent-owned
`GOCACHE`, `GOMODCACHE`, `GOPATH`, `GOLANGCI_LINT_CACHE` (and `GOFLAGS=-mod=mod`) via `ENV`/entrypoint
so `go build`, `go test -race`, and `golangci-lint run` work without permission errors.

## Pre-bake the module cache (important — the sandbox has allowlisted egress)

At build time, `COPY go.mod go.sum` (and the nested `hack/ask-probe/go.mod`), then `go mod download`
so krayt's dependencies are baked into `GOMODCACHE`. Goal: the agent can
`go build ./... && go test -race ./...` **offline**. Document in the README that *new* deps require
`proxy.golang.org` + `sum.golang.org` in the run's `--allow` list, and that Claude Code needs
`api.anthropic.com`.

## Proto without Nix

`make proto` wraps `nix run .#proto`; with no Nix it won't work. Provide a direct path — the
equivalent of the flake's command (verify against `flake.nix`):

```
protoc --proto_path=internal/protocol \
  --go_out=. --go_opt=module=github.com/418-cloud/krayt \
  --go-grpc_out=. --go-grpc_opt=module=github.com/418-cloud/krayt \
  internal/protocol/krayt.proto
```

Ship it as a `hack/krayt-dev`-local script (or a `make proto-direct` target). The generated
`internal/protocol/pb` is committed, so this is only for when the agent edits `krayt.proto`.

## Build + publish (`.github/workflows/dev-image.yml`)

- **Multi-arch amd64 + arm64** via `docker/setup-qemu-action` + `docker/setup-buildx-action` +
  `docker/build-push-action` (pin all actions by SHA, matching the repo's style; Renovate keeps
  them current).
- Push to **`ghcr.io/418-cloud/krayt-dev`** using `GITHUB_TOKEN` (`packages: write`).
- Tags: `latest`, `sha-<short>`, and a date tag; PRs touching the dev-image files build **without**
  pushing.
- Triggers: push to `main` path-filtered (`hack/krayt-dev/**`, `go.mod`, `go.sum`,
  `.github/workflows/dev-image.yml`), a **weekly schedule** (to pick up base-image + tool updates),
  and `workflow_dispatch`.

## Verify

`hadolint` the Dockerfile; `bash -n` the entrypoint; keep the repo build/test/lint green. You can't
run `docker build`/push in the sandbox — write the Dockerfile/workflow, log the build+push and a
first real dogfood run to `HUMAN_TODO.md`, and stop for my review at the plan and again before the
HUMAN steps.

First real dogfood run to document:

```
krayt run --image ghcr.io/418-cloud/krayt-dev --agent claude-code \
  --allow api.anthropic.com,proxy.golang.org,sum.golang.org \
  --secrets ./secrets.env --task ./some-krayt-task.md --repo .
```
