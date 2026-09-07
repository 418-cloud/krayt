//go:build windows

package cli

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hostFreeResources measures live free RAM (MiB) and free disk (GiB) on the volume backing
// krayt's caches (os.UserCacheDir()), for the run-start preflight (checkHostResources) —
// GlobalMemoryStatusEx and GetDiskFreeSpaceEx are the Windows analogues of Linux's
// /proc/meminfo read and Darwin's vm_stat/statfs (resources_linux.go, resources_darwin.go).
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

// memoryStatusEx mirrors Win32's MEMORYSTATUSEX struct (kernel32.dll's GlobalMemoryStatusEx).
// golang.org/x/sys/windows ships no wrapper for it, so this declares the raw call directly —
// the same idiom other Windows Go tooling (e.g. gopsutil) uses for APIs x/sys hasn't wrapped.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
)

// freeMemoryMiB reads AvailPhys — physical memory immediately available to a new workload
// without swapping, the same question /proc/meminfo's MemAvailable and vm_stat's free+inactive+
// speculative pages answer on Linux/macOS.
func freeMemoryMiB() (uint64, error) {
	var m memoryStatusEx
	m.length = uint32(unsafe.Sizeof(m))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, fmt.Errorf("cli: GlobalMemoryStatusEx: %w", err)
	}
	return m.availPhys / (1024 * 1024), nil
}

// freeDiskGiBAt reports free disk (GiB available to this user) on the volume containing dirFn's
// directory. dirFn is os.UserCacheDir, injected so a test can point at t.TempDir() instead.
func freeDiskGiBAt(dirFn func() (string, error)) (uint64, error) {
	dir, err := dirFn()
	if err != nil {
		return 0, fmt.Errorf("cli: resolve cache dir: %w", err)
	}
	ptr, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("cli: %s: %w", dir, err)
	}
	var freeBytesAvailable uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &freeBytesAvailable, nil, nil); err != nil {
		return 0, fmt.Errorf("cli: GetDiskFreeSpaceEx %s: %w", dir, err)
	}
	return freeBytesAvailable / (1024 * 1024 * 1024), nil
}
