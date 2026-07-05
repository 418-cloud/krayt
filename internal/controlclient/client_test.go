package controlclient_test

import (
	"context"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/controlclient"
	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/provider/fake"
)

// TestWaitReadyHello exercises the host control client against the in-process fake
// provider: dial the guest over DialControl, then poll Hello to readiness. This is the
// cross-OS stand-in for the Phase 1 boot+Hello round-trip (the real boot is HUMAN/CI).
func TestWaitReadyHello(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vm, err := fake.New().Create(ctx, provider.VMSpec{ID: "run_ready"})
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

	resp, err := c.WaitReady(ctx, 3*time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if resp.GetAgentVersion() != guest.Version {
		t.Errorf("agent_version = %q, want %q", resp.GetAgentVersion(), guest.Version)
	}
	if resp.GetBootId() == "" {
		t.Error("boot_id = \"\", want a non-empty id")
	}
}
