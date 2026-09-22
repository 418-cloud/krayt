package cli

// Tests for `krayt code`'s CLI surface (add-vscode-remote-ssh-session.md). Everything here runs
// offline against the scriptable fake `msb` (fakemsb_test.go) — no real sandbox, no sshd, no ssh.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
	"github.com/418-cloud/krayt/internal/task"
)

// seedCodeRun writes a minimal `kind: code` run dir, the shape `krayt code --stdio`, `ls`, `stop`,
// `patch` and the doctor orphan check all read.
func seedCodeRun(t *testing.T, repo, id, state string) string {
	t.Helper()
	sd := filepath.Join(repo, ".krayt")
	runDir := orchestrator.RunDir(sd, id)
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"` + id + `","kind":"code","state":"` + state + `","exit_code":0,"image_ref":"img:1",` +
		`"started_at":"2026-07-01T00:00:00Z","pid":0,"sandbox_name":"krayt-` + id + `","sandbox_user":"agent"}`
	write(t, filepath.Join(runDir, "meta.json"), meta)
	return runDir
}

// TestSSHDExecArgsUseStreamAsRoot is the golden argv for the one exec that carries the SSH
// protocol. `--stream` and `--tty` are mutually exclusive by msb's own clap config, and a pty
// would corrupt the binary stream inside the first round trip — so --tty must never appear here
// (decision 2), and --user root is decision 3.
func TestSSHDExecArgsUseStreamAsRoot(t *testing.T) {
	spec := sshdExecSpec("krayt-run_abc", nil, nil, nil)
	got := spec.Args()
	want := []string{
		"exec", "--user", "root", "--stream", "krayt-run_abc", "--",
		"/usr/sbin/sshd", "-i", "-e", "-f", "/.krayt/ssh/sshd_config",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sshd exec argv =\n  %v\nwant\n  %v", got, want)
	}
	for _, forbidden := range []string{"--tty", "--workdir"} {
		for _, a := range got {
			if a == forbidden {
				t.Errorf("sshd exec argv contains %q — this path needs --stream's separated, byte-exact pipes", forbidden)
			}
		}
	}
}

// TestStdioRoutesSSHDStderrAwayFromStdout is decision 4's regression test, and the one bug that
// would break every single connection if it regressed: stdout carries the SSH protocol, so a byte
// sshd writes to stderr (it is invoked with -e) must never appear there. It goes to the run's
// logs/sshd.log instead.
func TestStdioRoutesSSHDStderrAwayFromStdout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sandbox.BinEnv, testBinPath)
	const protocol = "SSH-2.0-OpenSSH_9.9\r\n\x00\x01binary"
	const diagnostic = "debug1: sshd version OpenSSH_9.9\nAccepted publickey for agent\n"
	writeFakeScript(t, home, fakeScript{Default: fakeResponse{Stdout: protocol, Stderr: diagnostic}})

	repo := t.TempDir()
	runDir := seedCodeRun(t, repo, "run_stdio", orchestrator.StateRunning)

	var stdout bytes.Buffer
	cmd := newCodeCmd()
	cmd.SetOut(&stdout)
	cmd.SetIn(strings.NewReader("SSH-2.0-client\r\n"))
	cmd.SetArgs([]string{"--repo", repo, "--stdio", "run_stdio"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("krayt code --stdio: %v", err)
	}

	if got := stdout.String(); got != protocol {
		t.Errorf("stdout = %q, want exactly the SSH byte stream %q", got, protocol)
	}
	if strings.Contains(stdout.String(), "debug1") || strings.Contains(stdout.String(), "Accepted publickey") {
		t.Error("sshd's stderr leaked into the SSH byte stream — every connection would fail")
	}

	logBytes, err := os.ReadFile(filepath.Join(runDir, "logs", sshdLogName))
	if err != nil {
		t.Fatalf("read sshd.log: %v", err)
	}
	if string(logBytes) != diagnostic {
		t.Errorf("sshd.log = %q, want sshd's stderr %q", logBytes, diagnostic)
	}

	// And the exec really was the sshd one, as root, streamed.
	var sawExec bool
	for _, c := range readFakeCalls(t, home) {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			sawExec = true
			if !containsArg(c.Args, "--stream") || !containsArg(c.Args, "root") {
				t.Errorf("exec argv = %v, want --stream and --user root", c.Args)
			}
		}
	}
	if !sawExec {
		t.Fatal("no `msb exec` observed — the ProxyCommand never reached the sandbox")
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestStdioRefusesNonCodeAndFinishedSessions: the ProxyCommand must fail loudly rather than exec
// sshd against a sandbox that is not a code session's, or one that no longer exists.
func TestStdioRefusesNonCodeAndFinishedSessions(t *testing.T) {
	repo := t.TempDir()
	pinMissingMsb(t, t.TempDir())
	seedShellRun(t, repo, "run_shell", orchestrator.StateKept)
	seedCodeRun(t, repo, "run_over", orchestrator.StateDone)

	for _, tc := range []struct{ id, want string }{
		{"run_shell", "is not a `krayt code` session"},
		{"run_over", "has ended"},
		{"run_missing", "no such run"},
	} {
		err := execErr(newCodeCmd(), "--repo", repo, "--stdio", tc.id)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("--stdio %s: err = %v, want one containing %q", tc.id, err, tc.want)
		}
	}
}

// resolveCodeSpecFor is the shared setup for the editor-allowlist tests: bind a real flag set,
// parse args against it, and resolve the spec exactly as `krayt code` does.
func resolveCodeSpecFor(t *testing.T, args ...string) (task.RunSpec, []string) {
	t.Helper()
	var f runFlags
	var cf codeFlags
	cmd := &cobra.Command{Use: "code", RunE: func(*cobra.Command, []string) error { return nil }}
	bindCodeFlags(cmd, &f, &cf)
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	spec, added, err := resolveCodeSpec(cmd, &f, &cf, t.TempDir(), "run_editor")
	if err != nil {
		t.Fatalf("resolveCodeSpec: %v", err)
	}
	return spec, added
}

// TestEditorAllowlistOrderedAfterDNSBeforeDenyGroups pins where decision 14's hosts land in the
// rendered msb policy. msb is first-match-wins within a direction, and the ordering that has
// actually bitten this repo is `allow@dns` before the deny groups — emit the denies first and
// every request in the sandbox fails ENOTFOUND (netpolicy_msb.go's allowDNSArgs comment). So this
// asserts, in order: `allow@dns` still comes first, every editor host is an allow rule after it,
// and — deliberately — the editor allows sit WITH the user's own allows, after the `deny@<group>`
// rules rather than ahead of them. They are public destinations that no deny group matches, so
// their position relative to those denies changes nothing except whether the private-range guard
// still applies to them; putting them first would weaken it for five hosts, which decision 17
// forbids.
func TestEditorAllowlistOrderedAfterDNSBeforeDenyGroups(t *testing.T) {
	spec, added := resolveCodeSpecFor(t, "--image", "img", "--net", "allowlist", "--allow", "api.anthropic.com")
	if len(added) != len(editorAllowHosts) {
		t.Fatalf("added = %v, want all %d editor hosts", added, len(editorAllowHosts))
	}

	args, err := task.NetworkArgs(spec.Network, false)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	idx := func(token string) int {
		for i, a := range args {
			if a == token {
				return i
			}
		}
		return -1
	}
	dns := idx("allow@dns")
	if dns < 0 {
		t.Fatalf("no allow@dns rule in %v", args)
	}
	firstDeny := -1
	for _, g := range []string{"deny@private", "deny@loopback", "deny@link-local", "deny@meta", "deny@multicast", "deny@host"} {
		i := idx(g)
		if i < 0 {
			t.Fatalf("deny group %s missing from %v — decision 17 forbids weakening these", g, args)
		}
		if firstDeny < 0 || i < firstDeny {
			firstDeny = i
		}
	}
	if dns > firstDeny {
		t.Errorf("allow@dns at %d comes after the first deny group at %d — every request in the sandbox would fail ENOTFOUND", dns, firstDeny)
	}
	lastDeny := 0
	for _, g := range []string{"deny@private", "deny@loopback", "deny@link-local", "deny@meta", "deny@multicast", "deny@host"} {
		if i := idx(g); i > lastDeny {
			lastDeny = i
		}
	}
	for _, h := range editorAllowHosts {
		i := idx("allow@" + h)
		if i < 0 {
			t.Errorf("editor host %s is not in the rendered policy: %v", h, args)
			continue
		}
		if i < dns {
			t.Errorf("editor host %s at %d precedes allow@dns at %d", h, i, dns)
		}
		if i < lastDeny {
			t.Errorf("editor host %s at %d precedes a deny group at %d — that would exempt it from the private-range guard", h, i, lastDeny)
		}
	}
	// The user's own entry keeps its place, first among the allows.
	if user, first := idx("allow@api.anthropic.com"), idx("allow@"+editorAllowHosts[0]); user > first {
		t.Errorf("the user's own allow at %d comes after krayt's added ones at %d", user, first)
	}
}

// TestEditorAllowlistOmittedWithFlag: --no-editor-allow leaves the user's policy exactly as they
// wrote it, and the banner says so rather than going quiet.
func TestEditorAllowlistOmittedWithFlag(t *testing.T) {
	spec, added := resolveCodeSpecFor(t, "--image", "img", "--net", "allowlist", "--allow", "api.anthropic.com", "--no-editor-allow")
	if len(added) != 0 {
		t.Errorf("added = %v, want none with --no-editor-allow", added)
	}
	if got, want := spec.Network.Allow, []string{"api.anthropic.com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("allow = %v, want exactly the user's own %v", got, want)
	}

	var banner bytes.Buffer
	if err := printEditorAllow(&banner, task.NetworkAllowlist, true, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(banner.String(), "--no-editor-allow") {
		t.Errorf("banner = %q, want it to say the allowlist was disabled", banner.String())
	}
}

// TestEditorAllowlistOmittedUnderNetNone: `--net none` renders `--no-net` and zero rules by
// construction, so there is no policy to add to — krayt must add nothing and warn instead of
// pretending the editor will be able to install itself.
func TestEditorAllowlistOmittedUnderNetNone(t *testing.T) {
	spec, added := resolveCodeSpecFor(t, "--image", "img", "--net", "none")
	if len(added) != 0 {
		t.Errorf("added = %v, want none under --net none", added)
	}
	if len(spec.Network.Allow) != 0 {
		t.Errorf("allow = %v, want empty under --net none", spec.Network.Allow)
	}
	args, err := task.NetworkArgs(spec.Network, false)
	if err != nil {
		t.Fatalf("NetworkArgs: %v", err)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "allow@") {
			t.Errorf("--net none rendered %q — a single stray rule punches a hole through no-network", a)
		}
	}

	var banner bytes.Buffer
	if err := printEditorAllow(&banner, task.NetworkNone, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(banner.String(), "cannot download its server") {
		t.Errorf("banner = %q, want the warning that Remote-SSH cannot fetch its server", banner.String())
	}
}

// TestEditorAllowlistSkipsDuplicates: a user who already allowed one of these hosts is not shown
// it as "added by krayt", and it is not listed twice.
func TestEditorAllowlistSkipsDuplicates(t *testing.T) {
	spec, added := resolveCodeSpecFor(t, "--image", "img", "--allow", "marketplace.visualstudio.com")
	if containsArg(added, "marketplace.visualstudio.com") {
		t.Errorf("added = %v, must not claim to have added a host the user already allowed", added)
	}
	n := 0
	for _, h := range spec.Network.Allow {
		if h == "marketplace.visualstudio.com" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("marketplace.visualstudio.com appears %d times in %v, want 1", n, spec.Network.Allow)
	}
}

// TestLsShowsCodeSessions — decision 9's audit: a code session must be listable, with its kind.
func TestLsShowsCodeSessions(t *testing.T) {
	repo := t.TempDir()
	seedCodeRun(t, repo, "run_code", orchestrator.StateKept)
	seedShellRun(t, repo, "run_shell", orchestrator.StateDone)
	seedRun(t, repo, "run_plain", orchestrator.StateDone)

	out := run(t, newLsCmd(), "--repo", repo)
	for _, want := range []string{"run_code", "code", "run_shell", "shell", "run_plain", "run"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output missing %q; got:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "run_code") && !strings.HasSuffix(strings.TrimSpace(line), "code") {
			t.Errorf("ls line for the code session = %q, want it to end in the kind `code`", line)
		}
	}
}

// TestStopDestroysKeptCodeSession — `krayt code --keep` leaves the identical state a kept shell
// session does, so `krayt stop` must destroy its sandbox by name and flip the record to `done`.
// It must also drop the session's alias out of the aggregate ssh config: an alias that resolves to
// a sandbox that no longer exists is worse than no alias at all.
func TestStopDestroysKeptCodeSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sandbox.BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Default: fakeResponse{ExitCode: 0}})

	repo := t.TempDir()
	runDir := seedCodeRun(t, repo, "run_kept_code", orchestrator.StateKept)
	sd := filepath.Join(repo, ".krayt")
	// A per-run ssh config and an aggregate that Includes it, as a live session would have left.
	if err := os.MkdirAll(filepath.Join(runDir, "ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(runDir, "ssh", "config"), "Host krayt-run_kept_code\n")
	if err := orchestrator.RefreshAggregateSSHConfig(sd); err != nil {
		t.Fatal(err)
	}
	aggregate := filepath.Join(sd, "ssh", "config")
	if b, err := os.ReadFile(aggregate); err != nil || !strings.Contains(string(b), "run_kept_code") {
		t.Fatalf("aggregate config = %q (err %v), want it to Include the live session first", b, err)
	}

	out := run(t, newStopCmd(), "--repo", repo, "run_kept_code")
	if !strings.Contains(out, "stopped kept code session run_kept_code") {
		t.Errorf("stop output = %q, want it to name the kind and the run", out)
	}

	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != orchestrator.StateDone {
		t.Errorf("State after stop = %q, want %q", rec.State, orchestrator.StateDone)
	}
	var sawStop, sawRm bool
	for _, c := range readFakeCalls(t, home) {
		switch c.Args[0] {
		case "stop":
			sawStop = true
		case "rm":
			sawRm = true
		}
	}
	if !sawStop || !sawRm {
		t.Errorf("sawStop=%v sawRm=%v, want both true", sawStop, sawRm)
	}

	b, err := os.ReadFile(aggregate)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "runs/run_kept_code") {
		t.Errorf("aggregate config still Includes the stopped session:\n%s", b)
	}
}

// TestPatchLiveCodeSessionRoutesThroughPatchLiveShell — `krayt patch` must re-derive the patch for
// a live code session exactly as it does for a shell one (decision 9's audit of every
// EffectiveKind call site).
func TestPatchLiveCodeSessionRoutesThroughPatchLiveShell(t *testing.T) {
	for _, state := range []string{orchestrator.StateRunning, orchestrator.StateKept} {
		t.Run(state, func(t *testing.T) {
			repo := t.TempDir()
			pinMissingMsb(t, t.TempDir())
			seedCodeRun(t, repo, "run_live_code", state)

			err := execErr(newPatchCmd(), "--repo", repo, "run_live_code")
			if err == nil || !strings.Contains(err.Error(), "patch run") {
				t.Fatalf("err = %v, want the live-session re-derive path's error", err)
			}
			if strings.Contains(err.Error(), "no patch for run") {
				t.Errorf("err = %v, must not be the plain-stat path", err)
			}
		})
	}
}

// TestDoctorDoesNotReportCodeSessionAsOrphan — a kept code session's sandbox is owned by design,
// exactly like a kept shell session's, so the orphan check must stay quiet about it.
func TestDoctorDoesNotReportCodeSessionAsOrphan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sandbox.BinEnv, testBinPath)
	writeFakeScript(t, home, fakeScript{Responses: map[string]fakeResponse{
		"ls --format json": {Stdout: `[{"name":"krayt-run_code_kept"},{"name":"krayt-run_code_live"}]`},
	}})
	stubProcessAlive(t, true)

	repo := t.TempDir()
	seedCodeRun(t, repo, "run_code_kept", orchestrator.StateKept)
	seedCodeRun(t, repo, "run_code_live", orchestrator.StateRunning)

	res := orphanSandboxCheck(context.Background(), repo)
	if !res.ok {
		t.Errorf("orphanSandboxCheck = %+v, want ok=true — a kept/live code session owns its sandbox", res)
	}
}

// TestPrintCodeSessionBlock is Done-when #2 at the printing layer: the block a human reads must
// carry a usable `ssh -F … <alias>`, the one-time Include line, and the vscode-remote:// URI — and
// must not pretend to launch anything (decision 13).
func TestPrintCodeSessionBlock(t *testing.T) {
	var out bytes.Buffer
	err := printCodeSession(&out, orchestrator.CodeSession{
		RunID: "run_abc", Alias: "krayt-run_abc", User: "agent",
		ConfigPath:    "/repo/.krayt/runs/run_abc/ssh/config",
		AggregatePath: "/repo/.krayt/ssh/config",
		SSHCommand:    `ssh -F "/repo/.krayt/runs/run_abc/ssh/config" krayt-run_abc`,
		IncludeLine:   "Include /repo/.krayt/ssh/config",
		VSCodeURI:     "vscode-remote://ssh-remote+krayt-run_abc/workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		`ssh -F "/repo/.krayt/runs/run_abc/ssh/config" krayt-run_abc`,
		"Include /repo/.krayt/ssh/config",
		"vscode-remote://ssh-remote+krayt-run_abc/workspace",
		"Ctrl-C",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("connection block missing %q:\n%s", want, got)
		}
	}
}

// TestCodeCommandSurface pins the flag set decision 5 and the task's flag list describe: shell's
// flags minus the tty-only ones, --no-editor-allow, and a HIDDEN --stdio (it is a ProxyCommand
// entry point, not a user-facing verb).
func TestCodeCommandSurface(t *testing.T) {
	cmd := newCodeCmd()
	for _, name := range []string{
		"config", "image", "repo", "secrets", "include-dirty", "net", "allow", "bundle-depth",
		"cpus", "memory", "disk", "max-concurrency", "skip-resource-check", "keep",
		"no-editor-allow", "stdio",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("krayt code is missing --%s", name)
		}
	}
	for _, name := range []string{"timeout", "detach", "on-question", "agent", "transcript", "attach", "exec"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("krayt code must not carry --%s (same reasoning as krayt shell)", name)
		}
	}
	if f := cmd.Flags().Lookup("stdio"); f != nil && !f.Hidden {
		t.Error("--stdio must be hidden — it exists for ssh to call, not for a human (decision 5)")
	}
}
