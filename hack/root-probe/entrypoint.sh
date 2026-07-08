#!/bin/sh
# If this ever executes, withEnforceNonRoot() (internal/guest/runner/containerd_linux.go, §8.2)
# failed to reject a root (uid 0) image before the container launched -- that is exactly the
# regression TestRootImageFailsClosed exists to catch. Say so loudly and fail.
echo "root-probe: SHOULD NEVER RUN -- withEnforceNonRoot did not reject a root (uid 0) image" >&2
id >&2
exit 99
