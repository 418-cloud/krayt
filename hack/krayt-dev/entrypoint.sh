#!/usr/bin/env bash
# krayt-dev adapter entrypoint (§6.14, §8.2) — the dogfooding image's entrypoint. This image is a
# non-root Claude Code agent plus krayt's dev toolchain; the entrypoint fulfills the contract:
#   1. materializes the model credential from the per-task secrets tmpfs (/run/secrets) into the
#      environment (§6.14);
#   2. runs Claude Code non-interactively against the task, editing /workspace — here, krayt's
#      own repo, so the agent can build/test/lint/regenerate proto before returning its patch;
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
  echo "[krayt-dev] no credential in $SECRETS_DIR (expected ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, or ANTHROPIC_AUTH_TOKEN)" >&2
  echo "[krayt-dev] diag: running as $(id)" >&2
  if ls -la "$SECRETS_DIR" >&2 2>&1; then :; else
    echo "[krayt-dev] diag: cannot list $SECRETS_DIR — a non-root container can't read a root-only secrets mount (needs the krayt guest secrets-perms fix + base image rebuild)" >&2
  fi
  exit 78 # EX_CONFIG
fi
echo "[krayt-dev] authenticated via $cred"

if [ ! -f "$TASK_FILE" ]; then
  echo "[krayt-dev] task file $TASK_FILE not found" >&2
  exit 66 # EX_NOINPUT
fi

cd "$WORKSPACE"

# The workspace's .git is owned by root (the guest ingests it as root, then makes the tree
# writable), so the non-root agent's own git commands would refuse it with "dubious ownership".
# Mark it safe for this user.
git config --global --add safe.directory "$WORKSPACE" 2>/dev/null || true
git config --global --add safe.directory '*' 2>/dev/null || true

# When questions are enabled the adapter sets KRAYT_ASK_SOCKET (§6.13); register the ask_human
# MCP server so Claude can ask the human. `krayt-ask --mcp` bridges to that socket. In fail mode
# the var is unset and no server is registered — the run stays autonomous.
extra=()
if [ -n "${KRAYT_ASK_SOCKET:-}" ] && command -v krayt-ask >/dev/null 2>&1; then
  cat > /tmp/krayt-mcp.json <<EOF
{
  "mcpServers": {
    "ask-human": {
      "command": "krayt-ask",
      "args": ["--mcp"],
      "env": { "KRAYT_ASK_SOCKET": "${KRAYT_ASK_SOCKET}" }
    }
  }
}
EOF
  extra+=(--mcp-config /tmp/krayt-mcp.json)
  echo "[krayt-dev] registered ask_human MCP server (questions enabled)"
fi

# CLAUDE_MODEL/CLAUDE_EFFORT (set via krayt.yaml's `env:`, §8.1) pick the model + reasoning
# effort for this run; default to claude-sonnet-5 on high effort when unset.
model="${CLAUDE_MODEL:-claude-sonnet-5}"
effort="${CLAUDE_EFFORT:-high}"
extra+=(--model "$model" --effort "$effort")

echo "[krayt-dev] running claude -p in $(pwd) (model: $model, effort: $effort)"
# Print/headless mode with autonomous edits — safe because the whole run is already isolated in
# the krayt micro-VM, so the tool-permission prompts add nothing. The task prompt is expected to
# tell the agent to build/test/lint/regenerate proto as needed (see README + task.example.md).
# Tee its final summary into /output/report.md so it surfaces in the krayt report's Notes;
# pipefail keeps the pipeline's exit code Claude's, not tee's.
claude -p "$(cat "$TASK_FILE")" --dangerously-skip-permissions "${extra[@]}" | tee /output/report.md
echo "[krayt-dev] done"
