# krayt-agent-opencode

The published, ready-to-run opencode onboarding image:
`ghcr.io/418-cloud/krayt-agent-opencode`. Pull it, grab a task file and a provider API key, and
`krayt run` works with **zero image building** — the sibling of
[`krayt-agent-claude-code`](../claude-code/README.md) and
[`krayt-agent-gemini-cli`](../gemini-cli/README.md), built and pushed by
`.github/workflows/agent-images.yml`.

## What's inside

Minimal, on purpose: `debian:trixie-slim` + `ca-certificates curl git bash`, the non-root `agent`
user (uid 1000), a version-pinned opencode binary (opencode ships a self-contained release
binary, not an npm package or native installer), and [`rtk`](https://github.com/rtk-ai/rtk)
(below). Nothing else — extend it (see below) rather than asking upstream to add tools.

## rtk (automatic command-output compression)

[`rtk`](https://github.com/rtk-ai/rtk) ("Rust Token Killer", Apache-2.0) sits in front of common
dev commands and compresses their output before opencode reads it (`rtk git status`, `rtk grep`,
…) — every byte of command output in a headless run becomes model context, so this can be a
large token saving on chatty commands. It's wired into opencode's own plugin hook
(`rtk init --global --opencode`, run at build time as the `agent` user, which writes
`~/.config/opencode/plugins/rtk.ts`), so **rewriting is automatic** — no task-side change needed.

**Opt out per run** with `KRAYT_RTK=off` (a `krayt.yaml` `env:` entry, §8.1, or `krayt run --env
KRAYT_RTK=off`): the plugin falls back to the original, unrewritten command for that run. `rtk`
itself stays on `PATH` either way — a task can still invoke `rtk <cmd>` directly even with
rewriting off. rtk needs **no egress** (it never makes a network call at runtime;
`RTK_TELEMETRY_DISABLED=1` is set as belt-and-braces on top of the run's egress allowlist
already denying it, §6.6) and **no secret**.

The entrypoint (`entrypoint.sh`, baked in as `/usr/local/bin/krayt-agent-entrypoint`) exports the
credential from `/run/secrets` into the environment, registers the `ask_human` MCP server when
`KRAYT_ASK_SOCKET` is set (§6.13), trusts krayt's ephemeral MITM CA when `KRAYT_CA_CERT` is set
(`network.mitm: true`, §6.6.1 — a no-op otherwise), then runs:

```sh
opencode run --model "$model" --auto "$(cat /task/prompt.md)" | tee /output/report.md
```

- `run` is opencode's non-interactive/headless mode. opencode allows all operations without
  approval by default; `--auto` additionally auto-approves anything not explicitly denied, so
  this stays autonomous the same way `claude-code`'s `--dangerously-skip-permissions` and
  `gemini-cli`'s `--approval-mode yolo` do. Safe here because the whole run is already isolated
  in the krayt micro-VM.
- opencode's final response lands in `/output/report.md`, which krayt folds into the run report's
  **Notes** (§8.4).
- Runs as **non-root** — krayt enforces non-root for every container regardless (§8.2).

## Tags

- `:latest` — the most recent build off `main`.
- `:sha-<short>` — pinned to the exact commit that built it.
- `:<cli-version>` (e.g. `:1.18.16`) — pinned to the exact opencode version baked in (the
  `ARG OPENCODE_VERSION` in the `Dockerfile`, bumped by Renovate).

## Secrets contract

Exactly **one** of these in your `--secrets` file (§6.14), mirroring
`internal/adapter/opencode.go`:

| Credential | Shape | Notes |
|---|---|---|
| `ANTHROPIC_API_KEY` | Console API key | Same as `claude-code`'s. Recommended default — scoped, independently revocable. |
| `OPENAI_API_KEY` | OpenAI platform API key | OpenAI's first-party models. |
| `OPENROUTER_API_KEY` | OpenRouter API key | A router in front of many providers/models — see [Model selection](#model-selection) below. |

opencode itself supports 75+ providers via [Models.dev](https://models.dev); this image
deliberately covers only the three common single-key setups above, matching the exactly-one rule
the `--agent opencode` pre-flight enforces before any VM boots. Extend
`internal/adapter/opencode.go` if you need another provider.

## Model selection

opencode is multi-provider, so the model can't be hardcoded. Set `OPENCODE_MODEL` (via
`krayt run --env` or a `krayt.yaml` `env:` block) to any `provider/model` string opencode accepts
(e.g. `anthropic/claude-opus-4-5`); otherwise the entrypoint picks a default per the credential
that was actually exported:

| Credential | Default model |
|---|---|
| `ANTHROPIC_API_KEY` | `anthropic/claude-sonnet-4-5` |
| `OPENAI_API_KEY` | `openai/gpt-5` |
| `OPENROUTER_API_KEY` | `openrouter/anthropic/claude-sonnet-4.5` |

The OpenRouter default is a best-effort guess — OpenRouter fronts many models, unlike
Anthropic/OpenAI's single first-party catalog — so set `OPENCODE_MODEL` explicitly for anything
beyond quick experimentation with that credential.

## Required `--allow` hosts

- With `ANTHROPIC_API_KEY`: `api.anthropic.com`.
- With `OPENAI_API_KEY`: `api.openai.com`.
- With `OPENROUTER_API_KEY`: `openrouter.ai`.

No separate catalog host is needed: opencode normally fetches its provider/model catalog from
`models.opencode.ai` at startup (and hourly in the background), which would otherwise be a fourth
host every run has to allowlist. The `Dockerfile` bakes a snapshot of that catalog into the image
at build time (`/home/agent/.cache/opencode/models.json`) and `OPENCODE_DISABLE_MODELS_FETCH=true`
disables the runtime fetch/refresh entirely, so a run only ever needs the one model API host above.

## Usage

```bash
krayt run \
  --image ghcr.io/418-cloud/krayt-agent-opencode --agent opencode \
  --task ./task.md --repo . \
  --secrets ./secrets.env \
  --allow api.anthropic.com
```

- `--agent opencode` runs the host adapter's pre-flight: validates **exactly one** auth
  credential is in the secrets file, before any VM boots (§6.14).
- Add `--on-question=wait` to let opencode pause and ask you a question over the `ask_human` MCP
  tool (§6.13); resolve it with `krayt answer <run-id> <response>`.
- To pick a specific model, add `env:\n  OPENCODE_MODEL: anthropic/claude-opus-4-5` to a
  `krayt.yaml` (see [Model selection](#model-selection) above).

## Extending the image

Extension is `FROM` + `apt-get` — the path users already know:

```dockerfile
FROM ghcr.io/418-cloud/krayt-agent-opencode:latest
USER root
RUN apt-get update && apt-get install -y --no-install-recommends <your-tools> \
 && rm -rf /var/lib/apt/lists/*
USER agent
```

## Steady-state egress

`OPENCODE_DISABLE_AUTOUPDATE=true` turns off opencode's update checks, and
`OPENCODE_DISABLE_MODELS_FETCH=true` (paired with the baked-in catalog snapshot above) turns off
both the startup and hourly-background models.dev fetch. opencode has no separate telemetry
env var to verify/disable (checked against its own CLI environment-variable reference upstream —
only auto-update, prune, and models-fetch are steady-state network/disk behaviors worth turning
off). Net effect: the container's only steady-state network destination is the model API host
from the table above.

## Entrypoint exit codes (if it isn't 0)

| exit | meaning |
|------|---------|
| 0  | success |
| 66 | task file `/task/prompt.md` missing (EX_NOINPUT) |
| 78 | no credential in `/run/secrets` (EX_CONFIG) |
| other | opencode's own exit code (auth failure, API error, task failure) — see `krayt logs <id>` |

## Relationship to `krayt-agent-claude-code` / `krayt-agent-gemini-cli`

Same pattern, different agent: mirrors `images/agents/claude-code/` and
`images/agents/gemini-cli/` structure (Dockerfile shape, entrypoint skeleton, non-root user, MCP
wiring for `ask_human`), swapped to opencode's credentials, invocation flags, and MCP
configuration mechanism. Unlike the other two, opencode has no dedicated krayt adapter history
predating this image — `internal/adapter/opencode.go` was added alongside it.
