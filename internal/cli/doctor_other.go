//go:build !darwin && !linux

package cli

// hostChecks reports that krayt has no supported VM backend on this platform. The
// OS-agnostic core still builds here so cross-platform development is possible.
func hostChecks() []checkResult {
	return []checkResult{{
		name:   "supported VM backend",
		ok:     false,
		detail: "krayt supports macOS (vfkit) and Linux (firecracker); this platform has no backend",
	}}
}
