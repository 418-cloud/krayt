//go:build integration && darwin

// Integration test for the Phase 1 "Done when": on a real Apple-Silicon Mac with vfkit
// installed, krayt boots the base VM image and a Hello RPC round-trips host↔guest over
// the vfkit vsock socket (§14 Phase 1). This cannot run in a cloud agent — it needs real
// virtualization hardware and a built VM image — so it is gated behind the `integration`
// build tag and run by a human / CI on a Mac.
//
// Run:
//
//	KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
//	  go test -tags 'integration darwin' -run TestBootHello -v ./internal/provider/vfkit/
//
// Point the env vars at a base image produced by `images/flake.nix` (built in CI, §11.5)
// and pulled with `krayt image pull`.
package vfkit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/controlclient"
	"github.com/418-cloud/krayt/internal/provider"
)

func TestBootHello(t *testing.T) {
	kernel := os.Getenv("KRAYT_KERNEL")
	rootfs := os.Getenv("KRAYT_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set KRAYT_KERNEL, KRAYT_INITRD, KRAYT_ROOTFS to a built base image to run")
	}
	cmdline := os.Getenv("KRAYT_CMDLINE")
	if cmdline == "" {
		cmdline = "console=hvc0 root=/dev/vda"
	}

	p := New("", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	vm, err := p.Create(ctx, provider.VMSpec{
		ID:        "run_integration",
		Kernel:    kernel,
		Initrd:    os.Getenv("KRAYT_INITRD"),
		Cmdline:   cmdline,
		RootFS:    rootfs,
		CPUs:      2,
		MemoryMiB: 2048,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := vm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = vm.Destroy(context.Background()) })

	c, err := controlclient.Dial(vm, provider.ControlPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Boot contract: the guest-agent must answer Hello within N seconds of Start (§11.6).
	resp, err := c.WaitReady(ctx, 60*time.Second, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitReady (boot + Hello): %v", err)
	}
	if resp.GetAgentVersion() == "" {
		t.Fatal("empty agent version in Hello response")
	}
	t.Logf("guest-agent ready: version=%s containerd=%s",
		resp.GetAgentVersion(), resp.GetContainerdVersion())
}
