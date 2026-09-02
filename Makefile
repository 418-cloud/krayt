.PHONY: proto proto-direct build krayt test lint tidy clean guest-bins

BIN := bin
GUESTBIN_DIR := internal/sandbox/guestbin/bin

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

# Cross-build the static Linux guest binaries krayt embeds (internal/sandbox/guestbin) and
# `msb copy`s into a sandbox per run (add-krayt-guest-helper.md). Not committed — the bin/ dir is
# gitignored except a .gitkeep, so a plain `go build ./...` still compiles on a fresh clone
# without this target; running it is what actually populates the embed for a real run, or for
# the guestbin/krayt-helper tests that exercise it.
guest-bins:
	mkdir -p $(GUESTBIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(GUESTBIN_DIR)/krayt-helper-linux-amd64 ./cmd/krayt-helper
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o $(GUESTBIN_DIR)/krayt-helper-linux-arm64 ./cmd/krayt-helper
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(GUESTBIN_DIR)/krayt-ask-linux-amd64 ./cmd/krayt-ask
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o $(GUESTBIN_DIR)/krayt-ask-linux-arm64 ./cmd/krayt-ask

build: guest-bins
	go build ./...

# Build the krayt CLI binary into ./bin (host OS/arch).
krayt:
	mkdir -p $(BIN)
	go build -o $(BIN)/krayt ./cmd/krayt

test: guest-bins
	go test ./...

lint:
	golangci-lint run

tidy:
	go mod tidy

clean:
	rm -rf $(BIN)
