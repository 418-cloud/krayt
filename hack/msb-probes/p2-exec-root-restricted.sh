#!/bin/sh
# P2 — does `msb exec --user root` still work under `--security restricted`?  (BLOCKING)
#
# docs/ai-tasks/probe-microsandbox-feasibility.md. `--security restricted` sets no_new_privs,
# drops the mount-admin capability, and forces nosuid,nodev on user mounts
# (docs/security/hardening.mdx:57). agentd runs as PID 1 as root and spawns each exec, so
# `msb exec --user root` should still work — but no_new_privs is exactly the kind of flag that
# breaks a privilege *raise*, and the guest helper's whole value (add-krayt-guest-helper.md) is
# running as root against a git dir the agent (running as `agent`) cannot write
# (fix-guest-git-config-rce.md's property).
#
# PASS requires all three:
#   1. `msb exec --user agent … -- id -u` prints 1000.
#   2. `msb exec --user root … -- id -u` prints 0.
#   3. the property that actually matters, not just the uid: a root-created 0700 directory is
#      unreadable by a `--user agent` exec.
#
# On FAIL the finding says whether root exec was refused outright or merely ran unprivileged
# (uid still 1000) — those are different failures with different fixes, and either one means
# add-krayt-guest-helper.md must choose between --security restricted and the helper's privilege
# separation (it cannot have both).
set -eu

PROBE=p2-exec-root-restricted
IMAGE=${KRAYT_MSB_PROBE_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code}
SANDBOX=krayt-probe-p2

cleanup() {
  msb rm --force "$SANDBOX" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $PROBE — $1"
  exit 1
}

# Every exec below passes --no-tty. Without it msb allocates a PTY whenever the caller's stdin is a
# terminal (`crates/cli/lib/commands/exec.rs`, use_interactive_tty = stdin_is_terminal && !no_tty)
# — and command substitution does not redirect stdin, so running this by hand takes the PTY path.
# A PTY re-introduces echo and CRLF (msb's own words, exec.rs's --stream doc comment): `id -u`
# then comes back as "1000\r\n", command substitution strips the \n and leaves the \r, the exact
# compare below fails on a correct uid, and echoing the value inside a longer message returns the
# cursor to column 0 so the start of the FAIL line is overwritten by its own tail.
# exec_output is the belt to --no-tty's braces: strip CRs, keep the last non-empty line (which
# also tolerates a status line printed ahead of the command's own output).
exec_output() {
  printf '%s\n' "$1" | tr -d '\r' | awk 'NF { last = $0 } END { print last }'
}

command -v msb >/dev/null 2>&1 \
  || fail "msb not on PATH — install with: curl -fsSL https://install.microsandbox.dev | sh"

echo "msb version: $(msb --version)"

echo "[$PROBE] creating sandbox $SANDBOX from $IMAGE (--security restricted --user agent)…"
if ! msb create --security restricted --user agent "$IMAGE" --name "$SANDBOX" >&2; then
  fail "msb create --security restricted failed outright — the restricted profile itself is unusable with this image"
fi

echo "[$PROBE] checking uid as agent…"
raw=$(msb exec --no-tty --user agent "$SANDBOX" -- id -u) || fail "msb exec --user agent failed to run 'id -u' at all"
agent_uid=$(exec_output "$raw")
if [ "$agent_uid" != "1000" ]; then
  fail "msb exec --user agent -- id -u printed '$agent_uid', not 1000 — the --user agent exec itself is not landing as the expected uid"
fi

echo "[$PROBE] checking uid as root…"
if ! raw=$(msb exec --no-tty --user root "$SANDBOX" -- id -u); then
  fail "msb exec --user root … -- id -u was refused outright under --security restricted (root exec does not work at all)"
fi
root_uid=$(exec_output "$raw")
if [ "$root_uid" != "0" ]; then
  fail "msb exec --user root … -- id -u succeeded but printed '$root_uid', not 0 — root exec ran but did NOT raise privilege (succeeded-but-unprivileged, not an outright refusal)"
fi

echo "[$PROBE] creating /probe-root-only (mode 0700, root-owned) and a file inside it…"
if ! msb exec --no-tty --user root "$SANDBOX" -- sh -c 'mkdir -p /probe-root-only && echo secret > /probe-root-only/x && chmod 0700 /probe-root-only' >&2; then
  fail "root exec could not create /probe-root-only even though 'id -u' as root reported 0 — inconsistent root exec behaviour"
fi

echo "[$PROBE] confirming agent cannot read it…"
if msb exec --no-tty --user agent "$SANDBOX" -- cat /probe-root-only/x >/dev/null 2>&1; then
  fail "agent (uid 1000) COULD read a root-only 0700 file — root exec ran as root by uid but the privilege separation property does not hold"
fi

echo "PASS: $PROBE — msb exec --user root works under --security restricted (uid 0, and a root-only 0700 path is unreadable by --user agent)"
