#!/bin/sh
# secrets-probe: leaks its mounted credential three ways so TestSecretConfinementInArtifacts
# (internal/orchestrator/integration_test.go) can prove the §6.8/§10 confinement fix on real
# hardware — redaction previously covered only the live log, not report.md/questions/changes.patch.
#
# Reads $K from /run/secrets/ANTHROPIC_API_KEY, then:
#   1. writes /output/report.md containing $K            (must come back redacted)
#   2. writes a tracked /workspace file containing $K     (must stay byte-exact in changes.patch,
#                                                            but flagged in Safety)
#   3. asks via krayt-ask with $K in the prompt + a choice (prompt/choices must come back redacted
#                                                            in questions/<id>.json)
# Exits 0 regardless of the answer — this probe is about what leaks, not the decision.
set -eu

echo "[secrets-probe] start (uid=$(id -u))"

SECRET_FILE=/run/secrets/ANTHROPIC_API_KEY
if [ ! -r "$SECRET_FILE" ]; then
  echo "[secrets-probe] $SECRET_FILE missing or unreadable — not mounted as expected" >&2
  exit 10
fi
K=$(cat "$SECRET_FILE")

printf 'Authenticate with the key %s before running.\n' "$K" > /output/report.md
echo "[secrets-probe] wrote /output/report.md"

printf 'api_key=%s\n' "$K" > /workspace/config.txt
echo "[secrets-probe] wrote /workspace/config.txt"

if ! command -v krayt-ask >/dev/null 2>&1; then
  echo "[secrets-probe] krayt-ask not on PATH — runner didn't mount it, or the image lacks /usr/local/bin on PATH" >&2
  exit 11
fi

echo "[secrets-probe] asking the human…"
if ans=$(krayt-ask --choices "use $K,skip" "Use the key $K?"); then
  echo "[secrets-probe] got answer: $ans"
else
  echo "[secrets-probe] no answer (sentinel/timeout) — proceeding anyway"
fi

echo "[secrets-probe] done"
