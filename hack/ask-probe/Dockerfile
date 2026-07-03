# syntax=docker/dockerfile:1
# Throwaway agent image that exercises the §6.13 question channel on hardware.
# Static binary on scratch -> no libc/base-image variables, fewest unknowns.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /ask-probe .

FROM scratch
COPY --from=build /ask-probe /ask-probe
ENTRYPOINT ["/ask-probe"]
