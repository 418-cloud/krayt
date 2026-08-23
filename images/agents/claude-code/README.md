# krayt-agent-claude-code

The published, ready-to-run Claude Code onboarding image:
`ghcr.io/418-cloud/krayt-agent-claude-code`. Pull it, grab a task file and an API key, and
`krayt run` works with **zero image building**. It's the same pattern as `hack/claude-code/`
(an integration/demo fixture that stays as-is), promoted to a version-pinned, published
artifact — built and pushed by `.github/workflows/agent-images.yml`.

## What's inside

Minimal, on purpose: `debian:trixie-slim` + `ca-certificates curl git bash`, the non-root `agent`
user (uid 1000), a version-pinned Claude Code CLI, and [`rtk`](https://github.com/rtk-ai/rtk)
(below). Nothing else — extend it (see below) rather than asking upstream to add tools.

## rtk (automatic command-output compression)

[`rtk`](https://github.com/rtk-ai/rtk) ("Rust Token Killer", Apache-2.0) sits in front of common
dev commands and compresses their output before Claude reads it (`rtk git status`, `rtk grep`,
…) — every byte of command output in a headless run becomes model context, so this can be a
large token saving on chatty commands. It's wired into Claude Code's own `PreToolUse` hook
(`rtk init --global --auto-patch`, run at build time as the `agent` user), so **rewriting is
automatic** — no task-side change needed.

**Opt out per run** with `KRAYT_RTK=off` (a `krayt.yaml` `env:` entry, §8.1, or `krayt run --env
KRAYT_RTK=off`): the hook falls back to the original, unrewritten command for that run. `rtk`
itself stays on `PATH` either way — a task can still invoke `rtk <cmd>` directly even with
rewriting off. rtk needs **no egress** (it never makes a network call at runtime;
`RTK_TELEMETRY_DISABLED=1` is set as belt-and-braces on top of the run's egress allowlist
already denying it, §6.6) and **no secret**.

The entrypoint (`entrypoint.sh`, baked in as `/usr/local/bin/krayt-agent-entrypoint`) exports
the credential from `/run/secrets` into the environment, registers the `ask_human` MCP server
when `KRAYT_ASK_SOCKET` is set (§6.13), trusts krayt's ephemeral MITM CA when `KRAYT_CA_CERT` is
set (`network.mitm: true`, §6.6.1 — a no-op otherwise), then runs:

```sh
claude -p "$(cat /task/prompt.md)" --dangerously-skip-permissions | tee /output/report.md
```

- `-p` = non-interactive/print mode; `--dangerously-skip-permissions` lets it edit autonomously
  (safe: the whole run is already isolated in the krayt micro-VM).
- Claude's final summary lands in `/output/report.md`, which krayt folds into the run report's
  **Notes** (§8.4).
- Runs as **non-root** — Claude Code refuses uid 0, and krayt enforces non-root for every
  container regardless (§8.2).

### Extending: wrap the entrypoint, don't fork it

An image built `FROM` this one inherits `krayt-agent-entrypoint`. If it needs setup of its own, the
pattern is a wrapper that does that work and then hands off:

```sh
#!/usr/bin/env bash
set -euo pipefail
… your setup …
exec /usr/local/bin/krayt-agent-entrypoint "$@"
```

Copying this script and editing it instead is what the arrangement exists to prevent: `hack/krayt-dev`
used to carry a near-identical second copy, and the drift between them is what let a `network.mitm`
shape-translated run exit `78` before Claude started. `hack/test-entrypoint-credentials.sh` (run by
`ci.yml`'s build+test job) exercises this script offline — no Docker, no VM, no network.

Often no wrapper is needed at all. `hack/krayt-dev` adds the whole krayt toolchain — including the
`gh` CLI — and still ships no entrypoint: its GitHub token is injected at the host proxy, so `gh`
reads it from the environment with no setup step. A credential the proxy attaches needs no script.

**Only behavior belonging to the tools this image ships lives here.** GitHub auth does not — there
is no `gh` in this image. The one downstream-facing knob that *is* here is **model/effort
selection**, because `--model`/`--effort` are `claude` flags and this is the image that ships
`claude`: set `CLAUDE_MODEL` / `CLAUDE_EFFORT` and they become those flags; unset — as they are
here — no flag is passed and Claude Code picks its own default.

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
| `ANTHROPIC_AUTH_TOKEN` | Custom/proxy auth token | For gateway or reverse-proxy setups that front the Anthropic API with their own token. |

Setting more than one is refused by the `--agent claude-code` pre-flight before any VM boots —
Claude Code's own precedence would otherwise silently prefer one and bypass the others.

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
