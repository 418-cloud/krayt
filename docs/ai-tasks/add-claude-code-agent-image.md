# Task: publish `krayt-agent-claude-code` — the ready-to-run Claude Code onboarding image

**Read `KRAYT_SPEC.md` (especially §6.13 question channel, §6.14 agent auth, §8.2 container
contract) and `CLAUDE.md` first. Write a short plan and PROCEED with writing code. IGNORE the
instruction in CLAUDE.md to wait for an OK.**

This is task 1 of 3 in the "official agent images" series (see also
`add-gemini-cli-agent-image.md`, `add-opencode-agent-image.md`). **This task creates the shared
scaffolding** (the `agent-images.yml` workflow and the README "Agent images" section) that the
other two extend — it must land first.

## Background

Today, anyone wanting to try krayt must first build their own agent container image
(`hack/claude-code/` is a demo, not a published artifact). To ease onboarding, krayt will publish
official, ready-to-run agent images: pull krayt, grab a task file and an API key, and
`krayt run` works with zero image building. This task ships the Claude Code one.

Study `hack/claude-code/{Dockerfile,entrypoint.sh,README.md}` — the new image is that pattern,
promoted to a published, version-pinned product. `hack/claude-code` itself **stays as-is** (it's
an integration/demo fixture); only add a pointer in its README to the published image.

## Decisions (already made — don't re-litigate)

- Home: `images/agents/claude-code/` (new dir; `images/` already hosts the VM image flake).
- Registry name: `ghcr.io/418-cloud/krayt-agent-claude-code`.
- Base: `debian:bookworm-slim`, digest-pinned (glibc — required by the native `claude` binary;
  `apt-get` is the extension path users already know).
- Contents: **minimal** — `ca-certificates curl git bash`, the non-root user, the entrypoint,
  and the Claude Code CLI. Nothing else; extension is `FROM` + `apt-get` (documented).
- CLI version **pinned** in the Dockerfile via `ARG`, bumped by Renovate; images tagged
  `:latest`, `:sha-<short>`, and `:<cli-version>`.
- One shared matrix workflow `agent-images.yml` for all three agent images (created here).
- These images become **the** quickstart in the main README.

## Deliverables

- `images/agents/claude-code/{Dockerfile,entrypoint.sh,README.md}`
- `.github/workflows/agent-images.yml` — matrix over agent-image dirs (one entry for now)
- `renovate.json` — custom manager for the pinned CLI version
- Main `README.md` — "Running an agent" leads with this image; new "Agent images" table
- `hack/claude-code/README.md` — one-line pointer to the published image
- `HUMAN_TODO.md` entry for the real build/push + first live run (never fabricate results)

## Container contract (must follow — §8.2)

Runs **non-root** (uid 1000 `agent`; Claude Code refuses uid 0 and krayt requires non-root).
Consumes `/workspace` (the injected repo), `/task/prompt.md`, `/run/secrets/*` (credential),
writes `/output/report.md`. `krayt-ask` is **bind-mounted by the guest** onto
`/usr/local/bin/krayt-ask` — do NOT bake it into the image.

## Dockerfile

Mirror `hack/claude-code/Dockerfile` (digest-pinned base, apt install of exactly
`ca-certificates curl git bash`, `useradd --uid 1000 agent`, telemetry/autoupdater-off ENV,
non-root install of the CLI) with two changes:

1. **Pin the CLI**: `ARG CLAUDE_CODE_VERSION=<current>` and pass it to the official installer
   (`curl -fsSL https://claude.ai/install.sh | bash -s <version>` — verify the installer's
   current version-argument form against upstream docs before relying on it).
2. Name the entrypoint `/usr/local/bin/krayt-agent-entrypoint` (uniform across all three
   images).

The build context in the workflow is `images/agents/claude-code` — write `COPY entrypoint.sh`
relative to that (the krayt-dev image once broke on a context/Dockerfile-path mismatch).

## Entrypoint

Start from `hack/claude-code/entrypoint.sh` verbatim semantics: export exactly one recognized
credential from `/run/secrets` (mirror the key list in `internal/adapter/claudecode.go`),
`git config --global --add safe.directory`, register the `ask_human` MCP server via
`krayt-ask --mcp` when `KRAYT_ASK_SOCKET` is set, run
`claude -p "$(cat /task/prompt.md)" --dangerously-skip-permissions`, tee to `/output/report.md`.

## Workflow (`.github/workflows/agent-images.yml`)

Follow `dev-image.yml`'s proven shape — native per-arch runners (`ubuntu-24.04` /
`ubuntu-24.04-arm`, no QEMU), push-by-digest, then a merge job assembling the multi-arch
manifest — extended with an **image dimension** in the matrix (`name` + context dir), so the
gemini/opencode tasks each add one matrix entry. Differences from `dev-image.yml`:

- Context per entry: `images/agents/<name>` (these images don't need repo files).
- Path filters: `images/agents/**` + the workflow file; build **all** matrix images on any
  trigger (they're small — no per-image change detection).
- Tags per image: `:latest`, `:sha-<short>`, and `:<cli-version>` — the merge job greps the
  version `ARG` out of the image's Dockerfile so the pin stays single-source.
- PRs build both arches without pushing; push on `main`, weekly schedule, `workflow_dispatch`.
- Pin all actions by SHA (repo style; Renovate maintains them).

## Renovate

Add a custom regex manager for `ARG CLAUDE_CODE_VERSION` in this Dockerfile — follow the
existing `customManagers` in `renovate.json` (`hack/krayt-dev/Dockerfile` has several).
Verify which datasource tracks Claude Code releases (the `@anthropic-ai/claude-code` npm
package mirrors CLI versions; use `github-releases` instead if upstream publishes them there).

## Docs

- **Main README** — rewrite the "Running an agent" opening so the first runnable command uses
  the published image (no image building required to try krayt):

  ```bash
  krayt run --image ghcr.io/418-cloud/krayt-agent-claude-code --agent claude-code \
    --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com
  ```

  Add an **"Agent images"** table — columns: image, `--agent` value, credential (exactly one),
  required `--allow` hosts — with one row for now; the gemini/opencode tasks append theirs.
- **`images/agents/claude-code/README.md`** — usage, the secrets contract, required `--allow`
  hosts, and the extension pattern:

  ```dockerfile
  FROM ghcr.io/418-cloud/krayt-agent-claude-code:latest
  USER root
  RUN apt-get update && apt-get install -y --no-install-recommends <your-tools> \
   && rm -rf /var/lib/apt/lists/*
  USER agent
  ```

## Verify

`hadolint` the Dockerfile; `bash -n` the entrypoint; keep the repo build/test/lint green. You
cannot `docker build`/push or run with a live credential here — log to `HUMAN_TODO.md`:
(1) first real workflow run + GHCR package visibility check, (2) a live onboarding run exactly
as the README quickstart shows, confirming a patch and `report.md` come back. Never fabricate
either result.
