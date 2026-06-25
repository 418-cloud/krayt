//go:build linux

package cli

import "os"

// hostChecks verifies the Linux prerequisites: /dev/kvm must be present for the
// (Phase 6) firecracker provider (§13).
func hostChecks() []checkResult {
	return []checkResult{kvmCheck()}
}

func kvmCheck() checkResult {
	c := checkResult{name: "/dev/kvm present"}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		c.detail = "/dev/kvm not found — KVM is required for the firecracker provider"
		return c
	}
	c.ok = true
	return c
}
