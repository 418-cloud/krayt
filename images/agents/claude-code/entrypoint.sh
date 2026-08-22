#!/usr/bin/env bash
# krayt-agent-claude-code entrypoint (§6.14, §8.2). Baked in as
# /usr/local/bin/krayt-agent-entrypoint, and INHERITED by every image built FROM this one —
# hack/krayt-dev is the one in this repo. There is deliberately no second copy: the two scripts
# used to be near-identical, and the drift between them is what let a shape-translated run exit 78
# before Claude started (see hack/test-entrypoint-credentials.sh's own header).
#
# A downstream image that needs setup of its own WRAPS this script rather than forking it: do the
# extra work, then `exec krayt-agent-entrypoint "$@"`. Often none is needed — hack/krayt-dev adds
# the entire krayt toolchain, `gh` included, and still ships no entrypoint, because its GitHub
# token is injected at the host proxy and `gh` reads it straight from the environment. Only
# behavior belonging to the tools THIS image actually ships lives here. It:
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
# Overridable for the same reason SECRETS_DIR/WORKSPACE/TASK_FILE are: it lets
# hack/test-entrypoint-credentials.sh exercise this script outside a container.
OUTPUT_DIR="${KRAYT_OUTPUT:-/output}"

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
# network.mitm shape translation (§6.6.1, §8.2): the credential is deliberately withheld from
# $SECRETS_DIR and attached to outgoing requests by the HOST proxy instead, so no file will ever
# arrive for it. krayt configures the container with the same credential env var carrying a
# placeholder value, which only has to satisfy Claude Code's own "a credential is configured"
# check — the real value never enters this container. Accepting an already-set var is what lets
# krayt choose that placeholder (a self-describing sk-ant-…-do-not-use string, legible in a log);
# without this branch a run using shape translation would find no file, conclude it has no
# credential, and exit 78 before starting.
if [ -z "$cred" ]; then
  for key in ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_AUTH_TOKEN; do
    if [ -n "${!key:-}" ]; then
      cred="$key"
      break
    fi
  done
fi
# Backward compatibility with the pre-shape-translation contract (§8.2): KRAYT_INJECTED_CREDENTIAL
# names the withheld credential (never its value) for a krayt that sets the name but no placeholder
# value. Ordered after the branch above so krayt's own placeholder wins when both are present.
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
  # git and curl read none of the three above on Debian: libcurl is built with its own CA path
  # compiled in, and git consults GIT_SSL_CAINFO/http.sslCAInfo. Point both at the same bundle, or
  # a `git clone` / `curl` to an intercepted host fails where node and Go succeed.
  export GIT_SSL_CAINFO="$bundle"
  export CURL_CA_BUNDLE="$bundle"
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

# Optional model + reasoning-effort selection. Unset, no flag is passed and Claude Code picks its
# own default (this image's behavior); an image or a run that wants to choose sets CLAUDE_MODEL /
# CLAUDE_EFFORT — krayt-dev sets both as ENV defaults, and krayt.yaml's `env:` block (§8.1)
# overrides them per run. Claude also reads ANTHROPIC_MODEL from the environment on its own.
if [ -n "${CLAUDE_MODEL:-}" ]; then
  extra+=(--model "$CLAUDE_MODEL")
fi
if [ -n "${CLAUDE_EFFORT:-}" ]; then
  extra+=(--effort "$CLAUDE_EFFORT")
fi

echo "[claude-code] running claude -p in $(pwd) (model: ${CLAUDE_MODEL:-${ANTHROPIC_MODEL:-default}}${CLAUDE_EFFORT:+, effort: $CLAUDE_EFFORT})"
# Print/headless mode with autonomous edits — safe because the whole run is already isolated in
# the krayt micro-VM, so the tool-permission prompts add nothing. Claude reads ANTHROPIC_MODEL
# from the environment if set. Tee its final summary into /output/report.md so it surfaces in the
# krayt report's Notes; pipefail keeps the pipeline's exit code Claude's, not tee's.
# ${extra[@]+"${extra[@]}"} rather than a bare "${extra[@]}": under `set -u`, bash 3.2 (what macOS
# ships) treats an EMPTY array's expansion as unbound and aborts. The image's own bash is 5.x and
# does not care, but hack/test-entrypoint-credentials.sh runs this script with the host's bash to
# exercise the credential logic offline, and that path must not depend on the host's bash version.
claude -p "$(cat "$TASK_FILE")" --dangerously-skip-permissions ${extra[@]+"${extra[@]}"} | tee "$OUTPUT_DIR/report.md"
echo "[claude-code] done"
