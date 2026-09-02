#!/bin/sh
# P1 — can a non-root guest process open AF_VSOCK under msb, and does the round trip complete?
#
# docs/ai-tasks/probe-microsandbox-feasibility.md. `msb create --vsock HOST_PATH:PORT` exposes a
# host unix socket at guest CID 2 on PORT (docs/networking/host-sockets.mdx). krayt's agent
# images run as the non-root user `agent` (uid 1000, images/agents/claude-code/Dockerfile).
# Whether that user may open an AF_VSOCK socket in msb's guest is undocumented, and
# dial-ask-channel-over-vsock.md's whole design — krayt-ask dialing the host directly, no guest
# daemon — depends on the answer.
#
# Also doubles as the ADR's open question 1: the sandbox is created from the real
# krayt-agent-claude-code image with --user agent, so a pass here also confirms that image runs
# unmodified under msb.
#
# ## Three shapes, many iterations
#
# The dial is settled — it works, for `agent` and for root, and msb bridges to the host socket as
# the invoking user (decision 10). The *echo* took longer: on msb 0.6.16 it intermittently came
# back empty, the guest reading EOF after the host had already logged both the bytes it read and
# the echo it wrote. Three single-sample runs on 2026-09-02 went fail/fail, then pass/pass, then
# pass/fail/pass — enough to know something was wrong, never enough to say what, and the third of
# them concluded "only the lingering host works" from one sample per shape while the bare
# non-lingering host had passed in that very run. So this probe measures a **rate**:
# $KRAYT_P1_ITERATIONS round trips per shape, inside a single `msb exec`. That is what turned an
# intermittent mystery into a number and a cause.
#
# The three shapes, all on one sandbox (--vsock is repeatable), differ in one property each:
#
#   bare    a path straight in $TMPDIR — the shape that passed on 2026-08-29
#   priv    ask.sock, mode 0600, inside a 0700 directory (internal/askbridge.Listen's socket)
#   linger  priv, but the host waits for the guest to close instead of closing first — what
#           internal/askbridge does today, and the shape this probe's verdict tracks
#
# bare vs priv isolates the private directory (internal/askbridge.Listen, decision 4/10). priv vs
# linger isolates who closes the connection first.
#
# That last comparison is settled, and it moved production: on 2026-09-02 (msb 0.6.16,
# Apple-Silicon Mac) the close-first shapes completed 21 of 75 round trips while the lingering
# shape completed 25 of 25, so internal/askbridge now writes its answer and waits for the sandbox
# to close (lingerUntilPeerCloses, KRAYT_SPEC.md §6.13). **linger is therefore the shape krayt
# actually uses, and the shape this probe passes or fails on.** bare and priv stay because they
# characterise msb's defect — and because the defect disappearing is a finding too: if they start
# completing every iteration, msb has fixed the drop and §6.13's wait becomes belt-and-braces
# rather than load-bearing. Either way the probe says which, in a line of its own, without failing
# over it: a probe that fails on a known, worked-around defect is noise, and noise is what stops
# anyone re-running it.
#
# PASS: the linger shape — what internal/askbridge does — completes every iteration as `agent`.
# FAIL: reports all four rates and the leg each failure broke at (dial / write / read / mismatch).
# A dial failure keeps P1's original discrimination: whether root gets through where agent does
# not (dial-ask-channel-over-vsock.md would then need a root-owned in-guest forwarder — the guest
# daemon B1 exists to delete).
set -eu

PROBE=p1-vsock-nonroot

# Everything this run prints — including the three background listeners' output, which is half the
# evidence and the half that scrolls away first — is teed to one file. Chasing an intermittent
# failure through a terminal's scrollback loses exactly the lines that matter.
if [ -z "${KRAYT_P1_TRANSCRIPT:-}" ]; then
  KRAYT_P1_TRANSCRIPT=$(mktemp "${TMPDIR:-/tmp}/krayt-p1-transcript.XXXXXX")
  export KRAYT_P1_TRANSCRIPT
  RC_FILE=$(mktemp "${TMPDIR:-/tmp}/krayt-p1-rc.XXXXXX")
  # `|| RC=$?` rather than a bare call: set -e would otherwise abort this group the moment the
  # real run exits non-zero, and the exit code — the probe's whole reporting contract alongside
  # its one PASS/FAIL line — would never be written.
  { RC=0; "$0" "$@" || RC=$?; echo "$RC" >"$RC_FILE"; } 2>&1 | tee "$KRAYT_P1_TRANSCRIPT"
  echo "[$PROBE] full transcript: $KRAYT_P1_TRANSCRIPT"
  RC=$(cat "$RC_FILE" 2>/dev/null || echo 1)
  [ -n "$RC" ] || RC=1
  rm -f "$RC_FILE"
  exit "$RC"
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)

IMAGE=${KRAYT_MSB_PROBE_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code}
SANDBOX=krayt-probe-p1
MSB_HOME=${MSB_HOME:-$HOME/.microsandbox}
ITERATIONS=${KRAYT_P1_ITERATIONS:-25}
FULL="$ITERATIONS/$ITERATIONS"

PORT_BARE=51234
PORT_PRIV=51235
PORT_LINGER=51236

BUILD_DIR=$(mktemp -d)
GUEST_BIN="$BUILD_DIR/vsock-probe-guest"
HOST_BIN="$BUILD_DIR/vsock-echo-host"
HOST_PIDS=

SOCK_BARE=$(mktemp -u "${TMPDIR:-/tmp}/krayt-p1-vsock.XXXXXX")
PRIV_DIR=$(mktemp -d "${TMPDIR:-/tmp}/krayt-p1-vsock-dir.XXXXXX")
LINGER_DIR=$(mktemp -d "${TMPDIR:-/tmp}/krayt-p1-vsock-lng.XXXXXX")
chmod 700 "$PRIV_DIR" "$LINGER_DIR"
SOCK_PRIV="$PRIV_DIR/ask.sock"
SOCK_LINGER="$LINGER_DIR/ask.sock"

cleanup() {
  for pid in $HOST_PIDS; do
    kill "$pid" >/dev/null 2>&1 || true
    wait "$pid" 2>/dev/null || true
  done
  msb rm --force "$SANDBOX" >/dev/null 2>&1 || true
  rm -rf "$BUILD_DIR" "$PRIV_DIR" "$LINGER_DIR"
  rm -f "$SOCK_BARE"
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $PROBE — $1"
  exit 1
}

command -v msb >/dev/null 2>&1 \
  || fail "msb not on PATH — install with: curl -fsSL https://install.microsandbox.dev | sh"
command -v go >/dev/null 2>&1 \
  || fail "go not on PATH — needed to build the two probe binaries"

echo "msb version: $(msb --version)"
echo "[$PROBE] $ITERATIONS iteration(s) per shape (override with \$KRAYT_P1_ITERATIONS)"

echo "[$PROBE] cross-compiling vsock-probe-guest (linux/arm64)…"
if ! (cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$GUEST_BIN" ./hack/msb-probes/vsock-probe-guest); then
  fail "cross-compiling hack/msb-probes/vsock-probe-guest failed"
fi

echo "[$PROBE] building vsock-echo-host (host arch)…"
if ! (cd "$REPO_ROOT" && go build -o "$HOST_BIN" ./hack/msb-probes/vsock-echo-host); then
  fail "building hack/msb-probes/vsock-echo-host failed"
fi

start_host() { # label conns sockpath [extra flags…]
  _label=$1
  _conns=$2
  _sock=$3
  shift 3
  "$HOST_BIN" -label "$_label" -conns "$_conns" -timeout 600s "$@" "$_sock" &
  HOST_PIDS="$HOST_PIDS $!"

  _i=0
  while [ ! -S "$_sock" ]; do
    _i=$((_i + 1))
    if [ "$_i" -gt 100 ]; then
      fail "host echo listener ($_label) never created $_sock"
    fi
    sleep 0.1
  done
}

# linger gets twice the connections: it is the shape re-run as root when the agent pass fails.
echo "[$PROBE] starting three host echo listeners…"
start_host bare "$ITERATIONS" "$SOCK_BARE"
start_host priv "$ITERATIONS" "$SOCK_PRIV"
start_host linger "$((ITERATIONS * 2))" "$SOCK_LINGER" -linger

echo "[$PROBE] creating sandbox $SANDBOX from $IMAGE (--user agent, three --vsock endpoints)…"
if ! msb create \
  --vsock "$SOCK_BARE:$PORT_BARE" \
  --vsock "$SOCK_PRIV:$PORT_PRIV" \
  --vsock "$SOCK_LINGER:$PORT_LINGER" \
  --user agent "$IMAGE" --name "$SANDBOX" >&2; then
  fail "msb create failed — see output above. If msb rejected the three --vsock endpoints rather than the image (its own --help says the flag is repeatable), re-run with only --vsock $SOCK_PRIV:$PORT_PRIV to get the production shape's answer at least"
fi

echo "[$PROBE] copying vsock-probe-guest into the sandbox…"
if ! msb copy "$GUEST_BIN" "$SANDBOX:/tmp/vsock-probe-guest" >&2; then
  fail "msb copy failed"
fi
if ! msb exec --no-tty --user root "$SANDBOX" -- chmod +x /tmp/vsock-probe-guest >&2; then
  fail "chmod +x on the copied guest binary failed (as root)"
fi

# attempt runs $ITERATIONS round trips in one exec and echoes "<ok>/<total>" — with ":<legs>"
# appended when any failed, e.g. "24/25:read:1". It reads the guest's own SUMMARY line rather
# than trusting msb to propagate an exit code, and passes the guest's whole output through to
# stderr so the transcript keeps every per-iteration detail the rate summarises.
attempt() { # label port user
  _out="$BUILD_DIR/exec-$1-$3.log"
  _code=0
  msb exec --no-tty --user "$3" "$SANDBOX" -- \
    /tmp/vsock-probe-guest "$2" "krayt-p1-$1-$3-$$" "$ITERATIONS" >"$_out" 2>&1 || _code=$?
  sed "s|^|[$PROBE][$1/$3] |" "$_out" >&2

  _sum=$(grep '^SUMMARY ' "$_out" | tail -1)
  if [ -z "$_sum" ]; then
    # Nothing the guest binary prints is in there at all, so `msb exec` failed before the binary
    # ran (a stopped sandbox, a bad --user). Reporting a leg here would blame the kernel for
    # msb's error.
    echo "no-guest-output(exit $_code)"
    return
  fi
  _ok=${_sum##*ok=}
  _ok=${_ok%% *}
  _legs=${_sum##*legs=}
  if [ "$_ok" = "$ITERATIONS" ]; then
    echo "$_ok/$ITERATIONS"
  else
    echo "$_ok/$ITERATIONS:$_legs"
  fi
}

RESULT_BARE=$(attempt bare "$PORT_BARE" agent)
RESULT_PRIV=$(attempt priv "$PORT_PRIV" agent)
RESULT_LINGER=$(attempt linger "$PORT_LINGER" agent)

echo "[$PROBE] as agent (uid 1000): linger=$RESULT_LINGER (what internal/askbridge does) | bare=$RESULT_BARE priv=$RESULT_PRIV (host closes first)"

# Only the shape krayt uses is re-run as root, and only when it failed: telling "denied to agent,
# works as root" from "broken for everyone" is the one thing the second run buys.
RESULT_LINGER_ROOT=skipped
if [ "$RESULT_LINGER" != "$FULL" ]; then
  echo "[$PROBE] the production shape did not complete every iteration as agent — retrying as root…" >&2
  RESULT_LINGER_ROOT=$(attempt linger "$PORT_LINGER" root)
  echo "[$PROBE] as root: linger=$RESULT_LINGER_ROOT"

  echo "[$PROBE] msb's own account of the sandbox (its relay logs the host side of --vsock):" >&2
  msb logs "$SANDBOX" --tail 80 >&2 2>/dev/null \
    || tail -80 "$MSB_HOME/sandboxes/$SANDBOX/logs/runtime.log" >&2 2>/dev/null \
    || echo "[$PROBE] no msb log available for $SANDBOX" >&2
fi

RATES="linger=$RESULT_LINGER linger-as-root=$RESULT_LINGER_ROOT bare=$RESULT_BARE priv=$RESULT_PRIV"

# The close-first shapes are reported, never failed on: krayt does not use them any more. What
# they are worth is saying whether msb's defect is still there — in both directions.
if [ "$RESULT_BARE" = "$FULL" ] && [ "$RESULT_PRIV" = "$FULL" ]; then
  echo "[$PROBE] NOTE: the close-first shapes completed every iteration too — msb may have fixed the reply drop measured at 21/75 on 0.6.16. internal/askbridge.lingerUntilPeerCloses would then be belt-and-braces rather than load-bearing; check the msb version and amend KRAYT_SPEC.md §6.13 rather than dropping the wait on one run's evidence"
else
  echo "[$PROBE] NOTE: msb still drops the reply when the host closes first (bare=$RESULT_BARE priv=$RESULT_PRIV) — expected on 0.6.16, and exactly why internal/askbridge waits for the sandbox to close (KRAYT_SPEC.md §6.13). Not a failure of this probe"
fi

if [ "$RESULT_LINGER" = "$FULL" ]; then
  echo "PASS: $PROBE — every AF_VSOCK round trip completed as non-root user agent (uid 1000) against a 0600 socket in a 0700 directory, with the host waiting for the guest to close as internal/askbridge does ($RATES)"
  exit 0
fi

case "$RESULT_LINGER" in
no-guest-output*)
  fail "the guest probe produced no output at all for the production shape ($RATES); msb exec failed before the binary ran, so this says nothing about AF_VSOCK — see the transcript"
  ;;
0/*:dial:*)
  if [ "$RESULT_LINGER_ROOT" = "$FULL" ]; then
    fail "AF_VSOCK works as root but every dial is refused to non-root agent (uid 1000) ($RATES); dial-ask-channel-over-vsock.md needs a root-owned in-guest forwarder instead of a direct krayt-ask dial"
  fi
  fail "the AF_VSOCK dial itself failed every iteration for both agent and root ($RATES); not a privilege issue, msb's --vsock host-socket feature itself did not work here"
  ;;
esac

# Past the dial, on the shape krayt ships. This is the one that matters: the host did everything
# §6.13 asks of it and the reply was still lost.
fail "the shape internal/askbridge actually uses lost iterations ($RATES): the host waited for the sandbox to close and the answer was dropped anyway, so §6.13's ordering is no longer sufficient. A human's answer would be lost this often, silently — the guest sees EOF with nothing read and cannot tell that from a host that never answered. Take the transcript and msb's relay log upstream before run-tasks-on-microsandbox.md makes this the only question channel"
