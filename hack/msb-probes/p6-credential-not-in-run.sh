#!/bin/sh
# P6 — during a real `krayt run`, does the credential stay out of everything but `msb create`?
# (non-blocking; needs a live credential — closes HUMAN_TODO.md's last msb hardware item)
#
# HUMAN_TODO.md's `[hardware] run-tasks-on-microsandbox.md` entry, criterion 3. Unlike P1–P5 this
# probe does not build a synthetic sandbox: it drives a REAL `krayt run` and inspects it, because
# the claim under test is about krayt's own invocation of msb, not about msb in isolation. What
# `internal/sandbox/msb_test.go` already asserts offline — that only `Client.Create` may carry
# extra env, structurally — this checks against the real binaries on real hardware.
#
# THE OBSERVATION WINDOW. `msb create` boots the sandbox in the background and exits, so its
# environ cannot be caught reliably by polling. A `--on-question=wait` run solves this: it parks
# in `waiting` with the sandbox alive, `msb exec` streaming, and the agent blocked in ask_human,
# for as long as the probe needs.
#
# THE POSITIVE HALF IS ALREADY PROVEN BY THE RUN ITSELF. Criterion 3 asks that the value be in
# `msb create`'s environ and nowhere else. Reaching `waiting` REQUIRES the agent to have made a
# successful authenticated call to the model API — the question it asked is the evidence — which
# can only happen if the real value reached msb. So this probe does not chase the short-lived
# create process; it establishes the negatives, which are the security-relevant half.
#
# FOUR READINGS, THREE OF THEM AUTHORITATIVE ON BOTH PLATFORMS:
#   1. argv of every live msb process        — authoritative (argv is always readable)
#   2. environ of every live msb process     — control-gated, see below
#   3. the guest's own env for the key       — authoritative (`msb exec printenv`)
#   4. every file under the run directory    — authoritative (patch, report, meta, console.log)
#
# WHY READING 2 IS CONTROL-GATED. macOS may refuse to show another process's environment even at
# the same uid, exactly as P4 found — so "the value was not in the output" is unreadable on its
# own: a genuinely clean environ and an environ macOS declines to print look identical. This probe
# reuses P4's fix, a same-uid control process carrying a known marker, and reports reading 2 as
# INCONCLUSIVE rather than clean whenever the control proves the method cannot see environs here.
# An inconclusive reading 2 does not fail the probe — readings 1, 3 and 4 stand on their own — but
# it is named in the PASS line, never silently folded in.
#
# THE VALUE NEVER TOUCHES THIS SCRIPT'S OWN argv OR ENVIRONMENT. It is copied from the secrets
# file into a mode-0600 temp file and every comparison is `grep -Ff` against that file. Writing it
# as `grep -F "$VALUE"` would put the credential into grep's own argv — visible in `ps` to exactly
# the observer this probe is playing, and it would match itself and report a false finding.
#
# Usage: p6-credential-not-in-run.sh
#   KRAYT_IMAGE    agent image (default ghcr.io/418-cloud/krayt-agent-claude-code:latest)
#   KRAYT_SECRETS  path to a real secrets.env  (required)
#   KRAYT_PROBE_SECRET_KEY  which key to track (default: the file's first key)
#   KRAYT_BIN      krayt binary (default ./bin/krayt, else PATH)
#
# PASS: the run authenticated (so the credential reached msb), and the value appears in no live
# msb process's argv, no guest environment, and no run artifact.
# FAIL: any hit — each names exactly where the value surfaced and what that breaks — or the probe
# could not establish the window (no run, never reached waiting, no msb processes to inspect).
set -eu

PROBE=p6-credential-not-in-run
IMAGE=${KRAYT_IMAGE:-ghcr.io/418-cloud/krayt-agent-claude-code:latest}
WAIT_TIMEOUT=${KRAYT_PROBE_WAIT_TIMEOUT:-300}

scratch=
valfile=
runid=
control_pid=

cleanup() {
  if [ -n "$runid" ] && [ -n "$scratch" ] && [ -n "${KRAYT:-}" ]; then
    "$KRAYT" stop --repo "$scratch" "$runid" >/dev/null 2>&1 || true
  fi
  [ -n "$valfile" ] && rm -f "$valfile"
  [ -n "$scratch" ] && rm -rf "$scratch"
  [ -n "$control_pid" ] && kill "$control_pid" >/dev/null 2>&1
  return 0
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $PROBE — $1"
  exit 1
}

command -v msb >/dev/null 2>&1 \
  || fail "msb not on PATH — install with: curl -fsSL https://install.microsandbox.dev | sh"

KRAYT=${KRAYT_BIN:-}
if [ -z "$KRAYT" ]; then
  if [ -x ./bin/krayt ]; then KRAYT=$(cd "$(dirname ./bin/krayt)" && pwd)/krayt
  elif command -v krayt >/dev/null 2>&1; then KRAYT=$(command -v krayt)
  else fail "no krayt binary — run 'make build' first, or set KRAYT_BIN"; fi
fi

[ -n "${KRAYT_SECRETS:-}" ] || fail "KRAYT_SECRETS is unset — this probe needs a real secrets file; nothing here can fabricate a credential"
[ -r "$KRAYT_SECRETS" ] || fail "KRAYT_SECRETS ($KRAYT_SECRETS) is not readable"

echo "msb version: $(msb --version)"
echo "[$PROBE] krayt: $KRAYT"

# --- the tracked key, and its value in a file rather than a variable on any argv ---

KEY=${KRAYT_PROBE_SECRET_KEY:-}
if [ -z "$KEY" ]; then
  KEY=$(grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "$KRAYT_SECRETS" | head -n1 | cut -d= -f1 || true)
fi
[ -n "$KEY" ] || fail "could not find a KEY=VALUE line in $KRAYT_SECRETS (set KRAYT_PROBE_SECRET_KEY)"

valfile=$(mktemp) || fail "mktemp failed"
chmod 600 "$valfile"
grep -E "^$KEY=" "$KRAYT_SECRETS" | head -n1 | cut -d= -f2- | tr -d '\r\n' >"$valfile"
vallen=$(wc -c <"$valfile" | tr -d ' ')
[ "$vallen" -ge 12 ] \
  || fail "the value of $KEY is only $vallen bytes — too short to search for without matching unrelated text everywhere; a probe that cannot tell a hit from a coincidence proves nothing"
echo "[$PROBE] tracking key '$KEY' ($vallen-byte value, never printed)"

# grep -Ff so the pattern lives in a file, never in argv. Returns 0 when the value is FOUND.
value_in_stdin() { grep -qFf "$valfile"; }
value_in_path()  { grep -rqFf "$valfile" "$1" 2>/dev/null; }

# --- a scratch repo and a parked run ---

scratch=$(mktemp -d) || fail "mktemp -d failed"
repo=$scratch/repo
mkdir -p "$repo"
printf '# scratch\n\nThrowaway repo for %s.\n' "$PROBE" >"$repo/README.md"
git -C "$repo" init -q
git -C "$repo" add -A
git -C "$repo" -c user.name='krayt probe' -c user.email='probe@example.invalid' commit -qm init

echo "[$PROBE] starting a --on-question=wait run (this makes a real, billed model call)…"
start_out=$(printf '%s\n' "Before you change anything, you MUST call the ask_human tool to ask whether you should add a 'Status' section to README.md. Do not answer that question yourself and do not edit any file until the human replies." |
  "$KRAYT" run --repo "$repo" --image "$IMAGE" --task - --secrets "$KRAYT_SECRETS" \
    --agent claude-code --allow api.anthropic.com --on-question wait --detach 2>&1) \
  || fail "krayt run failed to start: $start_out"

runid=$(printf '%s\n' "$start_out" | sed -n 's/^run \(run_[A-Za-z0-9]*\) started.*/\1/p' | head -n1)
[ -n "$runid" ] || fail "could not parse a run id out of krayt's output: $start_out"
rundir=$repo/.krayt/runs/$runid
echo "[$PROBE] run $runid — waiting up to ${WAIT_TIMEOUT}s for state 'waiting'…"

run_state() {
  [ -r "$rundir/meta.json" ] || { printf 'unknown'; return 0; }
  grep -o '"state"[[:space:]]*:[[:space:]]*"[^"]*"' "$rundir/meta.json" |
    head -n1 | sed 's/.*"\([^"]*\)"$/\1/'
}

waited=0
while [ "$waited" -lt "$WAIT_TIMEOUT" ]; do
  state=$(run_state)
  case "$state" in
  waiting) break ;;
  failed | done)
    err=$(grep -o '"error"[[:space:]]*:[[:space:]]*"[^"]*"' "$rundir/meta.json" 2>/dev/null | head -n1 || true)
    fail "the run reached '$state' without ever parking in 'waiting' — the agent never asked, so there is no window to inspect and nothing was measured. ${err:-(no error recorded)}"
    ;;
  esac
  sleep 2
  waited=$((waited + 2))
done
[ "$(run_state)" = waiting ] \
  || fail "the run did not reach 'waiting' within ${WAIT_TIMEOUT}s (state '$(run_state)') — no observation window; raise KRAYT_PROBE_WAIT_TIMEOUT or check the run's logs under $rundir"

echo "[$PROBE] parked in 'waiting' — the agent authenticated and asked a question, so the credential DID reach msb create."

# --- reading 1: argv of every live msb process (authoritative) ---

# Every live process whose executable is msb — the create invocation (if still up), the per-sandbox
# runtime, and the exec streaming the agent. `pgrep -x` matches the process name exactly, so it
# cannot pick up this probe or the krayt supervisor (which legitimately holds the value: it read
# the secrets file). The ps form is the fallback where pgrep is unavailable.
# Exit status distinguishes the three outcomes that must never be conflated: 0 = pids on stdout,
# 1 = the host enumerated fine and nothing matched, 2 = the host would not let us list processes
# at all. That last case is not hypothetical — a sandboxed shell returns "pgrep: Cannot get
# process list" (exit 3), and with stderr discarded that is indistinguishable from "no msb is
# running". Reporting readings 1 and 2 as clean off an empty list nobody was allowed to build is
# the same false pass the environ control below exists to prevent, so it is a hard failure here.
msb_pids() {
  if command -v pgrep >/dev/null 2>&1; then
    out=$(pgrep -x msb 2>/dev/null)
    case $? in
    0) printf '%s\n' "$out"; return 0 ;;
    1) return 1 ;;
    *) return 2 ;;
    esac
  fi
  out=$(ps -Ao pid=,comm= 2>/dev/null | awk '{ n = split($2, p, "/"); if (p[n] == "msb") print $1 }') \
    || return 2
  [ -n "$out" ] || return 1
  printf '%s\n' "$out"
}

pids=$(msb_pids) || pid_rc=$?
pid_rc=${pid_rc:-0}
case "$pid_rc" in
2) fail "this host will not let the probe list processes (pgrep/ps could not enumerate) — readings 1 and 2 are about process argv and environ, so criterion 3 cannot be established here at all. An empty process list nobody was permitted to build is not evidence of a clean one. Re-run from a shell that can see the process table" ;;
1) fail "no live 'msb' process while the run is parked in 'waiting' — the process table WAS readable, so this means msb's process naming changed since 0.6.16 or the sandbox is not actually running; either way readings 1 and 2 would be vacuously clean" ;;
esac
pids=$(printf '%s' "$pids" | tr '\n' ' ' | sed 's/ *$//')
echo "[$PROBE] live msb pids: $pids"

argv_hit=
for p in $pids; do
  if ps -ww -o args= -p "$p" 2>/dev/null | value_in_stdin; then argv_hit="$argv_hit $p"; fi
done
[ -z "$argv_hit" ] \
  || fail "the credential value appears in the ARGV of msb process(es)$argv_hit — argv is world-readable via ps, so this is a live credential disclosure to every process on the host. krayt must pass secret values only in the child's environment (internal/sandbox/msb.go's commandWithEnv/SecretEnv); find what put it on the command line"
echo "[$PROBE] reading 1 — argv of $(printf '%s' "$pids" | wc -w | tr -d ' ') msb process(es): clean [authoritative]"

# --- reading 2: environ of every live msb process (control-gated, see the header) ---

os=$(uname -s)
env_verdict=inconclusive
env_hit=

read_environ() {
  if [ "$os" = Linux ] && [ -r "/proc/$1/environ" ]; then
    tr '\0' '\n' <"/proc/$1/environ"
  else
    ps -Eww -p "$1" 2>/dev/null || true
  fi
}

# Can this method read a plain same-uid child's environment at all here? Without this, an empty
# result is unreadable — P4 hit exactly that on darwin.
KRAYT_P6_CONTROL=control-canary-visible
export KRAYT_P6_CONTROL
sleep 30 &
control_pid=$!
control_can_read=no
if read_environ "$control_pid" | grep -q 'KRAYT_P6_CONTROL='; then control_can_read=yes; fi
echo "[$PROBE] control: can this host's method read a same-uid child's environ? $control_can_read"

if [ "$control_can_read" = yes ]; then
  for p in $pids; do
    if read_environ "$p" | value_in_stdin; then env_hit="$env_hit $p"; fi
  done
  if [ -n "$env_hit" ]; then
    env_verdict=hit
  else
    env_verdict=clean
  fi
fi

case "$env_verdict" in
hit)
  fail "the credential value is in the ENVIRON of live msb process(es)$env_hit while the run is parked. Only the short-lived 'msb create' invocation may carry it (hand-secrets-to-msb.md's Timing rule); a long-lived process holding it widens the exposure window to the whole run, readable by any same-uid process. Identify which msb process this is (ps -o args= -p <pid>) — if it is 'msb exec', krayt is handing secretEnv to a method that must never receive it"
  ;;
clean)   echo "[$PROBE] reading 2 — environ of every live msb process: clean [authoritative here: control proved the method works]" ;;
*)       echo "[$PROBE] reading 2 — environ: INCONCLUSIVE (this host will not show another process's environ; see P4) — not counted either way" ;;
esac

# --- reading 3: the guest's own environment (authoritative) ---

sandbox=krayt-$runid
guest_verdict=
if guest_env=$(MSB_BACKEND=local msb exec --no-tty "$sandbox" -- printenv "$KEY" 2>/dev/null); then
  if printf '%s' "$guest_env" | value_in_stdin; then
    fail "the guest's \$$KEY holds the REAL credential value. This breaks the boundary the whole msb cutover rests on — under B1 the sandbox must only ever see msb's placeholder (\$MSB_$KEY), with substitution happening host-side. Anything running in that sandbox, including the agent, can now read and exfiltrate the credential"
  fi
  guest_verdict="holds $(printf '%s' "$guest_env" | tr -d '\r' | cut -c1-40) — not the real value"
else
  guest_verdict="unset in the guest"
fi
echo "[$PROBE] reading 3 — guest \$$KEY: $guest_verdict [authoritative]"

# --- reading 4: everything the run wrote to disk (authoritative) ---

if value_in_path "$rundir"; then
  fail "the credential value appears in a file under $rundir — krayt's own artifacts. Secret redaction (internal/secrets.Redactor, applied to console.log and question records) and the host-side patch scan are supposed to make this impossible; find which file and which path wrote it unredacted"
fi
echo "[$PROBE] reading 4 — every file under the run dir (patch/report/meta/logs): clean [authoritative]"

# --- let the run finish so changes.patch exists, then check that too ---

echo "[$PROBE] answering the question so the run completes…"
"$KRAYT" answer --repo "$repo" "$runid" yes >/dev/null 2>&1 \
  || echo "[$PROBE] (krayt answer failed; the run will be stopped on cleanup — readings above still stand)" >&2

waited=0
while [ "$waited" -lt 180 ]; do
  case "$(run_state)" in done | failed) break ;; esac
  sleep 2
  waited=$((waited + 2))
done

if [ -f "$rundir/changes.patch" ]; then
  if value_in_path "$rundir/changes.patch"; then
    fail "the credential value appears in changes.patch — it would be applied straight into the user's repo, and from there very likely committed and pushed. The host-side PatchSecretKeys scan (§6.8) should have caught this"
  fi
  echo "[$PROBE] post-run — changes.patch ($(wc -c <"$rundir/changes.patch" | tr -d ' ') bytes): clean [authoritative]"
fi
runid= # completed; nothing for cleanup to stop

# --- verdict ---

case "$env_verdict" in
clean)
  echo "PASS: $PROBE — the run authenticated and parked in 'waiting' (so the credential reached 'msb create'), and the value appears NOWHERE else: not in any live msb process's argv or environ, not in the guest's own \$$KEY, not in any run artifact, not in changes.patch. All four readings authoritative on this host. Criterion 3 of HUMAN_TODO.md's run-tasks-on-microsandbox.md hardware entry is met"
  ;;
*)
  echo "PASS: $PROBE — the run authenticated and parked in 'waiting' (so the credential reached 'msb create'), and the value appears in no live msb process's argv, no guest environment, no run artifact, and not in changes.patch. Reading 2 (msb process environs) was INCONCLUSIVE — this host will not show another process's environment even at the same uid, the same darwin limitation P4 records — so the environ window is unmeasured here, not proven clean; rerun on Linux/KVM to close it. The other three readings are authoritative and cover the disclosure paths that matter most (argv is world-readable, the guest is attacker-controlled, artifacts get committed)"
  ;;
esac
exit 0
