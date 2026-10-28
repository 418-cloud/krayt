# syntax=docker/dockerfile:1 # renovate: ignore
# Exercises the krayt-ask CLI front-end (§6.13) on hardware: a NON-ROOT image that shells out to
# the `krayt-ask` binary krayt bind-mounts onto the PATH (/usr/local/bin/krayt-ask), proving both
# the binary round-trip and the non-root socket-connect fix. Alpine gives a small image with a
# shell + coreutils.
FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

# Non-root user (uid 1000) — also validates that a non-root agent can reach the ask socket.
RUN adduser -D -u 1000 agent

COPY --chmod=0755 entrypoint.sh /usr/local/bin/krayt-ask-probe

USER agent
ENTRYPOINT ["krayt-ask-probe"]
