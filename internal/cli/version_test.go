package cli

import (
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/vmimage"
)

// TestVersionCmd checks `krayt version` prints the version + pinned image to stdout and rejects
// positional args (uses the run/SetOut helpers from manage_test.go).
func TestVersionCmd(t *testing.T) {
	out := run(t, newVersionCmd())
	for _, want := range []string{"krayt " + Version, vmimage.PinnedRef, string(vmimage.PinnedDigest)} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q:\n%s", want, out)
		}
	}

	// version takes no positional args.
	cmd := newVersionCmd()
	cmd.SetArgs([]string{"unexpected"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	if err := cmd.Execute(); err == nil {
		t.Error("version should reject positional args")
	}
}
