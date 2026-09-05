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
		"context":   {ExitCode: 0, Stdout: `{"kind":"local","source":"MSB_BACKEND"}`},
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
	// The passing detail must carry msb's selection source, not just the backend: source=
	// MSB_BACKEND is the only evidence that krayt's pin reached the child, since "local" is
	// also what msb picks by default.
	if got := findCheck(t, checks, contextCheckName).Detail; !strings.Contains(got, "source=MSB_BACKEND") {
		t.Errorf("detail %q should report msb's selection source", got)
	}
}

func TestDoctorChecksFlagsNonLocalBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
		"--version": {ExitCode: 0, Stdout: "msb 0.6.16\n"},
		"context":   {ExitCode: 0, Stdout: `{"kind":"cloud","source":"MSB_PROFILE"}`},
		"doctor":    {ExitCode: 0, Stdout: "all prerequisites met\n"},
	}})

	got := findCheck(t, DoctorChecks(context.Background()), contextCheckName)
	if got.OK {
		t.Fatalf("context check OK = true for backend=cloud, want false")
	}
	if !strings.Contains(got.Detail, "cloud") {
		t.Errorf("detail %q should name the offending backend", got.Detail)
	}
}

// contextCheckName is contextCheck's own check name — kept in one place so a rename shows up as a
// compile-adjacent failure here rather than as silently-skipped assertions.
const contextCheckName = "msb context reports local backend"

func findCheck(t *testing.T, checks []CheckResult, name string) CheckResult {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %d checks", name, len(checks))
	return CheckResult{}
}

// The legacy flat {"backend":...} shape must keep working. It is not a shape msb has ever been
// observed to emit — it was parseContext's original guess — but the fallback keys exist to
// survive a rename, so they need coverage of their own.
func TestDoctorChecksAcceptsFallbackBackendKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
		"--version": {ExitCode: 0, Stdout: "msb 0.6.16\n"},
		"context":   {ExitCode: 0, Stdout: `{"backend":"local"}`},
		"doctor":    {ExitCode: 0, Stdout: "all prerequisites met\n"},
	}})

	if got := findCheck(t, DoctorChecks(context.Background()), contextCheckName); !got.OK {
		t.Errorf("fallback key {\"backend\":\"local\"} should pass, got detail: %s", got.Detail)
	}
}

// Regression for the real-world failure this check shipped with: msb 0.6.16 names the field
// "kind", parseContext only knew "backend"/"active_backend"/"current_backend", and a correctly
// configured host therefore failed doctor with a message blaming the host's MSB_BACKEND pin. The
// check must still fail closed on an unrecognised schema — but the detail has to show what msb
// actually printed, so the next drift is diagnosable from the output alone, and must not claim
// the host is misconfigured or that `krayt run` is blocked (nothing on the run path checks this).
func TestDoctorChecksUnrecognisedSchemaShowsRawOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(BinEnv, testBinPath)
	const raw = `{"some_future_field":"local"}`
	writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
		"--version": {ExitCode: 0, Stdout: "msb 0.6.16\n"},
		"context":   {ExitCode: 0, Stdout: raw},
		"doctor":    {ExitCode: 0, Stdout: "all prerequisites met\n"},
	}})

	got := findCheck(t, DoctorChecks(context.Background()), contextCheckName)
	if got.OK {
		t.Fatalf("unrecognised schema should fail closed, got OK with detail: %s", got.Detail)
	}
	if !strings.Contains(got.Detail, raw) {
		t.Errorf("detail should quote msb's actual output %s so the drift is diagnosable, got: %s", raw, got.Detail)
	}
	if strings.Contains(got.Detail, "refuse to start") {
		t.Errorf("detail must not claim `krayt run` is blocked — run.go does not check the backend; got: %s", got.Detail)
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
