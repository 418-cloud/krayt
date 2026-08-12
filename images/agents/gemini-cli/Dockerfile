# syntax=docker/dockerfile:1 # renovate: ignore
# krayt-agent-gemini-cli — the published, ready-to-run Gemini CLI onboarding image. Pulls,
# `krayt run --agent gemini-cli`, no image building required. Sibling of
# images/agents/claude-code/ (see that image's README for the shared pattern) under the shared
# ghcr.io/418-cloud/krayt-agent-gemini-cli name (see images/agents/gemini-cli/README.md).
#
# Gemini CLI is an npm package (Node.js), so the base is node:24-bookworm-slim — still Debian
# underneath, so the `apt-get` extension story matches the other agent images. 24 is upstream's
# current Active LTS ("krypton") as of this writing.
#
# NOT digest-pinned: resolving the current node:24-bookworm-slim digest needs registry access
# this build environment didn't have (see HUMAN_TODO.md). Renovate (pinDigests: true for the
# dockerfile manager, renovate.json) will open the bootstrap pin PR on its next run — merge that
# instead of hand-pinning.
FROM node:24-bookworm-slim@sha256:3638d9a6fe4030bd716be989438248074489337ba3275657f93595428be4fc03

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

# node:24-bookworm-slim already ships a non-root `node` user (uid 1000, home /home/node,
# useradd --create-home) — reuse it instead of creating a second one (§6.14/§8.2 only require
# some non-root uid).
USER node
WORKDIR /home/node

# Keep egress tight for the krayt allowlist (§6.6): no auto-update checks or anonymous usage
# stats, so the container's only steady-state destination is the model API. Gemini CLI has no
# env-var equivalent of these two (verified against docs/cli/settings.md and
# packages/cli/src/config/settingsSchema.ts) — settings.json is the only lever, so bake it here.
# The entrypoint rewrites this same file at runtime to add mcpServers when questions are enabled,
# repeating these two keys so they stay in effect either way.
RUN mkdir -p /home/node/.gemini \
 && cat > /home/node/.gemini/settings.json <<'EOF'
{
  "general": { "enableAutoUpdate": false },
  "privacy": { "usageStatisticsEnabled": false }
}
EOF

ENTRYPOINT ["krayt-agent-entrypoint"]
