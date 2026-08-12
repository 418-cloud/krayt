#!/usr/bin/env bash
# krayt-agent-gemini-cli entrypoint (§6.14, §8.2). It:
#   1. materializes the model credential from the per-task secrets tmpfs (/run/secrets) into the
#      environment — the in-container half of agent auth; the host adapter
#      (internal/adapter/geminicli.go) already enforced the exactly-one rule before boot (§6.14);
#   2. runs Gemini CLI non-interactively against the task, editing /workspace (which krayt diffs
#      into changes.patch);
#   3. writes Gemini's final response to /output/report.md, which krayt folds into the run
#      report's Notes section (§8.4).
set -euo pipefail

SECRETS_DIR="${KRAYT_SECRETS_DIR:-/run/secrets}"
WORKSPACE="${KRAYT_WORKSPACE:-/workspace}"
TASK_FILE="${KRAYT_TASK:-/task/prompt.md}"
SETTINGS_FILE="$HOME/.gemini/settings.json"

# Export exactly one recognized credential from the secrets tmpfs (§6.14). The host adapter
# already guaranteed exactly one is present; this just turns the file into an env var Gemini CLI
# reads.
cred=""
for key in GEMINI_API_KEY GOOGLE_API_KEY; do
  if [ -f "$SECRETS_DIR/$key" ]; then
    export "$key=$(cat "$SECRETS_DIR/$key")"
    cred="$key"
    break
  fi
done
if [ -z "$cred" ]; then
  echo "[gemini-cli] no credential in $SECRETS_DIR (expected GEMINI_API_KEY or GOOGLE_API_KEY)" >&2
  # Diagnostics: the usual cause is a permissions mismatch — krayt wrote the secrets tmpfs
  # root-only while this container runs non-root, so it can't read them. Print who we are and
  # what (if anything) we can see.
  echo "[gemini-cli] diag: running as $(id)" >&2
  if ls -la "$SECRETS_DIR" >&2 2>&1; then :; else
    echo "[gemini-cli] diag: cannot list $SECRETS_DIR — a non-root container can't read a root-only secrets mount" >&2
  fi
  exit 78 # EX_CONFIG
fi
# GOOGLE_API_KEY alone isn't auto-detected: Gemini CLI's env-based auth resolution
# (packages/core/src/core/contentGenerator.ts:getAuthTypeFromEnv) only checks
# GOOGLE_GENAI_USE_VERTEXAI / GEMINI_API_KEY, not GOOGLE_API_KEY. GOOGLE_API_KEY authenticates
# via Vertex AI Express mode — a different product/endpoint (aiplatform.googleapis.com) than the
# Gemini Developer API (generativelanguage.googleapis.com) that GEMINI_API_KEY hits — so it must
# be paired with GOOGLE_GENAI_USE_VERTEXAI=true or the CLI reports no auth method configured.
if [ "$cred" = "GOOGLE_API_KEY" ]; then
  export GOOGLE_GENAI_USE_VERTEXAI=true
fi
echo "[gemini-cli] authenticated via $cred"

if [ ! -f "$TASK_FILE" ]; then
  echo "[gemini-cli] task file $TASK_FILE not found" >&2
  exit 66 # EX_NOINPUT
fi

cd "$WORKSPACE"

# The workspace's .git is owned by root (the guest ingests it as root, then makes the tree
# writable), so the non-root agent's own git commands would refuse it with "dubious ownership".
# Mark it safe for this user.
git config --global --add safe.directory "$WORKSPACE" 2>/dev/null || true
git config --global --add safe.directory '*' 2>/dev/null || true

# When questions are enabled the adapter sets KRAYT_ASK_SOCKET (§6.13); register the ask_human
# MCP server so Gemini can ask the human. Gemini CLI configures MCP servers via the top-level
# `mcpServers` key in settings.json (packages/cli/src/config/settingsSchema.ts,
# schemas/settings.schema.json) — there is no per-invocation --mcp-config flag like Claude Code's,
# so rewrite the settings file baked at build time, repeating its two static keys (auto-update and
# usage-stats off) so they stay in effect. `krayt-ask --mcp` bridges to the question-channel
# socket. krayt-ask itself is bind-mounted by the guest onto /usr/local/bin/krayt-ask — never
# baked into the image. In fail mode the var is unset and settings.json keeps its build-time
# contents (no mcpServers) — the run stays autonomous.
if [ -n "${KRAYT_ASK_SOCKET:-}" ] && command -v krayt-ask >/dev/null 2>&1; then
  mkdir -p "$(dirname "$SETTINGS_FILE")"
  cat > "$SETTINGS_FILE" <<EOF
{
  "general": { "enableAutoUpdate": false },
  "privacy": { "usageStatisticsEnabled": false },
  "mcpServers": {
    "ask-human": {
      "command": "krayt-ask",
      "args": ["--mcp"],
      "env": { "KRAYT_ASK_SOCKET": "${KRAYT_ASK_SOCKET}" }
    }
  }
}
EOF
  echo "[gemini-cli] registered ask_human MCP server (questions enabled)"
fi

echo "[gemini-cli] running gemini -p in $(pwd) (model: ${GEMINI_MODEL:-default})"
# Print/headless mode (-p forces it) with auto-approved edits — safe because the whole run is
# already isolated in the krayt micro-VM, so the tool-confirmation prompts add nothing.
# --approval-mode yolo is the current unified flag (the older --yolo/-y is deprecated). Gemini
# reads GEMINI_MODEL from the environment if set (falls back to settings.model.name, then "auto").
# Tee the final response into /output/report.md so it surfaces in the krayt report's Notes;
# pipefail keeps the pipeline's exit code Gemini's, not tee's.
gemini --prompt "$(cat "$TASK_FILE")" --approval-mode yolo | tee /output/report.md
echo "[gemini-cli] done"
