# syntax=docker/dockerfile:1 # renovate: ignore
# Throwaway agent image that proves the least-privilege container OCI spec (§6.10, §10,
# security-review findings #1/#3) on real hardware. Static binary on scratch, run as a numeric
# non-root uid -> no libc/base-image variables, fewest unknowns (same shape as ../ask-probe).
FROM golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /hardening-probe .

FROM scratch
COPY --from=build /hardening-probe /hardening-probe
# Numeric uid: scratch has no /etc/passwd, and none is needed — the probe only calls
# os.Getuid()/syscall.Setuid(), which need no NSS lookup. This is also the KRAYT_HARDENING_IMAGE
# contract (HUMAN_TODO.md): a non-root image.
USER 1000:1000
ENTRYPOINT ["/hardening-probe"]
