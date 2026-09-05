#!/bin/sh
# P8 — does sandbox.extra_conf behave the way add-msb-extra-conf-escape-hatch.md's decisions 1-3
# say it does?  (non-blocking hardware check for that task's "Done when (hardware)" bullet — see
# also KRAYT_SPEC.md §8.1/§10; needs no credential, every canary is a value this script invents)
#
# THE QUESTIONS, and what a source read (msb v0.6.16/v0.6.17, superradcompany/microsandbox)
# already says about each:
#
#   1. PRECEDENCE. krayt emits its own network policy as flags (--net-default*/--net-rule), never
#      through a --conf. Does an extra_conf's `network.allow` reach a host krayt's own policy does
#      not name? Expected: no. `crates/cli/lib/commands/create.rs` applies the file-config layer
#      first (`resolved.apply(builder)`) and the CLI-flag layer second
#      (`apply_sandbox_opts_after_config`), and `apply_network_opts` in `common.rs:2388-2396`
#      replaces the WHOLE policy object the moment any of --net/--no-net/--net-default/
#      --net-default-egress/--net-default-ingress is present (`replaces_configured_policy`) rather
#      than merging into it. krayt always emits --net-default, so this branch always fires.
#
#   2. PER-SECRET on_violation TIGHTENING — add-msb-extra-conf-escape-hatch.md decision 2 claims an
#      extra_conf secret entry carrying `on_violation: Block` tightens that one secret past krayt's
#      unconditional `--on-secret-violation passthrough`, because the internal `SecretEntry` struct
#      (`packages/microsandbox-types/rust/lib/domain.rs:2172-2174`) carries a per-entry
#      `on_violation: Option<ViolationAction>`.
#
#      THIS SCRIPT'S OWN SOURCE READ DISAGREES, and says so before any hardware confirms it: the
#      internal struct having the field is not the same as the CLI's config-file schema exposing
#      it. `crates/cli/lib/sandbox_config.rs`'s `SecretInput` (the type both a root `--conf`'s
#      `secrets:` map AND `--secret-conf`'s unwrapped map deserialize into, `#[serde(default,
#      deny_unknown_fields)]`, lines 418-425) has exactly four fields: `value`, `allow`, `inject`,
#      `require_tls_identity`. No `on_violation`. `deny_unknown_fields` means a config file that
#      names one is REJECTED outright, not silently ignored. And `materialize_secrets`
#      (`sandbox_config.rs:1760-1769`) hard-codes `on_violation: None` on every entry it builds
#      from a config file — there is no code path from ANY file-based source to that field. The
#      CLI's only other secret-related surfaces are `--secret ENV@HOST[,HOST...]` (`parse_secret`,
#      `common.rs:2704`, no violation-action component in the grammar) and the *global*
#      `--on-secret-violation` flag (`parse_violation_action`, `common.rs:2762`, sets
#      `SecretsConfig.on_violation`, not any one entry's). So per-secret `on_violation` looks
#      unreachable from the `msb` CLI entirely, through any of --conf/--net-conf/--secret-conf or
#      any flag — which would mean decision 2 is wrong, not merely a style choice, and
#      add-msb-extra-conf-escape-hatch.md's "Influencing the secret-violation policy" section and
#      KRAYT_SPEC.md §8.1's matching paragraph need correcting once this is confirmed on hardware.
#
#      Measurement B below attempts it anyway — msb is beta, this repo's own reading has been wrong
#      before (ADR correction 1, withdrawn) — and classifies the result three ways: REJECTED
#      (confirms the finding above), TIGHTENED (decision 2 was right after all, this source read
#      was wrong), or an unexpected combination (investigate by hand).
#
#   3. WIDENING allowed_hosts — decision 3's escalation. `SandboxBuilder::secret_entry`
#      (`sdk/rust/lib/sandbox/builder.rs`) does `network.secrets.secrets.push(entry)` with no
#      dedupe on `env_var`, confirmed by reading `common.rs:2353-2374`: the CLI's own `--secret`
#      loop dedupes ONLY among `--secret` flags themselves (`secret_specs.iter_mut().find(...)`)
#      and is applied to the builder with no knowledge of what a `--conf`-sourced `secrets:` entry
#      already added. So a krayt-declared secret (`--secret NAME@HOST_A`) and an extra_conf entry
#      for the SAME name (`secrets: NAME: {allow: [HOST_B]}`, no `value:` -> defaults to reading
#      the host env var NAME, same as krayt's own default, `sandbox_config.rs:1732-1735`) both
#      reach msb, and HOST_B is a live substitution target for the real value already sitting in
#      the child's env for krayt's own `--secret`. Expected: reachable, and the dangerous one.
#
# WHY FIVE SANDBOXES. Each question gets a measurement plus the control that makes it readable
# alone (the p7 lesson: an unreadable measurement is worse than an absent one):
#   PRECEDENCE          — extra_conf allows a host krayt's own policy excludes; expect BLOCKED.
#   PRECEDENCE-CTRL      — the SAME extra_conf file, with none of krayt's own --net-default*/
#     --net-rule flags present, so nothing overrides it. Expect REACHABLE. Without this, a BLOCKED
#     result above is equally consistent with "krayt's flags win" (the claim) and "the file was
#     never parsed at all" (a probe bug) — this rules out the second reading.
#   VIOLATION            — extra_conf tries to tighten one krayt-declared secret's on_violation to
#     block while a second krayt-declared secret has no override. See measurement B's three-way
#     verdict above.
#   WIDEN                — extra_conf widens a krayt-declared secret's allowed_hosts to a second,
#     already network-allowed host. Expect the REAL value at that host.
#   WIDEN-CTRL            — identical sandbox, no extra_conf. Expect the PLACEHOLDER (unchanged,
#     per p7's passthrough finding) at that same host. Without this, WIDEN's real-value result is
#     equally consistent with "the escalation is real" and "msb substitutes at any network-allowed
#     host regardless of secret scope, which would be a much bigger, unrelated finding" — this
#     control is what pins the effect on extra_conf specifically.
#
# TWO ARMS, AND WHY THE SYNTHETIC ONE IS NOT ENOUGH ON ITS OWN. Measurements 1-3 above build
# `msb create` argv by hand, which establishes a CONDITIONAL: *given* argv A, msb behaves thus.
# The antecedent — that krayt actually emits argv A when a krayt.yaml says `sandbox.extra_conf:` —
# is asserted by internal/sandbox/msb_test.go against a fake `msb`, whose expected argv is itself
# hand-written. Both sides of that join are transcriptions, and nothing mechanically compares them,
# so the pair can drift together and still both pass. (It had drifted: every `msb create` here once
# emitted --conf LAST, the opposite of CreateSpec.Args().) §8.1's actual claim — "krayt's own flags
# outrank an extra_conf, so it cannot relax the run's network policy" — is a claim about KRAYT, and
# a conditional plus a transcription does not test it.
#
# So this probe has a second, KRAYT-DRIVEN arm (measurement 4), on the same reasoning that made p6
# the odd one out among p1-p7: a claim about krayt's own invocation of msb cannot be answered by a
# synthetic sandbox, however faithfully transcribed. It drives a real `krayt run` whose krayt.yaml
# carries a real `sandbox.extra_conf:`, parks it at --on-question=wait for an observation window
# (p6's trick), and then measures THAT sandbox — the one krayt's own argv built — with the same
# curl readings the synthetic arm uses.
#
# The two arms are complementary, not redundant, and neither replaces the other:
#   - The synthetic arm answers "how does msb resolve --conf against flags?" — a fact about msb,
#     true regardless of krayt, and the only way to ask measurement 2 at all (a config msb REJECTS
#     cannot be delivered through a krayt run: `msb create` fails and there is no sandbox to read).
#   - The krayt arm answers "does krayt's shipped argv actually get that resolution?" — the claim
#     §8.1 makes and the one an operator relies on.
#   - When they DISAGREE, the pair localises the fault: synthetic PASS + krayt FAIL means krayt
#     builds different argv than documented (look at CreateSpec.Args()); both FAIL means msb itself
#     changed. Either alone would leave that ambiguous.
#   - The synthetic controls (PRECEDENCE-CTRL, WIDEN-CTRL) do double duty: they use the SAME
#     generated config content the krayt arm hands to `sandbox.extra_conf`, so they establish that
#     the file's directives are live and that msb does not substitute at any network-allowed host
#     regardless of scope. The krayt arm therefore needs no controls of its own.
#
# The krayt arm needs a LIVE AGENT CREDENTIAL (KRAYT_SECRETS), for the same reason p6 does: some
# agent has to authenticate for the run to reach `waiting` and hold the sandbox open. It does NOT
# need a real credential for the secret under test — krayt's msb-era secrets are `network.inject[]`
# keys resolved from the secrets file, so the invented KRAYT_P8_CANARY below works exactly as it
# does in the synthetic arm. Without KRAYT_SECRETS the krayt arm is SKIPPED and the PASS line says
# so explicitly, because a run that skipped it has not tested §8.1's claim about krayt.
#
# WHY --conf COMES FIRST IN EVERY `msb create` BELOW. It mirrors krayt's own emitted argv exactly:
# `CreateSpec.Args()` (internal/sandbox/msb.go) renders `--conf <path>` immediately after the image
# and before every krayt-owned flag. The claim under test in measurement 1 is that msb's precedence
# is layer-based (file layer, then flag layer) rather than argv-order-based, so the position should
# not matter — but that is exactly what is being measured, and a PASS obtained from an ordering
# krayt never ships would not confirm krayt's shipped ordering. Keep these in krayt's order.
#
# Usage: p8-extra-conf-precedence.sh [endpoint-a-url] [endpoint-b-url]
#   Both must echo request headers back in the response body (default: postman-echo.com and
#   httpbin.org, the shape P3/P7 already rely on). Substitute ones you trust. They must be two
#   DIFFERENT hostnames — HostPattern scoping in msb is by hostname, so measurement 3 needs two
#   distinct, real, network-reachable hosts to tell "widened" apart from "always was allowed".
#
#   KRAYT_SECRETS  path to a real secrets.env holding a live agent credential. Set it to run
#                  measurement 4 (the krayt-driven arm); unset, that arm is skipped and only msb's
#                  own behaviour is measured. This makes a real, billed model call.
#   KRAYT_IMAGE    agent image for measurement 4 (default ghcr.io/418-cloud/krayt-agent-claude-code:latest)
#   KRAYT_BIN      krayt binary (default ./bin/krayt, else PATH)
#   KRAYT_PROBE_WAIT_TIMEOUT  seconds to wait for the run to park in `waiting` (default 300)
#
# PASS: PRECEDENCE blocked / PRECEDENCE-CTRL reachable (krayt's flags win, and the file is live),
# WIDEN carries the real value at the widened host while WIDEN-CTRL carries only the placeholder
# there (the escalation is real and attributable to extra_conf), VIOLATION is either REJECTED
# (matching this script's own source read) or cleanly TIGHTENED (decision 2 was right) — either is
# a pass for the run — and, when KRAYT_SECRETS is set, KRAYT-PRECEDENCE/KRAYT-WIDEN reproduce the
# same two outcomes in a sandbox built by krayt's own argv. What fails the run is an unexpected
# combination on any measurement. A PASS whose line says "krayt arm SKIPPED" has measured msb only:
# it does not close §8.1's claim, and HUMAN_TODO.md's entry stays open.
set -eu

PROBE=p8-extra-conf-precedence
ENDPOINT_A=${1:-https://postman-echo.com/get}
ENDPOINT_B=${2:-https://httpbin.org/get}
IMAGE=${KRAYT_MSB_PROBE_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code}

# Measurement 4 only. Tagged, unlike IMAGE above: the synthetic arm never boots an agent, but the
# krayt arm needs one that actually starts and authenticates, so pin what it pulls.
KRAYT_ARM_IMAGE=${KRAYT_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code:latest}
KRAYT_WAIT_TIMEOUT=${KRAYT_PROBE_WAIT_TIMEOUT:-300}

# krayt pins MSB_BACKEND=local on every msb child it spawns (internal/sandbox/msb.go's childEnv,
# add-msb-sandbox-driver.md decision 5) so an operator who has ever exported `cloud`, or who has a
# cloud active_profile, cannot silently redirect a run to a hosted service. Pin it here too: the
# synthetic arm is supposed to mirror krayt's invocation, and the krayt arm has to reach the very
# same backend krayt's own `msb create` used or `msb exec` will not find the sandbox at all.
export MSB_BACKEND=local

SBX_PREC=krayt-probe-p8-precedence
SBX_PREC_CTRL=krayt-probe-p8-precedence-ctrl
SBX_VIOL=krayt-probe-p8-violation
SBX_WIDEN=krayt-probe-p8-widen
SBX_WIDEN_CTRL=krayt-probe-p8-widen-ctrl

# A host reachable in general but never named in any of this probe's krayt-style --net-rule
# allowlists, so measurement 1 can ask "did the network layer let this through" with a plain GET —
# no header echo needed, since this is a connectivity question, not a secrets one. Stable, no rate
# limits, no auth semantics: safe to actually contact, unlike p7's OTHER_HOST which is never
# dialled.
DENIED_HOST=example.com

hostof() { printf '%s' "$1" | sed -E 's#^[a-zA-Z][a-zA-Z0-9+.-]*://##; s#/.*##; s#:.*##'; }
HOST_A=$(hostof "$ENDPOINT_A")
HOST_B=$(hostof "$ENDPOINT_B")
[ -n "$HOST_A" ] || { echo "FAIL: $PROBE — could not parse a host out of '$ENDPOINT_A'"; exit 1; }
[ -n "$HOST_B" ] || { echo "FAIL: $PROBE — could not parse a host out of '$ENDPOINT_B'"; exit 1; }
[ "$HOST_A" != "$HOST_B" ] || { echo "FAIL: $PROBE — endpoint-a and endpoint-b resolve to the same host ($HOST_A); measurement 3 needs two distinct hosts"; exit 1; }

REAL_CANARY="krayt-p8-canary-real-$$"
REAL_OTHER="krayt-p8-other-real-$$"
export KRAYT_P8_CANARY="$REAL_CANARY" # msb reads these from this process's env at `msb create` time
export KRAYT_P8_OTHER="$REAL_OTHER"

WORKDIR=$(mktemp -d)

# Set by the krayt arm as it goes; cleanup has to cope with every one of them still being empty,
# since the arm may be skipped outright or fail at any step.
KRAYT_SCRATCH=
KRAYT_RUNID=
KRAYT_MERGED_SECRETS=

cleanup() {
  for s in "$SBX_PREC" "$SBX_PREC_CTRL" "$SBX_VIOL" "$SBX_WIDEN" "$SBX_WIDEN_CTRL"; do
    msb rm --force "$s" >/dev/null 2>&1 || true
  done
  # `krayt stop` tears down the sandbox the supervisor owns; killing the script alone would leave
  # a parked --on-question=wait run holding a VM open indefinitely.
  if [ -n "$KRAYT_RUNID" ] && [ -n "$KRAYT_SCRATCH" ] && [ -n "${KRAYT:-}" ]; then
    "$KRAYT" stop --repo "$KRAYT_SCRATCH/repo" "$KRAYT_RUNID" >/dev/null 2>&1 || true
  fi
  # Holds a copy of the operator's real agent credential — remove it before anything else can
  # outlive this process.
  [ -n "$KRAYT_MERGED_SECRETS" ] && rm -f "$KRAYT_MERGED_SECRETS"
  [ -n "$KRAYT_SCRATCH" ] && rm -rf "$KRAYT_SCRATCH"
  rm -rf "$WORKDIR"
  return 0
}
trap cleanup EXIT INT TERM

fail() { echo "FAIL: $PROBE — $1"; exit 1; }

command -v msb >/dev/null 2>&1 \
  || fail "msb not on PATH — install with: curl -fsSL https://install.microsandbox.dev | sh"

echo "msb version: $(msb --version)"
echo "[$PROBE] endpoint-a=$ENDPOINT_A (host=$HOST_A)  endpoint-b=$ENDPOINT_B (host=$HOST_B)  denied-host=$DENIED_HOST"

# --no-tty for the same reason P3/P7 use it: msb allocates a PTY when the caller's stdin is a
# terminal, and a PTY re-introduces echo and CRLF, corrupting every exact compare below.
msb_exec() { sbx=$1; shift; msb exec --no-tty --user agent "$sbx" -- "$@"; }

# --- config files: krayt never parses these, so they are exactly what an operator would hand to
# sandbox.extra_conf — the same wrapped, root-`--conf`-shaped keys (§8.1 decision 1) ---

NET_CONF="$WORKDIR/net-allow-denied-host.yaml"
cat >"$NET_CONF" <<EOF
network:
  allow:
    - "$DENIED_HOST"
EOF

VIOLATION_CONF="$WORKDIR/secret-violation.yaml"
cat >"$VIOLATION_CONF" <<EOF
secrets:
  KRAYT_P8_CANARY:
    # Deliberately the SAME host krayt's own --secret flag already scoped this secret to (not
    # $HOST_B, which measurement 3 uses to test widening) — msb's own validation rejects a secret
    # entry with no allowed_hosts at all, and reusing $HOST_A here keeps this file from also
    # widening scope, which would conflate this measurement with measurement 3's.
    allow:
      - "$HOST_A"
    on_violation: block
EOF

WIDEN_CONF="$WORKDIR/secret-widen.yaml"
cat >"$WIDEN_CONF" <<EOF
secrets:
  KRAYT_P8_CANARY:
    allow:
      - "$HOST_B"
EOF

# Checks plain reachability: BLOCKED (msb drops the connection — curl exit 52/56/35, not an HTTP
# status) or REACHABLE. No secret involved, so no header to classify.
check_reachable() {
  sbx=$1 url=$2
  if msb_exec "$sbx" sh -c 'curl -fsS --max-time 20 -o /dev/null "$1"' _ "$url" 2>/dev/null; then
    printf 'REACHABLE'
  else
    printf 'BLOCKED'
  fi
}

# Sends $envvar's value (indirect expansion, since the same script probes two differently-named
# secrets) in a header and classifies what came back against this measurement's own real value —
# BLOCKED (transport-level drop), REAL:<value>, PLACEHOLDER, NO_HEADER_ECHOED, or OTHER:<value>.
send_and_classify() {
  sbx=$1 url=$2 envvar=$3 realvalue=$4
  if ! out=$(msb_exec "$sbx" sh -c \
    'v=$(eval echo "\$$1"); curl -fsS --max-time 20 -H "Authorization: Bearer $v" "$2" 2>/dev/null' \
    _ "$envvar" "$url"); then
    printf 'BLOCKED'
    return 0
  fi
  auth=$(printf '%s' "$out" | tr -d '\r' | grep -io '"authorization"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n1)
  case "$auth" in
  '') printf 'NO_HEADER_ECHOED' ;;
  *"$realvalue"*) printf 'REAL:%s' "$realvalue" ;;
  *"MSB_$envvar"*) printf 'PLACEHOLDER' ;;
  *) printf 'OTHER:%s' "$auth" ;;
  esac
}

# =====================================================================================
# Measurement 1 — PRECEDENCE: does krayt's own network policy fully replace extra_conf's?
# =====================================================================================

echo "[$PROBE] 1a: creating $SBX_PREC (krayt-style policy allowing only $HOST_A + extra_conf allowing $DENIED_HOST too)…"
msb create "$IMAGE" --conf "$NET_CONF" --name "$SBX_PREC" --user agent \
  --net-default deny --net-rule "allow@dns" --net-rule "allow@$HOST_A" \
  --on-secret-violation passthrough >&2 \
  || fail "msb create (precedence) failed — see stderr above"
prec_allowed=$(check_reachable "$SBX_PREC" "$ENDPOINT_A")
prec_denied=$(check_reachable "$SBX_PREC" "https://$DENIED_HOST/")
echo "[$PROBE] 1a: krayt-allowed host ($HOST_A) -> $prec_allowed;  extra_conf-only host ($DENIED_HOST) -> $prec_denied"

echo "[$PROBE] 1b: creating $SBX_PREC_CTRL (SAME extra_conf file, none of krayt's own network flags)…"
msb create "$IMAGE" --conf "$NET_CONF" --name "$SBX_PREC_CTRL" --user agent >&2 \
  || fail "msb create (precedence control) failed — see stderr above"
prec_ctrl_denied=$(check_reachable "$SBX_PREC_CTRL" "https://$DENIED_HOST/")
echo "[$PROBE] 1b: extra_conf-only host ($DENIED_HOST), nothing overriding it -> $prec_ctrl_denied"

# =====================================================================================
# Measurement 2 — PER-SECRET on_violation: is decision 2's tightening reachable at all?
# =====================================================================================

echo "[$PROBE] 2: creating $SBX_VIOL (KRAYT_P8_CANARY tightened via extra_conf, KRAYT_P8_OTHER left at krayt's global passthrough)…"
viol_create_err="$WORKDIR/viol-create.stderr"
if msb create "$IMAGE" --conf "$VIOLATION_CONF" --name "$SBX_VIOL" --user agent \
  --net-default deny --net-rule "allow@dns" --net-rule "allow@$HOST_A" --net-rule "allow@$HOST_B" \
  --secret "KRAYT_P8_CANARY@$HOST_A" --secret "KRAYT_P8_OTHER@$HOST_A" --tls-intercept \
  --on-secret-violation passthrough >"$viol_create_err" 2>&1; then
  viol_canary=$(send_and_classify "$SBX_VIOL" "$ENDPOINT_B" KRAYT_P8_CANARY "$REAL_CANARY")
  viol_other=$(send_and_classify "$SBX_VIOL" "$ENDPOINT_B" KRAYT_P8_OTHER "$REAL_OTHER")
  echo "[$PROBE] 2: extra_conf accepted on_violation — tightened secret at out-of-scope host -> $viol_canary; untouched secret at same host -> $viol_other"
  viol_verdict=accepted
else
  cat "$viol_create_err" >&2
  if grep -qi 'on_violation' "$viol_create_err"; then
    echo "[$PROBE] 2: msb create REJECTED the config naming the field 'on_violation' — matches this script's own source read (sandbox_config.rs's SecretInput has no such field)"
  else
    echo "[$PROBE] 2: msb create failed, but not obviously because of 'on_violation' — inspect $viol_create_err by hand"
  fi
  viol_verdict=rejected
fi

# =====================================================================================
# Measurement 3 — WIDENING allowed_hosts: does the real value follow to the new host?
# =====================================================================================

echo "[$PROBE] 3a: creating $SBX_WIDEN (KRAYT_P8_CANARY scoped to $HOST_A by krayt, widened to $HOST_B by extra_conf)…"
msb create "$IMAGE" --conf "$WIDEN_CONF" --name "$SBX_WIDEN" --user agent \
  --net-default deny --net-rule "allow@dns" --net-rule "allow@$HOST_A" --net-rule "allow@$HOST_B" \
  --secret "KRAYT_P8_CANARY@$HOST_A" --tls-intercept \
  --on-secret-violation passthrough >&2 \
  || fail "msb create (widen) failed — see stderr above"
widen_inscope=$(send_and_classify "$SBX_WIDEN" "$ENDPOINT_A" KRAYT_P8_CANARY "$REAL_CANARY")
widen_widened=$(send_and_classify "$SBX_WIDEN" "$ENDPOINT_B" KRAYT_P8_CANARY "$REAL_CANARY")
echo "[$PROBE] 3a: original scope ($HOST_A) -> $widen_inscope;  widened-by-extra_conf host ($HOST_B) -> $widen_widened"

echo "[$PROBE] 3b: creating $SBX_WIDEN_CTRL (identical, no extra_conf)…"
msb create "$IMAGE" --name "$SBX_WIDEN_CTRL" --user agent \
  --net-default deny --net-rule "allow@dns" --net-rule "allow@$HOST_A" --net-rule "allow@$HOST_B" \
  --secret "KRAYT_P8_CANARY@$HOST_A" --tls-intercept \
  --on-secret-violation passthrough >&2 \
  || fail "msb create (widen control) failed — see stderr above"
widenctrl_widened=$(send_and_classify "$SBX_WIDEN_CTRL" "$ENDPOINT_B" KRAYT_P8_CANARY "$REAL_CANARY")
echo "[$PROBE] 3b: same host ($HOST_B) without extra_conf -> $widenctrl_widened"

# =====================================================================================
# Measurement 4 — the KRAYT-DRIVEN arm: does krayt's OWN argv get that same resolution?
# =====================================================================================
#
# Measurements 1-3 hand-build argv. This one does not: it writes a krayt.yaml carrying a real
# `sandbox.extra_conf:` and lets krayt construct the `msb create` invocation itself, so what is
# under test is the whole path — LoadConfig -> resolveAgainstDir -> Spec.ExtraConf ->
# CreateSpec.ExtraConf -> CreateSpec.Args() -> the real msb. Nothing here transcribes krayt's argv;
# a mistake anywhere along that path shows up as a changed reading rather than being assumed away.
#
# The extra_conf handed to krayt is the union of measurement 1's and measurement 3's files, so one
# run (one billed agent session) covers both. `on_violation` is deliberately NOT in it: if msb
# rejects that field — which measurement 2 exists to find out — `msb create` fails, krayt's run
# dies at creation, and there would be no sandbox left to read anything else from. That question
# belongs to the synthetic arm, which can afford a create that fails.

krayt_verdict=skipped
k_denied=
k_allowed=
k_inscope=
k_widened=

if [ -z "${KRAYT_SECRETS:-}" ]; then
  echo "[$PROBE] 4: SKIPPED — KRAYT_SECRETS is unset. Measurements 1-3 answer questions about msb; this is the only one that tests krayt's own invocation of it, which is what KRAYT_SPEC.md §8.1 actually claims. Set KRAYT_SECRETS to a secrets.env holding a live agent credential to run it."
else
  [ -r "$KRAYT_SECRETS" ] || fail "KRAYT_SECRETS ($KRAYT_SECRETS) is not readable"

  KRAYT=${KRAYT_BIN:-}
  if [ -z "$KRAYT" ]; then
    if [ -x ./bin/krayt ]; then KRAYT=$(cd ./bin && pwd)/krayt
    elif command -v krayt >/dev/null 2>&1; then KRAYT=$(command -v krayt)
    else fail "no krayt binary — run 'make build' first, or set KRAYT_BIN"; fi
  fi
  echo "[$PROBE] 4: krayt: $KRAYT"

  KRAYT_SCRATCH=$(mktemp -d) || fail "mktemp -d failed"
  krepo=$KRAYT_SCRATCH/repo
  mkdir -p "$krepo"
  printf '# scratch\n\nThrowaway repo for %s measurement 4.\n' "$PROBE" >"$krepo/README.md"
  git -C "$krepo" init -q
  git -C "$krepo" add -A
  git -C "$krepo" -c user.name='krayt probe' -c user.email='probe@example.invalid' commit -qm init

  # The operator's real credential plus this probe's invented canary, in one file krayt can read.
  # umask rather than a later chmod: the window between creation and chmod is exactly when another
  # process could open it. Never printed, never passed on any argv.
  KRAYT_MERGED_SECRETS=$KRAYT_SCRATCH/secrets.env
  (umask 077 && cat "$KRAYT_SECRETS" >"$KRAYT_MERGED_SECRETS") \
    || fail "could not copy $KRAYT_SECRETS into the scratch secrets file"
  # A secrets file whose last line has no newline would otherwise get the canary glued onto the end
  # of the operator's own last KEY=VALUE — corrupting their credential and this probe's at once.
  if [ "$(tail -c1 "$KRAYT_MERGED_SECRETS" | od -An -tx1 | tr -d ' \n')" != 0a ]; then
    printf '\n' >>"$KRAYT_MERGED_SECRETS"
  fi
  printf 'KRAYT_P8_CANARY=%s\n' "$REAL_CANARY" >>"$KRAYT_MERGED_SECRETS"

  # measurement 1's network.allow + measurement 3's widened secret scope, in one file — the same
  # content the synthetic controls already proved is live and does not widen scope by accident.
  KRAYT_EXTRA_CONF=$KRAYT_SCRATCH/msb-extra.yaml
  cat >"$KRAYT_EXTRA_CONF" <<EOF
network:
  allow:
    - "$DENIED_HOST"
secrets:
  KRAYT_P8_CANARY:
    allow:
      - "$HOST_B"
EOF

  # An explicit --config, not a <repo>/krayt.yaml: §8.3 refuses sandbox.extra_conf (and
  # network.inject) from an auto-loaded repo-local config, so writing this into $krepo/krayt.yaml
  # would test the refusal, not the feature. Paths are absolute — an explicit --config leaves
  # relative paths anchored to the working directory, which this script must not depend on.
  #
  # $HOST_A/$HOST_B are deliberately NOT in `passthrough`: substitution only happens on a host msb
  # intercepts, and a passthrough host is tunnelled unread. $DENIED_HOST appears nowhere — that is
  # the whole point of measurement 1.
  KRAYT_YAML=$KRAYT_SCRATCH/krayt.yaml
  cat >"$KRAYT_YAML" <<EOF
image: $KRAYT_ARM_IMAGE
secrets: $KRAYT_MERGED_SECRETS

agent:
  adapter: claude-code

bundle_depth: 1

resources:
  timeout: 30m

network:
  mode: allowlist
  allow:
    - api.anthropic.com
    - "$HOST_A"
    - "$HOST_B"
  inject:
    - key: KRAYT_P8_CANARY
      hosts: ["$HOST_A"]

sandbox:
  extra_conf: $KRAYT_EXTRA_CONF
EOF

  echo "[$PROBE] 4: starting a real --on-question=wait krayt run (this makes a real, billed model call)…"
  # --on-question=wait is the observation window, exactly as in p6: it parks the run with the
  # sandbox alive and the agent blocked, for as long as the readings below need. Reaching `waiting`
  # at all also proves the run got far enough to have created the sandbox from this config.
  kstart=$(printf '%s\n' "Before you change anything, you MUST call the ask_human tool to ask whether you should add a 'Status' section to README.md. Do not answer that question yourself and do not edit any file until the human replies." |
    "$KRAYT" run --config "$KRAYT_YAML" --repo "$krepo" --task - \
      --on-question wait --detach 2>&1) \
    || fail "krayt run failed to start: $kstart. If the error names sandbox.extra_conf, krayt refused the file rather than passing it — check internal/cli/run.go's provenance split (§8.3)"

  KRAYT_RUNID=$(printf '%s\n' "$kstart" | sed -n 's/^run \(run_[A-Za-z0-9]*\) started.*/\1/p' | head -n1)
  [ -n "$KRAYT_RUNID" ] || fail "could not parse a run id out of krayt's output: $kstart"
  krundir=$krepo/.krayt/runs/$KRAYT_RUNID
  KRAYT_SBX=krayt-$KRAYT_RUNID
  echo "[$PROBE] 4: run $KRAYT_RUNID (sandbox $KRAYT_SBX) — waiting up to ${KRAYT_WAIT_TIMEOUT}s for state 'waiting'…"

  krun_state() {
    [ -r "$krundir/meta.json" ] || { printf 'unknown'; return 0; }
    grep -o '"state"[[:space:]]*:[[:space:]]*"[^"]*"' "$krundir/meta.json" |
      head -n1 | sed 's/.*"\([^"]*\)"$/\1/'
  }

  kwaited=0
  while [ "$kwaited" -lt "$KRAYT_WAIT_TIMEOUT" ]; do
    kstate=$(krun_state)
    case "$kstate" in
    waiting) break ;;
    failed | done)
      kerr=$(grep -o '"error"[[:space:]]*:[[:space:]]*"[^"]*"' "$krundir/meta.json" 2>/dev/null | head -n1 || true)
      fail "the krayt run reached '$kstate' without parking in 'waiting' — no sandbox to measure, so measurement 4 observed nothing. ${kerr:-(no error recorded)}. Check $krundir/console.log"
      ;;
    esac
    sleep 2
    kwaited=$((kwaited + 2))
  done
  [ "$(krun_state)" = waiting ] \
    || fail "the krayt run did not reach 'waiting' within ${KRAYT_WAIT_TIMEOUT}s (state '$(krun_state)') — no observation window. Raise KRAYT_PROBE_WAIT_TIMEOUT or read $krundir/console.log"

  # meta.json records sandbox.extra_conf's path and digest (§8.1 decision 4). Reading it back is
  # the independent confirmation that the file this probe wrote is the file krayt actually
  # consumed — without it, every reading below could be measuring a run that silently ignored the
  # key, which is precisely the failure mode this whole arm exists to rule out.
  #
  # The digest is compared, not just printed. `ExtraConf *ExtraConfMeta json:"extra_conf,omitempty"`
  # means the key's mere presence already proves krayt registered SOME file — but only recomputing
  # digest.Canonical (sha256:<hex>) over the file this probe wrote proves it was THIS one, which is
  # what makes the readings below attributable. meta.json is json.MarshalIndent'd, so the object
  # spans several lines: match the leaf fields, never the `{...}` on one line.
  krayt_conf_path=$(sed -n 's/^[[:space:]]*"path"[[:space:]]*:[[:space:]]*"\(.*\)".*$/\1/p' "$krundir/meta.json" | head -n1)
  krayt_conf_digest=$(sed -n 's/^[[:space:]]*"digest"[[:space:]]*:[[:space:]]*"\(.*\)".*$/\1/p' "$krundir/meta.json" | head -n1)
  [ -n "$krayt_conf_digest" ] \
    || fail "the run parked in 'waiting' but its meta.json records no \"extra_conf\" digest — krayt did not register sandbox.extra_conf for this run at all, so every reading below would be measuring a sandbox built without it. Check internal/orchestrator/orchestrator.go's extraConfMeta and internal/cli/run.go's config resolution"
  if command -v sha256sum >/dev/null 2>&1; then
    want_digest=sha256:$(sha256sum "$KRAYT_EXTRA_CONF" | cut -d' ' -f1)
  else
    want_digest=sha256:$(shasum -a 256 "$KRAYT_EXTRA_CONF" | cut -d' ' -f1)
  fi
  [ "$krayt_conf_path" = "$KRAYT_EXTRA_CONF" ] \
    || fail "meta.json's extra_conf.path is '$krayt_conf_path' but this probe wrote '$KRAYT_EXTRA_CONF' — krayt resolved the config's sandbox.extra_conf to a different file than the one named. Check resolveAgainstDir (internal/cli/run.go)"
  [ "$krayt_conf_digest" = "$want_digest" ] \
    || fail "meta.json's extra_conf.digest ($krayt_conf_digest) does not match this probe's file ($want_digest) — krayt recorded a digest of something other than the file it was pointed at, so nothing below is attributable to the extra_conf under test"
  echo "[$PROBE] 4: meta.json's extra_conf matches the file this probe wrote (path + $krayt_conf_digest)"

  echo "[$PROBE] 4: parked in 'waiting' — measuring the sandbox krayt's own argv built…"
  k_denied=$(check_reachable "$KRAYT_SBX" "https://$DENIED_HOST/")
  k_allowed=$(check_reachable "$KRAYT_SBX" "$ENDPOINT_A")
  echo "[$PROBE] 4a: extra_conf-only host ($DENIED_HOST) -> $k_denied;  krayt-allowed host ($HOST_A) -> $k_allowed"
  k_inscope=$(send_and_classify "$KRAYT_SBX" "$ENDPOINT_A" KRAYT_P8_CANARY "$REAL_CANARY")
  k_widened=$(send_and_classify "$KRAYT_SBX" "$ENDPOINT_B" KRAYT_P8_CANARY "$REAL_CANARY")
  echo "[$PROBE] 4b: krayt-declared scope ($HOST_A) -> $k_inscope;  widened-by-extra_conf host ($HOST_B) -> $k_widened"
  krayt_verdict=measured
fi

# =====================================================================================
# Verdict
# =====================================================================================

# Checked first and on its own, same as p7: this is the finding that would contradict krayt's own
# security-relevant precedence claim (§8.1 decision 2), and must not be buried among the others.
if [ "$prec_denied" = REACHABLE ]; then
  fail "PRECEDENCE: extra_conf's network.allow REACHED a host krayt's own policy excludes ($DENIED_HOST -> $prec_denied). This contradicts §8.1 decision 2 and KRAYT_SPEC.md's claim that krayt's own flags always replace an extra_conf's network policy — msb has changed, or krayt's argv construction is wrong. Do not ship sandbox.extra_conf as documented until this is re-read against current msb source"
fi
if [ "$prec_allowed" != REACHABLE ]; then
  fail "PRECEDENCE: krayt's OWN allowed host ($HOST_A) came back '$prec_allowed', not REACHABLE — the sandbox's network is broken in a way unrelated to extra_conf; fix that before trusting any other result here"
fi
if [ "$prec_ctrl_denied" != REACHABLE ]; then
  fail "PRECEDENCE-CTRL: the SAME extra_conf file, with nothing overriding it, did not make $DENIED_HOST reachable ('$prec_ctrl_denied'). That means measurement 1's BLOCKED result is unreadable — it is equally consistent with 'krayt's flags win' and 'this extra_conf file was never parsed at all'. Check the YAML shape against sandbox_config.rs's SandboxConfigInput/NetworkConfigInput before re-running"
fi

if [ "$widen_inscope" != "REAL:$REAL_CANARY" ]; then
  fail "WIDEN: the secret's own original scope ($HOST_A) did not carry the real value ('$widen_inscope') — the sandbox's secret substitution is broken in a way unrelated to extra_conf; fix that before trusting the widen result"
fi
if [ "$widenctrl_widened" = "REAL:$REAL_CANARY" ]; then
  fail "WIDEN-CTRL: the real value reached $HOST_B with NO extra_conf involved at all. That means msb substitutes at any network-allowed host regardless of the secret's declared scope — a much bigger finding than decision 3's escalation, and it means §6.6.1's per-secret host scoping does not hold even without this escape hatch. Stop and investigate independently of sandbox.extra_conf"
fi
if [ "$widen_widened" = "REAL:$REAL_CANARY" ] && [ "$widenctrl_widened" = PLACEHOLDER ]; then
  echo "[$PROBE] WIDEN confirmed: extra_conf's widened allowed_hosts got the REAL credential substituted at $HOST_B ($widen_widened), while the identical sandbox without extra_conf only ever showed the placeholder there ($widenctrl_widened) — the escalation in §8.1/§10 is real and specifically attributable to extra_conf."
else
  fail "WIDEN: unexpected combination — widened host with extra_conf='$widen_widened', without extra_conf='$widenctrl_widened'. Expected REAL:$REAL_CANARY and PLACEHOLDER respectively. Inspect $WIDEN_CONF and both sandboxes by hand"
fi

case "$viol_verdict" in
rejected)
  echo "[$PROBE] VIOLATION: msb rejected the per-secret on_violation config — confirms this script's own source read (sandbox_config.rs's SecretInput/materialize_secrets expose no such field on any file-based config surface). add-msb-extra-conf-escape-hatch.md decision 2 and the matching paragraph in KRAYT_SPEC.md §8.1 are WRONG as written and should be corrected: the only per-secret lever extra_conf actually reaches is scope (allowed_hosts, measurement 3), not violation action. Tightening one secret's on_violation is not achievable through sandbox.extra_conf on this msb version."
  ;;
accepted)
  if [ "$viol_canary" = BLOCKED ] && [ "$viol_other" = PLACEHOLDER ]; then
    echo "[$PROBE] VIOLATION confirmed: extra_conf's on_violation: block tightened KRAYT_P8_CANARY at an out-of-scope host ($viol_canary) while krayt's global --on-secret-violation passthrough still governed KRAYT_P8_OTHER at the identical host ($viol_other) — decision 2 was right, and this script's own source read above was wrong. Correct this probe's header comment, not the spec."
  else
    fail "VIOLATION: msb accepted the on_violation config, but the outcome doesn't match decision 2 — tightened secret at out-of-scope host='$viol_canary' (want BLOCKED), untouched secret at same host='$viol_other' (want PLACEHOLDER). Inspect $VIOLATION_CONF and $SBX_VIOL by hand"
  fi
  ;;
esac

# Checked last, and against the synthetic arm's already-established results rather than in
# isolation: by this point measurement 1 has shown msb blocks the extra_conf-only host given
# krayt-shaped argv, and measurement 3 has shown the widened scope carries the real value. So any
# divergence here localises itself — msb did one thing when handed argv by hand and a different
# thing when handed argv by krayt, which can only mean krayt's argv is not what it is documented
# to be (internal/sandbox/msb.go's CreateSpec.Args()).
case "$krayt_verdict" in
measured)
  if [ "$k_denied" = REACHABLE ]; then
    fail "KRAYT-PRECEDENCE: in a sandbox built by KRAYT'S OWN argv, extra_conf's network.allow REACHED $DENIED_HOST — a host the run's own allowlist does not name. The synthetic arm blocked the same host with the same file, so msb is not the difference: krayt is emitting something other than the documented argv, and KRAYT_SPEC.md §8.1's claim that an extra_conf cannot relax a run's network policy is FALSE AS SHIPPED. This is the finding that would make sandbox.extra_conf unsafe to ship as documented — stop here"
  fi
  if [ "$k_allowed" != REACHABLE ]; then
    fail "KRAYT-PRECEDENCE: the run's own allowed host ($HOST_A) came back '$k_allowed', not REACHABLE — this sandbox's network is broken in a way unrelated to extra_conf, so its blocked result above proves nothing. Fix that first"
  fi
  if [ "$k_inscope" != "REAL:$REAL_CANARY" ]; then
    fail "KRAYT-WIDEN: the secret's krayt-declared scope ($HOST_A) did not carry the real value ('$k_inscope') — substitution is broken in this run independently of extra_conf, so the widened reading below is unreadable. Check that network.inject's key matches the secrets file and that $HOST_A is not in network.passthrough"
  fi
  if [ "$k_widened" = "REAL:$REAL_CANARY" ]; then
    echo "[$PROBE] KRAYT-WIDEN confirmed: through a real krayt run, an extra_conf entry widened a krayt-declared secret's allowed_hosts and the REAL credential was substituted at $HOST_B — a host krayt itself scoped the secret away from. §10's escalation is real for operators, not just for hand-built argv."
  else
    fail "KRAYT-WIDEN: the synthetic arm got REAL:$REAL_CANARY at $HOST_B but the krayt-built sandbox got '$k_widened'. Same msb, same extra_conf content, different argv — so krayt's emitted argv differs from what this probe transcribes. Diff \`msb create\` argv from $krundir/console.log against CreateSpec.Args() before trusting either arm"
  fi
  echo "PASS: $PROBE — precedence holds ($DENIED_HOST blocked despite extra_conf, $HOST_A still reachable, and the control shows the file itself is live), the secret-scope-widening escalation is real and attributable to extra_conf, the per-secret violation-action question resolved to a definite, non-ambiguous answer (see VIOLATION line above), AND the krayt-driven arm reproduced both outcomes in a sandbox built by krayt's own argv — so §8.1's claim is tested, not merely transcribed."
  ;;
*)
  echo "PASS: $PROBE — precedence holds ($DENIED_HOST blocked despite extra_conf, $HOST_A still reachable, and the control shows the file itself is live), the secret-scope-widening escalation is real and attributable to extra_conf, and the per-secret violation-action question resolved to a definite, non-ambiguous answer (see VIOLATION line above for which one)."
  echo "NOTE: $PROBE — the krayt-driven arm was SKIPPED (no KRAYT_SECRETS). Everything above is a fact about MSB given argv this script builds by hand; that krayt emits that argv is asserted by unit tests against a fake msb, not observed here. KRAYT_SPEC.md §8.1's claim is NOT closed by this run — leave HUMAN_TODO.md's entry open."
  ;;
esac
