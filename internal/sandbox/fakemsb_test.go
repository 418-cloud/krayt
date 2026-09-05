package sandbox

// A fake, scriptable `msb`, following the repo's established re-exec-the-test-binary pattern
// (internal/orchestrator/climit_test.go's TestMain, internal/orchestrator/egressproxy_internal_test.go's
// runEgressHelper): every test in this package points BinEnv at this binary's own path, and a
// re-exec of it is recognized and dispatched to runFakeMsb instead of the test suite.
//
// Detection is by ARGV, not an env-var flag, and that is deliberate rather than an oversight: the
// whole point of childEnv is that the msb child gets a CLOSED allowlist, never os.Environ(), so a
// marker set on the outer `go test` process's own environment before m.Run() would never reach
// the re-exec'd child — exactly the reasoning egressproxy's tests give for using egressHelperArg
// instead of an env flag. Every msb verb this driver ever issues (--version, context, create,
// exec, copy, logs, stop, rm, pull, doctor, images, rmi, image) is a token `go test`'s own flags
// never produce, so os.Args[1] unambiguously tells the two apart.
//
// HOME doubles as the fixture directory: it is already in childEnv's allowlist (msb genuinely
// resolves state under it), so tests set HOME to a temp dir holding a small JSON "script" keyed
// by verb, and the fake records every call's argv + observed environment there — which is what
// makes the child-env allowlist test real, not simulated.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMsbVerbs is the complete set of first-argv-token values the real Client ever issues. Kept
// as an explicit allowlist (rather than "anything that isn't a go-test flag") so a typo in a new
// Client method's argv is caught by TestMain falling through to m.Run() and failing loudly,
// instead of silently being swallowed as "must be the fake".
var fakeMsbVerbs = map[string]bool{
	"--version": true,
	"context":   true,
	"create":    true,
	"exec":      true,
	"copy":      true,
	"logs":      true,
	"stop":      true,
	"rm":        true,
	"pull":      true,
	"doctor":    true,
	"images":    true,
	"rmi":       true,
	"image":     true,
}

// testBinPath is this test binary's own path, captured once before m.Run() so tests can point
// BinEnv at it (mirrors climit_test.go's use of os.Executable()).
var testBinPath string

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && fakeMsbVerbs[os.Args[1]] {
		os.Exit(runFakeMsb())
	}
	if self, err := os.Executable(); err == nil {
		testBinPath = self
	}
	os.Exit(m.Run())
}

// fakeResponse is one scripted reply for one msb verb.
type fakeResponse struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	EchoStdin bool   `json:"echo_stdin"` // copy stdin to stdout, proving a real pipe carried bytes
}

// fakeScript is the fixture a test writes to $HOME before invoking a Client method. Responses is
// keyed by the msb verb (args[0]); Default answers anything not explicitly scripted.
type fakeScript struct {
	Responses map[string]fakeResponse `json:"responses"`
	Default   fakeResponse            `json:"default"`
}

// fakeCall is one recorded invocation of the fake, appended to fakeCallsFile.
type fakeCall struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

const (
	fakeScriptFile = "fake-msb-script.json"
	fakeCallsFile  = "fake-msb-calls.jsonl"
)

func writeFakeScript(t *testing.T, home string, script fakeScript) {
	t.Helper()
	b, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal fake script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, fakeScriptFile), b, 0o600); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
}

func readFakeCalls(t *testing.T, home string) []fakeCall {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, fakeCallsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fake calls: %v", err)
	}
	var calls []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var c fakeCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("unmarshal fake call %q: %v", line, err)
		}
		calls = append(calls, c)
	}
	return calls
}

func lastFakeCall(t *testing.T, home string) fakeCall {
	t.Helper()
	calls := readFakeCalls(t, home)
	if len(calls) == 0 {
		t.Fatalf("no fake-msb calls recorded under %s", home)
	}
	return calls[len(calls)-1]
}

// runFakeMsb is this test binary re-exec'd as `msb`: it records its own argv + environment under
// $HOME, then replies with whatever fakeScript says for that verb (or Default).
func runFakeMsb() int {
	home := os.Getenv("HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "fake-msb: HOME not set — cannot find its fixture/recording dir")
		return 1
	}
	args := os.Args[1:]
	appendFakeCall(home, fakeCall{Args: args, Env: envMap(os.Environ())})

	script := readFakeScriptOrDefault(home)
	resp := script.Default
	if len(args) > 0 {
		if r, ok := script.Responses[args[0]]; ok {
			resp = r
		}
	}

	if resp.Stdout != "" {
		_, _ = fmt.Fprint(os.Stdout, resp.Stdout)
	}
	if resp.EchoStdin {
		_, _ = io.Copy(os.Stdout, os.Stdin)
	}
	if resp.Stderr != "" {
		_, _ = fmt.Fprint(os.Stderr, resp.Stderr)
	}
	return resp.ExitCode
}

func appendFakeCall(home string, call fakeCall) {
	f, err := os.OpenFile(filepath.Join(home, fakeCallsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	b, err := json.Marshal(call)
	if err != nil {
		return
	}
	_, _ = w.Write(b)
	_, _ = w.WriteString("\n")
	_ = w.Flush()
}

func readFakeScriptOrDefault(home string) fakeScript {
	b, err := os.ReadFile(filepath.Join(home, fakeScriptFile))
	if err != nil {
		return fakeScript{}
	}
	var s fakeScript
	if err := json.Unmarshal(b, &s); err != nil {
		return fakeScript{}
	}
	return s
}

func envMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[name] = value
	}
	return m
}
