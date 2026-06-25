.PHONY: proto build test lint tidy

# Regenerate the gRPC control protocol from internal/protocol/krayt.proto into
# internal/protocol/pb (§9.2). Wraps the pinned Nix codegen target so plugin/version
# skew never produces noisy diffs. The generated Go is committed; building krayt needs
# no protoc.
proto:
	nix --extra-experimental-features nix-command --extra-experimental-features flakes run .#proto

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run

tidy:
	go mod tidy
