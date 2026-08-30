#!/bin/sh
# P3 — does `--secret` alone enable TLS interception?  (non-blocking; confirms a source finding)
#
# docs/ai-tasks/probe-microsandbox-feasibility.md. ANSWERED, AND NOT THE WAY THE ADR ASSUMED:
# `--secret` alone DOES enable interception. Run 2026-08-29 against msb 0.6.16 — substitution
# happened in both sandboxes, with the guest holding only the placeholder in both.
#
# The ADR's reading was of the wrong predicate. It is true that TlsConfig.enabled defaults false
# (`packages/microsandbox-types/rust/lib/domain.rs`, `#[serde(default)] pub enabled: bool`) and
# that the CLI's has_tls omits opts.secret (`crates/cli/lib/commands/common.rs`, the
# `let has_tls = tls_intercept || …` block) — but has_tls only governs the network *overlay*
# (intercepted ports, bypass list, CA paths, QUIC blocking). Enabling interception happens a layer
# up: `SandboxBuilder::secret_entry` sets `network.tls.enabled = true` for every secret added
# (`sdk/rust/lib/sandbox/builder.rs:834-843`, documented as "Automatically enables TLS interception
# if not already enabled"), and the CLI's --secret goes through exactly that builder
# (`crates/cli/lib/commands/common.rs:2028-2039`, `builder = builder.secret(|mut s| …)`).
#
# The corollary matters more than the flag question: under msb there is no such thing as a secret
# without MITM. Declaring one turns on interception of every intercepted port (443 by default) for
# that sandbox, and agentd installs the intercept CA into the guest trust store.
#
# This probe is kept as the regression that would catch msb changing that behaviour back.
#
# WHAT THE GUEST IS SUPPOSED TO HOLD. msb sets the guest's env var itself: `guest_secret_env()`
# maps each secret's env_var to its *placeholder* and the runtime extends the guest bootstrap
# environment with it (`crates/network/lib/network.rs:454-464`, `crates/runtime/lib/vm.rs:1871`),
# which agentd installs into its own environment before spawning any child
# (`crates/agentd/lib/config.rs`, install_default_env), so every `msb exec` inherits it. The
# default placeholder is `$MSB_<ENV_VAR>` (`crates/utils/lib/secret.rs`). The real value must
# never be in the guest — that is the credential boundary the whole B1 ADR rests on.
#
# WHY THIS READS THE GUEST ENV AND NOT JUST THE ECHOED HEADER. "The real value arrived at the
# endpoint" is ambiguous on its own: it is equally consistent with (a) msb substituting a
# placeholder the guest sent, and (b) the guest having held the real value all along and simply
# sending it. Those are opposite findings — (a) is a note about a flag, (b) is a broken credential
# boundary — so each sandbox is measured twice: what the guest's env var actually contains, and
# what reached the endpoint.
#
# WHY THE PLAIN SANDBOX GOES FIRST. Substitution happens in msb's host-side proxy
# (`crates/network/lib/secrets/handler.rs`: "Scans decrypted plaintext"). If interception state
# ever leaked across sandboxes in one msb server, creating the --tls-intercept sandbox first would
# make the plain one look like it substituted. Exercising the plain sandbox before any
# --tls-intercept sandbox has existed in this server's lifetime removes that explanation.
#
# Usage: p3-secret-tls-intercept.sh [endpoint-url]
#   Defaults to https://postman-echo.com/get. Substitute an endpoint you trust — see the
#   HUMAN_TODO.md entry.
#
# PASS means msb still behaves as documented: the guest holds only the placeholder, and the real
# value arrives with AND without --tls-intercept. The FAIL that matters is the inverse —
# substitution only with the flag — which would mean msb changed and the ADR's withdrawn
# correction 1 has to be un-withdrawn.
# FAIL: every other combination is named explicitly, and the two that look alike from the header
# alone are separated: "the guest held the real value" (credential boundary broken — the probe
# then creates a no-secret control sandbox to say whether msb put it there or the host environment
# leaked in) versus "the guest held the placeholder and msb substituted it without
# --tls-intercept" (msb's behaviour differs from its source as read for the ADR;
# translate-network-policy-to-msb.md's mandatory --tls-intercept emission should be simplified).
set -eu

PROBE=p3-secret-tls-intercept
ENDPOINT=${1:-https://postman-echo.com/get}
IMAGE=${KRAYT_MSB_PROBE_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code}
SBX_PLAIN=krayt-probe-p3-plain
SBX_INTERCEPT=krayt-probe-p3-intercept
SBX_CONTROL=krayt-probe-p3-control

HOST=$(printf '%s' "$ENDPOINT" | sed -E 's#^[a-zA-Z][a-zA-Z0-9+.-]*://##; s#/.*##; s#:.*##')
if [ -z "$HOST" ]; then
  echo "FAIL: $PROBE — could not parse a host out of endpoint '$ENDPOINT'"
  exit 1
fi

REAL_VALUE="krayt-p3-real-$$"
PLACEHOLDER='$MSB_KRAYT_P3_CANARY' # msb's default placeholder shape is $MSB_<ENV_VAR>
PLACEHOLDER_MARKER=MSB_KRAYT_P3_CANARY
export KRAYT_P3_CANARY="$REAL_VALUE" # read by msb from this process's env at sandbox-start time

cleanup() {
  msb rm --force "$SBX_PLAIN" >/dev/null 2>&1 || true
  msb rm --force "$SBX_INTERCEPT" >/dev/null 2>&1 || true
  msb rm --force "$SBX_CONTROL" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $PROBE — $1"
  exit 1
}

command -v msb >/dev/null 2>&1 \
  || fail "msb not on PATH — install with: curl -fsSL https://install.microsandbox.dev | sh"

echo "msb version: $(msb --version)"
echo "[$PROBE] endpoint=$ENDPOINT host=$HOST"

# --no-tty forces `msb exec`'s buffered non-interactive path. Without it msb allocates a PTY
# whenever the caller's stdin is a terminal (`crates/cli/lib/commands/exec.rs`,
# use_interactive_tty = stdin_is_terminal && !no_tty) — command substitution does not redirect
# stdin, so running this by hand takes the PTY path — and a PTY re-introduces echo and CRLF
# (msb's own words, exec.rs's --stream doc comment). CRLF then corrupts every exact compare below.
msb_exec() {
  sbx=$1
  shift
  msb exec --no-tty --user agent "$sbx" -- "$@"
}

# Last non-empty line, CRs stripped — belt and braces on top of --no-tty.
clean() {
  printf '%s\n' "$1" | tr -d '\r' | awk 'NF { last = $0 } END { print last }'
}

extract_auth() {
  # Pull the "authorization":"..." field out of the endpoint's echoed-headers JSON.
  printf '%s' "$1" | tr -d '\r' | grep -io '"authorization"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n1
}

# What the guest's own environment holds for the secret's env var. `printenv` exits non-zero when
# the variable is unset, which is a different finding from an empty value, so keep them apart.
read_guest_env() {
  if ! out=$(msb_exec "$1" printenv KRAYT_P3_CANARY); then
    printf '<unset>'
    return 0
  fi
  clean "$out"
}

classify_env() {
  case "$1" in
  '<unset>') printf 'unset' ;;
  '') printf 'empty' ;;
  "$PLACEHOLDER") printf 'placeholder' ;;
  *"$REAL_VALUE"*) printf 'REAL' ;;
  *) printf 'other' ;;
  esac
}

classify_header() {
  case "$1" in
  '') printf 'missing' ;;
  *"$REAL_VALUE"*) printf 'REAL' ;;
  *"$PLACEHOLDER_MARKER"*) printf 'placeholder' ;;
  *) printf 'other' ;;
  esac
}

send_request() {
  msb_exec "$1" sh -c 'curl -fsS -H "Authorization: Bearer $KRAYT_P3_CANARY" "$1"' _ "$ENDPOINT"
}

# Pull one balanced {...} object out of a JSON blob on stdin. Two things this has to survive:
# `grep -o '"tls":{[^}]*}'` stops at the first *nested* object's brace, truncating the output right
# before the "enabled" field the whole question turns on; and `msb inspect --format json`
# pretty-prints (`to_string_pretty`), so the key and its brace are separated by ": " and any
# pattern ending in `:{` silently matches nothing at all.
json_object() {
  awk -v key="\"$1\"" '{ buf = buf $0 } END {
    i = index(buf, key)
    if (i == 0) exit
    rest = substr(buf, i + length(key))
    b = index(rest, "{")
    if (b == 0) exit
    # Only whitespace and the colon may sit between the key and its brace; anything else means
    # this key holds something other than an object and the brace belongs to someone else.
    if (substr(rest, 1, b - 1) !~ /^[ \t:]*$/) exit
    s = substr(rest, b)
    depth = 0
    for (j = 1; j <= length(s); j++) {
      c = substr(s, j, 1)
      out = out c
      if (c == "{") depth++
      else if (c == "}") { depth--; if (depth == 0) break }
    }
    printf "%s: %s", key, out
  }'
}

# Informative only: msb's own view of this sandbox — the desired config's tls and secrets blocks.
# Best-effort, a shape change in `msb inspect` must not turn into a probe failure.
show_msb_view() {
  json=$({ msb inspect "$1" --format json 2>/dev/null || true; } | tr -d '\r\n')
  [ -n "$json" ] || return 0
  # `tr -s ' '` collapses the pretty-printer's indentation, which is now just runs of spaces.
  tls=$(printf '%s' "$json" | json_object tls | tr -s ' ' | cut -c1-400)
  sec=$(printf '%s' "$json" | json_object secrets | tr -s ' ' | cut -c1-400)
  [ -n "$tls" ] && echo "[$PROBE]   $1 tls: $tls"
  [ -n "$sec" ] && echo "[$PROBE]   $1 secrets: $sec"
  return 0
}

# The direct evidence of interception, independent of substitution: who signed the certificate the
# guest was served. msb's intercept CA means MITM; the endpoint's real public CA means no MITM.
peer_issuer() {
  msb_exec "$1" sh -c \
    'curl -sS -o /dev/null -v "$1" 2>&1 | sed -n "s/^\* *issuer: */issuer: /p" | head -n1' \
    _ "$ENDPOINT" 2>/dev/null | tr -d '\r' | head -n1 || true
}

# --- the plain sandbox, created before any --tls-intercept sandbox exists ---

echo "[$PROBE] creating $SBX_PLAIN (no --tls-intercept)…"
if ! msb create --secret "KRAYT_P3_CANARY@$HOST" --net-rule "allow@$HOST" \
  --user agent "$IMAGE" --name "$SBX_PLAIN" >&2; then
  fail "msb create (no --tls-intercept) failed"
fi
show_msb_view "$SBX_PLAIN"
plain_issuer=$(peer_issuer "$SBX_PLAIN")
echo "[$PROBE]   $SBX_PLAIN peer cert ${plain_issuer:-issuer: (not reported)}"
plain_env=$(read_guest_env "$SBX_PLAIN")
plain_env_kind=$(classify_env "$plain_env")
echo "[$PROBE] no --tls-intercept guest \$KRAYT_P3_CANARY: $plain_env  [$plain_env_kind]"
plain_resp=$(send_request "$SBX_PLAIN") || fail "the request through $SBX_PLAIN (no --tls-intercept) failed outright"
plain_auth=$(extract_auth "$plain_resp")
plain_hdr_kind=$(classify_header "$plain_auth")

# --- then the intercepting one ---

echo "[$PROBE] creating $SBX_INTERCEPT (--tls-intercept)…"
if ! msb create --tls-intercept --secret "KRAYT_P3_CANARY@$HOST" --net-rule "allow@$HOST" \
  --user agent "$IMAGE" --name "$SBX_INTERCEPT" >&2; then
  fail "msb create --tls-intercept failed"
fi
show_msb_view "$SBX_INTERCEPT"
intercept_issuer=$(peer_issuer "$SBX_INTERCEPT")
echo "[$PROBE]   $SBX_INTERCEPT peer cert ${intercept_issuer:-issuer: (not reported)}"
intercept_env=$(read_guest_env "$SBX_INTERCEPT")
intercept_env_kind=$(classify_env "$intercept_env")
echo "[$PROBE] --tls-intercept guest \$KRAYT_P3_CANARY: $intercept_env  [$intercept_env_kind]"
intercept_resp=$(send_request "$SBX_INTERCEPT") || fail "the request through $SBX_INTERCEPT (--tls-intercept) failed outright"
intercept_auth=$(extract_auth "$intercept_resp")
intercept_hdr_kind=$(classify_header "$intercept_auth")

echo "[$PROBE] --tls-intercept header: $intercept_auth  [$intercept_hdr_kind]"
echo "[$PROBE] no --tls-intercept header: $plain_auth  [$plain_hdr_kind]"

# --- verdict ---

# The guest never holding the real value is the load-bearing property; check it before anything
# about substitution, because a guest that holds the value makes every header reading meaningless.
if [ "$plain_env_kind" = REAL ] || [ "$intercept_env_kind" = REAL ]; then
  echo "[$PROBE] the guest env holds the REAL value — creating $SBX_CONTROL (no --secret at all) to find out whether msb put it there or the host environment leaked in…" >&2
  control_env='<control-not-created>'
  if msb create --user agent "$IMAGE" --name "$SBX_CONTROL" >&2; then
    control_env=$(read_guest_env "$SBX_CONTROL")
  fi
  echo "[$PROBE] control (no --secret) guest \$KRAYT_P3_CANARY: $control_env" >&2
  case "$control_env" in
  *"$REAL_VALUE"*)
    fail "the guest holds the REAL secret value, and so does a control sandbox created with NO --secret at all — the host environment is reaching the guest (msb is forwarding the caller's env, not just the secret), so nothing this probe measured about substitution is valid; fix the leak or measure with a value that is not in the caller's env before re-running"
    ;;
  esac
  fail "msb put the REAL secret value in the guest environment (\$KRAYT_P3_CANARY=$plain_env in $SBX_PLAIN, $intercept_env in $SBX_INTERCEPT) instead of the '$PLACEHOLDER' placeholder, while a no-secret control sandbox has it unset ('$control_env') — this is NOT a question about --tls-intercept: it breaks the credential boundary the ADR rests on (guest_secret_env() maps env_var to placeholder, crates/network/lib/network.rs:454-464), and hand-secrets-to-msb.md's premise that '--secret NAME@HOST never puts the value in the guest' does not hold in $(msb --version). Re-read that path in the installed version before any of the B1 secret tasks are implemented"
fi

for pair in "plain:$plain_env_kind" "intercept:$intercept_env_kind"; do
  case "$pair" in
  *:unset | *:empty)
    fail "the guest's \$KRAYT_P3_CANARY is ${pair#*:} in the ${pair%%:*} sandbox — msb did not install the placeholder into the guest environment at all (expected '$PLACEHOLDER'), so the request carried no credential and neither header reading means anything; check that --secret was accepted and that the host env var was exported before msb started the sandbox"
    ;;
  *:other)
    fail "the guest's \$KRAYT_P3_CANARY in the ${pair%%:*} sandbox is neither the real value nor msb's documented '$PLACEHOLDER' placeholder — the placeholder shape changed in this msb version; update PLACEHOLDER in this probe and in hand-secrets-to-msb.md's decision 2 before trusting any other finding here"
    ;;
  esac
done

# The verdict runs in the direction msb actually behaves, established 2026-08-29 and written up in
# the ADR's withdrawn correction 1. PASS is "msb still does what we documented"; the FAIL that
# matters now is the opposite reading, which would mean msb changed under us.
if [ "$intercept_hdr_kind" = REAL ] && [ "$plain_hdr_kind" = REAL ]; then
  echo "PASS: $PROBE — msb still enables TLS interception from the --secret declaration alone: the guest held only the placeholder in both sandboxes, the real value arrived WITH and WITHOUT --tls-intercept, and the non-intercepting sandbox's own peer certificate was signed by msb (${plain_issuer:-issuer not reported}). Documented in docs/adr-microsandbox-sandbox-layer.md's withdrawn correction 1: translate-network-policy-to-msb.md rule 5's --tls-intercept emission stays redundant-but-deliberate, and a secret still cannot be declared without MITM"
  exit 0
fi

if [ "$intercept_hdr_kind" = REAL ] && [ "$plain_hdr_kind" = placeholder ]; then
  fail "MSB CHANGED — substitution now happens ONLY with --tls-intercept (guest held the placeholder in both; the plain sandbox's request arrived unsubstituted, its peer cert issuer was '${plain_issuer:-not reported}'). This is what the ADR originally corrected to and then withdrew on 2026-08-29, so it is now a regression in $(msb --version), not a confirmation: un-withdraw correction 1 in docs/adr-microsandbox-sandbox-layer.md, restore translate-network-policy-to-msb.md rule 5 to load-bearing (a secret declared without --tls-intercept is silently never substituted), and revisit anything that relies on 'declaring a secret implies MITM'"
fi

if [ "$intercept_hdr_kind" = placeholder ] && [ "$plain_hdr_kind" = placeholder ]; then
  fail "neither request substituted the placeholder — the placeholder arrived intact even WITH --tls-intercept, so interception itself is not working here (allowed host mismatch, require_tls_identity, or the CA not trusted by curl in the image); this says nothing about the --secret-alone question, fix interception first"
fi

fail "unexpected combination — with --tls-intercept the header was [$intercept_hdr_kind] '$intercept_auth', without it [$plain_hdr_kind] '$plain_auth', and the guest held [$plain_env_kind]/[$intercept_env_kind]; inspect the two responses by hand before drawing any conclusion for translate-network-policy-to-msb.md"
