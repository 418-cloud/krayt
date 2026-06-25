//go:build darwin

package cli

import (
	"os/exec"
	"strings"
)

// hostChecks verifies the macOS prerequisites: the vfkit binary must be installed and
// runnable, since the entitlement lives on it (not krayt) and the v1 provider drives it
// as a subprocess (§12).
func hostChecks() []checkResult {
	return []checkResult{vfkitCheck()}
}

func vfkitCheck() checkResult {
	c := checkResult{name: "vfkit installed + runnable"}
	path, err := exec.LookPath("vfkit")
	if err != nil {
		c.detail = "vfkit not found on PATH — install with `brew install vfkit`"
		return c
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		c.detail = "vfkit found at " + path + " but `vfkit --version` failed: " + err.Error()
		return c
	}
	c.ok = true
	c.detail = path + " (" + strings.TrimSpace(string(out)) + ")"
	return c
}
