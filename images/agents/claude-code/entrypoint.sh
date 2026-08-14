#!/usr/bin/env bash
# krayt-agent-claude-code entrypoint (§6.14, §8.2). It:
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
# network.mitm + network.inject (§6.6.1, add-tls-mitm-credential-injection.md §2): this
# credential is deliberately withheld from $SECRETS_DIR and attached to outgoing requests by the
# host proxy instead. KRAYT_INJECTED_CREDENTIAL names it (never its value) so this loop can start
# without a file that will never arrive; the placeholder below only satisfies Claude Code's "a
# credential is configured" check — the real value never enters this container.
if [ -z "$cred" ] && [ -n "${KRAYT_INJECTED_CREDENTIAL:-}" ]; then
  cred="$KRAYT_INJECTED_CREDENTIAL"
  export "$cred=krayt-injected-at-host-proxy"
fi
if [ -z "$cred" ]; then
  echo "[claude-code] no credential in $SECRETS_DIR (expected ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, or ANTHROPIC_AUTH_TOKEN)" >&2
  # Diagnostics: the usual cause is a permissions mismatch — krayt wrote the secrets tmpfs
  # root-only while this container runs non-root, so it can't read them. Print who we are and
  # what (if anything) we can see.
  echo "[claude-code] diag: running as $(id)" >&2
  if ls -la "$SECRETS_DIR" >&2 2>&1; then :; else
    echo "[claude-code] diag: cannot list $SECRETS_DIR — a non-root container can't read a root-only secrets mount" >&2
  fi
  exit 78 # EX_CONFIG
fi
echo "[claude-code] authenticated via $cred"

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
  echo "[claude-code] trusting krayt's ephemeral MITM CA (network.mitm enabled)"
fi

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

# When questions are enabled the adapter sets KRAYT_ASK_SOCKET (§6.13); register the ask_human
# MCP server so Claude can ask the human. `krayt-ask --mcp` bridges to that socket. krayt-ask
# itself is bind-mounted by the guest onto /usr/local/bin/krayt-ask — never baked into the image.
# In fail mode the var is unset and no server is registered — the run stays autonomous.
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
  echo "[claude-code] registered ask_human MCP server (questions enabled)"
fi

echo "[claude-code] running claude -p in $(pwd) (model: ${ANTHROPIC_MODEL:-default})"
# Print/headless mode with autonomous edits — safe because the whole run is already isolated in
# the krayt micro-VM, so the tool-permission prompts add nothing. Claude reads ANTHROPIC_MODEL
# from the environment if set. Tee its final summary into /output/report.md so it surfaces in the
# krayt report's Notes; pipefail keeps the pipeline's exit code Claude's, not tee's.
claude -p "$(cat "$TASK_FILE")" --dangerously-skip-permissions "${extra[@]}" | tee /output/report.md
echo "[claude-code] done"
