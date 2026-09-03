# syntax=docker/dockerfile:1 # renovate: ignore
# krayt-agent-gemini-cli — the published, ready-to-run Gemini CLI onboarding image. Pulls,
# `krayt run --agent gemini-cli`, no image building required. Sibling of
# images/agents/claude-code/ (see that image's README for the shared pattern) under the shared
# ghcr.io/418-cloud/krayt-agent-gemini-cli name (see images/agents/gemini-cli/README.md).
#
# Gemini CLI is an npm package (Node.js), so the base is node:24-trixie-slim — still Debian
# underneath (trixie, not bookworm — see images/agents/claude-code/Dockerfile for why: rtk's
# arm64 build needs glibc >= 2.39, which bookworm's 2.36 doesn't clear but trixie's 2.41 does),
# so the `apt-get` extension story matches the other agent images. 24 is upstream's current
# Active LTS ("krypton") as of this writing.
FROM node:24-trixie-slim@sha256:50c3b2f6988dfc307b86e5301d69611af31f4789bdf232863b07d3b02fe55ae0

# ca-certificates: HTTPS to the model API and npm registry; curl: install-time fetches; git:
# Gemini CLI shells out to it for repo ops; bash: the entrypoint. Nothing else — extend via
# `FROM` + `apt-get` (see README).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git bash \
 && rm -rf /var/lib/apt/lists/*

# CLI version pinned so a run is reproducible; bumped by Renovate's custom regex manager
# (renovate.json). Installed as root (global npm installs need a root-owned prefix, unlike
# Claude Code's user-local installer) — the resulting /usr/local/bin/gemini is world-executable,
# so the non-root user below can still run it.
ARG GEMINI_CLI_VERSION=0.55.1
RUN npm install -g @google/gemini-cli@${GEMINI_CLI_VERSION} \
 && npm cache clean --force

COPY --chmod=0755 entrypoint.sh /usr/local/bin/krayt-agent-entrypoint

# rtk (Rust Token Killer, Apache-2.0, https://github.com/rtk-ai/rtk): sits in front of common dev
# commands and compresses their output before this headless agent reads it — every byte of
# command output here becomes model context. Installed as a pinned prebuilt release tarball, the
# same exception pattern as hack/krayt-dev's protoc/gh/hadolint layers: no Rust toolchain, no
# `cargo install` (the crates.io `rtk` crate is an unrelated project, "Rust Type Kit — query Rust
# types and produce FFI types"). Asset names are per-arch and NOT symmetric — amd64 is musl,
# arm64 is gnu, and the gnu build's glibc floor is why this image is trixie (see the FROM comment
# above). Bumped by Renovate's custom regex manager (renovate.json). The binary lands at
# /usr/local/lib/rtk/rtk, deliberately NOT on PATH — the `rtk` wrapper below is what's actually on
# PATH, so KRAYT_RTK=off (see ./rtk) can't be bypassed by anything resolving `rtk` normally.
ARG RTK_VERSION=0.45.0
ARG TARGETARCH
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

# node:24-trixie-slim already ships a non-root `node` user (uid 1000, home /home/node,
# useradd --create-home) — reuse it instead of creating a second one (§6.14/§8.2 only require
# some non-root uid).
USER node
WORKDIR /home/node

# Keep egress tight for the krayt allowlist (§6.6): no auto-update checks or anonymous usage
# stats, so the container's only steady-state destination is the model API. Gemini CLI has no
# env-var equivalent of these two (verified against docs/cli/settings.md and
# packages/cli/src/config/settingsSchema.ts) — settings.json is the only lever, so bake it here.
# The entrypoint MERGES into this same file at runtime to add mcpServers when questions are
# enabled, so these two keys (and rtk's hook, added below) stay in effect either way.
RUN mkdir -p /home/node/.gemini \
 && cat > /home/node/.gemini/settings.json <<'EOF'
{
  "general": { "enableAutoUpdate": false },
  "privacy": { "usageStatisticsEnabled": false }
}
EOF

# rtk's own telemetry is opt-in and off by default, and the run's egress allowlist denies unknown
# destinations anyway (§6.6) — belt-and-braces. Also load-bearing for the RUN below: `rtk init`
# prompts for telemetry consent unless this is set (verified against src/hooks/init.rs's
# prompt_telemetry_consent, which checks this var before its TTY heuristic — some non-interactive
# builders hand a process a pseudo-TTY, which would otherwise hang here).
ENV RTK_TELEMETRY_DISABLED=1

# Wire rtk's native Gemini CLI integration, as the image's own non-root user (rtk's --global
# writes into $HOME). This writes ~/.gemini/hooks/rtk-hook-gemini.sh — a thin wrapper that does
# `exec rtk hook gemini` (confirmed against rtk's own source, src/hooks/init.rs's
# GEMINI_HOOK_SCRIPT constant) — and MERGES a BeforeTool entry into the settings.json baked above
# (confirmed: patch_gemini_settings reads the existing file and inserts under the `hooks` key,
# rather than overwriting it, so it does not clobber the general/privacy keys set above). Must run
# AFTER the settings.json bake above, not before — reversing the order would let the bake's
# wholesale `cat >` erase whatever rtk just registered, the same class of bug fixed in
# entrypoint.sh (see its own rtk merge comment). --auto-patch is required here, not just for
# claude-code: confirmed against src/hooks/init.rs that Gemini's patch, same as Claude's, defaults
# to an interactive y/N prompt reading stdin when this flag is absent, which a non-interactive
# `docker build` cannot answer. `rtk init --show` only ever reports Claude/Codex status
# (src/hooks/init.rs's show_config), so it would misleadingly print "not registered" here; cat the
# actual settings.json into the build log instead as evidence the hook merged in.
RUN rtk init --global --gemini --auto-patch && cat "$HOME/.gemini/settings.json"

ENTRYPOINT ["krayt-agent-entrypoint"]
