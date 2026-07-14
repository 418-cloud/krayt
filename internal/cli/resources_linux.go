//go:build linux

package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// hostFreeResources measures live free RAM (MiB) and free disk (GiB) on the volume backing
// krayt's caches (os.UserCacheDir()), for the run-start preflight (checkHostResources).
//
// The disk figure matters more on Linux than on macOS: without a reflink-capable filesystem
// the firecracker provider's CoW clone is a full copy of the base rootfs (~2 GiB) per VM, so a
// run really does consume that much before the container even starts (§6.3, clone.go).
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

// freeMemoryMiB reads MemAvailable from /proc/meminfo — the kernel's own estimate of what a
// new workload can claim without swapping, which is exactly the question being asked here (and
// a better answer than MemFree, which ignores reclaimable page cache).
func freeMemoryMiB() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("cli: open /proc/meminfo: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		value, found := strings.CutPrefix(sc.Text(), "MemAvailable:")
		if !found {
			continue
		}
		// The line is "MemAvailable:   12345678 kB".
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return 0, fmt.Errorf("cli: /proc/meminfo: malformed MemAvailable line")
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cli: /proc/meminfo: parse MemAvailable: %w", err)
		}
		return kb / 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("cli: read /proc/meminfo: %w", err)
	}
	return 0, fmt.Errorf("cli: /proc/meminfo: no MemAvailable line")
}

// freeDiskGiBAt reports free disk (GiB available to this user) on the filesystem containing
// dirFn's directory. dirFn is os.UserCacheDir, injected so a test can point at t.TempDir().
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
