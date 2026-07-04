# syntax=docker/dockerfile:1
# A real-world krayt agent image: runs Claude Code headlessly against the task and edits
# /workspace, which krayt diffs into changes.patch (§6.14, §8.2). This is the image behind the
# HUMAN_TODO "[Phase 5] Agent adapter end-to-end with live credentials".
#
# Debian slim (glibc) so the official native `claude` binary runs; a dedicated non-root user
# because Claude Code refuses to run as uid 0 (§8.2 container contract).
FROM debian:bookworm-slim

# git: Claude Code shells out to it for repo ops; curl + ca-certificates: install + HTTPS to the
# model API; bash: the entrypoint.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git bash \
 && rm -rf /var/lib/apt/lists/*

# Non-root agent user (uid 1000). Any non-root uid satisfies Claude Code (§6.14/§8.2).
RUN useradd --create-home --uid 1000 --shell /bin/bash agent

COPY --chmod=0755 entrypoint.sh /usr/local/bin/krayt-claude-entrypoint

USER agent
WORKDIR /home/agent

# Official installer (native binary; the npm package is deprecated). Installs to ~/.local/bin.
RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH="/home/agent/.local/bin:${PATH}"

# Keep egress tight for the krayt allowlist (§6.6): no autoupdater, telemetry, or error
# reporting, so the container's only steady-state destination is the model API.
ENV DISABLE_AUTOUPDATER=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

ENTRYPOINT ["krayt-claude-entrypoint"]
