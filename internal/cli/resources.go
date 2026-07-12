package cli

import "fmt"

const (
	memMarginMiB  = 2048 // headroom for host OS + other processes after this run's own allocation
	diskMarginGiB = 5
)

// checkHostResources compares already-measured free host RAM/disk against what a run requests
// plus a fixed safety margin. Pure function — the OS-specific measurement lives elsewhere so this
// stays unit-testable without a real host.
func checkHostResources(freeMemMiB, freeDiskGiB, wantMemMiB, wantDiskGiB uint64) error {
	if freeMemMiB < wantMemMiB+memMarginMiB {
		return fmt.Errorf(
			"insufficient free memory to start this run: %d MiB free, need %d MiB (--memory) + %d MiB (safety margin); "+
				"free up memory, lower --memory, or pass --skip-resource-check to override",
			freeMemMiB, wantMemMiB, memMarginMiB)
	}
	if freeDiskGiB < wantDiskGiB+diskMarginGiB {
		return fmt.Errorf(
			"insufficient free disk to start this run: %d GiB free, need %d GiB (--disk) + %d GiB (safety margin); "+
				"free up disk (see `krayt image prune` if available), lower --disk, or pass --skip-resource-check to override",
			freeDiskGiB, wantDiskGiB, diskMarginGiB)
	}
	return nil
}
