# krayt-dev — dogfooding agent image

A non-root **Claude Code** agent image plus krayt's own dev toolchain (Go, `golangci-lint`,
`protoc`/`buf` codegen, `oras`), so an agent running **inside a krayt sandbox** can build, test,
lint, and regenerate protocol code for **krayt itself** before returning its patch. This is how
krayt dogfoods its own development: `krayt run --image ghcr.io/418-cloud/krayt-dev --agent
claude-code`, with the krayt repo injected at `/workspace`.

**Container contract (§8.2, §6.14).** Runs **non-root** (uid 1000 `agent`; Claude Code refuses
uid 0). krayt injects `/workspace` (the repo), `/task/prompt.md`, `/run/secrets/*`, and
`/output/`; the `ask_human` MCP server is wired when `--on-question=wait`. The entrypoint exports
**exactly one** model credential from `/run/secrets` — `ANTHROPIC_API_KEY`,
`CLAUDE_CODE_OAUTH_TOKEN`, or `ANTHROPIC_AUTH_TOKEN` (the host `--agent claude-code` adapter
enforces exactly-one *before* boot, §6.14) — then runs `claude -p` headlessly against the task and
tees its summary to `/output/report.md`.

## What's in the image

- **Go 1.26** (matches `go.mod`), `git`, `curl`, `ca-certificates`, `bash`, `unzip`.
- `golangci-lint` (matches `.golangci.yml`'s `version: "2"` config schema).
- `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` + `buf` — krayt's protocol codegen
  toolchain (§9.2), installed without Nix (see below).
- `oras` — for anyone poking at the vmimage OCI artifacts (§6.11) from inside the sandbox.
- **Nix** (single-user, agent-owned `/nix`, flakes enabled) — so the agent can regenerate the
  guest-agent's `vendorHash` in `images/flake.nix` when a guest dependency changes. See
  **Regenerating vendorHash (Nix)** below.
- Agent-owned, writable Go caches (`GOCACHE`, `GOMODCACHE`, `GOPATH`,
  `GOLANGCI_LINT_CACHE`) and `GOFLAGS=-mod=mod`, so `go build`, `go test -race`, and
  `golangci-lint run` all work under the non-root agent uid with no permission errors.
- krayt's own `go.mod`/`go.sum` (and the nested stdlib-only `hack/ask-probe/go.mod`) are
  baked into `GOMODCACHE` at image build time — see **Offline module cache** below.

Tool versions are pinned via Dockerfile `ARG`s (`PROTOC_VERSION`, `PROTOC_GEN_GO_VERSION`,
`PROTOC_GEN_GO_GRPC_VERSION`, `BUF_VERSION`, `ORAS_VERSION`, `GOLANGCI_LINT_VERSION`) that
Renovate's custom regex manager (`renovate.json`) tracks against each tool's real upstream
repo/module, independently of the base image tag.

Krayt's own source is **not** `COPY`'d into the image — only `go.mod`/`go.sum` (for the
module cache prebake). The repo itself arrives at `/workspace` at run time, injected by krayt
(§6.7), same as any other agent image.

## Model + effort selection

The entrypoint passes `--model` and `--effort` to `claude -p`, taken from the `CLAUDE_MODEL` and
`CLAUDE_EFFORT` env vars (set via `krayt.yaml`'s `env:`, §8.1). Unset, they default to
`claude-sonnet-5` on `high` effort:

```yaml
env:
  CLAUDE_MODEL: claude-opus-4-8
  CLAUDE_EFFORT: max
```

## Offline module cache

`go mod download` runs at **image build time** against krayt's `go.mod`/`go.sum`, so
`GOMODCACHE` already has every dependency krayt currently declares. That means, inside the
sandbox, with krayt's *existing* deps:

```sh
go build ./...
go test -race ./...
golangci-lint run
```

all work **offline** — no `--allow` entries needed for them.

If the agent's task adds a **new** dependency (edits `go.mod` to something not already
vendored into this image), `go mod download`/`go build` will need to reach the module proxy —
add `proxy.golang.org` and `sum.golang.org` to the run's `--allow` list. Claude Code itself
needs `api.anthropic.com` (plus whatever host your credential's provider requires).

## Proto codegen (direct, no Nix needed)

`make proto` shells out to `nix run .#proto`. Nix *is* present in this image (for `vendorHash`,
below), but proto codegen doesn't need it — the direct `protoc` path is lighter and pulls no flake
inputs, so prefer it. If a task has the agent edit `internal/protocol/krayt.proto`, it needs to
regenerate `internal/protocol/pb` — the generated code is committed, so this only matters when the
`.proto` file itself changes.

Two equivalent no-Nix paths, both wrapping the same command as the flake's `proto` derivation
(verified against `flake.nix`):

```sh
make proto-direct
# or directly:
hack/krayt-dev/proto-direct.sh
```

which runs:

```sh
protoc \
  --proto_path=internal/protocol \
  --go_out=. --go_opt=module=github.com/418-cloud/krayt \
  --go-grpc_out=. --go-grpc_opt=module=github.com/418-cloud/krayt \
  internal/protocol/krayt.proto
```

Tell the agent (in the task prompt) to run this — and to re-run `go build ./...` /
`go test ./...` afterwards — whenever it changes `krayt.proto`.

## Regenerating vendorHash (Nix)

The guest-agent is built with Nix `buildGoModule`, whose `vendorHash` (in `images/flake.nix`) pins
the exact set of Go modules. When a task changes the guest-agent's dependencies (`go.mod`/`go.sum`),
that hash goes stale and must be recomputed — which needs **Nix** (there's no non-Nix way to derive
it). This image ships **single-user Nix** (agent-owned `/nix`, flakes on) so the agent can do it:

1. set `vendorHash = pkgs.lib.fakeHash;` in `images/flake.nix`;
2. run the guest-agent Nix build; read the reported `got: sha256-…` from the hash-mismatch error;
3. paste that real hash back into `vendorHash`.

**Egress.** `nix build` fetches from the binary cache + flake inputs + the Go proxy, so a run that
does this needs `--allow` to include at least
`cache.nixos.org,github.com,codeload.github.com,proxy.golang.org,sum.golang.org` (plus
`api.anthropic.com`). The first such build downloads a large Nix closure. **Never fabricate a
`vendorHash`** — it must be a real build's `got:` value; if the build can't run, leave it and
document the step.

## Build + publish

Multi-arch (amd64 + arm64): `.github/workflows/dev-image.yml` builds each arch on its **own native
runner** (`ubuntu-24.04` + `ubuntu-24.04-arm`, no QEMU) and merges them into one manifest, so both
arches pull under the same tags (`:latest`, `:sha-<short>`, `:<date>`). It runs on pushes to `main`
(path-filtered to `hack/krayt-dev/**`, `go.mod`, `go.sum`), weekly (to pick up base-image + tool
updates), and `workflow_dispatch`; PRs build both arches to validate the Dockerfile but never push.

To build locally, build **only your host arch** — a multi-arch local build emulates the other arch
under QEMU and is very slow, since the image compiles several Go tools (`golangci-lint`, `buf`, …)
from source. On Apple Silicon that's arm64, which is also what the krayt VM runs, so it's all you
need locally — let CI produce the multi-arch image:

```sh
cd /path/to/krayt   # repo root — the Dockerfile COPYs go.mod/go.sum from here
docker buildx build --platform linux/arm64 \
  -f hack/krayt-dev/Dockerfile \
  -t ghcr.io/418-cloud/krayt-dev:local .
```

## A first dogfood run

```sh
krayt run --image ghcr.io/418-cloud/krayt-dev --agent claude-code \
  --allow api.anthropic.com,proxy.golang.org,sum.golang.org \
  --secrets ./secrets.env --task ./some-krayt-task.md --repo .
```

- `--repo .` from krayt's own repo root — that's what gets bundled to `/workspace` (§6.7).
- The `proxy.golang.org,sum.golang.org` allow entries only matter if the task's changes add a
  new dependency; drop them to prove the module cache prebake is actually working offline.
- A good first task: ask the agent to run `go build ./... && go test -race ./... &&
  golangci-lint run`, fix anything red, and summarize.
- Success: `krayt ls` reaches `done` (exit 0), `krayt patch <id>` applies cleanly, and
  `report.md` carries Claude's notes.

## Entrypoint exit codes

- `66` (`EX_NOINPUT`) — the task file (`/task/prompt.md`) is missing.
- `78` (`EX_CONFIG`) — no recognized credential in `/run/secrets` (`ANTHROPIC_API_KEY`,
  `CLAUDE_CODE_OAUTH_TOKEN`, or `ANTHROPIC_AUTH_TOKEN`).
- any other code — Claude Code's own exit code.
