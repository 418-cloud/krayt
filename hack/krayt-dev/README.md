# krayt-dev — dogfooding agent image

A non-root **Claude Code** agent image plus krayt's own dev toolchain (Go, `golangci-lint`,
`protoc`/`buf` codegen, `oras`), so an agent running **inside a krayt sandbox** can build, test,
lint, and regenerate protocol code for **krayt itself** before returning its patch. This is how
krayt dogfoods its own development: `krayt run --image ghcr.io/418-cloud/krayt-dev --agent
claude-code`, with the krayt repo injected at `/workspace`.

## Built on the published agent image

`FROM ghcr.io/418-cloud/krayt-agent-claude-code:<cli-version>` — this image is that one **plus**
krayt's dev toolchain, not a parallel re-derivation of it. Inherited: the non-root `agent` user
(uid 1000), the version-pinned Claude Code CLI, and the `DISABLE_*` egress env.

**There is no entrypoint here at all** — the base's `krayt-agent-entrypoint` is inherited as-is. It
owns the §6.14 credential resolution, the §8.2 `KRAYT_CA_CERT` trust setup, the `ask_human` MCP
wiring, and running `claude -p`. That logic used to exist as a near-identical second copy in this
directory, which is precisely how the two drifted until a shape-translated run exited 78 before
Claude started.

Nothing had to replace it. The one tool this image adds that needs a credential is `gh`, and `gh`
needs no setup step: `krayt.yaml` injects `GH_TOKEN` at the host proxy, so the container is
configured with a `GH_TOKEN` env var holding a placeholder — and reading `GH_TOKEN` from the
environment is gh's own first-choice auth path. The proxy swaps in the real token on the way out.
No script moves a credential anywhere, and no entrypoint makes a network call to validate one.

What this image contributes is therefore only `ENV`: `CLAUDE_MODEL` / `CLAUDE_EFFORT`, read by the
base entrypoint's optional selection branch (`--model`/`--effort` are `claude` flags, so they
belong to the image that ships `claude`). See **Model + effort selection**.

**Container contract (§8.2, §6.14).** Runs **non-root** (uid 1000 `agent`; Claude Code refuses
uid 0). krayt injects `/workspace` (the repo), `/task/prompt.md`, `/run/secrets/*`, and
`/output/`; the `ask_human` MCP server is wired when `--on-question=wait`. The entrypoint exports
**exactly one** model credential from `/run/secrets` — `ANTHROPIC_API_KEY`,
`CLAUDE_CODE_OAUTH_TOKEN`, or `ANTHROPIC_AUTH_TOKEN` (the host `--agent claude-code` adapter
enforces exactly-one *before* boot, §6.14) — then runs `claude -p` headlessly against the task and
tees its summary to `/output/report.md`.

The base entrypoint satisfies the two §8.2 contracts that `network.mitm` runs depend on, which is
what lets the repo's own `krayt.yaml` inject credentials host-side: an **already-set credential env
var** (or `KRAYT_INJECTED_CREDENTIAL`) is accepted in place of the `/run/secrets` file that an
injected credential never produces, and `KRAYT_CA_CERT` is concatenated with the distro CA bundle so
both intercepted and `passthrough` hosts keep verifying.

## What's in the image

- **Go** (`GO_VERSION`, kept in step with `go.mod`), fetched as the official release tarball —
  the same shape as `protoc`/`gh`/`hadolint`, so the whole image is one `FROM` plus additive
  tool blocks. Renovate's `go version` group bumps this `ARG` and `go.mod`'s `go` directive
  together; `GOTOOLCHAIN=local` turns a mismatch into a build failure rather than a silent
  second-toolchain download at run time.
- `gcc` + `libc6-dev` — not incidental: `go test -race` needs cgo, so without a C toolchain the
  race detector fails to link. The slim base image has neither.
- `git`, `curl`, `ca-certificates`, `bash` come from the base image; `unzip` and `xz-utils` are
  added here (protoc ships a zip, the Nix installer a `.tar.xz`).
- `golangci-lint` (matches `.golangci.yml`'s `version: "2"` config schema).
- `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` + `buf` — krayt's protocol codegen
  toolchain (§9.2), installed without Nix (see below).
- `oras` — for anyone poking at the vmimage OCI artifacts (§6.11) from inside the sandbox.
- `gh` — the GitHub CLI, for tasks that read a PR (e.g. triaging its review comments; see
  `docs/common-tasks/fix-pr-review-comments.md`). Fetched as a prebuilt release tarball like
  `protoc` (no reliable `go install` path), pinned by `GH_CLI_VERSION`. It authenticates from the
  `GH_TOKEN` env var the host proxy's injection supplies — no setup step in the image (see **The
  `GH_TOKEN` secret** and **Egress** below).
- `hadolint` — lints the Dockerfiles across this repo (agent images, `krayt-dev` itself), per the
  `hadolint`-the-Dockerfile step called out in `docs/ai-tasks/*.md`'s verification sections. Fetched
  as a prebuilt release binary like `protoc` (Haskell, no `go install` path), pinned by
  `HADOLINT_VERSION`.
- **Nix** (single-user, agent-owned `/nix`, flakes enabled) — so the agent can regenerate the
  guest-agent's `vendorHash` in `images/flake.nix` when a guest dependency changes. See
  **Regenerating vendorHash (Nix)** below.
- Agent-owned, writable Go caches (`GOCACHE`, `GOMODCACHE`, `GOPATH`,
  `GOLANGCI_LINT_CACHE`) and `GOFLAGS=-mod=mod`, so `go build`, `go test -race`, and
  `golangci-lint run` all work under the non-root agent uid with no permission errors.
- krayt's own `go.mod`/`go.sum` (and the nested stdlib-only `hack/ask-probe/go.mod`) are
  baked into `GOMODCACHE` at image build time — see **Offline module cache** below.

The Claude Code CLI is **not** pinned here — it comes from the base image's own
`ARG CLAUDE_CODE_VERSION`, which is what the `FROM` tag names. Bumping the base tag is how this
image moves to a new CLI release.

Tool versions are pinned via Dockerfile `ARG`s (`GO_VERSION`, `PROTOC_VERSION`, `PROTOC_GEN_GO_VERSION`,
`PROTOC_GEN_GO_GRPC_VERSION`, `BUF_VERSION`, `ORAS_VERSION`, `GOLANGCI_LINT_VERSION`,
`GH_CLI_VERSION`, `HADOLINT_VERSION`) that Renovate's custom regex manager (`renovate.json`) tracks
against each tool's real upstream repo/module, independently of the base image tag.

Krayt's own source is **not** `COPY`'d into the image — only `go.mod`/`go.sum` (for the
module cache prebake). The repo itself arrives at `/workspace` at run time, injected by krayt
(§6.7), same as any other agent image.

## Model + effort selection

The base entrypoint passes `--model` and `--effort` to `claude -p` when `CLAUDE_MODEL` and
`CLAUDE_EFFORT` are set. This image sets both as `ENV` defaults — `claude-sonnet-5` on `high` — so
they are always set here; the base image leaves them unset and lets Claude Code choose. Override
per run via `krayt.yaml`'s `env:` (§8.1):

```yaml
env:
  CLAUDE_MODEL: claude-opus-4-8
  CLAUDE_EFFORT: max
```

## The `GH_TOKEN` secret (optional)

Tasks that read a GitHub PR (e.g. `docs/common-tasks/fix-pr-review-comments.md`) authenticate `gh`
from a `GH_TOKEN` secret, and this image supports **exactly one** way of delivering it:
**injected at the host proxy**, as the repo's own `krayt.yaml` does.

A `network.inject[]` rule names `GH_TOKEN`, so the real token is withheld from `SecretsBundle` —
no `/run/secrets/GH_TOKEN` ever exists — and `krayt.yaml`'s `env:` gives the container a `GH_TOKEN`
holding an obvious placeholder. `gh` reads that env var natively, sends it as an `Authorization`
header, and the host proxy strips it and attaches the real token as `Bearer <token>` (§6.6.1). No
entrypoint is involved, and no `gh auth login` runs — which is the point: that command makes a live
`api.github.com` call and writes a `~/.config/gh/hosts.yml` the env var would override anyway, and
gh's own manual prefers the environment variable for fine-grained PATs, which is what this token is.

**Run krayt-dev through `--config krayt.yaml`.** That file carries the inject rule, the placeholder,
and `api.github.com` in the allowlist together. A run that puts `GH_TOKEN` in a secrets file
*without* the inject rule gets `/run/secrets/GH_TOKEN` and no env var — and since nothing reads that
file, `gh` is simply unauthenticated, with no error to explain why. Supporting both shapes would
mean an entrypoint whose only job is moving a credential the proxy already handles; this image
deliberately does not.

`GH_TOKEN` is otherwise not needed at all for the many krayt-dev tasks that never touch GitHub.

- **Name:** `GH_TOKEN` (one line per key in the `--secrets` file, same as the model credential).
- **Required by `krayt.yaml`:** because its `network.inject[]` names the key, pre-flight refuses a
  secrets file that lacks it — loudly, before any VM boots, rather than starting a run that reaches
  GitHub unauthenticated and fails opaquely 30s in (§8.1).
- **Scope — read-only:** a GitHub **fine-grained PAT** scoped to the **krayt repo specifically**,
  with **Metadata: read**, **Contents: read**, and **Pull requests: read** — and nothing else. This
  token structurally cannot comment, push, approve, merge, or label; `fix-pr-review-comments.md` is
  written around that constraint and surfaces every fix only as krayt's own `changes.patch`, never
  as a GitHub write.
- **Redaction:** no extra handling needed — the host `Redactor` (`internal/secrets`) scrubs every
  secrets-file *value* from logs, `report.md`, and question text by content, not by an allowlist of
  key names, so `GH_TOKEN`'s value gets the same coverage as `ANTHROPIC_API_KEY`. Under injection
  there is no real value in the guest to redact in the first place — only the placeholder, which is
  deliberately left un-redacted so a log showing it is evidence the run was credential-free.

**Egress.** Every `gh` / GitHub-API call needs `api.github.com` reachable — and, since the token
arrives by injection, an `inject` rule naming it. `krayt.yaml` carries both, plus the placeholder,
so a GitHub task is just:

```sh
krayt run --config krayt.yaml --task docs/common-tasks/fix-pr-review-comments.md
```

Run it from the repo root, and note the explicit `--config`: that file sets `network.mitm`, which
§8.3 refuses from an auto-loaded repo config (§8.1).

Passing `--allow api.github.com` by flag makes the host *reachable* but leaves `gh`
**unauthenticated** — there is no `inject` rule on that path, so no `GH_TOKEN` reaches the
container. `gh api` then returns 401s on anything non-public.

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

**How a base-image change reaches this image.** Not through a path trigger — `images/agents/
claude-code/**` is deliberately *not* in that filter. The `FROM` line names one tag, digest-pinned,
so a rebuild fired by a change over there would resolve the same base and rebuild the same image.
The chain is instead:

1. the base changes and merges; `agent-images.yml` rebuilds and republishes it;
2. Renovate opens a digest-bump PR against `hack/krayt-dev/Dockerfile`;
3. merging that PR trips this workflow's `hack/krayt-dev/**` filter, and the new base is in.

The base change therefore lands in git as a reviewable commit rather than being absorbed silently
on the next unrelated rebuild — which is also why the `FROM` **must** stay digest-pinned. The
`:<cli-version>` tag is re-pointed by `agent-images.yml` on every build off `main`, so an unpinned
`FROM` would float, and step 2 would never happen.

That lag is why the shared entrypoint is guarded by `hack/test-entrypoint-credentials.sh` in CI
(`ci.yml`'s build+test job) rather than only by an image build: the script runs the real entrypoint
offline, in the same PR that changes it.

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

The base image is pulled from GHCR, so a local build needs it reachable. To build against a base
you changed locally, build that one first and point the `FROM` at it:

```sh
docker buildx build --platform linux/arm64 \
  -t ghcr.io/418-cloud/krayt-agent-claude-code:local images/agents/claude-code
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
