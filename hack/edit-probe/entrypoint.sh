#!/bin/sh
# edit-probe: the positive control for the real-VM end-to-end suite. It makes a single
# deterministic, idempotent edit inside the writable /workspace bind mount and exits 0. After the
# container exits, the guest-agent generates changes.patch from the workspace (internal/patch.Diff);
# TestEndToEndRealVM asserts that patch is produced and applies cleanly to the host repo, and
# TestConcurrentRealVMs runs several of these at once. No sentinel/attack logic — this proves the
# happy path works, not that a control holds.
set -eu

echo "[edit-probe] start (uid=$(id -u))"

if [ ! -d /workspace ]; then
  echo "[edit-probe] /workspace missing -- not bind-mounted as expected" >&2
  exit 10
fi

# One fixed line in a fixed file. Idempotent: re-running overwrites the same content, so the patch
# is identical every time (deterministic across the concurrent runs, too).
printf 'edited by krayt edit-probe\n' > /workspace/EDITED_BY_KRAYT.txt
echo "[edit-probe] wrote /workspace/EDITED_BY_KRAYT.txt"

echo "[edit-probe] done"
