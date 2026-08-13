# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status

**Completed:** Phase 8 (`move-egress-proxy-to-host.md`) hardware verification is complete — the
new `PinnedRef` is set and the Phase-3 egress hardware suite has passed on both backends
(Apple Silicon/vfkit and Linux/KVM/firecracker).

**Open:** the `krayt-agent-claude-code`, `krayt-agent-gemini-cli`, and (new)
`krayt-agent-opencode` published images each need a real CI run + a real live onboarding run —
see the `[tooling]` entries below. The gemini-cli image also needs its `node:24-bookworm-slim`
base digest pinned (currently tag-only — this sandbox's egress proxy has no route to a Docker
registry) and a `hadolint` pass (binary unreachable here too); the opencode image needs the same
`hadolint` pass, for the same reason. All non-blocking (nothing downstream depends on them yet;
the codex agent-image task builds on the *code* landing here, not on these verifications).

---

## [tooling] Publish `krayt-agent-claude-code` — real workflow run + live onboarding run
- Needed:
  1. A real push to `main` (or `workflow_dispatch`) triggering `.github/workflows/agent-images.yml`,
     confirming both the `linux/amd64` and `linux/arm64` builds succeed, the merge job assembles a
     multi-arch manifest, and `ghcr.io/418-cloud/krayt-agent-claude-code` is pushed with `:latest`,
     `:sha-<short>`, and `:2.1.226` (the pinned CLI version) — plus a check that the GHCR package is
     publicly visible (matching the other published images, e.g. `krayt-dev`/`krayt-probe`).
  2. A live onboarding run exactly as the main README's quickstart shows, with a real
     `ANTHROPIC_API_KEY`, confirming a `changes.patch` and `/output/report.md` come back:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-claude-code --agent claude-code \
       --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com
     ```
- Why the agent can't: no `docker build`/push access and no live Anthropic credential in this
  environment; also can't confirm GHCR package visibility without a real push.
- Exact steps/commands: push this change (or `gh workflow run agent-images.yml`) and watch the
  run; then, on a Mac with `krayt` built and the base VM image pulled, create a scratch repo +
  `task.md` + `secrets.env` (one `ANTHROPIC_API_KEY`) and run the quickstart command above.
- Verify success by: `agent-images.yml` green with both arches in the manifest
  (`docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-claude-code:latest` shows
  `linux/amd64,linux/arm64`); the GHCR package page loads without auth; `krayt ls` shows the run
  reaching `done` with `EXIT 0`; `.krayt/runs/<id>/changes.patch` applies cleanly and
  `report.md` contains Claude's summary.
- Blocking: no — the gemini-cli/opencode agent-image tasks depend on this task's *code* (the
  `agent-images.yml` scaffolding + README table) having landed, not on this verification.

## [tooling] Pin the `node:24-bookworm-slim` digest in `images/agents/gemini-cli/Dockerfile`
- Needed: `images/agents/gemini-cli/Dockerfile` currently reads `FROM node:24-bookworm-slim`
  (tag only, no `@sha256:...`) — every other base image in this repo is digest-pinned, and
  `renovate.json`'s `pinDigests: true` (for the `dockerfile` manager) expects one to maintain.
- Why the agent can't: this environment's egress proxy blocks `registry.npmjs.org`,
  `hub.docker.com`, `registry-1.docker.io`, `ghcr.io`, and GitHub release-asset downloads (all
  return `403`/connection-refused) — only `api.github.com` (via the authenticated `gh` CLI) is
  reachable, which has no route to a Docker registry's manifest digest. Inventing a
  plausible-looking `sha256:...` would silently break the build, which is worse than leaving it
  unpinned — see `CLAUDE.md`'s "never fabricate ... invented image digests."
- Exact steps/commands: either (a) merge Renovate's bootstrap PR — `pinDigests: true` makes
  Renovate open a PR adding the digest to this exact unpinned `FROM` line on its next run, no
  human digest-lookup needed, or (b) resolve it manually first:
  `docker buildx imagetools inspect node:24-bookworm-slim` (or `crane digest
  node:24-bookworm-slim`) and hand-edit the `FROM` line.
- Verify success by: `FROM node:24-bookworm-slim@sha256:<digest>` in the Dockerfile, and
  `agent-images.yml` still builds both arches successfully off it.
- Blocking: no — the tag alone still resolves to a real, current image; this only affects
  reproducibility/supply-chain pinning, not whether the image builds or runs.

## [tooling] Publish `krayt-agent-gemini-cli` — real workflow run + live onboarding run
- Needed:
  1. A real push to `main` (or `workflow_dispatch`) triggering `.github/workflows/agent-images.yml`,
     confirming both the `linux/amd64` and `linux/arm64` builds succeed, the merge job assembles a
     multi-arch manifest, and `ghcr.io/418-cloud/krayt-agent-gemini-cli` is pushed with `:latest`,
     `:sha-<short>`, and `:0.55.1` (the pinned CLI version) — plus a check that the GHCR package is
     publicly visible (matching the other published images).
  2. A live onboarding run exactly as this image's README quickstart shows, with a real
     `GEMINI_API_KEY`, confirming a `changes.patch` and `/output/report.md` come back:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-gemini-cli --agent gemini-cli \
       --task ./task.md --repo . --secrets ./secrets.env --allow generativelanguage.googleapis.com
     ```
  3. One `--on-question=wait` run against the same image, prompting the task to ask a genuine
     question, confirming the `ask_human` MCP server (registered via the entrypoint's runtime
     `~/.gemini/settings.json` rewrite) actually surfaces the question to `krayt questions`/
     `krayt answer` and the run resumes — this is the one piece of MCP wiring that can't be
     verified without a real `gemini` process talking to a real socket.
- Why the agent can't: no `docker build`/push access, no live Gemini API key, and no way to
  drive a real `--on-question=wait` round-trip in this environment.
- Exact steps/commands: push this change (or `gh workflow run agent-images.yml`) and watch the
  run; then, on a Mac with `krayt` built and the base VM image pulled, create a scratch repo +
  `task.md` + `secrets.env` (one `GEMINI_API_KEY`) and run the quickstart command above; repeat
  with `--on-question=wait` and a task prompt that instructs the agent to ask a clarifying
  question before proceeding.
- Verify success by: `agent-images.yml` green with both arches in the manifest
  (`docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-gemini-cli:latest` shows
  `linux/amd64,linux/arm64`); the GHCR package page loads without auth; `krayt ls` shows the run
  reaching `done` with `EXIT 0`; `.krayt/runs/<id>/changes.patch` applies cleanly and
  `report.md` contains Gemini's response; the `--on-question=wait` run shows `waiting` in
  `krayt ls`, `krayt questions <run-id>` lists the real question, and `krayt answer` resumes it
  to completion.
- Blocking: no — nothing downstream depends on this verification; the opencode agent-image task
  builds on the `agent-images.yml` scaffolding landing, not on this run happening.

## [tooling] `hadolint` the gemini-cli Dockerfile
- Needed: `images/agents/gemini-cli/Dockerfile` was hand-reviewed against common hadolint rules
  (pinned `USER` before `ENTRYPOINT`, JSON-form `ENTRYPOINT`, apt lists cleaned up, no `latest`
  tag) but never actually run through `hadolint` — the binary isn't installed in this
  environment and the sandbox's egress proxy blocks the GitHub release-asset host it ships from
  (`release-assets.githubusercontent.com` → `403`), so it couldn't be fetched either.
- Why the agent can't: no package manager access or reachable download host for the `hadolint`
  binary in this sandbox.
- Exact steps/commands: `hadolint images/agents/gemini-cli/Dockerfile` (or
  `docker run --rm -i hadolint/hadolint < images/agents/gemini-cli/Dockerfile`).
- Verify success by: no unexpected findings (the image intentionally mirrors
  `images/agents/claude-code/Dockerfile`'s already-passing shape).
- Blocking: no — this is a lint pass, not a build/runtime correctness issue.

## [tooling] Publish `krayt-agent-opencode` — real workflow run + live onboarding run
- Needed:
  1. A real push to `main` (or `workflow_dispatch`) triggering `.github/workflows/agent-images.yml`,
     confirming both the `linux/amd64` and `linux/arm64` builds succeed (including the models.dev
     catalog snapshot fetched at build time — confirm it actually lands at
     `/home/agent/.cache/opencode/models.json` in the built image and isn't empty/stale), the
     merge job assembles a multi-arch manifest, and `ghcr.io/418-cloud/krayt-agent-opencode` is
     pushed with `:latest`, `:sha-<short>`, and `:1.18.16` (the pinned CLI version) — plus a check
     that the GHCR package is publicly visible (matching the other published images).
  2. A live onboarding run exactly as this image's README quickstart shows, with a real
     `ANTHROPIC_API_KEY` (one provider is enough per the task), confirming a `changes.patch` and
     `/output/report.md` come back:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-opencode --agent opencode \
       --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com
     ```
     Also confirms the egress mitigation actually holds: with only `api.anthropic.com`
     allowlisted, opencode must NOT hit `models.opencode.ai` (the baked-in catalog snapshot +
     `OPENCODE_DISABLE_MODELS_FETCH=true` should make that unnecessary) — a run that stalls or
     fails on a blocked catalog fetch means that mitigation didn't actually work and needs
     revisiting.
  3. One `--on-question=wait` run against the same image, prompting the task to ask a genuine
     question, confirming the `ask_human` MCP server (registered via the entrypoint's runtime
     `OPENCODE_CONFIG` file pointing at a generated `mcp` block) actually surfaces the question to
     `krayt questions`/`krayt answer` and the run resumes — this is the one piece of MCP wiring
     that can't be verified without a real `opencode` process talking to a real socket, and the
     first time this image's specific MCP config shape (`type: "local"`, array `command`) has been
     exercised against a live opencode binary rather than just checked against docs.
- Why the agent can't: no `docker build`/push access, no live provider API key, and no way to
  drive a real `--on-question=wait` round-trip in this environment.
- Exact steps/commands: push this change (or `gh workflow run agent-images.yml`) and watch the
  run; then, on a Mac with `krayt` built and the base VM image pulled, create a scratch repo +
  `task.md` + `secrets.env` (one `ANTHROPIC_API_KEY`) and run the quickstart command above; repeat
  with `--on-question=wait` and a task prompt that instructs the agent to ask a clarifying
  question before proceeding.
- Verify success by: `agent-images.yml` green with both arches in the manifest
  (`docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-opencode:latest` shows
  `linux/amd64,linux/arm64`); the GHCR package page loads without auth; `krayt ls` shows the run
  reaching `done` with `EXIT 0`; `.krayt/runs/<id>/changes.patch` applies cleanly and
  `report.md` contains opencode's response; the `--on-question=wait` run shows `waiting` in
  `krayt ls`, `krayt questions <run-id>` lists the real question, and `krayt answer` resumes it
  to completion.
- Blocking: no — nothing downstream depends on this verification; the codex agent-image task
  builds on the `agent-images.yml` scaffolding landing, not on this run happening.

## [tooling] `hadolint` the opencode Dockerfile
- Needed: `images/agents/opencode/Dockerfile` was hand-reviewed against common hadolint rules
  (pinned `USER` before `ENTRYPOINT`, JSON-form `ENTRYPOINT`, apt lists cleaned up, no `latest`
  tag, digest-pinned `FROM`) but never actually run through `hadolint` — same tooling gap as the
  gemini-cli entry above (binary not installed, release-asset host unreachable from this sandbox).
- Why the agent can't: no package manager access or reachable download host for the `hadolint`
  binary in this sandbox.
- Exact steps/commands: `hadolint images/agents/opencode/Dockerfile` (or
  `docker run --rm -i hadolint/hadolint < images/agents/opencode/Dockerfile`).
- Verify success by: no unexpected findings (the image intentionally mirrors
  `images/agents/claude-code/Dockerfile`'s already-passing shape, modulo the `TARGETARCH` case
  statement it shares with `hack/krayt-dev/Dockerfile`'s already-reviewed pattern instead).
- Blocking: no — this is a lint pass, not a build/runtime correctness issue.
