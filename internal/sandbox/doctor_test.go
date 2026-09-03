package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestDoctorChecksNoMsb(t *testing.T) {
	// Hermetic "msb is not installed": no BinEnv override, and PATH points only at an empty
	// directory, so exec.LookPath("msb") fails deterministically regardless of what happens to
	// be installed on the host running this test.
	t.Setenv(BinEnv, "")
	t.Setenv("PATH", t.TempDir())

	checks := DoctorChecks(context.Background())
	if len(checks) != 4 {
		t.Fatalf("got %d checks, want 4 (found, version, context, doctor passthrough)", len(checks))
	}
	for _, c := range checks {
		if c.OK {
			t.Errorf("check %q: OK = true, want false when msb cannot be found", c.Name)
		}
		if c.Optional {
			t.Errorf("check %q: Optional = true, want false — these are mandatory now (see DoctorChecks' doc comment)", c.Name)
		}
		if c.Detail == "" {
			t.Errorf("check %q: no detail text — must be actionable, not a bare failure", c.Name)
		}
	}
	if !strings.Contains(checks[0].Detail, BinEnv) && !strings.Contains(checks[0].Detail, "install") {
		t.Errorf("first check detail %q does not look actionable (no install pointer)", checks[0].Detail)
	}
}

func TestDoctorChecksWithFakeMsbHealthy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
		"--version": {ExitCode: 0, Stdout: "msb 0.6.16\n"},
		"context":   {ExitCode: 0, Stdout: `{"backend":"local"}`},
		"doctor":    {ExitCode: 0, Stdout: "all prerequisites met\n"},
	}})

	checks := DoctorChecks(context.Background())
	if len(checks) != 4 {
		t.Fatalf("got %d checks, want 4", len(checks))
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("check %q: OK = false, want true — detail: %s", c.Name, c.Detail)
		}
	}
}

func TestDoctorChecksFlagsNonLocalBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
		"--version": {ExitCode: 0, Stdout: "msb 0.6.16\n"},
		"context":   {ExitCode: 0, Stdout: `{"backend":"cloud"}`},
		"doctor":    {ExitCode: 0, Stdout: "all prerequisites met\n"},
	}})

	checks := DoctorChecks(context.Background())
	var contextCheckResult *CheckResult
	for i := range checks {
		if checks[i].Name == "msb context reports local backend" {
			contextCheckResult = &checks[i]
		}
	}
	if contextCheckResult == nil {
		t.Fatal("no context check found")
	}
	if contextCheckResult.OK {
		t.Fatalf("context check OK = true for backend=cloud, want false")
	}
	if !strings.Contains(contextCheckResult.Detail, "cloud") {
		t.Errorf("detail %q should name the offending backend", contextCheckResult.Detail)
	}
}

func TestDoctorChecksBelowFloorVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
		"--version": {ExitCode: 0, Stdout: "msb 0.6.15\n"},
	}})

	checks := DoctorChecks(context.Background())
	if checks[1].OK {
		t.Fatalf("version check OK = true for 0.6.15 < MinVersion %s, want false", MinVersion)
	}
}
