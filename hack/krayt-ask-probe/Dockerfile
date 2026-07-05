# syntax=docker/dockerfile:1 # renovate: ignore
# Exercises the krayt-ask CLI front-end (§6.13) on hardware: a NON-ROOT image that shells out to
# the `krayt-ask` binary krayt bind-mounts onto the PATH (/usr/local/bin/krayt-ask), proving both
# the binary round-trip and the non-root socket-connect fix. Alpine gives a small image with a
# shell + coreutils.
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

# Non-root user (uid 1000) — also validates that a non-root agent can reach the ask socket.
RUN adduser -D -u 1000 agent

COPY --chmod=0755 entrypoint.sh /usr/local/bin/krayt-ask-probe

USER agent
ENTRYPOINT ["krayt-ask-probe"]
