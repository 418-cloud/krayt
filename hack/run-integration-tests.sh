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
# The runs happen in a throwaway git repo under $TMPDIR (see below), never in this checkout —
# this repo's krayt.yaml is not auto-loadable, and a hardware check should not depend on it.
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

# `make build` is `go build ./...`, which compiles every package but writes no artifact — it
# would leave ./bin/krayt absent (or, worse, a stale binary from an earlier build, silently
# verifying the wrong code). `make krayt` is the target that produces the binary, and it pulls in
# guest-bins so the embed carries real krayt-helper/krayt-ask binaries to copy into the sandbox.
echo "==> Building krayt"
make krayt

echo "==> Preflight: krayt doctor"
if ! ./bin/krayt doctor; then
  echo "error: 'krayt doctor' reported unmet prerequisites (see its output above) — install msb" >&2
  echo "       (https://github.com/superradcompany/microsandbox) and re-run." >&2
  exit 1
fi

# The runs below target a THROWAWAY repo, not this one. krayt auto-loads `<repo>/krayt.yaml`
# (run.go's applyConfig), and this repo's own dogfood config sets `network.inject`, which §8.3
# refuses for an auto-loaded config — "a repo you did not write must not be able to name which
# credential is injected into which host's requests". Run from the repo root, krayt therefore
# stops before it starts. A scratch repo has no krayt.yaml to pick up, so the run is driven
# entirely by KRAYT_IMAGE/KRAYT_SECRETS and the flags below — which is what this script's
# "Required environment" contract already promises, and keeps the hardware check independent of
# whatever krayt.yaml happens to say.
#
# It is deliberately NOT deleted on exit. The detached run outlives this script, and its artifacts
# (changes.patch, report.md, meta.json) are the things being verified.
scratch="$(mktemp -d)/repo"
mkdir -p "$scratch"
git init -q -b main "$scratch"
printf '# scratch\n\nThrowaway repo for krayt hardware verification.\n' > "$scratch/README.md"
git -C "$scratch" add -A
git -C "$scratch" -c user.name='krayt verify' -c user.email='verify@example.invalid' \
  commit -qm 'init'
echo "==> Scratch repo: $scratch"

# Both prompts are piped in on stdin (`--task -`) rather than written to a temp file. That is not
# a style choice: --detach re-execs the same argv in a background supervisor child, which reads
# `--task <path>` from disk itself, moments after this script returns. A temp file removed by an
# EXIT trap is therefore gone before the child reads it, and the detached run dies with "read task
# file: no such file or directory". krayt already solves this for stdin — the parent spools the
# bytes it read to .krayt/runs/<id>/prompt.md and points the child at that (§6.2) — so stdin is
# the one prompt source with a lifetime that outlives this script.

echo "==> Plain run (fail mode)"
printf '%s\n' "Make a small, harmless edit to README.md (e.g. fix a typo) and stop." |
  ./bin/krayt run --repo "$scratch" --image "$KRAYT_IMAGE" --task - --secrets "$KRAYT_SECRETS" \
    --agent claude-code --allow api.anthropic.com

# The prompt has to name the TOOL, not just the intention. "Ask the human whether you should
# proceed" reads to an agent as an instruction to ask conversationally: Claude Code answered in
# prose, exited 0 in 5s, and never touched the MCP server — which looks like a passing run while
# proving nothing about the ask channel. Naming ask_human explicitly, giving it a concrete
# decision to ask about, and forbidding it from answering its own question is what makes the
# round trip actually happen.
#
# The decision it asks about must also be one the agent can actually carry out, or answering
# "yes" proves only half the channel. An earlier version claimed "README.md has a typo"; the
# scratch README has none, so the agent correctly declined to invent one and the run finished
# with an empty patch — the answer reached it, but nothing downstream of the answer was
# exercised. Adding a section is always possible, so a "yes" here has to produce a real diff.
echo "==> Run with ask_human wired (--on-question=wait)"
printf '%s\n' "Before you change anything, you MUST call the ask_human tool to ask whether you should add a 'Status' section to README.md saying the repo is a scratch fixture. Do not answer that question yourself and do not edit any file until the human replies. If the answer is yes, make exactly that edit and stop." |
  ./bin/krayt run --repo "$scratch" --image "$KRAYT_IMAGE" --task - --secrets "$KRAYT_SECRETS" \
    --agent claude-code --allow api.anthropic.com --on-question wait --detach

# Print the detached run's REAL state rather than asserting it is healthy. This line used to say
# "still going" unconditionally; when the run had in fact died on startup, that claim sent two
# debugging rounds looking in the wrong place.
echo "==> Plain run finished. Detached run state right now:"
./bin/krayt ls --repo "$scratch" || true
echo "    (a run that is still starting may not show 'waiting' yet — re-run krayt ls to follow it)"
echo "    Every follow-up command needs --repo, since the state lives in the scratch repo:"
echo "        ./bin/krayt ls     --repo $scratch"
echo "        ./bin/krayt attach --repo $scratch <run-id>"
echo "        ./bin/krayt answer --repo $scratch <run-id> <your answer>   # once it reaches 'waiting'"
echo "        ./bin/krayt apply  --repo $scratch <run-id>"
echo "    Remove it when done: rm -rf $(dirname "$scratch")"
