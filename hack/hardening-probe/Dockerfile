# syntax=docker/dockerfile:1 # renovate: ignore
# Throwaway agent image that proves the least-privilege container OCI spec (§6.10, §10,
# security-review findings #1/#3) on real hardware. Static binary on scratch, run as a numeric
# non-root uid -> no libc/base-image variables, fewest unknowns (same shape as ../ask-probe).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
# TARGETARCH, not a hardcoded arch: krayt has two VM backends and therefore two guest arches —
# arm64 under vfkit on Apple Silicon, amd64 under firecracker on Linux/KVM (§6.3). This was
# pinned to arm64 while macOS was the only backend, which silently produced an arm64 binary
# inside an amd64 image (an exec-format failure at run time, not a build failure).
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -o /hardening-probe .

FROM scratch
COPY --from=build /hardening-probe /hardening-probe
# Numeric uid: scratch has no /etc/passwd, and none is needed — the probe only calls
# os.Getuid()/syscall.Setuid(), which need no NSS lookup. This is also the KRAYT_HARDENING_IMAGE
# contract (HUMAN_TODO.md): a non-root image.
USER 1000:1000
ENTRYPOINT ["/hardening-probe"]
