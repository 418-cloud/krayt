# syntax=docker/dockerfile:1 # renovate: ignore
# Throwaway agent image for TestGuestGitConfigInjectionInert (§6.7, §10 finding #2): a NON-ROOT
# image whose entrypoint writes a git-config/attributes injection into the writable
# /workspace/.git, attempting the container->guest-root escape that patch.SetupPatchGit's
# root-only patchgit snapshot is meant to neutralize. Alpine gives a small image with a shell +
# coreutils (same shape as ../krayt-ask-probe).
FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

# Non-root user (uid 1000) -- required by the KRAYT_GITCONFIG_IMAGE contract (HUMAN_TODO.md) and
# by withEnforceNonRoot (§8.2); the escape being probed is specifically
# container(non-root) -> guest-agent(root).
RUN adduser -D -u 1000 agent

COPY --chmod=0755 entrypoint.sh /usr/local/bin/gitconfig-probe

USER agent
ENTRYPOINT ["gitconfig-probe"]
