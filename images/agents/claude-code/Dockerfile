# syntax=docker/dockerfile:1 # renovate: ignore
# krayt-agent-claude-code — the published, ready-to-run Claude Code onboarding image. Pulls,
# `krayt run --agent claude-code`, no image building required. Mirrors hack/claude-code/ (an
# integration/demo fixture that stays as-is) but pins the CLI version and ships under the shared
# ghcr.io/418-cloud/krayt-agent-claude-code name (see images/agents/claude-code/README.md).
#
# Debian slim (glibc) so the official native `claude` binary runs; a dedicated non-root user
# because Claude Code refuses to run as uid 0 (§8.2 container contract).
#
# trixie, not bookworm: rtk's arm64 release (rtk-aarch64-unknown-linux-gnu.tar.gz) declares
# GLIBC_2.39 in its libc.so.6 version requirements. bookworm ships glibc 2.36 — the binary would
# install and then fail to start with "version 'GLIBC_2.39' not found", on the exact arch
# (arm64) Apple-Silicon dogfooding runs on. trixie ships glibc 2.41, comfortably over that floor.
#
# NOT digest-pinned: resolving the current debian:trixie-slim digest needs registry access this
# build environment didn't have (see HUMAN_TODO.md). Renovate (pinDigests: true for the
# dockerfile manager, renovate.json) will open the bootstrap pin PR on its next run — merge that
# instead of hand-pinning.
FROM debian:trixie-slim

# git: Claude Code shells out to it for repo ops; curl + ca-certificates: install + HTTPS to the
# model API; bash: the entrypoint. Nothing else — extend via `FROM` + `apt-get` (see README).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git bash \
 && rm -rf /var/lib/apt/lists/*

# Non-root agent user (uid 1000). Any non-root uid satisfies Claude Code (§6.14/§8.2).
RUN useradd --create-home --uid 1000 --shell /bin/bash agent

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

USER agent
WORKDIR /home/agent

# CLI version pinned so a run is reproducible; bumped by Renovate's custom regex manager
# (renovate.json). Official installer (native binary; the npm package is deprecated) — `-s
# <version>` pins the exact release instead of tracking `latest`. Downloaded to a file and run
# explicitly (rather than piped straight into bash) so a failed/truncated fetch is caught by
# curl's own exit code instead of silently executing a partial script.
ARG CLAUDE_CODE_VERSION=2.1.226
RUN curl -fsSL -o /tmp/claude-install.sh https://claude.ai/install.sh \
 && bash /tmp/claude-install.sh "${CLAUDE_CODE_VERSION}" \
 && rm -f /tmp/claude-install.sh
ENV PATH="/home/agent/.local/bin:${PATH}"

# Keep egress tight for the krayt allowlist (§6.6): no autoupdater, telemetry, or error
# reporting, so the container's only steady-state destination is the model API.
ENV DISABLE_AUTOUPDATER=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

# rtk's own telemetry is opt-in and off by default, and the run's egress allowlist denies unknown
# destinations anyway (§6.6) — belt-and-braces, matching DISABLE_TELEMETRY above. Also load-bearing
# for the RUN below: `rtk init` prompts for telemetry consent unless this is set (verified against
# src/hooks/init.rs's prompt_telemetry_consent, which checks this var before its TTY heuristic —
# some non-interactive builders hand a process a pseudo-TTY, which would otherwise hang here).
ENV RTK_TELEMETRY_DISABLED=1

# Wire rtk's native Claude Code integration, as the image's own non-root user (rtk's --global
# writes into $HOME). This registers a PreToolUse entry in ~/.claude/settings.json running
# `rtk hook claude` — v0.45.0's native in-process hook, confirmed against rtk's own source
# (src/hooks/init.rs) to be what --auto-patch's default mode (neither --claude-md nor
# --hook-only) writes; it does NOT write a ~/.claude/hooks/rtk-rewrite.sh script, so no `jq`
# dependency is needed here (that legacy shell-script path is what would need it). --auto-patch
# is init's documented non-interactive settings.json-patch flag (confirmed against the real
# `Init` arg definitions in src/main.rs, not just --help, since
# release-assets.githubusercontent.com is unreachable from this build environment — see
# HUMAN_TODO.md). `rtk init --show` prints "[ok] Hook: rtk hook claude (native binary command)"
# into the build log as evidence the hook actually registered, rather than something to take on
# faith.
RUN rtk init --global --auto-patch && rtk init --show

ENTRYPOINT ["krayt-agent-entrypoint"]
