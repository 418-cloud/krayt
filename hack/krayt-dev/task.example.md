# Task

This is krayt's own repository, injected at /workspace. Toolchain available: Go 1.26,
golangci-lint, protoc/protoc-gen-go/protoc-gen-go-grpc/buf, oras.

1. Run `go build ./...`, `go vet ./...`, `go test -race ./...`, and `golangci-lint run`.
2. Fix anything that fails. Keep changes small and scoped to the failure.
3. If you touch `internal/protocol/krayt.proto`, regenerate with `make proto-direct` (no Nix
   in this image) and re-run the checks above.
4. Do not `go get` a new dependency unless the task explicitly requires it — this image's
   module cache is offline by default; a new dependency needs `proxy.golang.org` and
   `sum.golang.org` on the run's `--allow` list.

When you are done, summarize what you changed, and the exact commands you ran with their
results.
