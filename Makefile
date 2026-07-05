.PHONY: proto proto-direct build krayt test lint tidy clean

BIN := bin

# Regenerate the gRPC control protocol from internal/protocol/krayt.proto into
# internal/protocol/pb (§9.2). Wraps the pinned Nix codegen target so plugin/version
# skew never produces noisy diffs. The generated Go is committed; building krayt needs
# no protoc.
proto:
	nix --extra-experimental-features nix-command --extra-experimental-features flakes run .#proto

# Same codegen without Nix (protoc + protoc-gen-go + protoc-gen-go-grpc on PATH), for the
# krayt-dev agent image (hack/krayt-dev), which has no Nix. See hack/krayt-dev/proto-direct.sh.
proto-direct:
	hack/krayt-dev/proto-direct.sh

build:
	go build ./...

# Build the krayt CLI binary into ./bin (host OS/arch).
krayt:
	mkdir -p $(BIN)
	go build -o $(BIN)/krayt ./cmd/krayt

test:
	go test ./...

lint:
	golangci-lint run

tidy:
	go mod tidy

clean:
	rm -rf $(BIN)
