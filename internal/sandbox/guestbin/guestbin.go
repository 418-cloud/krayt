// Package guestbin embeds the static Linux binaries krayt copies into a sandbox with
// `msb copy` and runs with `msb exec` — currently krayt-helper (add-krayt-guest-helper.md);
// dial-ask-channel-over-vsock.md adds krayt-ask alongside it, which is why this package and its
// Makefile target (`guest-bins`, not `helper`) are named for the plural from the start.
//
// The binaries are NOT committed (docs/adr-microsandbox-sandbox-layer.md, "Distribution"):
// `internal/sandbox/guestbin/bin/` is gitignored except for a committed `.gitkeep`, and `make
// guest-bins` cross-builds both architectures into it before a real run. `//go:embed all:bin`
// tolerates a directory holding only `.gitkeep`, so `go build ./...` still compiles on a fresh
// clone with no `make guest-bins` run — Binary then returns a clear runtime error instead of a
// compile error, naming the fix.
//
// Under msb the guest's architecture always equals the host's (libkrun runs a same-arch VM) and
// the guest OS is always Linux, so selection is `runtime.GOARCH` alone — no OS dimension, and so
// none of §11.1's backend-tagged-image problem (this is neither a kernel nor a rootfs).
package guestbin

import (
	"embed"
	"fmt"
)

//go:embed all:bin
var binFS embed.FS

// HelperName is the embedded name of the krayt-helper binary (add-krayt-guest-helper.md).
const HelperName = "krayt-helper"

// AskName is the embedded name of the krayt-ask binary (dial-ask-channel-over-vsock.md decision
// 6): built by the same `make guest-bins` target, `msb copy`'d in per run alongside krayt-helper,
// never baked into an agent image (images/agents/claude-code/entrypoint.sh already says so of
// today's bind-mounted krayt-ask).
const AskName = "krayt-ask"

// GuestRoot is where guestbin binaries are copied to inside a sandbox — deliberately outside
// both /workspace and /output (§8.2), so a copied-in helper binary can never land in the
// agent's changes.patch or be mistaken for a collected artifact.
const GuestRoot = "/.krayt"

// GuestPath returns the absolute in-sandbox path a binary is copied to.
func GuestPath(name string) string {
	return GuestRoot + "/" + name
}

// Binary returns the embedded static binary for name (e.g. HelperName), built for
// linux/goarch. A fresh clone's bin/ holds only .gitkeep, so a missing binary is expected
// runtime state, not a bug — the error names both remedies.
func Binary(name, goarch string) ([]byte, error) {
	path := "bin/" + name + "-linux-" + goarch
	b, err := binFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("guestbin: %s not embedded — run `make guest-bins`, or use a release build of krayt: %w", path, err)
	}
	return b, nil
}
