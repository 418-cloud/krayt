#!/usr/bin/env bash
# Exercises the credential-detection block of every agent image's entrypoint (§6.14, §8.2) — the
# seam no Go test covers, because these are shell scripts that only ever run inside a container.
# krayt-dev has no entrypoint of its own (it inherits krayt-agent-claude-code's), so the branches
# it depends on are exercised here too, at the bottom.
#
# It exists because that gap hid a real bug: krayt's shape-translation path configures the container
# with a credential env var and no /run/secrets file, and every entrypoint decided "do I have a
# credential?" by looking only for the FILE (or KRAYT_INJECTED_CREDENTIAL). Every such run would
# have exited 78 before the agent started, and every Go test still passed, because they assert the
# host side (spec.Env) and nothing executed the script.
#
# Runs offline on any machine with bash: the agent CLI, git, and the report directory are all
# stubbed, and the entrypoint's own KRAYT_SECRETS_DIR/KRAYT_WORKSPACE/KRAYT_TASK/KRAYT_OUTPUT
# overrides do the rest. No Docker, no VM, no network.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pass=0 fail=0

ok()  { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail + 1)); }

# run_entrypoint <image> <secrets-dir> [env assignments...]
# Echoes the entrypoint's stdout+stderr; returns its exit code.
run_entrypoint() {
  local image="$1" secrets="$2"; shift 2
  local sandbox stub
  # Fail loudly rather than silently degrading to paths like /bin/claude if TMPDIR is unwritable.
  # An explicit template is required, not stylistic: BSD mktemp (macOS) ignores TMPDIR when given
  # no template and always uses the Darwin per-user temp dir, which sandboxed environments may
  # refuse to write to. Rooting the template at TMPDIR makes both BSD and GNU mktemp agree.
  sandbox="$(mktemp -d "${TMPDIR:-/tmp}/krayt-entrypoint-test.XXXXXX")" || {
    echo "mktemp -d failed (TMPDIR=${TMPDIR:-unset} not writable?)" >&2; return 70; }
  stub="$sandbox/bin"
  mkdir -p "$stub" "$sandbox/workspace" "$sandbox/output" "$sandbox/task"
  echo "do nothing" > "$sandbox/task/prompt.md"

  # Stub every command the entrypoint invokes after the credential block. Each echoes the
  # credential it was handed via the environment, which is how the assertions below see it.
  for cmd in claude gemini opencode git node npm gh; do
    cat > "$stub/$cmd" <<STUB
#!/usr/bin/env bash
echo "STUB:$cmd args=[\$*] ANTHROPIC_API_KEY=\${ANTHROPIC_API_KEY:-} CLAUDE_CODE_OAUTH_TOKEN=\${CLAUDE_CODE_OAUTH_TOKEN:-} GEMINI_API_KEY=\${GEMINI_API_KEY:-} OPENAI_API_KEY=\${OPENAI_API_KEY:-}"
exit 0
STUB
    chmod +x "$stub/$cmd"
  done

  env -i \
    PATH="$stub:/usr/bin:/bin" HOME="$sandbox" \
    KRAYT_SECRETS_DIR="$secrets" KRAYT_WORKSPACE="$sandbox/workspace" \
    KRAYT_TASK="$sandbox/task/prompt.md" KRAYT_OUTPUT="$sandbox/output" \
    "$@" \
    bash "$REPO/images/agents/$image/entrypoint.sh" 2>&1
  local rc=$?
  rm -rf "$sandbox"
  return $rc
}

# check <description> <expected-substring> <image> <secrets-dir> [env...]
check() {
  local desc="$1" want="$2"; shift 2
  local out; out="$(run_entrypoint "$@")"
  if [[ "$out" == *"$want"* ]]; then ok "$desc"; else bad "$desc"$'\n        want substring: '"$want"$'\n        got: '"${out//$'\n'/ | }"; fi
}

# check_exit <description> <expected-code> <image> <secrets-dir> [env...]
check_exit() {
  local desc="$1" want="$2"; shift 2
  run_entrypoint "$@" >/dev/null 2>&1
  local rc=$?
  if [ "$rc" -eq "$want" ]; then ok "$desc"; else bad "$desc (exit $rc, want $want)"; fi
}

empty_secrets="$(mktemp -d "${TMPDIR:-/tmp}/krayt-entrypoint-secrets.XXXXXX")" || exit 70
file_secrets="$(mktemp -d "${TMPDIR:-/tmp}/krayt-entrypoint-secrets.XXXXXX")" || exit 70
printf 'sk-ant-real-key-from-the-secrets-tmpfs' > "$file_secrets/ANTHROPIC_API_KEY"
trap 'rm -rf "$empty_secrets" "$file_secrets"' EXIT

printf '\n\033[1mclaude-code\033[0m\n'
check "1. a /run/secrets file is read and exported" \
  "ANTHROPIC_API_KEY=sk-ant-real-key-from-the-secrets-tmpfs" \
  claude-code "$file_secrets"

check "2. an ALREADY-SET credential env var satisfies the check, value untouched" \
  "ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use" \
  claude-code "$empty_secrets" ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use

check "3. shape mirroring: an already-set OAuth var is accepted as the credential" \
  "authenticated via CLAUDE_CODE_OAUTH_TOKEN" \
  claude-code "$empty_secrets" CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-krayt-placeholder-do-not-use

check "4. the OAuth placeholder reaches the agent verbatim" \
  "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-krayt-placeholder-do-not-use" \
  claude-code "$empty_secrets" CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-krayt-placeholder-do-not-use

check "5. KRAYT_INJECTED_CREDENTIAL still works (pre-shape-translation krayt)" \
  "ANTHROPIC_API_KEY=krayt-injected-at-host-proxy" \
  claude-code "$empty_secrets" KRAYT_INJECTED_CREDENTIAL=ANTHROPIC_API_KEY

check "6. an already-set value WINS over the KRAYT_INJECTED_CREDENTIAL fallback" \
  "ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use" \
  claude-code "$empty_secrets" ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use KRAYT_INJECTED_CREDENTIAL=ANTHROPIC_API_KEY

check_exit "7. no credential anywhere still fails closed with EX_CONFIG" 78 \
  claude-code "$empty_secrets"

printf '\n\033[1mgemini-cli\033[0m\n'
check "8. an already-set GEMINI_API_KEY satisfies the check" \
  "STUB:gemini" \
  gemini-cli "$empty_secrets" GEMINI_API_KEY=krayt-placeholder-do-not-use GEMINI_CLI_TRUST_WORKSPACE=true
check_exit "9. no credential fails closed" 78 gemini-cli "$empty_secrets"

printf '\n\033[1mopencode\033[0m\n'
check "10. an already-set ANTHROPIC_API_KEY satisfies the check" \
  "STUB:opencode" \
  opencode "$empty_secrets" ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use
check_exit "11. no credential fails closed" 78 opencode "$empty_secrets"

printf '\n\033[1mkrayt-dev\033[0m (it has no entrypoint — these are the base branches it relies on)\n'

# krayt-dev sets CLAUDE_MODEL/CLAUDE_EFFORT as image ENV and krayt.yaml's `env:` overrides them per
# run; the flags themselves are the base's to pass, since it is the image that ships `claude`.
check "12. CLAUDE_MODEL and CLAUDE_EFFORT become --model/--effort" \
  "args=[-p do nothing --dangerously-skip-permissions --model claude-sonnet-5 --effort high]" \
  claude-code "$empty_secrets" ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use \
  CLAUDE_MODEL=claude-sonnet-5 CLAUDE_EFFORT=high

# Unset, no flag at all — krayt-agent-claude-code's own default, and what keeps that branch
# invisible to every user of the published onboarding image.
check "13. neither var set passes no model/effort flag" \
  "args=[-p do nothing --dangerously-skip-permissions]" \
  claude-code "$empty_secrets" ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use

check "14. either var alone passes only its own flag" \
  "args=[-p do nothing --dangerously-skip-permissions --effort max]" \
  claude-code "$empty_secrets" ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use CLAUDE_EFFORT=max

# No entrypoint touches GH_TOKEN anywhere, on purpose: krayt.yaml injects it at the host proxy, so
# `gh` reads the placeholder straight from the environment and the proxy swaps in the real token.
# A `gh auth login` creeping into an entrypoint would be a regression — it would make a live
# api.github.com call, and on the base image (which ships no gh) it could not even run. This is the
# guard. `gh` IS stubbed on PATH here, so a `command -v gh` branch would fire if one were added.
out="$(run_entrypoint claude-code "$empty_secrets" ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use GH_TOKEN=krayt-injected-at-host-proxy)"
if [[ "$out" == *"gh authenticating"* || "$out" == *"authenticated gh"* || "$out" == *"gh commands"* || "$out" == *"GH_TOKEN"* ]]; then
  bad "15. no entrypoint touches GH_TOKEN — the proxy owns it"$'\n        got: '"${out//$'\n'/ | }"
else
  ok "15. no entrypoint touches GH_TOKEN — the proxy owns it"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
exit $((fail > 0))
