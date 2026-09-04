#!/bin/sh
# P7 — what does `--on-secret-violation passthrough` actually do?  (blocking for the policy change;
# needs no credential — the canary is a value this script invents)
#
# THE QUESTION. krayt emits `--on-secret-violation block-and-log` on every sandbox. That value has
# no recorded rationale: it was set under "never inherit an msb default", never as a choice between
# block / block-and-log / block-and-terminate / passthrough. It has since killed two real runs
# (run_df87bfc8, run_4125ef2e) in a way no run can recover from — msb puts each secret's placeholder
# in the guest's own environment, so the moment an agent runs `env` the placeholder enters its
# conversation, and every later turn resends it to the model API, which is outside that secret's
# scope. block-and-log blocks all of them.
#
# WHAT msb's SOURCE SAYS, read at v0.6.16 — the run is the check on this, not a substitute for it:
#
#   1. Substitution is decided BEFORE the violation action is consulted. `secret_host_allowed`
#      gates the eligible list and `continue`s; `effective_violation_action` is only reached on the
#      else branch (crates/network/lib/secrets/handler.rs:579-600). So NO value of this flag can
#      cause a secret to be substituted at a host outside its own scope.
#   2. The CLI's `passthrough` is `Passthrough([HostPattern::Any])` — every host
#      (crates/cli/lib/commands/common.rs:2654).
#   3. A matching passthrough `continue`s past both the substitution list and the blocking list:
#      forwarded unchanged, not blocked (handler.rs:605-612). The type's own doc says "Forward the
#      request with the placeholder unchanged for matching hosts"
#      (packages/microsandbox-types/rust/lib/domain.rs:2198).
#   4. It is SILENT: tracing::warn! lives inside the BlockingAction match, which passthrough never
#      reaches (handler.rs:1508-1525). Adopting it means giving up the violation log.
#
# So the expected reading is: out-of-scope placeholder + passthrough => the PLACEHOLDER arrives at
# the endpoint and the request succeeds. The finding that would stop the change dead is the real
# value arriving instead.
#
# WHY THREE SANDBOXES. Any one measurement alone is unreadable:
#   A (passthrough, out of scope)   — the question itself.
#   B (block-and-log, out of scope) — the control. Without it, "A succeeded" is equally consistent
#     with "passthrough forwards" and "this probe never provoked a violation at all". B must FAIL.
#   C (passthrough, IN scope)       — proves passthrough does not break the normal path. Without
#     it, adopting passthrough could silently stop every credential from being substituted, which
#     would break every run in a way this probe would have called a pass.
#
# Usage: p7-passthrough-semantics.sh [endpoint-url]
#   Defaults to https://postman-echo.com/get, the same endpoint P3 uses. Substitute one you trust.
#
# PASS: A forwards the placeholder, B is blocked, C substitutes the real value — passthrough is
# safe to adopt, and krayt's NetworkArgs should switch to it.
# FAIL: each failure names what it means for the change, and the one that matters most is A
# carrying the REAL value: that would mean passthrough substitutes out of scope, and krayt must
# keep block-and-log.
set -eu

PROBE=p7-passthrough-semantics
ENDPOINT=${1:-https://postman-echo.com/get}
IMAGE=${KRAYT_MSB_PROBE_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code}
SBX_PASS=krayt-probe-p7-passthrough
SBX_BLOCK=krayt-probe-p7-block
SBX_INSCOPE=krayt-probe-p7-inscope

# A host the canary is scoped to that is deliberately NOT the endpoint, so every request this probe
# sends to the endpoint carries an out-of-scope placeholder. Never contacted.
OTHER_HOST=api.github.com

HOST=$(printf '%s' "$ENDPOINT" | sed -E 's#^[a-zA-Z][a-zA-Z0-9+.-]*://##; s#/.*##; s#:.*##')
[ -n "$HOST" ] || { echo "FAIL: $PROBE — could not parse a host out of '$ENDPOINT'"; exit 1; }

REAL_VALUE="krayt-p7-real-$$"
PLACEHOLDER='$MSB_KRAYT_P7_CANARY' # msb's default placeholder shape is $MSB_<ENV_VAR>
export KRAYT_P7_CANARY="$REAL_VALUE" # msb reads the value from this process's env at start time

cleanup() {
  for s in "$SBX_PASS" "$SBX_BLOCK" "$SBX_INSCOPE"; do
    msb rm --force "$s" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT INT TERM

fail() { echo "FAIL: $PROBE — $1"; exit 1; }

command -v msb >/dev/null 2>&1 \
  || fail "msb not on PATH — install with: curl -fsSL https://install.microsandbox.dev | sh"

echo "msb version: $(msb --version)"
echo "[$PROBE] endpoint=$ENDPOINT host=$HOST  canary scoped to=$OTHER_HOST (never contacted)"

# --no-tty for P3's reason: msb allocates a PTY when the caller's stdin is a terminal, and a PTY
# re-introduces echo and CRLF, which corrupts every exact compare below.
msb_exec() { sbx=$1; shift; msb exec --no-tty --user agent "$sbx" -- "$@"; }

# Mirrors krayt's own rendered policy rather than msb's defaults: an explicit deny default puts msb
# on the branch that adds no implicit DNS rule, so allow@dns is required or nothing resolves — and
# it must precede any deny rule, since msb is first-match-wins and `dns` is the gateway, which sits
# inside `private` (KRAYT_SPEC.md §6.6).
create_sandbox() {
  name=$1 violation=$2 scope=$3
  msb create "$IMAGE" --name "$name" --user agent \
    --net-default deny \
    --net-rule "allow@dns" \
    --net-rule "allow@$HOST" \
    --secret "KRAYT_P7_CANARY@$scope" \
    --on-secret-violation "$violation" >&2
}

# Send the placeholder in a header and report what came back, or "BLOCKED" when the request never
# completed. curl's exit code distinguishes the two: a blocked request dies at the transport (msb
# drops the connection), which is exit 52/56/35, not an HTTP status.
send_and_classify() {
  sbx=$1
  if ! out=$(msb_exec "$sbx" sh -c \
    'curl -fsS --max-time 30 -H "Authorization: Bearer $KRAYT_P7_CANARY" "$1" 2>/dev/null' \
    _ "$ENDPOINT"); then
    printf 'BLOCKED'
    return 0
  fi
  auth=$(printf '%s' "$out" | tr -d '\r' | grep -io '"authorization"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n1)
  case "$auth" in
  '') printf 'NO_HEADER_ECHOED' ;;
  *"$REAL_VALUE"*) printf 'REAL_VALUE' ;;
  *MSB_KRAYT_P7_CANARY*) printf 'PLACEHOLDER' ;;
  *) printf 'OTHER:%s' "$auth" ;;
  esac
}

# --- A: passthrough, placeholder OUT of scope — the question ---

echo "[$PROBE] A: creating $SBX_PASS (--on-secret-violation passthrough, canary scoped to $OTHER_HOST)…"
create_sandbox "$SBX_PASS" passthrough "$OTHER_HOST" || fail "msb create (passthrough) failed"
a=$(send_and_classify "$SBX_PASS")
echo "[$PROBE] A passthrough, out of scope -> $a"

# --- B: block-and-log, same request — the control ---

echo "[$PROBE] B: creating $SBX_BLOCK (--on-secret-violation block-and-log, same scope)…"
create_sandbox "$SBX_BLOCK" block-and-log "$OTHER_HOST" || fail "msb create (block-and-log) failed"
b=$(send_and_classify "$SBX_BLOCK")
echo "[$PROBE] B block-and-log, out of scope -> $b"

# --- C: passthrough, placeholder IN scope — the regression guard ---

echo "[$PROBE] C: creating $SBX_INSCOPE (--on-secret-violation passthrough, canary scoped to $HOST)…"
create_sandbox "$SBX_INSCOPE" passthrough "$HOST" || fail "msb create (in-scope) failed"
c=$(send_and_classify "$SBX_INSCOPE")
echo "[$PROBE] C passthrough, IN scope -> $c"

# --- verdict ---

# Checked first and on its own: this is the finding that stops the change, and it must not be
# reported as one bullet among several.
if [ "$a" = REAL_VALUE ]; then
  fail "passthrough SUBSTITUTED the real secret value at a host OUTSIDE the secret's scope. This contradicts msb's source as read at v0.6.16 (handler.rs:579-600 gates substitution on secret_host_allowed before the violation action is consulted), so msb has changed: krayt MUST keep --on-secret-violation block-and-log, and hand-secrets-to-msb.md's whole scoping premise needs re-verifying — a secret scoped to one host is reaching another"
fi

if [ "$b" != BLOCKED ]; then
  fail "the control did not block: with block-and-log an out-of-scope placeholder came back as '$b', not BLOCKED. This probe therefore never provoked a violation at all, and A's result means nothing — neither adopt nor reject passthrough on it. Check that TLS interception is on (any --secret enables it, P3) and that the placeholder really left the guest"
fi

if [ "$c" != REAL_VALUE ]; then
  fail "passthrough broke the NORMAL path: with the canary scoped to $HOST the value should have been substituted, but the endpoint saw '$c'. Adopting passthrough would stop every credential reaching its own allowed host — every run would fail authentication. Keep block-and-log"
fi

if [ "$a" = PLACEHOLDER ]; then
  echo "PASS: $PROBE — passthrough forwards the placeholder unchanged to an out-of-scope host ($a) and does NOT block, while block-and-log blocks the identical request ($b) and in-scope substitution still works ($c). msb behaves as its source reads: substitution is gated on the secret's own scope before the violation action is consulted, so passthrough cannot leak a value. krayt should switch internal/task/netpolicy_msb.go's NetworkArgs to --on-secret-violation passthrough. Note what is given up: passthrough is SILENT (tracing::warn! is inside the BlockingAction match, handler.rs:1508-1525), so the 'secret violation' lines that diagnosed run_df87bfc8 will no longer appear — the precise alternative, if that signal is wanted back, is a per-secret on_violation: Passthrough([<model host>]) via --secret-conf, which keeps block-and-log everywhere else (domain.rs:2125-2127, handler.rs:2322-2339)"
  exit 0
fi

fail "unexpected combination — A(passthrough,out-of-scope)='$a' B(block-and-log,out-of-scope)='$b' C(passthrough,in-scope)='$c'. A was expected to be PLACEHOLDER. Inspect the three by hand before changing krayt's policy; 'NO_HEADER_ECHOED' usually means the endpoint does not echo request headers, in which case pass a different endpoint as \$1"
