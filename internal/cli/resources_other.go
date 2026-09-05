//go:build !darwin && !linux && !windows

package cli

// hostFreeResources is a no-op on any platform msb itself doesn't support (macOS, Linux, and
// Windows all have real implementations — resources_darwin.go, resources_linux.go,
// resources_windows.go), so this check must not become a second, unrelated reason such a build
// fails. Returns very large values so checkHostResources always passes.
func hostFreeResources() (freeMemMiB, freeDiskGiB uint64, err error) {
	return 1 << 32, 1 << 32, nil
}
