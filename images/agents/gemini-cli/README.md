# krayt-agent-gemini-cli

The published, ready-to-run Gemini CLI onboarding image:
`ghcr.io/418-cloud/krayt-agent-gemini-cli`. Pull it, grab a task file and an API key, and
`krayt run` works with **zero image building** — the sibling of
[`krayt-agent-claude-code`](../claude-code/README.md), built and pushed by
`.github/workflows/agent-images.yml`.

## What's inside

Minimal, on purpose: `node:24-bookworm-slim` (Node's current LTS, "krypton") +
`ca-certificates curl git bash`, the non-root `node` user the base image already ships (uid
1000 — reused rather than creating a second one), and a version-pinned Gemini CLI
(`@google/gemini-cli`, installed globally via npm). Nothing else — extend it (see below) rather
than asking upstream to add tools.

The entrypoint (`entrypoint.sh`, baked in as `/usr/local/bin/krayt-agent-entrypoint`) exports
the credential from `/run/secrets` into the environment, registers the `ask_human` MCP server
when `KRAYT_ASK_SOCKET` is set (§6.13), then runs:

```sh
gemini --prompt "$(cat /task/prompt.md)" --approval-mode yolo | tee /output/report.md
```

- `--prompt`/`-p` forces non-interactive/headless mode; `--approval-mode yolo` auto-approves
  every tool call (the current unified flag — the older `--yolo`/`-y` is deprecated). Safe here
  because the whole run is already isolated in the krayt micro-VM.
- Gemini's final response lands in `/output/report.md`, which krayt folds into the run report's
  **Notes** (§8.4).
- Runs as **non-root** — krayt enforces non-root for every container regardless (§8.2).

## Tags

- `:latest` — the most recent build off `main`.
- `:sha-<short>` — pinned to the exact commit that built it.
- `:<cli-version>` (e.g. `:0.55.1`) — pinned to the exact Gemini CLI version baked in (the
  `ARG GEMINI_CLI_VERSION` in the `Dockerfile`, bumped by Renovate).

## Secrets contract

Exactly **one** of these in your `--secrets` file (§6.14), mirroring
`internal/adapter/geminicli.go`:

| Credential | Shape | Notes |
|---|---|---|
| `GEMINI_API_KEY` | AI Studio API key | Hits the Gemini Developer API (`generativelanguage.googleapis.com`). The recommended default — same tradeoffs as `claude-code`'s `ANTHROPIC_API_KEY` (scoped, independently revocable). |
| `GOOGLE_API_KEY` | Vertex AI API key | Hits **Vertex AI**, a different product/endpoint (`aiplatform.googleapis.com`), not the Gemini Developer API. The entrypoint pairs it with `GOOGLE_GENAI_USE_VERTEXAI=true` automatically — Gemini CLI's own env-based auth detection does not infer Vertex mode from `GOOGLE_API_KEY` alone, so without that pairing the run fails with "no auth method configured". |

Setting more than one is refused by the `--agent gemini-cli` pre-flight before any VM boots.

## Required `--allow` hosts

- With `GEMINI_API_KEY`: `generativelanguage.googleapis.com` — the Gemini Developer API
  inference endpoint. This is the default/recommended path.
- With `GOOGLE_API_KEY`: `aiplatform.googleapis.com` — the Vertex AI endpoint the CLI falls
  back to when no `GOOGLE_CLOUD_LOCATION` is set (Vertex AI "Express mode" over a global
  endpoint). If you do set `GOOGLE_CLOUD_LOCATION` to a specific region, the CLI instead calls a
  region-specific host (`<location>-aiplatform.googleapis.com`, or the `.rep.googleapis.com`
  multi-region variant) — verify against current docs and adjust `--allow` accordingly if a run
  stalls on egress.

## Usage

```bash
krayt run \
  --image ghcr.io/418-cloud/krayt-agent-gemini-cli --agent gemini-cli \
  --task ./task.md --repo . \
  --secrets ./secrets.env \
  --allow generativelanguage.googleapis.com
```

- `--agent gemini-cli` runs the host adapter's pre-flight: validates **exactly one** auth
  credential is in the secrets file, before any VM boots (§6.14).
- Add `--on-question=wait` to let Gemini pause and ask you a question over the `ask_human` MCP
  tool (§6.13); resolve it with `krayt answer <run-id> <response>`.
- To pick a specific model, add `env:\n  GEMINI_MODEL: gemini-2.5-flash` to a `krayt.yaml` —
  Gemini CLI reads `GEMINI_MODEL` from the environment (falls back to `settings.model.name`,
  then `auto`) the same way Claude Code reads `ANTHROPIC_MODEL`.

## Extending the image

Extension is `FROM` + `apt-get` — the path users already know:

```dockerfile
FROM ghcr.io/418-cloud/krayt-agent-gemini-cli:latest
USER root
RUN apt-get update && apt-get install -y --no-install-recommends <your-tools> \
 && rm -rf /var/lib/apt/lists/*
USER node
```

## Steady-state egress

`general.enableAutoUpdate` and `privacy.usageStatisticsEnabled` are turned off in
`~/.gemini/settings.json` (baked at build time, rewritten as-is by the entrypoint if it also adds
`mcpServers`) — Gemini CLI has no environment-variable equivalent for either (verified against
`docs/cli/settings.md` and `packages/cli/src/config/settingsSchema.ts` upstream), so settings.json
is the only lever. The OpenTelemetry pipeline (`docs/cli/telemetry.md`) defaults to `enabled:
false` already and needed no change. Net effect: the container's only steady-state network
destination is the model API host from the table above.

## Entrypoint exit codes (if it isn't 0)

| exit | meaning |
|------|---------|
| 0  | success |
| 1  | Gemini CLI general error or API failure |
| 42 | Gemini CLI input error (invalid prompt or arguments) |
| 53 | Gemini CLI turn limit exceeded |
| 66 | task file `/task/prompt.md` missing (EX_NOINPUT) |
| 78 | no credential in `/run/secrets` (EX_CONFIG) |

## Relationship to `krayt-agent-claude-code`

Same pattern, different agent: mirrors `images/agents/claude-code/` structure exactly (Dockerfile
shape, entrypoint skeleton, non-root user, MCP wiring for `ask_human`), swapped to Gemini CLI's
credentials, invocation flags, and MCP configuration mechanism.
