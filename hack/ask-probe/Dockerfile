# syntax=docker/dockerfile:1@sha256:87999aa3d42bdc6bea60565083ee17e86d1f3339802f543c0d03998580f9cb89 # renovate: ignore
# Throwaway agent image that exercises the §6.13 question channel on hardware.
# Static binary on scratch -> no libc/base-image variables, fewest unknowns.
FROM golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /ask-probe .

FROM scratch
COPY --from=build /ask-probe /ask-probe
ENTRYPOINT ["/ask-probe"]
