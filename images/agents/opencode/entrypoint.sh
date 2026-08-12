#!/usr/bin/env bash
# krayt-agent-opencode entrypoint (§6.14, §8.2). It:
#   1. materializes the model credential from the per-task secrets tmpfs (/run/secrets) into the
#      environment — the in-container half of agent auth; the host adapter
#      (internal/adapter/opencode.go) already enforced the exactly-one rule before boot (§6.14);
#   2. runs opencode non-interactively against the task, editing /workspace (which krayt diffs
#      into changes.patch);
#   3. writes opencode's final response to /output/report.md, which krayt folds into the run
#      report's Notes section (§8.4).
set -euo pipefail

SECRETS_DIR="${KRAYT_SECRETS_DIR:-/run/secrets}"
WORKSPACE="${KRAYT_WORKSPACE:-/workspace}"
TASK_FILE="${KRAYT_TASK:-/task/prompt.md}"

# Export exactly one recognized credential from the secrets tmpfs (§6.14). The host adapter
# already guaranteed exactly one is present; this just turns the file into an env var opencode's
# provider auth (packages/llm/src/providers/{anthropic,openai,openrouter}.ts upstream) reads.
cred=""
for key in ANTHROPIC_API_KEY OPENAI_API_KEY OPENROUTER_API_KEY; do
  if [ -f "$SECRETS_DIR/$key" ]; then
    export "$key=$(cat "$SECRETS_DIR/$key")"
    cred="$key"
    break
  fi
done
if [ -z "$cred" ]; then
  echo "[opencode] no credential in $SECRETS_DIR (expected ANTHROPIC_API_KEY, OPENAI_API_KEY, or OPENROUTER_API_KEY)" >&2
  # Diagnostics: the usual cause is a permissions mismatch — krayt wrote the secrets tmpfs
  # root-only while this container runs non-root, so it can't read them. Print who we are and
  # what (if anything) we can see.
  echo "[opencode] diag: running as $(id)" >&2
  if ls -la "$SECRETS_DIR" >&2 2>&1; then :; else
    echo "[opencode] diag: cannot list $SECRETS_DIR — a non-root container can't read a root-only secrets mount" >&2
  fi
  exit 78 # EX_CONFIG
fi
echo "[opencode] authenticated via $cred"

if [ ! -f "$TASK_FILE" ]; then
  echo "[opencode] task file $TASK_FILE not found" >&2
  exit 66 # EX_NOINPUT
fi

cd "$WORKSPACE"

# The workspace's .git is owned by root (the guest ingests it as root, then makes the tree
# writable), so the non-root agent's own git commands would refuse it with "dubious ownership".
# Mark it safe for this user.
git config --global --add safe.directory "$WORKSPACE" 2>/dev/null || true
git config --global --add safe.directory '*' 2>/dev/null || true

# opencode is multi-provider, so the model can't be hardcoded. Honor an optional OPENCODE_MODEL
# (passed through `krayt run --env` / a krayt.yaml env block); otherwise pick a sensible default
# per the credential that was actually exported. The openrouter default is a best guess (it's a
# router in front of many models, unlike Anthropic/OpenAI's single first-party catalog) — set
# OPENCODE_MODEL explicitly for anything other than quick experimentation.
if [ -n "${OPENCODE_MODEL:-}" ]; then
  model="$OPENCODE_MODEL"
else
  case "$cred" in
    ANTHROPIC_API_KEY) model="anthropic/claude-sonnet-4-5" ;;
    OPENAI_API_KEY) model="openai/gpt-5" ;;
    OPENROUTER_API_KEY) model="openrouter/anthropic/claude-sonnet-4.5" ;;
  esac
fi

# When questions are enabled the adapter sets KRAYT_ASK_SOCKET (§6.13); register the ask_human
# MCP server so opencode can ask the human. opencode reads MCP servers from its config's top-level
# `mcp` block (opencode.json — verified against packages/web/src/content/docs/mcp-servers.mdx
# upstream), and config files are merged (not replaced) across sources, so a config pointed at by
# OPENCODE_CONFIG only adds this one key rather than clobbering any project/global config.
# `krayt-ask --mcp` bridges to that socket. krayt-ask itself is bind-mounted by the guest onto
# /usr/local/bin/krayt-ask — never baked into the image. In fail mode the var is unset and no
# config is written — the run stays autonomous.
if [ -n "${KRAYT_ASK_SOCKET:-}" ] && command -v krayt-ask >/dev/null 2>&1; then
  cat > /tmp/krayt-opencode.json <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "mcp": {
    "ask-human": {
      "type": "local",
      "command": ["krayt-ask", "--mcp"],
      "enabled": true,
      "environment": { "KRAYT_ASK_SOCKET": "${KRAYT_ASK_SOCKET}" }
    }
  }
}
EOF
  export OPENCODE_CONFIG=/tmp/krayt-opencode.json
  echo "[opencode] registered ask_human MCP server (questions enabled)"
fi

echo "[opencode] running opencode run in $(pwd) (model: ${model})"
# `run` is opencode's non-interactive/headless mode; opencode allows all operations without
# approval by default (docs/config.mdx "Permissions" — there is no project-local permission
# config here to restrict it), and --auto additionally auto-approves anything not explicitly
# denied, so this stays autonomous the same way the other agent images do. Safe here because the
# whole run is already isolated in the krayt micro-VM. Tee opencode's final response into
# /output/report.md so it surfaces in the krayt report's Notes; pipefail keeps the pipeline's exit
# code opencode's, not tee's.
opencode run --model "$model" --auto "$(cat "$TASK_FILE")" | tee /output/report.md
echo "[opencode] done"
