#!/usr/bin/env bash
# No-Nix path to regenerate internal/protocol/pb from internal/protocol/krayt.proto (§9.2).
# `make proto` wraps `nix run .#proto`, which isn't available inside this image; this is the
# same command the flake's `proto` derivation runs (verified against flake.nix), for an agent
# that has edited krayt.proto and needs to regenerate the committed Go before returning its
# patch. Requires protoc + protoc-gen-go + protoc-gen-go-grpc on PATH (baked into this image).
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
mkdir -p internal/protocol/pb
protoc \
  --proto_path=internal/protocol \
  --go_out=. --go_opt=module=github.com/418-cloud/krayt \
  --go-grpc_out=. --go-grpc_opt=module=github.com/418-cloud/krayt \
  internal/protocol/krayt.proto
echo "generated internal/protocol/pb"
