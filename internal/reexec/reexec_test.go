package reexec_test

// These tests drive FastExit the way the real fakes do — by re-execing this test binary — because
// the whole mechanism IS a re-exec and anything less would only be testing the bookkeeping.

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/reexec"
)

// childArg is the argv[1] marker TestMain dispatches on, standing in for a real msb verb.
const childArg = "__reexec_child"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == childArg {
		reexec.FastExit()
		env := map[string]string{}
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
		// argv is reported too: the fakes dispatch on argv[1], so an exec that shifted or dropped
		// an argument would silently turn a fake msb call into a test-suite re-run.
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"argv": os.Args[1:], "env": env})
		os.Exit(7) // a distinctive code, to prove the exec preserved the child's own exit status
	}
	os.Exit(m.Run())
}

// TestFastExitSetsGoraceAndPreservesArgvAndExit covers the mechanism end to end: the re-exec'd
// image really does get atexit_sleep_ms=0 (the entire point), and is otherwise indistinguishable
// from the process the parent asked for.
func TestFastExitSetsGoraceAndPreservesArgvAndExit(t *testing.T) {
	argv, env, code := runChild(t, nil, childArg, "--name", "a b")

	if want := []string{childArg, "--name", "a b"}; !equal(argv, want) {
		t.Errorf("argv = %q, want %q passed through unchanged and unsplit", argv, want)
	}
	if code != 7 {
		t.Errorf("exit = %d, want 7 — the re-exec must not swallow the child's status", code)
	}
	if !reexec.RaceEnabled {
		if _, set := env["GORACE"]; set {
			t.Error("GORACE set without -race; FastExit should be inert there")
		}
		return
	}
	if got := env["GORACE"]; !strings.Contains(got, "atexit_sleep_ms=0") {
		t.Errorf("GORACE = %q, want it to carry atexit_sleep_ms=0", got)
	}
}

// TestSanitizeHidesOnlyTheHarnesssOwnGorace is the safety property that lets internal/sandbox's
// TestChildEnvAllowlistExact keep asserting an EXACTLY closed allowlist. A GORACE that FastExit
// invented must be invisible to the fake's env record; a GORACE that arrived from the parent —
// which, for a real msb child, could only mean krayt forwarded it — must stay visible, so that
// test still catches it.
func TestSanitizeHidesOnlyTheHarnesssOwnGorace(t *testing.T) {
	if !reexec.RaceEnabled {
		t.Skip("FastExit is inert without -race")
	}

	t.Run("invented is hidden", func(t *testing.T) {
		_, env, _ := runChild(t, nil, childArg)
		reexec.SanitizeChildEnv(env)
		if _, ok := env["GORACE"]; ok {
			t.Error("GORACE survived sanitizing; the allowlist test would fail on the harness's own variable")
		}
		if _, ok := env[reexec.MarkerEnv]; ok {
			t.Errorf("%s survived sanitizing", reexec.MarkerEnv)
		}
	})

	t.Run("inherited stays visible", func(t *testing.T) {
		_, env, _ := runChild(t, []string{"GORACE=halt_on_error=1"}, childArg)
		if got := env["GORACE"]; !strings.Contains(got, "halt_on_error=1") {
			t.Errorf("GORACE = %q, want the inherited value preserved", got)
		}
		reexec.SanitizeChildEnv(env)
		if _, ok := env["GORACE"]; !ok {
			t.Error("an inherited GORACE was hidden by sanitizing — a real leak would go undetected")
		}
	})
}

// TestSanitizeChildEnvTable states SanitizeChildEnv's contract directly, in-process and without
// needing -race, so the rule survives independently of whether the subprocess tests above are
// skipped on a given build. The third case is the load-bearing one: no marker means the variables
// are not the harness's, so nothing may be removed.
func TestSanitizeChildEnvTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{
			name: "marker says invented, so GORACE goes with it",
			in:   map[string]string{"HOME": "/h", "GORACE": "atexit_sleep_ms=0", reexec.MarkerEnv: "invented"},
			want: map[string]string{"HOME": "/h"},
		},
		{
			name: "marker says inherited, so GORACE stays and still fails the allowlist test",
			in:   map[string]string{"HOME": "/h", "GORACE": "atexit_sleep_ms=0,x=1", reexec.MarkerEnv: "inherited"},
			want: map[string]string{"HOME": "/h", "GORACE": "atexit_sleep_ms=0,x=1"},
		},
		{
			name: "no marker, so nothing is the harness's to remove",
			in:   map[string]string{"HOME": "/h", "GORACE": "halt_on_error=1"},
			want: map[string]string{"HOME": "/h", "GORACE": "halt_on_error=1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reexec.SanitizeChildEnv(tc.in)
			if len(tc.in) != len(tc.want) {
				t.Fatalf("got %v, want %v", tc.in, tc.want)
			}
			for k, v := range tc.want {
				if tc.in[k] != v {
					t.Errorf("[%s] = %q, want %q (full: %v)", k, tc.in[k], v, tc.in)
				}
			}
		})
	}
}

// TestFastExitDoesNotLoop guards the re-exec guard: a child that re-exec'd itself forever would
// hang the suite rather than fail it.
func TestFastExitDoesNotLoop(t *testing.T) {
	if !reexec.RaceEnabled {
		t.Skip("FastExit is inert without -race")
	}
	_, env, code := runChild(t, []string{reexec.MarkerEnv + "=invented"}, childArg)
	if code != 7 {
		t.Fatalf("exit = %d, want 7", code)
	}
	// Already marked, so FastExit must have returned without touching anything.
	if _, set := env["GORACE"]; set {
		t.Error("FastExit re-exec'd a process already marked as re-exec'd")
	}
}

// runChild re-execs this test binary with a closed environment — the shape sandbox.childEnv hands
// a real msb child — plus extra, and returns what the child reported.
func runChild(t *testing.T, extra []string, args ...string) (argv []string, env map[string]string, exitCode int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, args...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, extra...)
	out, err := cmd.Output()
	// A non-zero exit is expected (the child reports a distinctive one); anything else is not.
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run child: %v", err)
	}
	var got struct {
		Argv []string          `json:"argv"`
		Env  map[string]string `json:"env"`
	}
	if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
		t.Fatalf("child output %q: %v", out, jsonErr)
	}
	return got.Argv, got.Env, cmd.ProcessState.ExitCode()
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
