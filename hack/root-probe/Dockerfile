# syntax=docker/dockerfile:1 # renovate: ignore
# Negative-control image for TestRootImageFailsClosed (§8.2, security-review findings #1/#3):
# deliberately root (no USER set -> uid 0). withEnforceNonRoot must reject the run before this
# container ever starts, so the entrypoint below is not expected to execute; it exists only so
# the image is a normal runnable container and fails loudly if the fail-closed control regresses.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

COPY --chmod=0755 entrypoint.sh /usr/local/bin/root-probe

# Intentionally no USER directive: this image runs as uid 0 (root), the exact condition
# withEnforceNonRoot must reject (KRAYT_ROOT_IMAGE contract, HUMAN_TODO.md).
ENTRYPOINT ["root-probe"]
