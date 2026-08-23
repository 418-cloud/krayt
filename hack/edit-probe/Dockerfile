# syntax=docker/dockerfile:1 # renovate: ignore
# Positive-control agent image for TestEndToEndRealVM / TestConcurrentRealVMs (§14 Phase 2): a
# trivial, credential-free user image whose entrypoint edits one file in /workspace and exits 0.
# This is the KRAYT_IMAGE the end-to-end suite needs — "a trivial user image that edits a file in
# /workspace" (internal/orchestrator/integration_test.go's header) — so the whole integration
# suite can run with zero live LLM credentials. Unlike the security probes it asserts nothing from
# inside the container; it is the happy-path control that proves a real run captures a patch.
# Alpine gives a small image with a shell + coreutils (same shape as ../gitconfig-probe).
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# Non-root user (uid 1000) -- matches the other probes and withEnforceNonRoot (§8.2). The run is
# meant to look like an ordinary least-privilege agent, not a root escape.
RUN adduser -D -u 1000 agent

COPY --chmod=0755 entrypoint.sh /usr/local/bin/edit-probe

USER agent
ENTRYPOINT ["edit-probe"]
