# syntax=docker/dockerfile:1@sha256:87999aa3d42bdc6bea60565083ee17e86d1f3339802f543c0d03998580f9cb89 # renovate: ignore
# Exercises the krayt-ask CLI front-end (§6.13) on hardware: a NON-ROOT image that shells out to
# the `krayt-ask` binary krayt bind-mounts onto the PATH (/usr/local/bin/krayt-ask), proving both
# the binary round-trip and the non-root socket-connect fix. Alpine gives a small image with a
# shell + coreutils.
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# Non-root user (uid 1000) — also validates that a non-root agent can reach the ask socket.
RUN adduser -D -u 1000 agent

COPY --chmod=0755 entrypoint.sh /usr/local/bin/krayt-ask-probe

USER agent
ENTRYPOINT ["krayt-ask-probe"]
