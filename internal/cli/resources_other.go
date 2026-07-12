//go:build !darwin

package cli

// hostFreeResources is a no-op off macOS today — there is no real VM backend to protect yet
// (§14 Phase 7), and this check must not become a second, unrelated reason a future Linux run
// fails. Returns very large values so checkHostResources always passes.
func hostFreeResources() (freeMemMiB, freeDiskGiB uint64, err error) {
	return 1 << 32, 1 << 32, nil
}
