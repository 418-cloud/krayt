# syntax=docker/dockerfile:1 # renovate: ignore
# Throwaway agent image that proves the §6.6 egress control (L7 allowlist + the nftables L3 lock)
# on real hardware — the probe behind TestEgressEnforcement (KRAYT_NETPROBE_IMAGE), and the
# on-hardware regression for security-review finding #1. Static binary on scratch, run as a
# numeric non-root uid -> no libc/base-image variables (same shape as ../hardening-probe).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
# TARGETARCH, not a hardcoded arch: krayt has two guest arches (arm64/vfkit, amd64/firecracker).
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -o /netprobe .

FROM scratch
# The allowlisted check does real TLS to a public host through the proxy's CONNECT tunnel, so the
# image needs a trust store; scratch has none.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /netprobe /netprobe

# The host the run's network policy is expected to allowlist. It is baked in because the run does
# not forward the policy to the container — TestEgressEnforcement puts KRAYT_ALLOW_HOST into the
# policy, and this value must MATCH it, or the probe will (correctly) report the allowlisted host
# as unreachable.
ENV KRAYT_ALLOW_HOST=example.com
ENV KRAYT_DENY_HOST=www.wikipedia.org

# Numeric uid: scratch has no /etc/passwd and none is needed. Non-root is also the krayt container
# contract (§8.2) — a root image fails the run outright.
USER 1000:1000
ENTRYPOINT ["/netprobe"]
