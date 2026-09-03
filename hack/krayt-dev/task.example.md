# Task

This is krayt's own repository, injected at /workspace. Toolchain available: Go 1.26,
golangci-lint, oras.

1. Run `go build ./...`, `go vet ./...`, `go test -race ./...`, and `golangci-lint run`.
2. Fix anything that fails. Keep changes small and scoped to the failure.
3. Do not `go get` a new dependency unless the task explicitly requires it — this image's
   module cache is offline by default; a new dependency needs `proxy.golang.org` and
   `sum.golang.org` on the run's `--allow` list.

When you are done, summarize what you changed, and the exact commands you ran with their
results.
