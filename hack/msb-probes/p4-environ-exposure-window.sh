#!/bin/sh
# P4 — how long does the real secret value sit in a process environment?  (non-blocking; sizes an
# accepted residual)
#
# docs/ai-tasks/probe-microsandbox-feasibility.md. The ADR's secret-handling contract already
# ACCEPTS environ exposure — this decides nothing. What's unverified is whether the value lives
# only in the short-lived `msb create` process, or in the long-lived per-sandbox `msb sandbox`
# runtime for the whole run. The Linux form (/proc/<pid>/environ) is authoritative, since this is
# a property of msb's process structure, not the host OS; the macOS form (ps -Eww) is best-effort
# — macOS may refuse to show another process's environment even at the same uid, in which case the
# finding is "inconclusive on darwin", never a false negative treated as a real answer.
#
# WHAT msb's SOURCE SAYS THE ANSWER IS, read at tag v0.6.16 (2026-08-30) — the run is the check on
# this, not a substitute for it. On unix the value never enters the long-lived runtime's
# environment: `resolve_config_secret_sources` reads it with `std::env::var` in the process that
# holds the variable (`sdk/rust/lib/sandbox/config.rs`), and `spawn_sandbox` serializes it into an
# *anonymous* temp file — `tempfile::tempfile()`, no path in the filesystem — handed to the
# `msb sandbox` child at a fixed descriptor as `--config-fd`
# (`sdk/rust/lib/runtime/spawn.rs`, write_launch_config_fd). Not argv, not the child's environ, not
# a file on disk. So the expected reading here is ABSENT, and the environ window is only the
# process krayt invokes with the value in `cmd.Env`.
#
# That is *not* the same as "the value is gone": it is in the runtime's address space for the whole
# run — it has to be, to substitute — held in a `Zeroizing<String>` that is wiped on drop. This
# probe measures the environ, which is the surface another process at the same uid can read.
#
# Windows differs and is out of this probe's reach: there `write_launch_config_file` writes the
# same launch config to a NamedTempFile under the sandbox's runtime dir and passes its **path on
# argv**. Short-lived (dropped once the child reports startup) but real, and it belongs to
# `expand-platforms-under-msb.md` Part B rather than here.
#
# Because this probe cannot fail the underlying question either way (empty means a short window,
# non-empty means a long one, "inconclusive" means neither was established) it always reports
# PASS/exit 0 once it has actually run the check; FAIL is reserved for the probe itself not being
# able to run at all (msb missing, sandbox creation failing, unreadable proc entry on Linux).
set -eu

PROBE=p4-environ-exposure-window
SANDBOX=krayt-probe-p4
CANARY_VALUE="sk-canary-p4-$$"

cleanup() {
  msb rm --force "$SANDBOX" >/dev/null 2>&1 || true
  [ -n "${control_pid:-}" ] && kill "$control_pid" >/dev/null 2>&1
  return 0
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $PROBE — $1"
  exit 1
}

command -v msb >/dev/null 2>&1 \
  || fail "msb not on PATH — install with: curl -fsSL https://install.microsandbox.dev | sh"

echo "msb version: $(msb --version)"

export KRAYT_CANARY="$CANARY_VALUE"
echo "[$PROBE] creating sandbox $SANDBOX with --secret 'KRAYT_CANARY@api.example.com'…"
if ! msb create python --name "$SANDBOX" --secret 'KRAYT_CANARY@api.example.com' >&2; then
  fail "msb create failed"
fi
sleep 1 # let the per-sandbox 'msb sandbox' runtime process actually start

pid=$(pgrep -f 'msb sandbox' 2>/dev/null | head -n1 || true)
os=$(uname -s)

if [ "$os" = "Linux" ]; then
  if [ -z "$pid" ]; then
    fail "no 'msb sandbox' runtime process found via pgrep -f 'msb sandbox' — sandbox may not have started, or msb's process naming has changed since 0.6.16"
  fi
  if [ ! -r "/proc/$pid/environ" ]; then
    fail "/proc/$pid/environ is not readable (pid $pid) — cannot inspect the runtime's environment"
  fi
  if tr '\0' '\n' <"/proc/$pid/environ" | grep -q '^KRAYT_CANARY='; then
    echo "PASS: $PROBE — KRAYT_CANARY IS present in the long-lived 'msb sandbox' runtime's environ (pid $pid, /proc) — the exposure window is the whole run, not just the short-lived msb create process"
  else
    echo "PASS: $PROBE — KRAYT_CANARY is ABSENT from the long-lived 'msb sandbox' runtime's environ (pid $pid, /proc) — the exposure window is only the short-lived msb create process"
  fi
  exit 0
fi

if [ "$os" = "Darwin" ]; then
  echo "[$PROBE] macOS has no /proc — falling back to 'ps -Eww'; this may not show another process's environment even at the same uid" >&2

  # Positive control, run in the same shell against the same `ps`: without it, "KRAYT_CANARY was
  # not in the output" is unreadable — a genuinely absent value and an environment macOS simply
  # refuses to show look identical, and macOS has restricted `ps -E` for years. A plain same-uid
  # child is the fairest available comparison.
  KRAYT_P4_CONTROL=control-canary-visible
  export KRAYT_P4_CONTROL
  # Left to exit on its own rather than killed: killing a background job makes the shell print a
  # "Terminated" notice into the probe's output, and the whole contract here is one clean finding
  # line. The trap still reaps it if this exits early.
  sleep 5 &
  control_pid=$!
  control_out=$(ps -Eww -p "$control_pid" 2>/dev/null || true)
  control_shows_env=no
  case "$control_out" in
  *KRAYT_P4_CONTROL=*) control_shows_env=yes ;;
  esac
  echo "[$PROBE] control: can 'ps -Eww' read a plain same-uid child's environment here? $control_shows_env" >&2

  if [ -z "$pid" ]; then
    echo "PASS: $PROBE — inconclusive on darwin: no 'msb sandbox' runtime process found via pgrep; rerun on Linux/KVM for an authoritative answer"
    exit 0
  fi
  ps_out=$(ps -Eww -p "$pid" 2>/dev/null || true)
  if [ -z "$ps_out" ]; then
    echo "PASS: $PROBE — inconclusive on darwin: 'ps -Eww -p $pid' returned nothing; macOS refused to show this process's environment even at the same uid — rerun on Linux/KVM for an authoritative answer"
    exit 0
  fi
  if printf '%s' "$ps_out" | grep -q 'KRAYT_CANARY='; then
    echo "PASS: $PROBE — KRAYT_CANARY IS visible in the 'msb sandbox' runtime's environment on darwin (pid $pid, ps -Eww) — informative only, the Linux /proc form is authoritative"
  elif [ "$control_shows_env" = yes ]; then
    echo "PASS: $PROBE — KRAYT_CANARY is ABSENT from the long-lived 'msb sandbox' runtime's environment on darwin (pid $pid), and the reading carries weight here: a positive control in the same run proved 'ps -Eww' CAN read a plain same-uid child's environment on this machine. This matches msb's source — the resolved value reaches the runtime on an anonymous --config-fd, never its environ (sdk/rust/lib/runtime/spawn.rs, write_launch_config_fd) — so the environ window is only the process krayt invokes with the value in cmd.Env. The one remaining alternative is macOS withholding a hardened-runtime binary's environment; Linux/KVM settles that"
  else
    echo "PASS: $PROBE — inconclusive on darwin: 'ps -Eww -p $pid' did not show KRAYT_CANARY, AND a positive control proved 'ps -Eww' cannot read even a plain same-uid child's environment on this machine — the absence establishes nothing in either direction. Rerun on Linux/KVM for an authoritative answer"
  fi
  exit 0
fi

fail "unsupported OS '$os' — this probe only handles Linux (/proc) and Darwin (ps -Eww)"
