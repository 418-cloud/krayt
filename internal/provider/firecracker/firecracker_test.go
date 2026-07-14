//go:build linux

package firecracker

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestStopTimeoutThenKillReapsExactlyOnce is the regression for a data race in VM teardown.
//
// Stop asks the guest to shut down and waits, bounded. A guest that ignores the request — a wedged
// container, a kernel that does not act on ctrl-alt-del — makes that wait time out, and teardown
// then falls back to kill(). Both of those paths need the process's exit status, and the obvious
// way to get it (call cmd.Wait()) is wrong: exec.Cmd.Wait mutates the Cmd and must be called
// exactly once, so a wait() that gives up while its own Wait is still in flight, followed by a
// kill() that calls Wait again, is a data race inside os/exec.
//
// The fix is structural — a single reaper goroutine owns cmd.Wait(), and everything else observes
// the exit through a channel — so this test drives the exact sequence (graceful wait times out,
// kill takes over) and asserts the process really dies. **Run it under -race**, which is what turns
// the latent double-Wait into a hard failure; CI does (`go test -race ./...`).
func TestStopTimeoutThenKillReapsExactlyOnce(t *testing.T) {
	cmd := exec.Command("sleep", "60") // stands in for a firecracker that will not shut down
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	v := &vm{id: "run-teardown", cmd: cmd, exited: make(chan struct{})}
	go v.reap()

	// The graceful wait gives up: the guest is not going anywhere.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := v.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait should have timed out on a process that will not exit, got: %v", err)
	}

	// Teardown hands over to kill(). This is where a second cmd.Wait() used to fire.
	if err := v.kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if v.cmd != nil {
		t.Error("kill should have cleared cmd")
	}
	// And the process must actually be gone — a kill that returns before the process is reaped
	// would let Destroy pull the disks out from under a live firecracker.
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Error("process still alive after kill returned")
	}
}

// TestAllocSlotStepsOverABusyTap is the regression for a whole-host outage from one stale device.
//
// A slot's flock is released when its krayt process dies, but the tap device is not: if firecracker
// outlives krayt (an orphan, e.g. after a SIGKILL), the slot looks free while its tap is still
// attached and creating it returns EBUSY. Failing the allocation there means ONE leftover device
// stops every future VM on the host, even with hundreds of free slots — and the error blamed
// CAP_NET_ADMIN, sending the operator after a permissions problem that did not exist.
//
// Needs CAP_NET_ADMIN to create tap devices, so it skips without it (see the package's
// integration_test.go for how the test binary is granted the capability).
func TestAllocSlotStepsOverABusyTap(t *testing.T) {
	requireNetAdmin(t)

	// Take a slot the normal way.
	victim, err := allocSlot(0)
	if err != nil {
		t.Fatalf("allocSlot: %v", err)
	}
	t.Cleanup(func() { _ = victim.destroy() })

	// Now reproduce the orphan: hold the slot's tap open, then release its lock WITHOUT removing
	// the device — exactly the state krayt leaves behind when it dies and firecracker does not.
	busyFD := attachTAP(t, victim.tapName())
	_ = victim.lock.Close()
	victim.lock = nil
	t.Cleanup(func() {
		_ = unix.Close(busyFD) // detach first, or the tap cannot be removed
		_ = victim.deleteTAP()
	})

	next, err := allocSlot(0)
	if err != nil {
		t.Fatalf("one busy tap made allocation fail outright, with %d slots free: %v\n"+
			"a single orphaned device must not take the whole host down", maxSlots-1, err)
	}
	t.Cleanup(func() { _ = next.destroy() })

	if next.index == victim.index {
		t.Fatalf("allocSlot handed out slot %d again, whose tap is still in use", next.index)
	}
	if next.tapName() == victim.tapName() {
		t.Fatalf("allocSlot reused the busy tap %s", next.tapName())
	}
}

// attachTAP opens the named (already existing, persistent) tap device and keeps it attached, which
// is what makes it EBUSY for anyone else — the same thing a running firecracker does to it.
func attachTAP(t *testing.T, name string) int {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open /dev/net/tun: %v", err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatalf("ifreq: %v", err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("attach tap %s: %v", name, err)
	}
	return fd
}

// requireNetAdmin skips the test unless this process can actually create a tap device. The probe
// device is not persisted, so it disappears again when the fd closes.
func requireNetAdmin(t *testing.T) {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("cannot open /dev/net/tun: %v", err)
	}
	defer func() { _ = unix.Close(fd) }()

	ifr, err := unix.NewIfreq("kraytcapprobe")
	if err != nil {
		t.Fatalf("ifreq: %v", err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		t.Skipf("no CAP_NET_ADMIN, so tap devices cannot be created: %v — "+
			"build the test binary and grant it the capability "+
			"(`go test -c` + `sudo setcap cap_net_admin+ep`) to run this", err)
	}
}
