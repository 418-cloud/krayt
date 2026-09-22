//go:build windows

package reexec

// execSelf is a no-op on Windows, which has no exec-in-place. Nothing is lost: the native Windows
// CI job runs `go test` without -race (the race detector there needs cgo and a C compiler), so
// there is no teardown sleep to skip in the first place.
func execSelf(string, []string, []string) {}
