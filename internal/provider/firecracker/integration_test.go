//go:build integration && linux

// Integration test for the Phase 7 boot contract, the Linux counterpart of the vfkit
// provider's TestBootHello: on a Linux host with /dev/kvm and the firecracker binary, krayt
// boots the base VM image and a Hello RPC round-trips host↔guest over firecracker's vsock
// (§14 Phase 7, §11.6). It needs real virtualization and a built x86_64 VM image, so it is
// gated behind the `integration` build tag.
//
// Unlike the darwin integration tests this one CAN be run by a coding agent — any Linux host
// with nested virtualization will do — which is why Phase 7 is verifiable without hardware
// handoff.
//
// Run (the test binary needs CAP_NET_ADMIN to create the VM's tap device, and the invoking
// user needs /dev/kvm access):
//
//	go test -c -tags 'integration linux' -o /tmp/fc.test ./internal/provider/firecracker/
//	sudo setcap cap_net_admin+ep /tmp/fc.test
//	KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
//	  /tmp/fc.test -test.run TestBootHello -test.v
//
// Point the env vars at an x86_64 base image built by `nix build ./images#packages.x86_64-linux.vmImage`.
package firecracker

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/controlclient"
	"github.com/418-cloud/krayt/internal/provider"
)

func TestBootHello(t *testing.T) {
	kernel := os.Getenv("KRAYT_KERNEL")
	initrd := os.Getenv("KRAYT_INITRD")
	rootfs := os.Getenv("KRAYT_ROOTFS")
	if kernel == "" || initrd == "" || rootfs == "" {
		t.Skip("set KRAYT_KERNEL, KRAYT_INITRD, KRAYT_ROOTFS to a built base image to run")
	}

	p := New("", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	vm, err := p.Create(ctx, provider.VMSpec{
		ID:        "run_integration",
		Kernel:    kernel,
		Initrd:    initrd,
		RootFS:    rootfs,
		Cmdline:   os.Getenv("KRAYT_CMDLINE"), // provider supplies console/ip= itself
		CPUs:      2,
		MemoryMiB: 2048,
		DiskGiB:   10,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Tear down as soon as the VM exists, not once it has started: Start can fail after Create
	// has already claimed a tap device and a multi-GiB rootfs clone. This is the same
	// Create -> defer Destroy -> Start order the orchestrator uses (orchestrator.Run).
	t.Cleanup(func() { _ = vm.Destroy(context.Background()) })
	if err := vm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	c, err := controlclient.Dial(vm, provider.ControlPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Boot contract: the guest-agent must answer Hello within N seconds of Start (§11.6).
	resp, err := c.WaitReady(ctx, 90*time.Second, 500*time.Millisecond)
	if err != nil {
		dumpDiagnostics(t, vm)
		t.Fatalf("WaitReady (boot + Hello): %v", err)
	}
	if resp.GetAgentVersion() == "" {
		t.Fatal("empty agent version in Hello response")
	}
	t.Logf("guest-agent ready: version=%s containerd=%s",
		resp.GetAgentVersion(), resp.GetContainerdVersion())
}

// TestGuestNetwork asserts the guest actually configured the NIC the provider gave it.
//
// This is worth a test of its own because the failure is silent. Firecracker supplies no DHCP
// server, so the guest gets its address from the kernel `ip=` parameter the provider appends
// and the image's krayt.net=static networkd unit then leaves alone (tap.go, images/flake.nix).
// If any link in that chain breaks, the guest simply comes up with no address — and because
// krayt-agent only *wants* network-online.target, the VM still boots and still answers Hello.
// Everything would look fine right up until a task tried to reach the network.
func TestGuestNetwork(t *testing.T) {
	kernel := os.Getenv("KRAYT_KERNEL")
	initrd := os.Getenv("KRAYT_INITRD")
	rootfs := os.Getenv("KRAYT_ROOTFS")
	if kernel == "" || initrd == "" || rootfs == "" {
		t.Skip("set KRAYT_KERNEL, KRAYT_INITRD, KRAYT_ROOTFS to a built base image to run")
	}

	p := New("", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	machine, err := p.Create(ctx, provider.VMSpec{
		ID: "run-nettest", Kernel: kernel, Initrd: initrd, RootFS: rootfs,
		CPUs: 2, MemoryMiB: 2048, DiskGiB: 10,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = machine.Destroy(context.Background()) })
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the guest to be up at all before judging its network.
	c, err := controlclient.Dial(machine, provider.ControlPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.WaitReady(ctx, 90*time.Second, 500*time.Millisecond); err != nil {
		dumpDiagnostics(t, machine)
		t.Fatalf("WaitReady: %v", err)
	}

	guestIP := machine.(*vm).slot.guestIP().String()
	// ping(8) rather than a raw socket: it carries its own capability, so the test does not
	// need CAP_NET_RAW on top of the CAP_NET_ADMIN the provider already requires.
	out, err := exec.CommandContext(ctx, "ping", "-c", "3", "-W", "2", guestIP).CombinedOutput()
	if err != nil {
		dumpDiagnostics(t, machine)
		t.Fatalf("guest %s did not answer ping — it never configured its NIC from the `ip=` cmdline: %v\n%s",
			guestIP, err, out)
	}
	t.Logf("guest reachable at %s:\n%s", guestIP, out)
}

// dumpDiagnostics prints the firecracker + guest-console log to the test output so a boot
// failure is visible even though Destroy (t.Cleanup) later removes the run dir. Firecracker
// writes its own log and the guest's ttyS0 to the same stream, so there is one file.
func dumpDiagnostics(t *testing.T, machine provider.VM) {
	t.Helper()
	lp, ok := machine.(interface{ LogPaths() (string, string) })
	if !ok {
		return
	}
	_, consoleLog := lp.LogPaths()
	b, err := os.ReadFile(consoleLog)
	if err != nil {
		t.Logf("--- console.log: unavailable (%v) ---", err)
		return
	}
	t.Logf("--- console.log (%d bytes) ---\n%s", len(b), b)
}
