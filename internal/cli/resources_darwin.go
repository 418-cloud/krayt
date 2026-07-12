//go:build darwin

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// hostFreeResources measures live free RAM (MiB) and free disk (GiB) on the volume backing
// krayt's caches (os.UserCacheDir()), for the run-start preflight (checkHostResources).
func hostFreeResources() (freeMemMiB, freeDiskGiB uint64, err error) {
	freeMemMiB, err = freeMemoryMiB()
	if err != nil {
		return 0, 0, err
	}
	freeDiskGiB, err = freeDiskGiBAt(os.UserCacheDir)
	if err != nil {
		return 0, 0, err
	}
	return freeMemMiB, freeDiskGiB, nil
}

var vmStatPageLine = regexp.MustCompile(`page size of (\d+) bytes`)
var vmStatCountLine = regexp.MustCompile(`^(Pages free|Pages inactive|Pages speculative):\s+(\d+)\.`)

// freeMemoryMiB shells out to vm_stat (stable macOS system binary, no cgo/new dependency) and
// approximates "readily available without swapping" as free + inactive + speculative pages.
func freeMemoryMiB() (uint64, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, fmt.Errorf("cli: vm_stat: %w", err)
	}
	lines := strings.Split(string(out), "\n")
	var pageSize, pages uint64
	for _, l := range lines {
		if m := vmStatPageLine.FindStringSubmatch(l); m != nil {
			pageSize, _ = strconv.ParseUint(m[1], 10, 64)
		}
		if m := vmStatCountLine.FindStringSubmatch(l); m != nil {
			n, _ := strconv.ParseUint(m[2], 10, 64)
			pages += n
		}
	}
	if pageSize == 0 {
		return 0, fmt.Errorf("cli: vm_stat: could not parse page size")
	}
	return pages * pageSize / (1024 * 1024), nil
}

// freeDiskGiBAt reports free disk (GiB available to this user) on the volume containing dirFn's
// directory. dirFn is os.UserCacheDir, injected so a test can point at t.TempDir() instead.
func freeDiskGiBAt(dirFn func() (string, error)) (uint64, error) {
	dir, err := dirFn()
	if err != nil {
		return 0, fmt.Errorf("cli: resolve cache dir: %w", err)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("cli: statfs %s: %w", dir, err)
	}
	return stat.Bavail * uint64(stat.Bsize) / (1024 * 1024 * 1024), nil
}
