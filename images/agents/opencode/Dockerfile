# syntax=docker/dockerfile:1 # renovate: ignore
# krayt-agent-opencode — the published, ready-to-run opencode onboarding image. Pulls,
# `krayt run --agent opencode`, no image building required. Sibling of
# images/agents/claude-code/ and images/agents/gemini-cli/ (see those images' READMEs for the
# shared pattern) under the shared ghcr.io/418-cloud/krayt-agent-opencode name (see
# images/agents/opencode/README.md).
#
# opencode ships a self-contained binary (no npm/native-installer story), so the base is plain
# debian:bookworm-slim (glibc) — same tag+digest already pinned in images/agents/claude-code/
# (reused rather than re-resolved, since it's the identical upstream reference) — keeping the
# `apt-get` extension story uniform across every agent image.
FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818

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
    curl -fsSL -o /tmp/opencode.tar.gz \
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
 && curl -fsSL -o /home/agent/.cache/opencode/models.json https://models.opencode.ai/api.json \
 && chown -R agent:agent /home/agent/.cache

USER agent
WORKDIR /home/agent

# Keep egress tight for the krayt allowlist (§6.6): no autoupdate checks, and no models.dev
# refetch now that the catalog is baked in above (also removes the hourly background-refresh
# fetch loop). opencode has no separate telemetry toggle to verify/disable (confirmed against
# its own CLI env-var reference — only auto-update, prune, and models-fetch are steady-state
# network/disk behaviors worth turning off here).
ENV OPENCODE_DISABLE_AUTOUPDATE=true \
    OPENCODE_DISABLE_MODELS_FETCH=true

ENTRYPOINT ["krayt-agent-entrypoint"]
