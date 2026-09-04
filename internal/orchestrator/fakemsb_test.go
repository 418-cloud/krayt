package orchestrator_test

// A fake, scriptable `msb` for this package's tests, following the same re-exec-the-test-binary
// pattern internal/sandbox/fakemsb_test.go uses at the driver level (run-tasks-on-microsandbox.md
// decision 4: the orchestrator test seam moves from fakeProvider to a scriptable fake msb).
// TestMain (climit_test.go) recognizes a real msb verb as argv[1] and dispatches here instead of
// running the test suite.
//
// Unlike the driver-level fake, this one has to behave like a real sandbox well enough for
// internal/orchestrator's actual lifecycle (create → copy in → exec helper setup → exec agent →
// exec helper finish → copy out → stop/rm) to produce real artifacts: `create` makes a sandbox
// root directory under $HOME/state/<name>, `copy` moves real bytes in and out of it, and `exec`
// against the krayt-helper path runs the REAL internal/patch functions (the same ones
// cmd/krayt-helper's own runSetup/runFinish call) against paths translated into that root —
// so a test's changes.patch is produced by the genuine bundle→clone→diff pipeline, not a stub.
// The one piece that has no real analogue is "the agent" (there is no real agent image in a unit
// test): exec against the fixed containerEntrypoint path is scripted per test via
// fakeMsbScriptFile, including an optional ask_human exchange dialed for real against the vsock
// route's host path (a plain unix socket in tests) — internal/askclient.OverSocket, the same
// client code krayt-ask itself uses.
//
// HOME doubles as the fixture directory, exactly as the driver-level fake does: it's in msb's
// child-env allowlist, so it's genuinely forwarded to every invocation.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/askclient"
	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/sandbox"
)

var fakeMsbVerbs = map[string]bool{
	"--version": true, "context": true, "create": true, "exec": true,
	"copy": true, "logs": true, "stop": true, "rm": true, "pull": true, "doctor": true,
}

const (
	fakeMsbScriptFile = "fake-msb-script.json"
	fakeMsbCallsFile  = "fake-msb-calls.jsonl"
)

// fakeAskScript describes one ask_human exchange the fake agent performs.
type fakeAskScript struct {
	Prompt         string   `json:"prompt"`
	Choices        []string `json:"choices,omitempty"`
	AnswerFile     string   `json:"answer_file"`                 // relative path under /workspace to record the outcome into
	PostAskSleepMS int      `json:"post_ask_sleep_ms,omitempty"` // simulates the agent still working after the answer arrives
}

// fakeAgentScript describes what the fake "agent" exec (the fixed containerEntrypoint command)
// does: emit log lines, write files into /workspace and/or /output, optionally ask one question,
// then exit. Block simulates a stuck agent for wall-clock-timeout tests — it never returns on its
// own, relying on the real exec.CommandContext to SIGKILL it at the run's deadline, exactly as a
// real wedged agent would be killed. NoOutput simulates a driver failure (ErrMsbFailed): nothing
// is written to either stream and the process exits non-zero.
type fakeAgentScript struct {
	LogLines       []string          `json:"log_lines,omitempty"`
	LogLineDelayMS int               `json:"log_line_delay_ms,omitempty"` // pause between LogLines, for live-streaming tests
	WorkspaceFiles map[string]string `json:"workspace_files,omitempty"`
	OutputFiles    map[string]string `json:"output_files,omitempty"`
	ExitCode       int               `json:"exit_code"`
	NoOutput       bool              `json:"no_output,omitempty"`
	Ask            *fakeAskScript    `json:"ask,omitempty"`
	Block          bool              `json:"block,omitempty"`

	// SleepMS + TimingFile support concurrency tests: the fake sleeps for SleepMS, recording its
	// start/end (nanosecond UnixNano) as one "start end" line appended to TimingFile — a plain
	// host path (not translated into the sandbox root), so a test can compute peak overlap across
	// several concurrent runs the same way internal/orchestrator/climit_test.go's
	// TestAcquireSlotCrossProcess does for two.
	SleepMS    int    `json:"sleep_ms,omitempty"`
	TimingFile string `json:"timing_file,omitempty"`
}

// fakeMsbScript is the complete fixture a test writes to $HOME before calling orchestrator.Run.
type fakeMsbScript struct {
	Agent          fakeAgentScript `json:"agent"`
	CreateExitCode int             `json:"create_exit_code,omitempty"`

	// TranscriptFiles are written into the fake guest's $HOME/.claude/projects/-workspace at
	// create time, keyed by file name — the shape captureTranscript copies out. Absent means the
	// guest has no transcript, which must leave the run untouched rather than failing it.
	TranscriptFiles map[string]string `json:"transcript_files,omitempty"`
}

// fakeGuestHome is the $HOME the fake reports for `sh -c 'printf %s "$HOME"'` and roots its
// transcript under — the real claude-code and krayt-dev images' home, so the path the orchestrator
// builds here is the one it builds in production.
const fakeGuestHome = "/home/agent"

// fakeCall is one recorded invocation of the fake, appended to fakeMsbCallsFile — the basis for
// this package's "no secret value on any argv" and "teardown on every path" assertions.
type fakeCall struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

// newFakeSandbox points a *sandbox.Client at this re-exec'd test binary and writes script to
// home (a fresh per-test directory the caller sets as HOME via t.Setenv).
func newFakeSandbox(t *testing.T, home string, script fakeMsbScript) *sandbox.Client {
	t.Helper()
	if fakeMsbBinPath == "" {
		t.Fatal("fakeMsbBinPath not set — TestMain did not run (are tests being invoked oddly?)")
	}
	t.Setenv(sandbox.BinEnv, fakeMsbBinPath)
	t.Setenv("HOME", home)
	b, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal fake msb script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, fakeMsbScriptFile), b, 0o600); err != nil {
		t.Fatalf("write fake msb script: %v", err)
	}
	return &sandbox.Client{Bin: fakeMsbBinPath}
}

// newFakeSandboxWithLineDelay is newFakeSandbox for the live-streaming tests: the fake agent
// emits lines one at a time with a real pause between them, so a test can observe the log
// growing while the run is still in progress instead of appearing all at once at exit.
func newFakeSandboxWithLineDelay(t *testing.T, home string, lines []string, delay time.Duration) *sandbox.Client {
	t.Helper()
	return newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{
		LogLines: lines, LogLineDelayMS: int(delay.Milliseconds()), ExitCode: 0,
	}})
}

func readFakeMsbCalls(t *testing.T, home string) []fakeCall {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, fakeMsbCallsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fake msb calls: %v", err)
	}
	var calls []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var c fakeCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("unmarshal fake msb call %q: %v", line, err)
		}
		calls = append(calls, c)
	}
	return calls
}

func sandboxRoot(home, name string) string { return filepath.Join(home, "state", name) }

// runFakeMsb is this test binary re-exec'd as `msb`.
func runFakeMsb() int {
	home := os.Getenv("HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "fake-msb: HOME not set")
		return 1
	}
	args := os.Args[1:]
	appendFakeMsbCall(home, fakeCall{Args: args, Env: envMap(os.Environ())})
	script := readFakeMsbScript(home)

	switch args[0] {
	case "--version":
		_, _ = fmt.Fprintln(os.Stdout, "msb 0.6.16")
		return 0
	case "context":
		_, _ = fmt.Fprintln(os.Stdout, `{"backend":"local"}`)
		return 0
	case "create":
		return fakeMsbCreate(home, args[1:], script)
	case "copy":
		return fakeMsbCopy(home, args[1:])
	case "exec":
		return fakeMsbExec(home, args[1:], script)
	case "logs":
		_, _ = fmt.Fprintln(os.Stdout, `{"source":"system","line":"fake msb: boot ok"}`)
		return 0
	case "stop", "rm", "pull", "doctor":
		return 0
	}
	fmt.Fprintf(os.Stderr, "fake-msb: unhandled verb %q\n", args[0])
	return 1
}

func appendFakeMsbCall(home string, call fakeCall) {
	f, err := os.OpenFile(filepath.Join(home, fakeMsbCallsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(call)
	if err != nil {
		return
	}
	_, _ = f.Write(b)
	_, _ = f.WriteString("\n")
}

func readFakeMsbScript(home string) fakeMsbScript {
	b, err := os.ReadFile(filepath.Join(home, fakeMsbScriptFile))
	if err != nil {
		return fakeMsbScript{}
	}
	var s fakeMsbScript
	_ = json.Unmarshal(b, &s)
	return s
}

func envMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if ok {
			m[name] = value
		}
	}
	return m
}

// fakeMsbCreate makes the sandbox root directory and records the --vsock host path (if any) so a
// later exec can dial it for real.
func fakeMsbCreate(home string, args []string, script fakeMsbScript) int {
	if script.CreateExitCode != 0 {
		fmt.Fprintln(os.Stderr, "fake-msb: create scripted to fail")
		return script.CreateExitCode
	}
	var name, vsock string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			i++
			if i < len(args) {
				name = args[i]
			}
		case "--vsock":
			i++
			if i < len(args) {
				vsock = args[i]
			}
		}
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "fake-msb: create missing --name")
		return 1
	}
	root := sandboxRoot(home, name)
	for _, d := range []string{"workspace", "output", ".krayt"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "fake-msb create:", err)
			return 1
		}
	}
	if vsock != "" {
		hostPath, _, _ := strings.Cut(vsock, ":")
		if err := os.WriteFile(filepath.Join(root, ".vsock-host-path"), []byte(hostPath), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "fake-msb create:", err)
			return 1
		}
	}
	writeFakeTranscript(root, script.TranscriptFiles)
	return 0
}

// parseRemote splits a docker-cp-style token into (sandboxName, path, isRemote). A remote token
// looks like "name:/abs/path"; a local one is a plain (always-absolute, in these tests) host path.
func parseRemote(s string) (name, path string, remote bool) {
	if strings.HasPrefix(s, "/") {
		return "", s, false
	}
	n, p, ok := strings.Cut(s, ":")
	if !ok {
		return "", s, false
	}
	return n, p, true
}

func fakeMsbCopy(home string, args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "fake-msb: copy wants exactly 2 args")
		return 1
	}
	srcName, srcPath, srcRemote := parseRemote(args[0])
	dstName, dstPath, dstRemote := parseRemote(args[1])
	switch {
	case srcRemote && !dstRemote:
		from := filepath.Join(sandboxRoot(home, srcName), srcPath)
		to := filepath.Join(dstPath, filepath.Base(srcPath))
		return copyPathResult(from, to)
	case !srcRemote && dstRemote:
		to := filepath.Join(sandboxRoot(home, dstName), dstPath)
		// Real msb does NOT create a missing parent directory in the guest; it fails with
		// this exact error. The fake used to MkdirAll it, which is why the suite happily
		// passed while every real run died on the first copy into /task.
		if fi, err := os.Stat(filepath.Dir(to)); err != nil || !fi.IsDir() {
			fmt.Fprintln(os.Stderr, "error: sandbox fs error: open: No such file or directory (os error 2)")
			return 1
		}
		return copyPathResult(srcPath, to)
	default:
		fmt.Fprintln(os.Stderr, "fake-msb: copy needs exactly one remote side")
		return 1
	}
}

func copyPathResult(from, to string) int {
	if err := copyPath(from, to); err != nil {
		fmt.Fprintln(os.Stderr, "fake-msb copy:", err)
		return 1
	}
	return 0
}

func copyPath(from, to string) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return copyDirRecursive(from, to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	mode := fi.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	return os.WriteFile(to, b, mode)
}

func copyDirRecursive(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}

func fakeMsbExec(home string, args []string, script fakeMsbScript) int {
	i := 0
	var name string
	for i < len(args) {
		switch args[i] {
		case "--user":
			i += 2
		case "--stream":
			i++
		default:
			name = args[i]
			i++
			goto afterFlags
		}
	}
afterFlags:
	if name == "" || i >= len(args) || args[i] != "--" {
		fmt.Fprintln(os.Stderr, "fake-msb: exec malformed argv", args)
		return 1
	}
	cmd := args[i+1:]
	root := sandboxRoot(home, name)

	switch {
	case len(cmd) >= 2 && strings.HasSuffix(cmd[0], "/krayt-helper") && cmd[1] == "setup":
		return fakeHelperSetup(root, cmd[2:])
	case len(cmd) >= 2 && strings.HasSuffix(cmd[0], "/krayt-helper") && cmd[1] == "finish":
		return fakeHelperFinish(root, cmd[2:])
	// The $HOME probe captureTranscript issues before copying a transcript out. Emits on stdout
	// and exits 0 — Exec treats a non-zero exit with no output as a driver failure, so a probe
	// that stays silent would be misreported.
	case len(cmd) == 3 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(cmd[2], "$HOME"):
		fmt.Print(fakeGuestHome)
		return 0
	case len(cmd) >= 1 && cmd[0] == "mkdir":
		return fakeMkdir(root, cmd[1:])
	case len(cmd) >= 1 && cmd[0] == "chmod":
		return fakeChmod(root, cmd[1:])
	case len(cmd) == 1 && cmd[0] == "/usr/local/bin/krayt-agent-entrypoint":
		return fakeAgentExec(root, script.Agent)
	default:
		fmt.Fprintf(os.Stderr, "fake-msb: exec unrecognized command %v\n", cmd)
		return 1
	}
}

func inSandbox(root, p string) string { return filepath.Join(root, p) }

// writeFakeTranscript seeds the fake guest's transcript dir, mirroring where Claude Code writes
// one: $HOME/.claude/projects/<slug-of-cwd>/<session>.jsonl, cwd being /workspace in the sandbox.
func writeFakeTranscript(root string, files map[string]string) {
	if len(files) == 0 {
		return
	}
	dir := inSandbox(root, filepath.Join(fakeGuestHome, ".claude", "projects", "-workspace"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	for name, content := range files {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}
}

func fakeHelperSetup(root string, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bundle := fs.String("bundle", "", "")
	workspace := fs.String("workspace", "", "")
	patchGitFlag := fs.String("patch-git", "", "")
	agentUser := fs.String("agent-user", "", "")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	ctx := context.Background()
	ws, pg, bnd := inSandbox(root, *workspace), inSandbox(root, *patchGitFlag), inSandbox(root, *bundle)
	baseline, err := patch.Ingest(ctx, bnd, ws, patch.DefaultIdentity)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-msb helper setup:", err)
		return 1
	}
	if err := patch.SetupPatchGit(ws, pg); err != nil {
		fmt.Fprintln(os.Stderr, "fake-msb helper setup:", err)
		return 1
	}
	if err := patch.MakeContainerWritable(ws); err != nil {
		fmt.Fprintln(os.Stderr, "fake-msb helper setup:", err)
		return 1
	}
	return encodeJSON(map[string]any{
		"baseline": baseline, "workspace": *workspace, "patch_git": *patchGitFlag, "agent_user": *agentUser,
	})
}

func fakeHelperFinish(root string, args []string) int {
	fs := flag.NewFlagSet("finish", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	workspace := fs.String("workspace", "", "")
	patchGitFlag := fs.String("patch-git", "", "")
	baseline := fs.String("baseline", "", "")
	out := fs.String("out", "", "")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	ctx := context.Background()
	ws, pg, outDir := inSandbox(root, *workspace), inSandbox(root, *patchGitFlag), inSandbox(root, *out)
	if err := os.MkdirAll(outDir, 0o777); err != nil {
		fmt.Fprintln(os.Stderr, "fake-msb helper finish:", err)
		return 1
	}
	diff, err := patch.Diff(ctx, pg, ws, *baseline)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-msb helper finish:", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(outDir, "changes.patch"), diff, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fake-msb helper finish:", err)
		return 1
	}
	hasCommits, err := patch.BundleCommits(ctx, pg, ws, *baseline, filepath.Join(outDir, "commits.bundle"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-msb helper finish:", err)
		return 1
	}
	return encodeJSON(map[string]any{"baseline": *baseline, "commits_bundle": hasCommits, "diff_bytes": len(diff)})
}

func encodeJSON(v any) int {
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "fake-msb: encode:", err)
		return 1
	}
	return 0
}

// fakeMkdir implements the `mkdir -p DIR...` the copy-in step runs as root before any copy.
func fakeMkdir(root string, args []string) int {
	made := 0
	for _, p := range args {
		if p == "-p" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			fmt.Fprintf(os.Stderr, "fake-msb mkdir: not an absolute guest path: %q\n", p)
			return 1
		}
		if err := os.MkdirAll(inSandbox(root, p), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "fake-msb mkdir:", err)
			return 1
		}
		made++
	}
	if made == 0 {
		fmt.Fprintln(os.Stderr, "fake-msb mkdir: no operand")
		return 1
	}
	return 0
}

// fakeChmod handles both forms the orchestrator uses: a symbolic `+x` and an octal mode.
func fakeChmod(root string, args []string) int {
	mode := os.FileMode(0o755)
	for _, p := range args {
		if p == "+x" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			m, err := strconv.ParseUint(p, 8, 32)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fake-msb chmod: unrecognized operand %q\n", p)
				return 1
			}
			mode = os.FileMode(m)
			continue
		}
		if err := os.Chmod(inSandbox(root, p), mode); err != nil {
			fmt.Fprintln(os.Stderr, "fake-msb chmod:", err)
			return 1
		}
	}
	return 0
}

// fakeAgentExec simulates the one command the agent step ever runs: the fixed
// containerEntrypoint path. It writes scripted files into the fake sandbox's /workspace and
// /output, optionally performs one real ask_human exchange over the vsock route's recorded host
// path, and exits with the scripted code.
func fakeAgentExec(root string, script fakeAgentScript) int {
	if script.Block {
		select {} // killed by the real exec.CommandContext at the run's deadline
	}
	if script.TimingFile != "" {
		start := time.Now().UnixNano()
		if script.SleepMS > 0 {
			time.Sleep(time.Duration(script.SleepMS) * time.Millisecond)
		}
		end := time.Now().UnixNano()
		if f, err := os.OpenFile(script.TimingFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = fmt.Fprintf(f, "%d %d\n", start, end)
			_ = f.Close()
		}
	}
	if script.NoOutput {
		return 1 // nothing observed on either stream -> the driver reports ErrMsbFailed
	}
	for i, line := range script.LogLines {
		if i > 0 && script.LogLineDelayMS > 0 {
			time.Sleep(time.Duration(script.LogLineDelayMS) * time.Millisecond)
		}
		_, _ = fmt.Fprintln(os.Stdout, line)
	}
	ws := filepath.Join(root, "workspace")
	out := filepath.Join(root, "output")
	writeAll := func(dir string, files map[string]string) {
		for name, content := range files {
			p := filepath.Join(dir, name)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, []byte(content), 0o644)
		}
	}
	writeAll(ws, script.WorkspaceFiles)

	if script.Ask != nil {
		outcome := "NO_HUMAN_ANSWER"
		if hostPathBytes, err := os.ReadFile(filepath.Join(root, ".vsock-host-path")); err == nil {
			resp, noAnswer, aerr := askclient.OverSocket(strings.TrimSpace(string(hostPathBytes)), script.Ask.Prompt, script.Ask.Choices)
			if aerr == nil && !noAnswer {
				outcome = resp
			}
		}
		if script.Ask.PostAskSleepMS > 0 {
			time.Sleep(time.Duration(script.Ask.PostAskSleepMS) * time.Millisecond)
		}
		if script.Ask.AnswerFile != "" {
			p := filepath.Join(ws, script.Ask.AnswerFile)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, []byte(outcome+"\n"), 0o644)
		}
	}

	writeAll(out, script.OutputFiles)
	return script.ExitCode
}
