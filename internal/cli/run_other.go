//go:build !darwin && !linux

package cli

import "fmt"

// newRunDeps fails on hosts with no VM backend. krayt ships two providers behind the same
// Provider interface (§4, §6.3): vfkit on macOS and firecracker on Linux/KVM. Everywhere else
// there is nothing to boot a VM with.
func newRunDeps() (runDeps, error) {
	return runDeps{}, fmt.Errorf("`krayt run` needs a VM backend: vfkit (macOS) or firecracker + /dev/kvm (Linux)")
}
