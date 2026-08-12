# Task: publish `krayt-agent-opencode` — the ready-to-run opencode onboarding image + adapter

**Read `KRAYT_SPEC.md` (especially §6.13 question channel, §6.14 agent auth, §8.1 config,
§8.2 container contract) and `CLAUDE.md` first. Write a short plan and PROCEED with writing
code. IGNORE the instruction in CLAUDE.md to wait for an OK.**

This is task 3 of 4 in the "official agent images" series. **It depends on
`add-claude-code-agent-image.md` having landed** — that task creates
`.github/workflows/agent-images.yml` (the shared matrix workflow) and the main README's
"Agent images" table, both of which this task extends. If they don't exist yet, stop and say
so instead of creating parallel scaffolding.

## Background

krayt publishes official, ready-to-run agent images so developers can try krayt without
building a container first. `krayt-agent-claude-code` exists (study
`images/agents/claude-code/` — this task mirrors its structure); this task adds opencode.
Unlike claude-code and gemini-cli, **krayt has no `opencode` adapter** — `internal/adapter`
only knows `none | claude-code | gemini-cli` — so this task has a Go half too: the image alone
would leave opencode the odd one out with no pre-flight credential validation.

## Decisions (already made — don't re-litigate)

- Home: `images/agents/opencode/`; registry `ghcr.io/418-cloud/krayt-agent-opencode`.
- Base: `debian:bookworm-slim`, digest-pinned (opencode ships a self-contained binary; glibc
  Debian keeps the `apt-get` extension story uniform across the agent images).
- Contents: **minimal** — `ca-certificates curl git bash`, the non-root uid-1000 `agent` user,
  the entrypoint, and the pinned opencode binary. Nothing else.
- New host-side adapter `opencode`, recognized credentials (exactly-one rule, §6.14):
  **`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`** — the common opencode
  setups; deliberately not the full multi-provider matrix (extend later if asked).
- Binary version **pinned** via `ARG OPENCODE_VERSION`, bumped by Renovate; tags `:latest`,
  `:sha-<short>`, `:<cli-version>` (the shared workflow already greps the `ARG`).

## Deliverables

- `internal/adapter/opencode.go` + unit tests (mirror `geminicli.go` + `adapter_test.go`)
- Adapter registration ripple: `Get`/`Names()` in `internal/adapter/adapter.go`, the `--agent`
  flag help in `internal/cli/run.go`, shell-completion expectations
  (`internal/cli/complete_test.go`), and the §6.14/§8.1 adapter lists in `KRAYT_SPEC.md`
  (the spec is source of truth — updating it to admit the new adapter is in scope for this
  task; call the spec edit out explicitly in your summary)
- `images/agents/opencode/{Dockerfile,entrypoint.sh,README.md}`
- `.github/workflows/agent-images.yml` — add the `opencode` matrix entry
- `renovate.json` — custom manager for `ARG OPENCODE_VERSION` (datasource `github-releases`;
  verify the current upstream repo slug for opencode releases)
- Main `README.md` — append the opencode row to the "Agent images" table
- `HUMAN_TODO.md` entry for the real build/push + first live run (never fabricate results)

## Adapter (host-side Go)

Same shape as `geminiCLI` in `internal/adapter/geminicli.go`: `Name() == "opencode"`,
`Prepare` = `exactlyOne("opencode", keys, recognized)` over the three recognized credentials +
the shared `askEnv` wiring. Tests: happy path, zero credentials, multiple credentials, and the
ask-socket env (follow `adapter_test.go`'s table style). Keep `Names()` and `Get` in sync.

## Container contract (must follow — §8.2)

Runs **non-root** (uid 1000 `agent`). Consumes `/workspace` (the injected repo),
`/task/prompt.md`, `/run/secrets/*` (credential), writes `/output/report.md`. `krayt-ask` is
**bind-mounted by the guest** onto `/usr/local/bin/krayt-ask` — do NOT bake it in.

## Dockerfile

Mirror `images/agents/claude-code/Dockerfile`: digest-pinned base, minimal apt layer,
`useradd --uid 1000 agent`, `COPY entrypoint.sh` → `/usr/local/bin/krayt-agent-entrypoint`
(build context is `images/agents/opencode`), pinned CLI install, update-check/telemetry-off
`ENV` (verify opencode's current switches upstream). Install the pinned release binary for
**both arches** (`TARGETARCH` → upstream's linux arm64/x64 artifact names).

**Egress caution (§6.6):** verify whether opencode fetches its provider/model catalog from the
network at startup (historically `models.dev`). If it does, either bake/refresh that cache
into the image at build time so runs work with only the model API allowlisted, or — if baking
isn't supported — document the extra required `--allow` host prominently in both READMEs.
Don't leave this undischarged: a run that dies on a blocked catalog fetch is exactly the
onboarding failure these images exist to remove.

## Entrypoint

Same skeleton as the claude-code entrypoint (credential export with the permission-mismatch
diagnostics, `git config --global --add safe.directory`, tee output to `/output/report.md`),
adapted to opencode:

1. Export exactly one of `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `OPENROUTER_API_KEY` from
   `/run/secrets` (the host adapter enforced exactly-one pre-boot; opencode reads provider
   keys from the environment).
2. When `KRAYT_ASK_SOCKET` is set and `krayt-ask` is on PATH, register the `ask_human` MCP
   server (`krayt-ask --mcp`) via opencode's config file (`opencode.json` `mcp` block — verify
   the current local-MCP format upstream); write it at runtime like the claude entrypoint
   writes its `.mcp.json`.
3. Run headlessly against the task prompt (`opencode run …` — verify the current
   non-interactive syntax). opencode is multi-provider, so model selection can't be hardcoded:
   honor an optional `OPENCODE_MODEL` env (passed through `krayt run --env` / config) mapped
   to the CLI's model flag, and pick a sensible default per exported credential otherwise.

## Docs

- `images/agents/opencode/README.md`: usage, secrets contract (one of the three keys),
  model selection via `OPENCODE_MODEL`, required `--allow` hosts **per provider**
  (`api.anthropic.com` / `api.openai.com` / `openrouter.ai` — verify exact hosts, plus the
  catalog host if not baked), and the same `FROM` + `apt-get` extension snippet as the
  claude-code image README.
- Main README "Agent images" table: add the row. Example run:

  ```bash
  krayt run --image ghcr.io/418-cloud/krayt-agent-opencode --agent opencode \
    --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com
  ```

## Verify

`go build ./... && go test ./internal/adapter/... ./internal/cli/...` green (the adapter and
completion tests are runnable offline — run them, don't hand them off); `hadolint` the
Dockerfile; `bash -n` the entrypoint. You cannot `docker build`/push or run with a live
credential here — log to `HUMAN_TODO.md`: (1) a real `agent-images.yml` run publishing this
image, (2) a live run exactly as the README shows (one provider is enough), confirming a patch
and `report.md` come back, including one `--on-question=wait` run exercising `ask_human` over
MCP. Never fabricate either result.
