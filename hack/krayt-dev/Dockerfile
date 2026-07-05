# syntax=docker/dockerfile:1
# krayt-dev — the dogfooding agent image: a non-root Claude Code agent (§6.14) plus krayt's
# own dev toolchain, so an agent running `krayt run --image krayt-dev --agent claude-code`
# against krayt's own repo can build, test, lint, and regenerate protocol code inside the
# sandbox before returning its patch (§6.14, §8.2).
#
# Debian bookworm (glibc, not Nix) so the official native `claude` binary runs; non-root
# throughout, per the §8.2 container contract.
FROM golang:1.26-bookworm

# Tool versions, pinned so Renovate's custom regex manager (renovate.json) can bump them
# independently of the base image tag. `make proto` (Nix) stays the source of truth for the
# *committed* generated code; this toolchain is only for an agent regenerating it mid-run
# (see proto-direct.sh) when it edits internal/protocol/krayt.proto.
ARG PROTOC_VERSION=35.1
ARG PROTOC_GEN_GO_VERSION=v1.36.11
ARG PROTOC_GEN_GO_GRPC_VERSION=v1.6.2
ARG BUF_VERSION=v1.71.0
ARG ORAS_VERSION=v1.3.2
ARG GOLANGCI_LINT_VERSION=v2.12.2

# git: repo ops + module fetches; curl + ca-certificates: installers/HTTPS; bash: entrypoint +
# proto-direct.sh; unzip: protoc's release archive is a zip, not a tarball.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git bash unzip \
 && rm -rf /var/lib/apt/lists/*

# protoc itself has no Go module (it's the C++ compiler), so it's the one tool fetched as a
# prebuilt release archive; TARGETARCH is set per-platform by buildx during the multi-arch build.
ARG TARGETARCH
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) protoc_arch=x86_64 ;; \
      arm64) protoc_arch=aarch_64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/protoc.zip \
      "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-${protoc_arch}.zip" \
 && unzip -q /tmp/protoc.zip -d /usr/local \
 && rm /tmp/protoc.zip \
 && chmod -R a+rX /usr/local/bin/protoc /usr/local/include

# Non-root agent user (uid 1000) — Claude Code refuses uid 0 (§6.14/§8.2). Everything below
# runs as this user so it owns its own Go/lint caches (see ENV block).
RUN useradd --create-home --uid 1000 --shell /bin/bash agent

COPY --chmod=0755 hack/krayt-dev/entrypoint.sh /usr/local/bin/krayt-dev-entrypoint

USER agent
WORKDIR /home/agent

# Agent-owned, writable caches so `go build`, `go test -race`, and `golangci-lint run` work
# under a non-root uid with no permission errors.
ENV GOPATH=/home/agent/go \
    GOCACHE=/home/agent/.cache/go-build \
    GOMODCACHE=/home/agent/go/pkg/mod \
    GOLANGCI_LINT_CACHE=/home/agent/.cache/golangci-lint \
    GOFLAGS=-mod=mod \
    PATH="/home/agent/go/bin:/home/agent/.local/bin:${PATH}"

# Official installer (native binary; the npm package is deprecated).
RUN curl -fsSL https://claude.ai/install.sh | bash

# Keep egress tight for the krayt allowlist (§6.6): no autoupdater, telemetry, or error
# reporting, so the container's steady-state destinations are the model API plus, only for new
# deps, the Go module proxy (see README).
ENV DISABLE_AUTOUPDATER=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

# krayt's dev toolchain, pinned by the ARGs above. All installable via `go install`, so one
# command works for both target architectures with no per-arch binary URLs. An agent-owned BuildKit
# cache mount on GOCACHE keeps the compiled objects across rebuilds, so bumping a tool version (or a
# weekly base rebuild) recompiles in seconds, not minutes; the tool binaries land in $GOPATH/bin
# (outside the mount), so they persist into the image.
#
# GOMODCACHE is deliberately NOT mounted: it lives under $GOPATH/pkg, where `go` also writes its
# sumdb cache as a *sibling* dir — a cache mount there leaves that parent root-owned, which the
# non-root `go install` can't write ("open .../pkg/sumdb/...: no such file or directory"). GOCACHE
# (the compile) is the dominant cost anyway.
RUN --mount=type=cache,target=/home/agent/.cache/go-build,uid=1000,gid=1000 \
    go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" \
 && go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}" \
 && go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}" \
 && go install "github.com/bufbuild/buf/cmd/buf@${BUF_VERSION}" \
 && go install "oras.land/oras/cmd/oras@${ORAS_VERSION}"

# Pre-bake krayt's module graph into GOMODCACHE so `go build ./...` / `go test -race ./...` work
# offline inside the sandbox's allowlisted egress (see README). Only manifests are copied, not
# source, so this layer only busts when dependencies change; the nested hack/ask-probe module is
# stdlib-only today but downloaded in step in case that changes.
COPY --chown=agent:agent go.mod go.sum ./krayt-src/
COPY --chown=agent:agent hack/ask-probe/go.mod ./krayt-src/hack/ask-probe/go.mod
RUN cd krayt-src && go mod download \
 && cd hack/ask-probe && go mod download \
 && cd /home/agent && rm -rf krayt-src

ENTRYPOINT ["krayt-dev-entrypoint"]
