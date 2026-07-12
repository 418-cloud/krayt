//go:build !darwin && !linux

package cli

// hostFreeResources is a no-op where there is no VM backend to protect (macOS has vfkit and
// Linux has firecracker; anything else cannot boot a VM at all), so this check must not become
// a second, unrelated reason such a build fails. Returns very large values so
// checkHostResources always passes.
func hostFreeResources() (freeMemMiB, freeDiskGiB uint64, err error) {
	return 1 << 32, 1 << 32, nil
}
