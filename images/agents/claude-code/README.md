# krayt-agent-claude-code

The published, ready-to-run Claude Code onboarding image:
`ghcr.io/418-cloud/krayt-agent-claude-code`. Pull it, grab a task file and an API key, and
`krayt run` works with **zero image building**. It's the same pattern as `hack/claude-code/`
(an integration/demo fixture that stays as-is), promoted to a version-pinned, published
artifact — built and pushed by `.github/workflows/agent-images.yml`.

## What's inside

Minimal, on purpose: `debian:bookworm-slim` (digest-pinned) + `ca-certificates curl git bash`,
the non-root `agent` user (uid 1000), and a version-pinned Claude Code CLI. Nothing else —
extend it (see below) rather than asking upstream to add tools.

The entrypoint (`entrypoint.sh`, baked in as `/usr/local/bin/krayt-agent-entrypoint`) exports
the credential from `/run/secrets` into the environment, registers the `ask_human` MCP server
when `KRAYT_ASK_SOCKET` is set (§6.13), then runs:

```sh
claude -p "$(cat /task/prompt.md)" --dangerously-skip-permissions | tee /output/report.md
```

- `-p` = non-interactive/print mode; `--dangerously-skip-permissions` lets it edit autonomously
  (safe: the whole run is already isolated in the krayt micro-VM).
- Claude's final summary lands in `/output/report.md`, which krayt folds into the run report's
  **Notes** (§8.4).
- Runs as **non-root** — Claude Code refuses uid 0, and krayt enforces non-root for every
  container regardless (§8.2).

## Tags

- `:latest` — the most recent build off `main`.
- `:sha-<short>` — pinned to the exact commit that built it.
- `:<cli-version>` (e.g. `:2.1.226`) — pinned to the exact Claude Code CLI version baked in
  (the `ARG CLAUDE_CODE_VERSION` in the `Dockerfile`, bumped by Renovate).

## Secrets contract

Exactly **one** of these in your `--secrets` file (§6.14):

| Credential | Shape | Notes |
|---|---|---|
| `ANTHROPIC_API_KEY` | Console API key | Scoped, independently revocable — the recommended default for untrusted code / concurrent runs. |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` output | Subscription auth (Pro/Max/Team/Enterprise); suits low-concurrency, trusted runs. |

Setting both is refused by the `--agent claude-code` pre-flight before any VM boots — Claude
Code's own precedence would otherwise silently prefer the API key and bypass the subscription.

## Required `--allow` hosts

- `api.anthropic.com` — the inference endpoint. Required in every case.
- If using `CLAUDE_CODE_OAUTH_TOKEN`, the auth/refresh flow may also need `console.anthropic.com`
  and/or `claude.ai` — verify against current Anthropic docs if a run stalls on egress.

## Usage

```bash
krayt run \
  --image ghcr.io/418-cloud/krayt-agent-claude-code \
  --agent claude-code \
  --task ./task.md --repo . \
  --secrets ./secrets.env \
  --allow api.anthropic.com
```

- `--agent claude-code` runs the host adapter's pre-flight: validates **exactly one** auth
  credential is in the secrets file, before any VM boots (§6.14).
- Add `--on-question=wait` to let Claude pause and ask you a question over the `ask_human` MCP
  tool (§6.13); resolve it with `krayt answer <run-id> <response>`.
- To pick a cheaper model, add `env:\n  ANTHROPIC_MODEL: claude-haiku-4-5` to a `krayt.yaml`.

## Extending the image

Extension is `FROM` + `apt-get` — the path users already know:

```dockerfile
FROM ghcr.io/418-cloud/krayt-agent-claude-code:latest
USER root
RUN apt-get update && apt-get install -y --no-install-recommends <your-tools> \
 && rm -rf /var/lib/apt/lists/*
USER agent
```

## Entrypoint exit codes (if it isn't 0)

| exit | meaning |
|------|---------|
| 0  | success |
| 66 | task file `/task/prompt.md` missing (EX_NOINPUT) |
| 78 | no credential in `/run/secrets` (EX_CONFIG) |
| other | Claude Code's own exit code (auth failure, API error, task failure) — see `krayt logs <id>` |

## Relationship to `hack/claude-code/`

`hack/claude-code/` is an unpublished, build-it-yourself integration/demo fixture used to
exercise the `claude-code` adapter during development. This image is that pattern promoted to
a published, version-pinned product — use this one unless you're hacking on krayt itself.
