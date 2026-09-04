.PHONY: build krayt test lint tidy clean guest-bins

BIN := bin
GUESTBIN_DIR := internal/sandbox/guestbin/bin

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

# Build the krayt CLI binary into ./bin (host OS/arch). Depends on guest-bins because a krayt
# built with an empty embed cannot do a real run — it fails at `msb copy` time with guestbin's
# "not embedded" error — so the binary this target produces must always be a runnable one.
krayt: guest-bins
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
