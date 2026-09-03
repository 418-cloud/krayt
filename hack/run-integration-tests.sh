#!/usr/bin/env bash
# run-integration-tests.sh — hardware verification for a real msb-backed `krayt run`
# (run-tasks-on-microsandbox.md). There is no more `//go:build integration` Go suite: the
# vfkit/firecracker providers this script used to drive are deleted, and their replacement — msb —
# is a real subprocess with no in-process fake worth booting a VM to test twice (internal/sandbox's
# and internal/orchestrator's own unit tests already exercise the driver and the lifecycle against
# a scriptable fake msb). What is left to verify only on real hardware is msb itself: does a real
# sandbox boot, does a real agent image run in it, does `ask_human` round-trip over the guest's
# vsock dial to the host.
#
# This script is the runnable form of that recipe. It needs an Apple-Silicon Mac (or any host with
# msb installed) and a network-reachable model credential; it does not run in CI.
#
# Usage:
#   hack/run-integration-tests.sh                 # plain run + one --on-question=wait run
#
# Required environment:
#   KRAYT_IMAGE      real agent image, e.g. ghcr.io/418-cloud/krayt-agent-claude-code:latest
#   KRAYT_SECRETS    path to a secrets file with a real model credential (§6.8)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

: "${KRAYT_IMAGE:?set KRAYT_IMAGE to a real agent image, e.g. ghcr.io/418-cloud/krayt-agent-claude-code:latest}"
: "${KRAYT_SECRETS:?set KRAYT_SECRETS to a secrets file carrying a real model credential}"

echo "==> Building krayt"
make build

echo "==> Preflight: krayt doctor"
if ! ./bin/krayt doctor; then
  echo "error: 'krayt doctor' reported unmet prerequisites (see its output above) — install msb" >&2
  echo "       (https://github.com/superradcompany/microsandbox) and re-run." >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
task_file="$work/task.md"
echo "Make a small, harmless edit to README.md (e.g. fix a typo) and stop." > "$task_file"

echo "==> Plain run (fail mode)"
./bin/krayt run --image "$KRAYT_IMAGE" --task "$task_file" --secrets "$KRAYT_SECRETS" \
  --agent claude-code --allow api.anthropic.com

echo "==> Run with ask_human wired (--on-question=wait) — answer it with: krayt answer <run-id> <answer>"
question_task="$work/question-task.md"
echo "Before making any change, ask the human whether you should proceed. Then stop." > "$question_task"
./bin/krayt run --image "$KRAYT_IMAGE" --task "$question_task" --secrets "$KRAYT_SECRETS" \
  --agent claude-code --allow api.anthropic.com --on-question wait --detach

echo "==> Both runs launched. Verify: krayt ls, krayt attach <run-id>, and (for the second run)"
echo "    krayt answer <run-id> <your answer> once it reaches 'waiting'."
