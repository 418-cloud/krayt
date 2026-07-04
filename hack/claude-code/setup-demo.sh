#!/usr/bin/env bash
# Sets up a throwaway demo repo + task + secrets template for the claude-code test image, then
# prints the `krayt run` command. Run this on your Mac (it writes to /tmp, so not in a sandbox).
# Idempotent — safe to re-run. Usage: ./setup-demo.sh [demo-dir]
set -euo pipefail

IMAGE="${KRAYT_CLAUDE_IMAGE:-docker.io/tjololo/test-krayt:claude}"
DEMO="${1:-/tmp/claude-demo}"
HERE="$(cd "$(dirname "$0")" && pwd)"

mkdir -p "$DEMO"
cd "$DEMO"

# A tiny seed repo so Claude has a real (small) codebase to work in.
if [ ! -d .git ]; then
  git init -q
  git config user.email krayt-demo@example.com
  git config user.name "krayt demo"
fi
cat > main.py <<'PY'
def main():
    print("hi")


if __name__ == "__main__":
    main()
PY
cat > README.md <<'MD'
# claude-demo

A tiny throwaway project for exercising the krayt `claude-code` agent adapter.
MD
git add -A
git commit -qm "seed" || true # no-op on a re-run with no changes

# The task for the agent.
cp "$HERE/task.example.md" task.md

# Secrets template — EDIT with your real key. Exactly ONE credential (§6.14).
if [ ! -f secrets.env ]; then
  printf 'ANTHROPIC_API_KEY=REPLACE_WITH_YOUR_KEY\n' > secrets.env
  chmod 600 secrets.env
fi

cat <<EOF

──────────────────────────────────────────────────────────────────────────
Test env ready in: $DEMO

1) Put your real key in the secrets file (exactly ONE of
   ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN):

     \$EDITOR $DEMO/secrets.env

2) Run it (foreground — streams Claude's output):

     krayt run \\
       --image $IMAGE \\
       --agent claude-code \\
       --secrets $DEMO/secrets.env \\
       --task $DEMO/task.md --repo $DEMO \\
       --allow api.anthropic.com

   …or "park and walk away" in the background: add --detach

3) Review the result:

     krayt ls --repo $DEMO
     cat  $DEMO/.krayt/runs/<id>/report.md
     krayt patch <id> --repo $DEMO
──────────────────────────────────────────────────────────────────────────
EOF
