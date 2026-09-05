#!/bin/sh
# P5 — does Claude Code accept msb's default placeholder?  (non-blocking; needs a live credential)
#
# docs/ai-tasks/probe-microsandbox-feasibility.md. msb's default guest placeholder is
# $MSB_<ENV_VAR> (docs/sandboxes/secrets.mdx); krayt is taking that default rather than supplying
# a shaped one (hand-secrets-to-msb.md). The one thing not settled by source-reading is whether
# Claude Code itself rejects $MSB_ANTHROPIC_API_KEY client-side (length or sk-ant- prefix check)
# before any request leaves the container.
#
# Needs a live ANTHROPIC_API_KEY in THIS shell's environment — that's what makes it non-blocking
# and HUMAN-only; nothing here can fabricate a credential.
#
# --net-default-egress deny switches msb onto the "no implicit DNS rule" branch (ADR correction
# #2), so an explicit allow@dns rule is required alongside allow@api.anthropic.com or the guest
# cannot resolve anything — omitting it would fail this probe for an unrelated reason.
#
# PASS: claude -p replied containing "ok" — msb's default placeholder is accepted as-is.
# FAIL: names the fix that's already designed for this contingency (hand-secrets-to-msb.md): msb
# exposes a per-secret `placeholder` field, but only through --secret-conf, not argv, and
# --secret has no conflicts_with against --net-conf so the two can be combined.
set -eu

PROBE=p5-placeholder-accepted
IMAGE=${KRAYT_MSB_PROBE_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code}
SANDBOX=krayt-probe-p5

cleanup() {
  msb rm --force "$SANDBOX" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $PROBE — $1"
  exit 1
}

command -v msb >/dev/null 2>&1 \
  || fail "msb not on PATH — install with: curl -fsSL https://install.microsandbox.dev | sh"

if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  fail "ANTHROPIC_API_KEY is not set in this shell — export a live Anthropic key before running this probe"
fi

echo "msb version: $(msb --version)"

echo "[$PROBE] creating sandbox $SANDBOX (--tls-intercept, deny-default egress, allow api.anthropic.com + dns)…"
if ! msb create --tls-intercept \
    --net-default-egress deny \
    --net-rule "allow@api.anthropic.com" \
    --net-rule "allow@dns" \
    --secret 'ANTHROPIC_API_KEY@api.anthropic.com' \
    --user agent "$IMAGE" --name "$SANDBOX" >&2; then
  fail "msb create failed"
fi

echo "[$PROBE] running claude -p 'reply with the single word ok'…"
if ! reply=$(msb exec --no-tty --user agent "$SANDBOX" -- claude -p 'reply with the single word ok' 2>&1); then
  echo "$reply" >&2
  fail "claude -p exited non-zero — see the captured output above; this may mean Claude Code rejected \$MSB_ANTHROPIC_API_KEY client-side, or an unrelated failure (network policy, auth flow). The known fix if it's the placeholder: msb's per-secret 'placeholder' field is --secret-conf-only (no --secret-placeholder flag), and --secret has no conflicts_with against --net-conf, so hand-secrets-to-msb.md's contingency (a shaped placeholder via --secret-conf combined with --net-rule) is what to build"
fi

echo "[$PROBE] reply: $reply"
case "$reply" in
*[Oo][Kk]*)
  echo "PASS: $PROBE — claude -p replied containing 'ok'; msb's default \$MSB_ANTHROPIC_API_KEY placeholder is accepted as-is"
  exit 0
  ;;
*)
  fail "claude -p exited 0 but the reply did not contain 'ok' (got: $reply) — inconclusive on the placeholder question; inspect the reply above. If it looks like a credential-shape rejection, hand-secrets-to-msb.md's --secret-conf contingency is the fix"
  ;;
esac
