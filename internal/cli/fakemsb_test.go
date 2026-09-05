package cli

// A fake, scriptable `msb` for this package's image-command tests, following the re-exec-the-
// test-binary pattern internal/sandbox/fakemsb_test.go and internal/orchestrator/fakemsb_test.go
// use at their own layers: TestMain recognizes a real msb verb as argv[1] and dispatches to
// runFakeMsb instead of running the test suite, rather than a marker on the environment (a marker
// set on the outer `go test` process would never reach the re-exec'd child, since sandbox.Client
// hands the msb child a closed env allowlist — see internal/sandbox/fakemsb_test.go's doc comment
// for the full reasoning).
//
// Unlike those two, `krayt image ls/rm/prune` issue more than one distinct invocation of the same
// verb (`images --format json` for `ls`/`prune`'s own listing vs `images -q` for shell
// completion), so a response is looked up by the full argv joined with a space first, falling
// back to just the verb (args[0]), then to Default — a test can script broadly or precisely as it
// needs.
//
// HOME doubles as the fixture directory, exactly as the other two fakes do: it's in msb's
// child-env allowlist, so it's genuinely forwarded to every invocation.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMsbVerbs is the complete set of first-argv-token values `krayt image` ever issues.
var fakeMsbVerbs = map[string]bool{
	"images": true,
	"rmi":    true,
	"image":  true,
	"pull":   true,
}

// testBinPath is this test binary's own path, captured once before m.Run() so tests can point
// sandbox.BinEnv at it.
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

// fakeResponse is one scripted reply for one msb invocation.
type fakeResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// fakeScript is the fixture a test writes to $HOME before invoking a command. Responses is keyed
// by either the full argv (joined with a space) or just the verb (args[0]) — see the package doc.
// Default answers anything not explicitly scripted.
type fakeScript struct {
	Responses map[string]fakeResponse `json:"responses"`
	Default   fakeResponse            `json:"default"`
}

// fakeCall is one recorded invocation of the fake, appended to fakeCallsFile.
type fakeCall struct {
	Args []string `json:"args"`
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

// runFakeMsb is this test binary re-exec'd as `msb`: it records its own argv under $HOME, then
// replies with whatever fakeScript says for that invocation (or Default).
func runFakeMsb() int {
	home := os.Getenv("HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "fake-msb: HOME not set — cannot find its fixture/recording dir")
		return 1
	}
	args := os.Args[1:]
	appendFakeCall(home, fakeCall{Args: args})

	script := readFakeScriptOrDefault(home)
	resp := script.Default
	if r, ok := script.Responses[strings.Join(args, " ")]; ok {
		resp = r
	} else if len(args) > 0 {
		if r, ok := script.Responses[args[0]]; ok {
			resp = r
		}
	}

	if resp.Stdout != "" {
		_, _ = fmt.Fprint(os.Stdout, resp.Stdout)
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
