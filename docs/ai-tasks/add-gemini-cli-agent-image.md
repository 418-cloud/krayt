# Task: publish `krayt-agent-gemini-cli` — the ready-to-run Gemini CLI onboarding image

**Read `KRAYT_SPEC.md` (especially §6.13 question channel, §6.14 agent auth, §8.2 container
contract) and `CLAUDE.md` first. Write a short plan and PROCEED with writing code. IGNORE the
instruction in CLAUDE.md to wait for an OK.**

This is task 2 of 3 in the "official agent images" series. **It depends on
`add-claude-code-agent-image.md` having landed** — that task creates
`.github/workflows/agent-images.yml` (the shared matrix workflow) and the main README's
"Agent images" table, both of which this task extends. If they don't exist yet, stop and say
so instead of creating parallel scaffolding.

## Background

krayt publishes official, ready-to-run agent images so developers can try krayt without
building a container first. `krayt-agent-claude-code` exists (study
`images/agents/claude-code/` — this task mirrors its structure exactly); this task adds the
Gemini CLI sibling. krayt already has a host-side `gemini-cli` adapter
(`internal/adapter/geminicli.go` — recognized credentials `GEMINI_API_KEY` xor
`GOOGLE_API_KEY`), so no Go changes are needed.

## Decisions (already made — don't re-litigate)

- Home: `images/agents/gemini-cli/`; registry `ghcr.io/418-cloud/krayt-agent-gemini-cli`.
- Base: Debian bookworm-slim family. Gemini CLI needs Node.js, so use the current LTS
  `node:<lts>-bookworm-slim`, digest-pinned — still Debian underneath, so the `apt-get`
  extension story matches the other agent images.
- Contents: **minimal** — `ca-certificates curl git bash` (install what the base lacks), the
  non-root user, the entrypoint, and the pinned Gemini CLI. Nothing else.
- CLI version **pinned** via `ARG GEMINI_CLI_VERSION` (`npm install -g
  @google/gemini-cli@$GEMINI_CLI_VERSION`), bumped by Renovate; tags `:latest`, `:sha-<short>`,
  `:<cli-version>` (the shared workflow already greps the `ARG`).

## Deliverables

- `images/agents/gemini-cli/{Dockerfile,entrypoint.sh,README.md}`
- `.github/workflows/agent-images.yml` — add the `gemini-cli` matrix entry
- `renovate.json` — custom manager for `ARG GEMINI_CLI_VERSION` (datasource `npm`, package
  `@google/gemini-cli`), following the existing `customManagers` pattern
- Main `README.md` — append the gemini-cli row to the "Agent images" table
- `HUMAN_TODO.md` entry for the real build/push + first live run (never fabricate results)

## Container contract (must follow — §8.2)

Runs **non-root** (uid 1000 `agent` — `node` base images ship a uid-1000 `node` user; either
reuse it or create `agent`, but the uid must be non-zero and the home dir writable). Consumes
`/workspace` (the injected repo), `/task/prompt.md`, `/run/secrets/*` (credential), writes
`/output/report.md`. `krayt-ask` is **bind-mounted by the guest** onto
`/usr/local/bin/krayt-ask` — do NOT bake it into the image.

## Dockerfile

Mirror `images/agents/claude-code/Dockerfile`: digest-pinned base, minimal apt layer, non-root
user, `COPY entrypoint.sh` → `/usr/local/bin/krayt-agent-entrypoint` (build context is
`images/agents/gemini-cli`), pinned CLI install, telemetry/update-check-off `ENV`. For Gemini
CLI, verify against current upstream docs which switches disable usage statistics, update
checks, and any other non-model network traffic (settings.json keys and/or env vars) — the
sandbox has allowlisted egress (§6.6), so the steady-state destination must be the model API
only.

## Entrypoint

Same skeleton as the claude-code entrypoint (credential export with the permission-mismatch
diagnostics, `git config --global --add safe.directory`, tee output to `/output/report.md`),
adapted to Gemini:

1. Export exactly one of `GEMINI_API_KEY` / `GOOGLE_API_KEY` from `/run/secrets` (mirror
   `internal/adapter/geminicli.go` — the host adapter already enforced exactly-one pre-boot).
2. When `KRAYT_ASK_SOCKET` is set and `krayt-ask` is on PATH, register the `ask_human` MCP
   server (`krayt-ask --mcp` with `KRAYT_ASK_SOCKET` in its env). Gemini CLI configures MCP
   servers via its settings file (`~/.gemini/settings.json`, `mcpServers` key) — verify the
   current format against upstream docs; write it at runtime like the claude entrypoint writes
   its `.mcp.json`.
3. Run headlessly against the task prompt with auto-approved edits — safe because the run is
   already isolated in the krayt micro-VM. Verify the current non-interactive invocation
   (prompt flag + yolo/approval-mode flag) against upstream docs; honor `GEMINI_MODEL` (or the
   CLI's native model env/flag) if set, like the claude entrypoint honors `ANTHROPIC_MODEL`.

## Docs

- `images/agents/gemini-cli/README.md`: usage, secrets contract, required `--allow` hosts
  (with an API key the model endpoint is `generativelanguage.googleapis.com` — verify, and
  document anything else the CLI needs), and the same `FROM` + `apt-get` extension snippet as
  the claude-code image README.
- Main README "Agent images" table: add the row (image, `--agent gemini-cli`, credential,
  `--allow` hosts). Example run:

  ```bash
  krayt run --image ghcr.io/418-cloud/krayt-agent-gemini-cli --agent gemini-cli \
    --task ./task.md --repo . --secrets ./secrets.env --allow generativelanguage.googleapis.com
  ```

## Verify

`hadolint` the Dockerfile; `bash -n` the entrypoint; keep the repo build/test/lint green. You
cannot `docker build`/push or run with a live credential here — log to `HUMAN_TODO.md`:
(1) a real `agent-images.yml` run publishing this image, (2) a live run exactly as the README
shows (real `GEMINI_API_KEY`), confirming a patch and `report.md` come back, including one
`--on-question=wait` run exercising `ask_human` over MCP. Never fabricate either result.
