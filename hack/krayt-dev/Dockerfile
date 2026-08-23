# syntax=docker/dockerfile:1 # renovate: ignore
# krayt-dev — the dogfooding agent image: krayt's own dev toolchain layered on the published
# Claude Code agent image, so an agent running `krayt run --image krayt-dev --agent claude-code`
# against krayt's own repo can build, test, lint, and regenerate protocol code inside the sandbox
# before returning its patch (§6.14, §8.2).
#
# It inherits FROM krayt-agent-claude-code rather than re-deriving that image: the non-root `agent`
# user (uid 1000, §8.2 — Claude Code refuses uid 0), the version-pinned Claude Code CLI, the
# DISABLE_* egress env, and the ENTRYPOINT — including the §8.2 credential and KRAYT_CA_CERT
# contracts. Keeping a second near-identical copy of that logic here is what previously let the
# two drift until a shape-translated run exited 78 before Claude started.
#
# This image adds NO entrypoint of its own. It ships `gh`, but gh needs no setup step: krayt.yaml
# injects GH_TOKEN at the host proxy (§6.6.1), which means the container is configured with a
# GH_TOKEN env var holding a placeholder — and reading GH_TOKEN from the environment is gh's own
# first-choice auth path. The proxy swaps the placeholder for the real token on the way out. No
# script has to move a credential anywhere.
#
# One FROM, everything else additive: the base is the whole agent image, and each block below adds
# one tool on top of it.
#
# KEEP THIS DIGEST-PINNED. agent-images.yml re-points the `:<cli-version>` tag on every build off
# main, so the tag alone floats: this image would silently absorb base changes on any unrelated
# rebuild, and Renovate would never open the digest-bump PR that is how a base change is meant to
# arrive here (see dev-image.yml's `paths` comment). Renovate's dockerfile manager appends and
# maintains the digest (`pinDigests: true`, renovate.json) — the line is tag-only until its first
# PR lands, which is tracked in HUMAN_TODO.md.
FROM ghcr.io/418-cloud/krayt-agent-claude-code:2.1.226

# Tool versions, pinned so Renovate's custom regex manager (renovate.json) can bump them
# independently of the base image tag. `make proto` (Nix) stays the source of truth for the
# *committed* generated code; this toolchain is only for an agent regenerating it mid-run
# (see proto-direct.sh) when it edits internal/protocol/krayt.proto.
ARG GO_VERSION=1.26.3
ARG PROTOC_VERSION=35.1
ARG PROTOC_GEN_GO_VERSION=v1.36.11
ARG PROTOC_GEN_GO_GRPC_VERSION=v1.6.2
ARG BUF_VERSION=v1.71.0
ARG ORAS_VERSION=v1.3.2
ARG GOLANGCI_LINT_VERSION=v2.12.2
ARG GH_CLI_VERSION=2.96.0
ARG HADOLINT_VERSION=2.15.1

# The base image is debian:trixie-slim + `ca-certificates curl git bash`; these are what krayt's
# toolchain needs on top. gcc + libc6-dev are not optional: `go test -race` requires cgo, so
# without a C toolchain the race detector — which CI and every dogfooding task run — fails to
# link. unzip: protoc's release archive is a zip. xz-utils: the Nix installer's tarball is .tar.xz.
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev unzip xz-utils \
 && rm -rf /var/lib/apt/lists/*

# The Go toolchain, fetched as the official prebuilt tarball like protoc/gh/hadolint below — the
# base image is an agent image, not a language image, so Go is just another tool this one adds.
# Go's asset arch names ARE Docker's TARGETARCH values verbatim (amd64/arm64), like gh's.
# GO_VERSION must stay >= go.mod's `go` directive: with GOTOOLCHAIN=local (below) a lower one
# fails the build outright rather than silently downloading a second toolchain. Renovate's
# `go version` group bumps this ARG and that directive together (renovate.json).
ARG TARGETARCH
RUN curl -fsSL -o /tmp/go.tar.gz \
      "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
 && tar -C /usr/local -xzf /tmp/go.tar.gz \
 && rm /tmp/go.tar.gz

# protoc itself has no Go module (it's the C++ compiler), so it's the one tool fetched as a
# prebuilt release archive; TARGETARCH (declared above) is set per-platform by buildx during the
# multi-arch build.
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

# gh CLI, like protoc, has no `go install` story worth relying on, so it's fetched as a prebuilt
# release tarball. Unlike protoc, gh's asset arch names ARE Docker's TARGETARCH values
# (amd64/arm64) verbatim — no x86_64/aarch_64 translation needed. Needed by the
# fix-pr-review-comments task; installing the binary is ALL this image does for it, since gh
# authenticates from the GH_TOKEN the host proxy injects (§6.6.1) and reads that env var itself.
# No entrypoint anywhere touches the token — see README's GH_TOKEN secret.
RUN set -eu; \
    curl -fsSL -o /tmp/gh.tar.gz \
      "https://github.com/cli/cli/releases/download/v${GH_CLI_VERSION}/gh_${GH_CLI_VERSION}_linux_${TARGETARCH}.tar.gz" \
 && tar -xzf /tmp/gh.tar.gz -C /tmp \
 && install -m 0755 "/tmp/gh_${GH_CLI_VERSION}_linux_${TARGETARCH}/bin/gh" /usr/local/bin/gh \
 && rm -rf /tmp/gh.tar.gz "/tmp/gh_${GH_CLI_VERSION}_linux_${TARGETARCH}"

# hadolint lints the agent images' Dockerfiles (see docs/ai-tasks/*.md's verification steps); like
# protoc it ships as a prebuilt release binary (Haskell, no `go install` story), and like protoc its
# arch names aren't Docker's TARGETARCH verbatim.
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) hadolint_arch=x86_64 ;; \
      arm64) hadolint_arch=arm64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/hadolint \
      "https://github.com/hadolint/hadolint/releases/download/v${HADOLINT_VERSION}/hadolint-linux-${hadolint_arch}" \
 && chmod 0755 /usr/local/bin/hadolint

# Back to the base image's non-root `agent` user for everything below, so it owns its own Go/lint
# caches (see ENV block). The user, its home, and the Claude Code CLI all come from the base.
USER agent
WORKDIR /home/agent

# Agent-owned, writable caches so `go build`, `go test -race`, and `golangci-lint run` work
# under a non-root uid with no permission errors. GOTOOLCHAIN=local matches what the official Go
# image sets: without it, a go.mod naming a newer patch release than GO_VERSION makes `go` fetch a
# whole second toolchain at run time — an egress destination the run's allowlist does not have.
ENV GOPATH=/home/agent/go \
    GOCACHE=/home/agent/.cache/go-build \
    GOMODCACHE=/home/agent/go/pkg/mod \
    GOLANGCI_LINT_CACHE=/home/agent/.cache/golangci-lint \
    GOFLAGS=-mod=mod \
    GOTOOLCHAIN=local \
    PATH="/usr/local/go/bin:/home/agent/go/bin:/home/agent/.local/bin:${PATH}"

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

# --- Nix (single-user, agent-owned) so the agent can regenerate images/flake.nix vendorHash ---
# The guest-agent is built with Nix `buildGoModule`; when its Go deps change, `vendorHash` must be
# recomputed via `nix build`. This is a SINGLE-USER (`--no-daemon`) install — deliberately **not**
# the repo's CI Determinate installer (root README "Setup / deps"): Determinate defaults to a
# *multi-user* install that needs a root daemon at run time, which a non-root container can't run.
# Single-user is the only mode that works as uid 1000 — /nix is owned by the agent user and builds
# run directly as that user. Flakes are enabled for `nix build`. NOTE: at run time `nix build` needs egress to
# cache.nixos.org + the flake inputs (github / codeload) + the Go module proxy — add those to the
# run's --allow (see README). The install itself happens at image-build time (full network).
USER root
RUN install -d -m 0755 -o agent -g agent /nix
USER agent
RUN export USER=agent \
 && curl -fsSL https://nixos.org/nix/install | sh -s -- --no-daemon \
 && mkdir -p /home/agent/.config/nix \
 && printf 'experimental-features = nix-command flakes\n' > /home/agent/.config/nix/nix.conf
ENV PATH="/home/agent/.nix-profile/bin:/nix/var/nix/profiles/default/bin:${PATH}"

# Pre-bake krayt's module graph into GOMODCACHE so `go build ./...` / `go test -race ./...` work
# offline inside the sandbox's allowlisted egress (see README). Only manifests are copied, not
# source, so this layer only busts when dependencies change; the nested hack/ask-probe module is
# stdlib-only today but downloaded in step in case that changes.
COPY --chown=agent:agent go.mod go.sum ./krayt-src/
COPY --chown=agent:agent hack/ask-probe/go.mod ./krayt-src/hack/ask-probe/go.mod
RUN cd krayt-src && go mod download \
 && cd hack/ask-probe && go mod download \
 && cd /home/agent && rm -rf krayt-src

# Model + reasoning-effort defaults for this image, read by the base entrypoint's optional
# selection branch (those are `claude` flags, and the base is the image that ships `claude`).
# krayt.yaml's `env:` block (§8.1) overrides them per run; the base leaves both unset and lets
# Claude Code pick its own default.
ENV CLAUDE_MODEL=claude-sonnet-5 \
    CLAUDE_EFFORT=high

# No ENTRYPOINT and no USER: both come from the base (krayt-agent-entrypoint, running as `agent`).
# See the header for why this image needs no entrypoint of its own.
