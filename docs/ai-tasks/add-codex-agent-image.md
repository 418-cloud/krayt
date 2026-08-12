# Task: publish `krayt-agent-codex` — the ready-to-run OpenAI Codex CLI image + adapter

**Read `KRAYT_SPEC.md` (especially §6.13 question channel, §6.14 agent auth, §8.1 config,
§8.2 container contract) and `CLAUDE.md` first. Write a short plan and PROCEED with writing
code. IGNORE the instruction in CLAUDE.md to wait for an OK.**

This is task 4 of 4 in the "official agent images" series. **It depends on
`add-claude-code-agent-image.md` having landed** — that task creates
`.github/workflows/agent-images.yml` (the shared matrix workflow) and the main README's
"Agent images" table, both of which this task extends. If they don't exist yet, stop and say
so instead of creating parallel scaffolding.

## Background

krayt publishes official, ready-to-run agent images so developers can try krayt without
building a container first. `krayt-agent-claude-code` exists (study
`images/agents/claude-code/` — this task mirrors its structure); this task adds OpenAI's
Codex CLI, which completes the three big labs (Anthropic, Google, OpenAI) alongside the
BYO-provider opencode image.

Like opencode and unlike claude-code/gemini-cli, **krayt has no `codex` adapter** — so this
task has a Go half too, otherwise codex is the odd one out with no pre-flight credential
validation.

**Ordering note:** `add-opencode-agent-image.md` adds its own adapter and touches the exact
same lines (`Get`/`Names()`, the `--agent` flag help, `complete_test.go`, the §6.14/§8.1
adapter lists). Whichever lands second **extends** that list — read the current state of those
files and add to it; never revert the sibling adapter.

## Decisions (already made — don't re-litigate)

- Home: `images/agents/codex/`; registry `ghcr.io/418-cloud/krayt-agent-codex`.
- Base: `debian:bookworm-slim`, digest-pinned. Upstream ships a **static musl** binary, so no
  Node runtime is needed and the `apt-get` extension story stays uniform with the others.
- Contents: **minimal** — `ca-certificates curl git bash`, the non-root uid-1000 `agent` user,
  the entrypoint, and the pinned `codex` binary. Nothing else.
- New host-side adapter `codex`, recognized credentials (exactly-one rule, §6.14):
  **`OPENAI_API_KEY`, `CODEX_API_KEY`**. ChatGPT-subscription auth (a `~/.codex/auth.json`
  written by an interactive `codex login`) is **out of scope for v1** — it's a file, not an
  env-shaped secret; say so in the image README rather than half-supporting it.
- Binary version **pinned** via `ARG CODEX_VERSION`, bumped by Renovate; tags `:latest`,
  `:sha-<short>`, `:<cli-version>` (the shared workflow already greps the `ARG`).

## Deliverables

- `internal/adapter/codex.go` + unit tests (mirror `geminicli.go` + `adapter_test.go`)
- Adapter registration ripple: `Get`/`Names()` in `internal/adapter/adapter.go`, the `--agent`
  flag help in `internal/cli/run.go` (and the `agent` field comment at the top of the flags
  struct), shell-completion expectations (`internal/cli/complete_test.go`), and the §6.14/§8.1
  adapter lists in `KRAYT_SPEC.md` (the spec is source of truth — updating it to admit the new
  adapter is in scope; call the spec edit out explicitly in your summary)
- `images/agents/codex/{Dockerfile,entrypoint.sh,README.md}`
- `.github/workflows/agent-images.yml` — add the `codex` entries (two `build` rows, one
  `merge` row)
- `renovate.json` — custom manager for `ARG CODEX_VERSION` (datasource `github-releases`,
  package `openai/codex`; releases are tagged `rust-v<version>`, so an
  `extractVersionTemplate` of `^rust-v(?<version>.*)$` is needed — verify it selects the
  current release line and not some stale tag series)
- Main `README.md` — append the codex row to the "Agent images" table
- `HUMAN_TODO.md` entry for the real build/push + first live run (never fabricate results)

## Adapter (host-side Go)

Same shape as `geminiCLI` in `internal/adapter/geminicli.go`: `Name() == "codex"`, `Prepare` =
`exactlyOne("codex", keys, recognized)` over the two recognized credentials + the shared
`askEnv` wiring. Tests: happy path, zero credentials, multiple credentials, and the ask-socket
env (follow `adapter_test.go`'s table style). Keep `Names()` and `Get` in sync.

## Container contract (must follow — §8.2)

Runs **non-root** (uid 1000 `agent`). Consumes `/workspace` (the injected repo),
`/task/prompt.md`, `/run/secrets/*` (credential), writes `/output/report.md`. `krayt-ask` is
**bind-mounted by the guest** onto `/usr/local/bin/krayt-ask` — do NOT bake it in.

## Dockerfile

Mirror `images/agents/claude-code/Dockerfile`: digest-pinned base, minimal apt layer,
`useradd --uid 1000 agent`, `COPY entrypoint.sh` → `/usr/local/bin/krayt-agent-entrypoint`
(build context is `images/agents/codex`), pinned CLI install, update-check/telemetry-off `ENV`
(verify codex's current switches upstream — the sandbox has allowlisted egress (§6.6), so the
steady-state destination must be the model API only).

Install from the GitHub release for **both arches** — `TARGETARCH` → `x86_64` /`aarch64` in
the asset name `codex-<arch>-unknown-linux-musl.tar.gz`. The tarball holds a **single binary
named after the target triple**; install it as `/usr/local/bin/codex` (rename it). Download to
a file and check curl's exit code before unpacking, like the claude-code image does.

**Size caution:** the binary is large (~90 MB compressed, ~210 MB unpacked) — much bigger than
the other agent images. Keep it to one layer with the download and tarball removed in the same
`RUN`, and install only `codex` (the release also carries `codex-app-server`,
`codex-responses-api-proxy`, `bwrap`, symbols — none of them wanted here).

## Entrypoint

Same skeleton as the claude-code entrypoint (credential export with the permission-mismatch
diagnostics, `git config --global --add safe.directory`, report written to `/output/report.md`),
adapted to codex:

1. Export exactly one of `OPENAI_API_KEY` / `CODEX_API_KEY` from `/run/secrets` (the host
   adapter enforced exactly-one pre-boot).
2. When `KRAYT_ASK_SOCKET` is set and `krayt-ask` is on PATH, register the `ask_human` MCP
   server. Codex configures MCP servers in **TOML**, not JSON: an `[mcp_servers.<name>]` table
   in `$CODEX_HOME/config.toml` (`CODEX_HOME` defaults to `~/.codex`) with `command`, `args`,
   and `env`. Write it at runtime like the claude entrypoint writes its `.mcp.json`. **Gotcha
   worth getting right:** codex forwards only a fixed allowlist of env vars (`HOME`, `PATH`,
   `USER`, `LANG`, …) into a stdio MCP subprocess — `KRAYT_ASK_SOCKET` is **not** inherited, so
   it must be set explicitly in the server's own `env` table or the bridge starts socket-less.
   Verify the current key names/format upstream (`codex mcp add` writes the same file and is a
   good cross-check).
3. Run headlessly: `codex exec "$(cat "$TASK_FILE")"`. Verify the current flags upstream, but
   as of writing: headless mode forces the approval policy to `never`;
   `--dangerously-bypass-approvals-and-sandbox` (alias `--yolo`) is the current flag —
   `--full-auto` is deprecated. Use the bypass: krayt's container already drops all
   capabilities and applies seccomp (§6.10), so codex's own Linux sandbox has nothing to build
   on, and the micro-VM is the real boundary — same reasoning as claude's
   `--dangerously-skip-permissions`.
4. Write the report with codex's native `-o/--output-last-message /output/report.md` rather
   than `| tee` — it writes exactly the final agent message, and stdout still streams the
   run's progress into `krayt logs`.
5. Model selection: honor an optional `CODEX_MODEL` env (passed through `krayt run --env` /
   config) mapped to `-m/--model`, and otherwise let codex use its configured default — don't
   hardcode a model name.

## Docs

- `images/agents/codex/README.md`: usage, secrets contract (one of the two keys, plus the
  one-line note that ChatGPT-plan `auth.json` auth isn't supported), model selection via
  `CODEX_MODEL`, required `--allow` hosts (with an API key the endpoint is `api.openai.com` —
  verify, and document anything else the CLI reaches at startup, e.g. an update check), the
  entrypoint exit-code table, and the same `FROM` + `apt-get` extension snippet as the
  claude-code image README.
- Main README "Agent images" table: add the row (image, `--agent codex`, credential, `--allow`
  hosts). Example run:

  ```bash
  krayt run --image ghcr.io/418-cloud/krayt-agent-codex --agent codex \
    --task ./task.md --repo . --secrets ./secrets.env --allow api.openai.com
  ```

## Verify

`go build ./... && go test ./internal/adapter/... ./internal/cli/...` green (the adapter and
completion tests are runnable offline — run them, don't hand them off); `hadolint` the
Dockerfile; `bash -n` the entrypoint. You cannot `docker build`/push or run with a live
credential here — log to `HUMAN_TODO.md`: (1) a real `agent-images.yml` run publishing this
image, (2) a live run exactly as the README shows (real `OPENAI_API_KEY`), confirming a patch
and `report.md` come back, including one `--on-question=wait` run exercising `ask_human` over
MCP. Never fabricate either result.
