# syntax=docker/dockerfile:1 # renovate: ignore
# krayt-agent-opencode — the published, ready-to-run opencode onboarding image. Pulls,
# `krayt run --agent opencode`, no image building required. Sibling of
# images/agents/claude-code/ and images/agents/gemini-cli/ (see those images' READMEs for the
# shared pattern) under the shared ghcr.io/418-cloud/krayt-agent-opencode name (see
# images/agents/opencode/README.md).
#
# opencode ships a self-contained binary (no npm/native-installer story), so the base is plain
# debian:trixie-slim (glibc) — same tag already used in images/agents/claude-code/ (reused rather
# than re-resolved, since it's the identical upstream reference — see that Dockerfile for why
# trixie specifically) — keeping the `apt-get` extension story uniform across every agent image.
FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

# git: opencode shells out to it for repo ops; curl + ca-certificates: install-time fetches +
# HTTPS to the model API; bash: the entrypoint. Nothing else — extend via `FROM` + `apt-get`
# (see README).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git bash \
 && rm -rf /var/lib/apt/lists/*

# Non-root agent user (uid 1000), matching the other agent images (§6.14/§8.2 only require some
# non-root uid).
RUN useradd --create-home --uid 1000 --shell /bin/bash agent

COPY --chmod=0755 entrypoint.sh /usr/local/bin/krayt-agent-entrypoint

# CLI version pinned so a run is reproducible; bumped by Renovate's custom regex manager
# (renovate.json). opencode has no Dockerfile-manager datasource (single-binary release, not a
# base image or npm package) — fetched as a release tarball from its GitHub releases, same
# exception pattern as hack/krayt-dev's protoc/gh layers. TARGETARCH is buildx's standard
# per-platform arg; opencode's asset names use "x64" for amd64 (verified against the repo's own
# opencode.ai/install script, which performs the same translation).
ARG OPENCODE_VERSION=1.18.16
ARG TARGETARCH
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) oc_arch=x64 ;; \
      arm64) oc_arch=arm64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 -o /tmp/opencode.tar.gz \
      "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${oc_arch}.tar.gz" \
 && tar -xzf /tmp/opencode.tar.gz -C /tmp \
 && install -m 0755 /tmp/opencode /usr/local/bin/opencode \
 && rm -f /tmp/opencode.tar.gz /tmp/opencode

# Egress caution (§6.6): opencode fetches its provider/model catalog from models.opencode.ai at
# startup (packages/core/src/models-dev.ts upstream) and refreshes it hourly in the background.
# With only the model API host allowlisted that fetch would fail every run. Bake a fresh catalog
# snapshot into the image's cache path instead (populate() reads this file with no freshness
# check — only the background refresh loop checks TTL, and that loop is disabled below via
# OPENCODE_DISABLE_MODELS_FETCH), so a run never needs to reach models.opencode.ai.
RUN mkdir -p /home/agent/.cache/opencode \
 && curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 -o /home/agent/.cache/opencode/models.json https://models.opencode.ai/api.json \
 && chown -R agent:agent /home/agent/.cache

# rtk (Rust Token Killer, Apache-2.0, https://github.com/rtk-ai/rtk): sits in front of common dev
# commands and compresses their output before this headless agent reads it — every byte of
# command output here becomes model context. Installed as a pinned prebuilt release tarball, the
# same exception pattern as opencode's own install above and hack/krayt-dev's protoc/gh/hadolint
# layers: no Rust toolchain, no `cargo install` (the crates.io `rtk` crate is an unrelated
# project, "Rust Type Kit — query Rust types and produce FFI types"). Asset names are per-arch and
# NOT symmetric — amd64 is musl, arm64 is gnu, and the gnu build's glibc floor is why this image
# is trixie (see the FROM comment above). Bumped by Renovate's custom regex manager
# (renovate.json). The binary lands at /usr/local/lib/rtk/rtk, deliberately NOT on PATH — the
# `rtk` wrapper below is what's actually on PATH, so KRAYT_RTK=off (see ./rtk) can't be bypassed
# by anything resolving `rtk` normally.
ARG RTK_VERSION=0.45.0
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) rtk_target=x86_64-unknown-linux-musl ;; \
      arm64) rtk_target=aarch64-unknown-linux-gnu ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 -o /tmp/rtk.tar.gz \
      "https://github.com/rtk-ai/rtk/releases/download/v${RTK_VERSION}/rtk-${rtk_target}.tar.gz" \
 && tar -xzf /tmp/rtk.tar.gz -C /tmp \
 && install -d -m 0755 /usr/local/lib/rtk \
 && install -m 0755 /tmp/rtk /usr/local/lib/rtk/rtk \
 && rm -f /tmp/rtk.tar.gz /tmp/rtk

COPY --chmod=0755 rtk /usr/local/bin/rtk

USER agent
WORKDIR /home/agent

# Keep egress tight for the krayt allowlist (§6.6): no autoupdate checks, and no models.dev
# refetch now that the catalog is baked in above (also removes the hourly background-refresh
# fetch loop). opencode has no separate telemetry toggle to verify/disable (confirmed against
# its own CLI env-var reference — only auto-update, prune, and models-fetch are steady-state
# network/disk behaviors worth turning off here).
ENV OPENCODE_DISABLE_AUTOUPDATE=true \
    OPENCODE_DISABLE_MODELS_FETCH=true

# rtk's own telemetry is opt-in and off by default, and the run's egress allowlist denies unknown
# destinations anyway (§6.6) — belt-and-braces. Also load-bearing for the RUN below: `rtk init`
# prompts for telemetry consent unless this is set (verified against src/hooks/init.rs's
# prompt_telemetry_consent, which checks this var before its TTY heuristic — some non-interactive
# builders hand a process a pseudo-TTY, which would otherwise hang here).
ENV RTK_TELEMETRY_DISABLED=1

# Wire rtk's native OpenCode integration, as the image's own non-root user (rtk's --global writes
# into $HOME). This writes ~/.config/opencode/plugins/rtk.ts — a thin plugin that shells out to
# `rtk rewrite` (confirmed against rtk's own source, hooks/opencode/rtk.ts) — and nothing under
# ~/.claude: --opencode makes `rtk init` install ONLY the OpenCode plugin, not Claude Code's hook
# too (confirmed against src/hooks/init.rs's dispatch: `--opencode` sets install_claude = false).
# `rtk init --show` only ever reports Claude/Codex status (src/hooks/init.rs's show_config), so it
# would misleadingly print "not registered" here; cat the actual plugin file into the build log
# instead as evidence it was written.
RUN rtk init --global --opencode && cat "$HOME/.config/opencode/plugins/rtk.ts"

ENTRYPOINT ["krayt-agent-entrypoint"]
