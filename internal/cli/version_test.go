package cli

import (
	"strings"
	"testing"
)

// TestVersionCmd checks `krayt version` prints the version to stdout and rejects positional args
// (uses the run/SetOut helpers from manage_test.go).
func TestVersionCmd(t *testing.T) {
	out := run(t, newVersionCmd())
	if want := "krayt " + Version; !strings.Contains(out, want) {
		t.Errorf("version output missing %q:\n%s", want, out)
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
