#!/usr/bin/env bash
# krayt claude-code adapter entrypoint (§6.14, §8.2). It:
#   1. materializes the model credential from the per-task secrets tmpfs (/run/secrets) into the
#      environment — the in-container half of agent auth; the host adapter already enforced the
#      exactly-one rule before boot (§6.14);
#   2. runs Claude Code non-interactively against the task, editing /workspace (which krayt diffs
#      into changes.patch);
#   3. writes Claude's final summary to /output/report.md, which krayt folds into the run
#      report's Notes section (§8.4).
set -euo pipefail

SECRETS_DIR="${KRAYT_SECRETS_DIR:-/run/secrets}"
WORKSPACE="${KRAYT_WORKSPACE:-/workspace}"
TASK_FILE="${KRAYT_TASK:-/task/prompt.md}"

# Export exactly one recognized credential from the secrets tmpfs (§6.14). The host adapter
# already guaranteed exactly one is present; this just turns the file into an env var Claude
# Code reads.
cred=""
for key in ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_AUTH_TOKEN; do
  if [ -f "$SECRETS_DIR/$key" ]; then
    export "$key=$(cat "$SECRETS_DIR/$key")"
    cred="$key"
    break
  fi
done
if [ -z "$cred" ]; then
  echo "[claude-code] no credential in $SECRETS_DIR (expected ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN)" >&2
  # Diagnostics: the usual cause is a permissions mismatch — krayt wrote the secrets tmpfs
  # root-only while this container runs non-root, so it can't read them. Print who we are and
  # what (if anything) we can see.
  echo "[claude-code] diag: running as $(id)" >&2
  if ls -la "$SECRETS_DIR" >&2 2>&1; then :; else
    echo "[claude-code] diag: cannot list $SECRETS_DIR — a non-root container can't read a root-only secrets mount (needs the krayt guest secrets-perms fix + base image rebuild)" >&2
  fi
  exit 78 # EX_CONFIG
fi
echo "[claude-code] authenticated via $cred"

if [ ! -f "$TASK_FILE" ]; then
  echo "[claude-code] task file $TASK_FILE not found" >&2
  exit 66 # EX_NOINPUT
fi

cd "$WORKSPACE"

# The workspace's .git is owned by root (the guest ingests it as root, then makes the tree
# writable), so the non-root agent's own git commands would refuse it with "dubious ownership".
# Mark it safe for this user.
git config --global --add safe.directory "$WORKSPACE" 2>/dev/null || true
git config --global --add safe.directory '*' 2>/dev/null || true

echo "[claude-code] running claude -p in $(pwd) (model: ${ANTHROPIC_MODEL:-default})"
# Print/headless mode with autonomous edits — safe because the whole run is already isolated in
# the krayt micro-VM, so the tool-permission prompts add nothing. Claude reads ANTHROPIC_MODEL
# from the environment if set. Tee its final summary into /output/report.md so it surfaces in the
# krayt report's Notes; pipefail keeps the pipeline's exit code Claude's, not tee's.
claude -p "$(cat "$TASK_FILE")" --dangerously-skip-permissions | tee /output/report.md
echo "[claude-code] done"
