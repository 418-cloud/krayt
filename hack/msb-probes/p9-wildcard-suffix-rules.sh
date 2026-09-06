#!/bin/sh
# P9 — does msb enforce `*.<suffix>` the way its source reads?  (non-blocking hardware check for
# support-wildcard-network-hosts.md; needs no credential — every value this script uses is public)
#
# THE QUESTION. krayt's pre-flight now accepts `*.example.com` in network.allow, network.passthrough
# and a secret's scope, and passes the entry to msb verbatim (internal/task/netpolicy_msb.go). That
# is a change to what krayt's allowlist ACCEPTS, and the whole safety argument for it rests on
# claims read out of msb 0.6.16's source rather than measured:
#
#   1. `--net-rule allow@*.example.com` parses to Destination::DomainSuffix
#      (crates/cli/lib/net_rule.rs:443-449) and matches the apex plus any label-aligned subdomain
#      (matches_suffix, crates/network/lib/policy/types.rs:997-1013).
#   2. An ALLOW-side domain rule only fires once the connected IP has a DNS-cache binding to a
#      matching hostname (deferred_domain_match, types.rs:960-975) — deliberately stricter than the
#      deny side, so a guest cannot claim an arbitrary SNI on an unresolved IP. This is the same
#      mechanism krayt's existing EXACT-host allow rules already depend on; DomainSuffix and Domain
#      go through one function. If it did not fire for DomainSuffix, a wildcard allow entry would be
#      inert and krayt would be shipping an allowlist entry that permits nothing.
#   3. `--tls-bypass *.example.com` is a wildcard bypass pattern
#      (crates/network/lib/config/builder.rs:452-456, DomainPattern::matches_normalized,
#      crates/network/lib/tls/state.rs:236-243) — so a wildcard passthrough really does skip
#      interception, which is what makes `passthrough ⊆ allow` worth enforcing over wildcards.
#   4. msb REFUSES a single-label suffix on --net-rule (SuffixTooBroad,
#      crates/network/lib/policy/name.rs:65-107) and a bare `*` (net_rule.rs:833-838).
#
# WHY 5 MEASUREMENTS. Each of the four claims fails in a way the others would not catch:
#   A subdomain reachable  — claim 1+2. The headline: a DomainSuffix allow rule actually allows.
#   B apex reachable       — the `hostname == suffix` half of matches_suffix. It is the branch that
#                            regresses most easily and a subdomain-only test would miss it, while
#                            krayt documents `*.x.com` as covering `x.com` in §6.6 and README.
#   C neighbour denied     — label alignment. Without it `*.example.com` would match
#                            `evilexample.com`, and every wildcard in every krayt.yaml would be
#                            wider than it reads. This is the one that must FAIL to connect.
#   D bypass skips MITM    — claim 3, read off the SERVED CERTIFICATE's issuer (p7's method): under
#                            interception the issuer is msb's own CA, under a bypass it is the real
#                            upstream chain.
#   E `allow@*.com` refused— claim 4. krayt refuses this itself, so this measurement is not about
#                            krayt's behaviour: it pins that krayt's floor is ALIGNED with msb's
#                            guard rather than merely additive, so a future msb relaxation is
#                            noticed rather than silently inherited.
#
# Usage: p9-wildcard-suffix-rules.sh [suffix] [subdomain-url] [apex-url]
#   Defaults to the GitHub raw/objects family, which is public, needs no credential, serves the
#   apex and a subdomain over TLS, and is already in this repo's own allowlist. Substitute a
#   domain family you trust — the suffix must have BOTH a reachable apex and a reachable
#   subdomain, or B is untestable and this script says so rather than guessing.
#
# PASS: A and B connect, C is denied, D shows a non-msb issuer, E is rejected by msb — wildcard
# suffix rules behave as krayt now documents them (KRAYT_SPEC.md §6.6).
# FAIL: each failure names what it means for krayt. The two that matter most are C connecting
# (wildcards are wider than every krayt.yaml says) and A failing (a wildcard allow entry is inert,
# so a config that reads as permitting a host permits nothing).
set -eu

PROBE=p9-wildcard-suffix-rules
SUFFIX=${1:-github.com}
SUB_URL=${2:-https://api.github.com/zen}
APEX_URL=${3:-https://github.com/robots.txt}
IMAGE=${KRAYT_MSB_PROBE_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code}
SBX_ALLOW=krayt-probe-p9-allow
SBX_BYPASS=krayt-probe-p9-bypass
SBX_BROAD=krayt-probe-p9-broad

# The non-aligned neighbour: the suffix with a label glued onto its FRONT, no dot. A bare
# strings.HasSuffix-style matcher accepts this; a label-aligned one does not. It need not exist in
# DNS for the measurement to be meaningful — msb's allow side requires a DNS-cache binding to a
# hostname the rule matches, so an unresolvable name is denied for the same reason a resolvable
# non-matching one is. NXDOMAIN and "policy denied" are both DENIED here, and that is not a
# weakness of the test: the failure this measurement guards against is msb ALLOWING the request.
NEIGHBOUR="evil$SUFFIX"

cleanup() {
  for s in "$SBX_ALLOW" "$SBX_BYPASS" "$SBX_BROAD"; do
    msb rm --force "$s" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT INT TERM

fail() { echo "FAIL: $PROBE — $1"; exit 1; }

command -v msb >/dev/null 2>&1 \
  || fail "msb not on PATH — install with: curl -fsSL https://install.microsandbox.dev | sh"

echo "msb version: $(msb --version)"
echo "[$PROBE] suffix=*.$SUFFIX  subdomain=$SUB_URL  apex=$APEX_URL  neighbour=$NEIGHBOUR"

# --no-tty for P3's reason: msb allocates a PTY when the caller's stdin is a terminal, and a PTY
# re-introduces echo and CRLF, which corrupts every exact compare below.
msb_exec() { sbx=$1; shift; msb exec --no-tty --user agent "$sbx" -- "$@"; }

# Mirrors krayt's own rendered policy rather than msb's defaults (KRAYT_SPEC.md §6.6): an explicit
# deny default puts msb on the branch that adds no implicit DNS rule, so allow@dns is required or
# nothing resolves — and it must precede the deny groups, since msb is first-match-wins and `dns`
# is the gateway, which sits inside `private`. The ONE difference from krayt's argv is the allow
# rule under test, which is a suffix rather than an exact host.
create_wildcard_sandbox() {
  name=$1
  shift
  msb create "$IMAGE" --name "$name" --user agent \
    --net-default deny \
    --net-rule "allow@dns" \
    --net-rule "deny@private" \
    --net-rule "deny@loopback" \
    --net-rule "deny@link-local" \
    --net-rule "deny@meta" \
    --net-rule "deny@multicast" \
    --net-rule "deny@host" \
    --net-rule "allow@*.$SUFFIX" \
    "$@" >&2
}

# REACHED / DENIED, decided on curl's exit code rather than an HTTP status: a policy-denied request
# dies at the transport (52/56/35) or at resolution (6), never with a response.
reach() {
  sbx=$1 url=$2
  if msb_exec "$sbx" sh -c 'curl -fsS --max-time 30 -o /dev/null "$1"' _ "$url" >/dev/null 2>&1; then
    printf 'REACHED'
  else
    printf 'DENIED'
  fi
}

# --- A: a subdomain under the suffix rule ---

echo "[$PROBE] A: creating $SBX_ALLOW (--net-rule allow@*.$SUFFIX)…"
create_wildcard_sandbox "$SBX_ALLOW" || fail "msb create with --net-rule allow@*.$SUFFIX failed outright — msb no longer accepts the suffix shorthand krayt now emits (net_rule.rs:443-449). krayt's wildcard support is broken end to end; check msb's changelog before anything else"
a=$(reach "$SBX_ALLOW" "$SUB_URL")
echo "[$PROBE] A subdomain under *.$SUFFIX -> $a"

# --- B: the apex itself, same rule, same sandbox ---

b=$(reach "$SBX_ALLOW" "$APEX_URL")
echo "[$PROBE] B apex ($SUFFIX) under the same rule -> $b"

# --- C: the non-aligned neighbour, same rule, same sandbox ---

c=$(reach "$SBX_ALLOW" "https://$NEIGHBOUR/")
echo "[$PROBE] C non-aligned neighbour ($NEIGHBOUR) -> $c"

# --- D: does --tls-bypass *.<suffix> actually skip interception? ---
#
# Read off the served certificate's ISSUER, p7's method. A sandbox that declares a secret has TLS
# interception on for everything it does not bypass, so the issuer is msb's own CA there and the
# real upstream chain on a bypassed host. The secret is a value this script invents and is scoped
# to a host never contacted.
echo "[$PROBE] D: creating $SBX_BYPASS (same allow rule + --tls-bypass *.$SUFFIX + a secret)…"
export KRAYT_P9_CANARY="krayt-p9-canary-$$"
SUB_HOST=$(printf '%s' "$SUB_URL" | sed -E 's#^[a-zA-Z][a-zA-Z0-9+.-]*://##; s#/.*##; s#:.*##')
[ -n "$SUB_HOST" ] || fail "could not parse a host out of '$SUB_URL'"
create_wildcard_sandbox "$SBX_BYPASS" \
  --tls-bypass "*.$SUFFIX" \
  --secret "KRAYT_P9_CANARY@never-contacted.invalid" \
  --on-secret-violation passthrough \
  || fail "msb create with --tls-bypass *.$SUFFIX failed — msb no longer accepts a wildcard bypass pattern (config/builder.rs:452-456), which krayt now emits for every wildcard network.passthrough entry"

issuer=$(msb_exec "$SBX_BYPASS" sh -c \
  'echo | openssl s_client -connect "$1:443" -servername "$1" 2>/dev/null | openssl x509 -noout -issuer 2>/dev/null' \
  _ "$SUB_HOST" || true)
issuer=$(printf '%s' "$issuer" | tr -d '\r')
echo "[$PROBE] D issuer served for $SUB_HOST under --tls-bypass *.$SUFFIX -> ${issuer:-<none>}"

# --- E: is a single-label suffix refused by msb itself? ---

echo "[$PROBE] E: attempting msb create --net-rule allow@*.com (expected: rejected)…"
if msb create "$IMAGE" --name "$SBX_BROAD" --user agent \
  --net-default deny --net-rule "allow@dns" --net-rule "allow@*.com" >/dev/null 2>&1; then
  e=ACCEPTED
else
  e=REJECTED
fi
echo "[$PROBE] E allow@*.com -> $e"

# --- verdict ---

# Checked first and on its own: this is the finding that would make every wildcard in every
# krayt.yaml wider than it reads, and it must not be reported as one bullet among several.
if [ "$c" = REACHED ]; then
  fail "*.$SUFFIX ALLOWED the non-aligned neighbour $NEIGHBOUR. msb's suffix match is not label-aligned (contradicting matches_suffix, types.rs:997-1013), so every '*.x.com' entry also permits 'evilx.com' and anything else merely ENDING in the suffix. krayt's documented semantics (KRAYT_SPEC.md §6.6), its hostCovers mirror, and the safety argument in support-wildcard-network-hosts.md are all wrong as written — treat this as a security finding, not a doc bug"
fi

if [ "$a" != REACHED ]; then
  fail "a subdomain was NOT reachable under --net-rule allow@*.$SUFFIX ($a). Either the DNS-cache binding the allow side requires does not fire for DomainSuffix the way it does for Domain (deferred_domain_match, types.rs:960-975), or this endpoint is unreachable for an unrelated reason — check with an exact 'allow@$SUB_HOST' rule before concluding. If it is the former, a wildcard allow entry is INERT: a krayt.yaml that reads as permitting a host permits nothing, which is exactly the silently-ineffective entry krayt's pre-flight exists to refuse"
fi

if [ "$b" != REACHED ]; then
  fail "the APEX ($SUFFIX) was not reachable under allow@*.$SUFFIX ($b). msb's matches_suffix has a 'hostname == suffix' branch, so krayt documents and tests '*.x.com' as covering 'x.com' itself (KRAYT_SPEC.md §6.6, TestHostCovers's 'apex itself' case). If msb has dropped that branch, krayt's docs overstate what an entry grants and an operator relying on the apex would be denied at runtime with a config that pre-flight accepted"
fi

case "$issuer" in
'') fail "could not read the served certificate's issuer for $SUB_HOST — D is INCONCLUSIVE, not a pass. openssl may be absent from the probe image, or the connection may have been denied outright. Re-run with an image carrying openssl before drawing any conclusion about --tls-bypass" ;;
*[Mm]icrosandbox* | *msb*) fail "--tls-bypass *.$SUFFIX did NOT skip interception: $SUB_HOST was served msb's own CA ($issuer). A wildcard passthrough entry in a krayt.yaml therefore does not do what §6.6 says it does, and every pinning client behind one breaks the moment any secret is declared — which is the exact failure krayt's wildcard passthrough support exists to prevent" ;;
esac

if [ "$e" = ACCEPTED ]; then
  fail "msb ACCEPTED --net-rule allow@*.com. Its SuffixTooBroad guard (policy/name.rs:65-107) is gone, so krayt's own refusal of single-label suffixes (task.validateHostPattern) is now STRICTER than msb rather than aligned with it. Nothing is unsafe — krayt refusing more than msb only fails runs early — but support-wildcard-network-hosts.md decision 2's premise ('never more permissive than msb's guarded surface, and never relying on a guard msb only applies to one of the three') should be re-read against whatever msb does now"
fi

echo "PASS: $PROBE — --net-rule allow@*.$SUFFIX reaches a subdomain ($a) and the apex ($b) while the non-aligned neighbour $NEIGHBOUR is denied ($c), --tls-bypass *.$SUFFIX serves the real upstream chain rather than msb's CA ($issuer), and msb itself rejects allow@*.com ($e). msb enforces domain-suffix rules exactly as KRAYT_SPEC.md §6.6 describes them: apex-inclusive, label-aligned, one convention across --net-rule and --tls-bypass, with the same deferred DNS-cache binding krayt's exact-host rules already rely on. Record the date and this msb version in §6.6's wildcard paragraph"
