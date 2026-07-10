# syntax=docker/dockerfile:1 # renovate: ignore
# Exercises the §6.8/§10 secret-confinement fix on hardware (TestSecretConfinementInArtifacts):
# a NON-ROOT image whose entrypoint reads its mounted credential from /run/secrets and then
# (carelessly) sprays it into three artifacts — /output/report.md, a tracked /workspace file, and
# an ask_human prompt/choice — so the test can assert each is redacted (or, for changes.patch,
# left byte-exact but flagged) on the host. Alpine gives a small image with a shell + coreutils
# (same shape as ../krayt-ask-probe).
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# Non-root user (uid 1000) — matches the §8.2 non-root contract the other probes validate.
RUN adduser -D -u 1000 agent

COPY --chmod=0755 entrypoint.sh /usr/local/bin/secrets-probe

USER agent
ENTRYPOINT ["secrets-probe"]
