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
# network.mitm + network.inject (§6.6.1, add-tls-mitm-credential-injection.md §2): this
# credential is deliberately withheld from $SECRETS_DIR and attached to outgoing requests by the
# host proxy instead. KRAYT_INJECTED_CREDENTIAL names it (never its value) so this loop can start
# without a file that will never arrive; the placeholder below only satisfies Gemini CLI's "a
# credential is configured" check — the real value never enters this container.
if [ -z "$cred" ] && [ -n "${KRAYT_INJECTED_CREDENTIAL:-}" ]; then
  cred="$KRAYT_INJECTED_CREDENTIAL"
  export "$cred=krayt-injected-at-host-proxy"
fi
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

# Trust the run's ephemeral MITM CA, when network.mitm is enabled (§8.2,
# add-tls-mitm-credential-injection.md §5). KRAYT_CA_CERT is set by the guest only in that case;
# unset here means byte-identical behavior to before this feature. SSL_CERT_FILE/
# REQUESTS_CA_BUNDLE REPLACE the system trust store for Go/OpenSSL-based tools rather than
# appending to it, which would silently break verification for any `passthrough` host — so
# concatenate the distro bundle with the krayt CA into one file and point both vars at THAT.
# NODE_EXTRA_CA_CERTS is genuinely additive (and required, not optional: Node does not read the
# system trust store at all), so it can point at the krayt CA alone.
if [ -n "${KRAYT_CA_CERT:-}" ] && [ -f "${KRAYT_CA_CERT}" ]; then
  bundle=/tmp/krayt-ca-bundle.pem
  cat /etc/ssl/certs/ca-certificates.crt "$KRAYT_CA_CERT" > "$bundle"
  export SSL_CERT_FILE="$bundle"
  export REQUESTS_CA_BUNDLE="$bundle"
  export NODE_EXTRA_CA_CERTS="$KRAYT_CA_CERT"
  echo "[gemini-cli] trusting krayt's ephemeral MITM CA (network.mitm enabled)"
fi

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

# Gemini CLI gates tool use on whether it considers the working folder "trusted". In a headless
# run an untrusted folder silently downgrades --approval-mode yolo back to "default" and then
# aborts outright, so without this the image cannot complete ANY task (observed as exit 55,
# "not running in a trusted directory"). Trusting it here is correct rather than merely
# convenient: that prompt exists to protect a developer's own machine from a repo they just
# cloned, whereas krayt's whole model already assumes the repo is untrusted and puts the
# isolation boundary at the VM (§10). Set as an env var rather than the equivalent --skip-trust
# flag so an upstream flag rename can't turn this into an argument-parsing failure.
export GEMINI_CLI_TRUST_WORKSPACE=true

echo "[gemini-cli] running gemini -p in $(pwd) (model: ${GEMINI_MODEL:-default})"
# Print/headless mode (-p forces it) with auto-approved edits — safe because the whole run is
# already isolated in the krayt micro-VM, so the tool-confirmation prompts add nothing.
# --approval-mode yolo is the current unified flag (the older --yolo/-y is deprecated). Gemini
# reads GEMINI_MODEL from the environment if set (falls back to settings.model.name, then "auto").
# Tee the final response into /output/report.md so it surfaces in the krayt report's Notes;
# pipefail keeps the pipeline's exit code Gemini's, not tee's.
gemini --prompt "$(cat "$TASK_FILE")" --approval-mode yolo | tee /output/report.md
echo "[gemini-cli] done"
