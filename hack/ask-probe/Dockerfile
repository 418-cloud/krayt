# syntax=docker/dockerfile:1 # renovate: ignore
# Throwaway agent image that exercises the §6.13 question channel on hardware.
# Static binary on scratch -> no libc/base-image variables, fewest unknowns.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
# TARGETARCH, not a hardcoded arch — see hack/hardening-probe/Dockerfile for the rationale:
# krayt now has two guest arches (arm64/vfkit, amd64/firecracker), and pinning arm64 here would
# put an arm64 binary inside an amd64 image, failing at exec rather than at build.
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -o /ask-probe .

FROM scratch
COPY --from=build /ask-probe /ask-probe
ENTRYPOINT ["/ask-probe"]
