#!/bin/sh
# krayt-ask-probe: exercises the krayt-ask CLI front-end (§6.13) end to end. It shells out to the
# `krayt-ask` binary krayt mounts onto the PATH, submits one question, and writes the answer into
# /workspace so it appears in changes.patch. Runs non-root, so a success also proves the non-root
# socket-connect + workspace-write fixes. Distinct exit codes make a break obvious in `krayt ls`.
set -eu

echo "[krayt-ask-probe] start (uid=$(id -u))"

if ! command -v krayt-ask >/dev/null 2>&1; then
  echo "[krayt-ask-probe] krayt-ask not on PATH — runner didn't mount it, or the image lacks /usr/local/bin on PATH" >&2
  exit 10
fi

echo "[krayt-ask-probe] asking the human…"
if ans=$(krayt-ask --choices yes,no "krayt-ask-probe: proceed?"); then
  echo "[krayt-ask-probe] got answer: $ans"
  printf '%s\n' "$ans" > /workspace/krayt-ask-decision.txt
  echo "[krayt-ask-probe] wrote /workspace/krayt-ask-decision.txt"
else
  # Non-zero from krayt-ask = the no-answer sentinel (fail mode / timeout) — the agent falls back.
  echo "[krayt-ask-probe] no answer (sentinel) — proceeding autonomously"
  printf 'no-answer-sentinel\n' > /workspace/krayt-ask-decision.txt
fi

echo "[krayt-ask-probe] done"
