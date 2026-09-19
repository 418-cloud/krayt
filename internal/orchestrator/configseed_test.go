package orchestrator_test

// Tests for §7's new config-seed step (seed-agent-first-run-config.md): applyConfigSeeds, wired
// into both Run (before the agent exec) and Shell (before the tty attach), against the fake msb
// extended (fakemsb_test.go) to actually read/write files inside the fake sandbox root and honor
// a create-time --env value for the DirEnv probe.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/task"
)

// fakeSeedFilePath is where a ConfigSeed with the given DirEnv/Path ends up inside the fake
// sandbox root, mirroring guestBaseDir's resolution: DirEnv's recorded --env value if set, else
// fakeGuestHome.
func fakeSeedFilePath(home, id, dirEnvValue, relPath string) string {
	base := dirEnvValue
	if base == "" {
		base = fakeGuestHome
	}
	return filepath.Join(sandboxRoot(home, "krayt-"+id), base, relPath)
}

// writeFixture pre-seeds a file inside the fake sandbox root BEFORE the sandbox "exists" —
// legal because fakeMsbCreate only ever adds directories, never wipes ones already there, so a
// fixture written ahead of time survives Create untouched.
func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// containsArgSubstring reports whether any element of args contains sub.
func containsArgSubstring(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

func hasArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// isSeedWriteCall/isSeedReadCall/isTTYCall/isAgentExecCall classify one recorded fake-msb call by
// its argv, for ordering and presence assertions.
func isSeedWriteCall(c fakeCall) bool {
	return len(c.Args) > 0 && c.Args[0] == "exec" && containsArgSubstring(c.Args, "krayt-config-seed")
}
func isSeedReadCall(c fakeCall) bool {
	return len(c.Args) > 0 && c.Args[0] == "exec" && containsArgSubstring(c.Args, "krayt-seed-read")
}
func isTTYCall(c fakeCall) bool {
	return len(c.Args) > 0 && c.Args[0] == "exec" && hasArg(c.Args, "--tty")
}
func isAgentExecCall(c fakeCall) bool {
	return len(c.Args) > 0 && c.Args[0] == "exec" && hasArg(c.Args, "/usr/local/bin/krayt-agent-entrypoint")
}

// TestConfigSeedsWrittenBeforeAgentExecInRun proves the merged file lands in the guest, with the
// declared defaults, and that the write happens (as the agent user) strictly before the agent's
// own exec — decision 4/5.
func TestConfigSeedsWrittenBeforeAgentExecInRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})

	spec := task.RunSpec{
		ID: "run_seed_order", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(fakeSeedFilePath(home, spec.ID, "", ".claude.json"))
	if err != nil {
		t.Fatalf("seed file not written: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("seed file is not valid JSON: %v (%s)", err, got)
	}
	if obj["hasCompletedOnboarding"] != true {
		t.Errorf("seed file = %s, want hasCompletedOnboarding: true", got)
	}

	calls := readFakeMsbCalls(t, home)
	writeIdx, agentIdx := -1, -1
	var userAtWrite string
	for i, c := range calls {
		if isSeedWriteCall(c) {
			writeIdx = i
			for j, a := range c.Args {
				if a == "--user" && j+1 < len(c.Args) {
					userAtWrite = c.Args[j+1]
				}
			}
		}
		if isAgentExecCall(c) {
			agentIdx = i
		}
	}
	if writeIdx == -1 {
		t.Fatal("no seed write call recorded")
	}
	if agentIdx == -1 {
		t.Fatal("no agent exec call recorded")
	}
	if writeIdx >= agentIdx {
		t.Errorf("seed write (call %d) did not happen before the agent exec (call %d)", writeIdx, agentIdx)
	}
	if userAtWrite != "agent" {
		t.Errorf("seed write ran as user %q, want agent (decision 4: never root)", userAtWrite)
	}
}

// TestConfigSeedsWrittenBeforeTTYAttachInShell mirrors the above for `krayt shell` — the write
// must precede ExecTTY, not follow it.
func TestConfigSeedsWrittenBeforeTTYAttachInShell(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})

	spec := task.RunSpec{
		ID: "run_seed_shell_order", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir, false, nil); err != nil {
		t.Fatalf("Shell: %v", err)
	}

	if _, err := os.ReadFile(fakeSeedFilePath(home, spec.ID, "", ".claude.json")); err != nil {
		t.Fatalf("seed file not written: %v", err)
	}

	calls := readFakeMsbCalls(t, home)
	writeIdx, ttyIdx := -1, -1
	for i, c := range calls {
		if isSeedWriteCall(c) {
			writeIdx = i
		}
		if isTTYCall(c) {
			ttyIdx = i
		}
	}
	if writeIdx == -1 {
		t.Fatal("no seed write call recorded")
	}
	if ttyIdx == -1 {
		t.Fatal("no tty exec call recorded")
	}
	if writeIdx >= ttyIdx {
		t.Errorf("seed write (call %d) did not happen before the tty attach (call %d)", writeIdx, ttyIdx)
	}
}

// TestConfigSeedKeepsExistingKeys proves the fill-in-never-override rule end to end: an existing
// guest file's own keys (including an explicit false that contradicts the default) survive the
// merge untouched.
func TestConfigSeedKeepsExistingKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	id := "run_seed_keep"
	fixturePath := fakeSeedFilePath(home, id, "", ".claude.json")
	writeFixture(t, fixturePath, `{"otherKey":"keep-me","hasCompletedOnboarding":false}`)

	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})
	spec := task.RunSpec{
		ID: id, ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("not valid JSON: %v (%s)", err, got)
	}
	if obj["otherKey"] != "keep-me" {
		t.Errorf("otherKey lost: %s", got)
	}
	if obj["hasCompletedOnboarding"] != false {
		t.Errorf("existing hasCompletedOnboarding:false was overwritten: %s", got)
	}
}

// TestConfigSeedUnchangedPerformsNoWrite proves that when the merge changes nothing, no write
// exec runs at all — only the read.
func TestConfigSeedUnchangedPerformsNoWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	id := "run_seed_unchanged"
	fixturePath := fakeSeedFilePath(home, id, "", ".claude.json")
	writeFixture(t, fixturePath, `{"hasCompletedOnboarding":true}`)

	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})
	spec := task.RunSpec{
		ID: id, ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := readFakeMsbCalls(t, home)
	sawRead := false
	for _, c := range calls {
		if isSeedReadCall(c) {
			sawRead = true
		}
		if isSeedWriteCall(c) {
			t.Error("a write exec ran even though the merge was unchanged")
		}
	}
	if !sawRead {
		t.Error("expected a read (cat) call even for an unchanged merge")
	}
}

// TestConfigSeedInvalidJSONLeftByteIdenticalAndWarns proves decision 4.3: a guest file krayt
// cannot parse is never overwritten, a warning is printed, and the run still succeeds.
func TestConfigSeedInvalidJSONLeftByteIdenticalAndWarns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	id := "run_seed_invalid_json"
	fixturePath := fakeSeedFilePath(home, id, "", ".claude.json")
	const original = `{not valid json`
	writeFixture(t, fixturePath, original)

	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})
	spec := task.RunSpec{
		ID: id, ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	var warn bytes.Buffer
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb, Warn: &warn}, spec, runDir); err != nil {
		t.Fatalf("Run: %v (invalid JSON must not fail the run)", err)
	}

	got, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("file was modified: got %q, want the original %q untouched", got, original)
	}
	if !strings.Contains(warn.String(), "config seed") {
		t.Errorf("no warning printed: %q", warn.String())
	}
}

// TestConfigSeedUnreadableFileIsNeverOverwritten proves the other half of decision 4.2: only an
// ABSENT file means "{}". A file that exists but the sandbox user cannot read (a root-owned 0600
// config in a user-writable directory) must not be merged over — `mv -f` would replace it even
// though nothing could read what it held, silently destroying state the fill-in-never-overwrite
// contract promises to preserve. The seed is skipped with a warning and the run still succeeds.
func TestConfigSeedUnreadableFileIsNeverOverwritten(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	id := "run_seed_unreadable"
	fixturePath := fakeSeedFilePath(home, id, "", ".claude.json")
	const original = `{"hasCompletedOnboarding":false,"secretish":"keep me"}`
	writeFixture(t, fixturePath, original)

	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}, FailConfigSeedRead: true})
	spec := task.RunSpec{
		ID: id, ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	var warn bytes.Buffer
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb, Warn: &warn}, spec, runDir); err != nil {
		t.Fatalf("Run: %v (an unreadable seed file must not fail the run)", err)
	}

	got, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("an unreadable file was overwritten: got %q, want the original %q untouched", got, original)
	}
	for _, c := range readFakeMsbCalls(t, home) {
		if isSeedWriteCall(c) {
			t.Errorf("a write exec ran for a file krayt could not read: %v", c.Args)
		}
	}
	if !strings.Contains(warn.String(), "config seed") {
		t.Errorf("warning = %q, want a config-seed warning naming the failed read", warn.String())
	}
}

// TestConfigSeedInvalidPathRejectedBeforeAnyExec: adapter.ConfigSeed.Path is documented "clean,
// relative, and carries no '..'", and applyConfigSeed is where that contract has to hold, since
// it is the one place Path is joined to a base directory the guest helped resolve. A Path that
// escapes is refused before any exec, warned about, and does not fail the run — the same
// treatment an invalid DirEnv gets.
func TestConfigSeedInvalidPathRejectedBeforeAnyExec(t *testing.T) {
	for _, bad := range []string{"../escaped.json", "/etc/passwd", "a/../../escaped.json", ""} {
		t.Run(bad, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			home := t.TempDir()
			sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})
			spec := task.RunSpec{
				ID: "run_seed_bad_path", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
				BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
				ConfigSeeds: []task.ConfigSeed{{Path: bad, Defaults: map[string]any{"x": true}}},
			}
			runDir := filepath.Join(t.TempDir(), "run")
			var warn bytes.Buffer
			res, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb, Warn: &warn}, spec, runDir)
			if err != nil {
				t.Fatalf("Run: %v (an invalid Path must not fail the run)", err)
			}
			if res.ExitCode != 0 {
				t.Errorf("exit code = %d, want 0", res.ExitCode)
			}
			if !strings.Contains(warn.String(), "invalid Path") {
				t.Errorf("warning = %q, want it to name the invalid Path", warn.String())
			}
			for _, c := range readFakeMsbCalls(t, home) {
				if isSeedReadCall(c) || isSeedWriteCall(c) {
					t.Errorf("an exec ran for the invalid-Path seed: %v", c.Args)
				}
			}
		})
	}
}

// TestConfigSeedWriteScriptSetsRestrictiveUmask guards the one property of writeSeedFile's script
// that no other test can observe through the fake guest (which does its own os.WriteFile): the
// replacement is a new inode, so without a restrictive umask `mv -f` would carry the guest
// shell's default 0644 onto a config that was 0600 — widening a file that holds auth state.
func TestConfigSeedWriteScriptSetsRestrictiveUmask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})
	spec := task.RunSpec{
		ID: "run_seed_umask", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sawWrite := false
	for _, c := range readFakeMsbCalls(t, home) {
		if !isSeedWriteCall(c) {
			continue
		}
		sawWrite = true
		if !containsArgSubstring(c.Args, "umask 077") {
			t.Errorf("write script does not set a restrictive umask: %v", c.Args)
		}
	}
	if !sawWrite {
		t.Error("expected a seed write exec")
	}
}

// TestConfigSeedFailingExecDoesNotFailRunOrSession proves decision 6: a seed whose write exec
// genuinely fails (not a validation rejection — the guest command itself errors) is only a
// warning, never a failed run.
func TestConfigSeedFailingExecDoesNotFailRunOrSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}, FailConfigSeedWrite: true})
	spec := task.RunSpec{
		ID: "run_seed_exec_fails", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	var warn bytes.Buffer
	res, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb, Warn: &warn}, spec, runDir)
	if err != nil {
		t.Fatalf("Run: %v (a failing seed exec must not fail the run)", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(warn.String(), "config seed") {
		t.Errorf("warning = %q, want a config-seed warning naming the failure", warn.String())
	}
}

// TestConfigSeedInvalidDirEnvRejectedBeforeAnyExecAndRunStillSucceeds: decision 4.1 requires an
// invalid DirEnv name be rejected before it goes anywhere near a guest exec, and decision 6
// requires that rejection to be a warning, never a failed run.
func TestConfigSeedInvalidDirEnvRejectedBeforeAnyExecAndRunStillSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})
	spec := task.RunSpec{
		ID: "run_seed_bad_direnv", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			DirEnv:   "BAD-NAME", // hyphen is not a valid shell identifier character
			Path:     "settings.json",
			Defaults: map[string]any{"x": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	var warn bytes.Buffer
	res, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb, Warn: &warn}, spec, runDir)
	if err != nil {
		t.Fatalf("Run: %v (an invalid DirEnv must not fail the run)", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(warn.String(), "invalid DirEnv") {
		t.Errorf("warning = %q, want it to name the invalid DirEnv", warn.String())
	}

	for _, c := range readFakeMsbCalls(t, home) {
		if isSeedReadCall(c) || isSeedWriteCall(c) {
			t.Errorf("an exec ran for the invalid-DirEnv seed: %v", c.Args)
		}
		if containsArgSubstring(c.Args, "BAD-NAME") {
			t.Errorf("the invalid DirEnv name reached an exec argv: %v", c.Args)
		}
	}
}

// TestConfigSeedDirEnvRedirectsPath proves decision 4.1: when the guest environment has the
// named DirEnv set (here, via spec.Env reaching CreateSpec.Env), the seed file is written under
// that directory instead of $HOME.
func TestConfigSeedDirEnvRedirectsPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})
	spec := task.RunSpec{
		ID: "run_seed_direnv", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		Env: map[string]string{"MY_CONFIG_DIR": "/custom/base"},
		ConfigSeeds: []task.ConfigSeed{{
			DirEnv:   "MY_CONFIG_DIR",
			Path:     "settings.json",
			Defaults: map[string]any{"x": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	redirected := fakeSeedFilePath(home, spec.ID, "/custom/base", "settings.json")
	if _, err := os.ReadFile(redirected); err != nil {
		t.Fatalf("seed file not written under the DirEnv-redirected path %s: %v", redirected, err)
	}
	underHome := fakeSeedFilePath(home, spec.ID, "", "settings.json")
	if _, err := os.ReadFile(underHome); err == nil {
		t.Errorf("seed file was ALSO written under $HOME (%s); DirEnv should have redirected it entirely", underHome)
	}
}

// TestAttachShellPerformsNoSeedExec proves decision 5: AttachShell never seeds — the sandbox
// already exists and the human may have changed these files since. Established by attaching to a
// --keep session (whose initial Shell call did seed, once) and confirming AttachShell adds no
// further seed-related exec calls.
func TestAttachShellPerformsNoSeedExec(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})
	spec := task.RunSpec{
		ID: "run_seed_attach", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, Network: allowlistAll,
		ConfigSeeds: []task.ConfigSeed{{
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir, true, nil); err != nil {
		t.Fatalf("Shell (initial, --keep): %v", err)
	}

	before := len(readFakeMsbCalls(t, home))

	if _, err := orchestrator.AttachShell(ctx, orchestrator.Deps{Sandbox: sb}, runDir, "", nil); err != nil {
		t.Fatalf("AttachShell: %v", err)
	}

	after := readFakeMsbCalls(t, home)
	for _, c := range after[before:] {
		if isSeedReadCall(c) || isSeedWriteCall(c) {
			t.Errorf("AttachShell performed a seed exec: %v", c.Args)
		}
	}
}
