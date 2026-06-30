//go:build !darwin

package cli

import "fmt"

// newRunDeps fails on non-macOS hosts: the v1 provider is vfkit (macOS only). The Linux
// firecracker backend arrives in Phase 6 behind the same Provider interface (§4, §6.3).
func newRunDeps() (runDeps, error) {
	return runDeps{}, fmt.Errorf("`krayt run` needs the vfkit provider (macOS); the Linux firecracker backend is Phase 6")
}
