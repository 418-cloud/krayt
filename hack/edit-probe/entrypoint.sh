#!/bin/sh
# edit-probe: the positive control for the real-VM end-to-end suite. It appends a fixed line to
# the repo's greeting.txt (creating it if missing) inside the writable /workspace bind mount and
# exits 0. After the container exits, the guest-agent generates changes.patch from the workspace
# (internal/patch.Diff); TestEndToEndRealVM asserts that patch is produced and applies cleanly to
# the host repo.
#
# Appending to the EXISTING file — rather than writing an unrelated new one — matters for
# TestConcurrentRealVMs: it seeds each VM's clone with a per-run marker line in greeting.txt and
# checks the resulting patch still contains that marker, as proof the VMs' workspaces never
# crossed ("isolation is checked by construction, not by inspection", per that test's own
# comment). git's default 3-line diff context carries the untouched marker line straight into the
# patch alongside the appended edit. A blind overwrite/new-file edit would never carry that marker
# through, and the isolation check would fail regardless of whether isolation actually held — no
# sentinel/attack logic here otherwise, this is the happy-path control, not a security probe.
set -eu

echo "[edit-probe] start (uid=$(id -u))"

if [ ! -d /workspace ]; then
  echo "[edit-probe] /workspace missing -- not bind-mounted as expected" >&2
  exit 10
fi

target=/workspace/greeting.txt
[ -f "$target" ] || : > "$target"
printf 'edited by krayt edit-probe\n' >> "$target"
echo "[edit-probe] appended to $target"

echo "[edit-probe] done"
