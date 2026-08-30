#!/bin/sh
# P1 — can a non-root guest process open AF_VSOCK under msb?  (BLOCKING)
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
# PASS: the echo round-trips as `agent`.
# FAIL: says which of "denied to agent but works as root" (dial-ask-channel-over-vsock.md then
# needs a root-owned in-guest forwarder — the guest daemon B1 exists to delete) or "denied to
# both" (a different, deeper problem, not a privilege one) was found.
set -eu

PROBE=p1-vsock-nonroot
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)

IMAGE=${KRAYT_MSB_PROBE_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code}
SANDBOX=krayt-probe-p1
PORT=51234
MSG_AGENT="krayt-p1-agent-$$"
MSG_ROOT="krayt-p1-root-$$"
SOCK=$(mktemp -u "${TMPDIR:-/tmp}/krayt-p1-vsock.XXXXXX")
BUILD_DIR=$(mktemp -d)
GUEST_BIN="$BUILD_DIR/vsock-probe-guest"
HOST_BIN="$BUILD_DIR/vsock-echo-host"
HOST_PID=

cleanup() {
  if [ -n "$HOST_PID" ]; then
    kill "$HOST_PID" >/dev/null 2>&1 || true
    wait "$HOST_PID" 2>/dev/null || true
  fi
  msb rm --force "$SANDBOX" >/dev/null 2>&1 || true
  rm -rf "$BUILD_DIR"
  rm -f "$SOCK"
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

echo "[$PROBE] cross-compiling vsock-probe-guest (linux/arm64)…"
if ! (cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$GUEST_BIN" ./hack/msb-probes/vsock-probe-guest); then
  fail "cross-compiling hack/msb-probes/vsock-probe-guest failed"
fi

echo "[$PROBE] building vsock-echo-host (host arch)…"
if ! (cd "$REPO_ROOT" && go build -o "$HOST_BIN" ./hack/msb-probes/vsock-echo-host); then
  fail "building hack/msb-probes/vsock-echo-host failed"
fi

echo "[$PROBE] starting host echo listener on ${SOCK}…"
"$HOST_BIN" -conns 2 -timeout 90s "$SOCK" &
HOST_PID=$!

i=0
while [ ! -S "$SOCK" ]; do
  i=$((i + 1))
  if [ "$i" -gt 100 ]; then
    fail "host echo listener never created $SOCK"
  fi
  sleep 0.1
done

echo "[$PROBE] creating sandbox $SANDBOX from $IMAGE (--user agent, --vsock $SOCK:$PORT)…"
if ! msb create --vsock "$SOCK:$PORT" --user agent "$IMAGE" --name "$SANDBOX" >&2; then
  fail "msb create failed — see output above"
fi

echo "[$PROBE] copying vsock-probe-guest into the sandbox…"
if ! msb copy "$GUEST_BIN" "$SANDBOX:/tmp/vsock-probe-guest" >&2; then
  fail "msb copy failed"
fi
if ! msb exec --no-tty --user root "$SANDBOX" -- chmod +x /tmp/vsock-probe-guest >&2; then
  fail "chmod +x on the copied guest binary failed (as root)"
fi

echo "[$PROBE] dialing AF_VSOCK as agent (uid 1000)…"
if msb exec --no-tty --user agent "$SANDBOX" -- /tmp/vsock-probe-guest "$PORT" "$MSG_AGENT"; then
  echo "PASS: $PROBE — AF_VSOCK round-trip succeeded as non-root user agent (uid 1000)"
  exit 0
fi

echo "[$PROBE] failed as agent — retrying as root to tell the two failure modes apart…" >&2
if msb exec --no-tty --user root "$SANDBOX" -- /tmp/vsock-probe-guest "$PORT" "$MSG_ROOT"; then
  echo "FAIL: $PROBE — AF_VSOCK works as root but is refused to non-root agent (uid 1000); dial-ask-channel-over-vsock.md needs a root-owned in-guest forwarder instead of a direct krayt-ask dial"
else
  echo "FAIL: $PROBE — AF_VSOCK dial failed for both agent and root; not a privilege issue, msb's --vsock host-socket feature itself did not work here"
fi
exit 1
