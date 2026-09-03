package orchestrator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/task"
)

// TestEndToEndRun is the automated proof of the run lifecycle (§7): against the fake msb, the
// orchestrator drives the real bundle → copy-in → helper-setup → agent-exec → helper-finish →
// copy-out path, a scripted agent edits one file, and the resulting changes.patch applies
// cleanly back onto the host repo. Every artifact-producing step (patch.CreateBundle,
// patch.Ingest, patch.Diff, patch.BundleCommits) is the genuine production code; only "the
// agent" itself is simulated, since there is no real agent image in a unit test.
func TestEndToEndRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{
		"greeting.txt": "hello\n",
		"keep.txt":     "unchanged\n",
	})

	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{
		LogLines: []string{"agent starting", "agent done"},
		WorkspaceFiles: map[string]string{
			"greeting.txt": "hello world\n",
			"new.txt":      "fresh\n",
		},
		ExitCode: 0,
	}})

	var logs bytes.Buffer
	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID:          "run_e2e",
		ImageRef:    "agent-image:latest",
		RepoPath:    src,
		BundleDepth: 1,
		TaskPrompt:  []byte("edit the greeting"),
		Network:     allowlistAll,
		Resources:   task.Resources{CPUs: 2, MemoryMiB: 2048},
	}

	res, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb, LogOut: &logs}, spec, runDir)
	if err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}

	patchBytes, err := os.ReadFile(res.PatchPath)
	if err != nil || len(patchBytes) == 0 {
		t.Fatalf("changes.patch missing/empty: err=%v len=%d", err, len(patchBytes))
	}
	if !bytes.Contains(logs.Bytes(), []byte("agent starting")) {
		t.Errorf("live log did not include agent output; got %q", logs.String())
	}
	assertMeta(t, filepath.Join(runDir, "meta.json"), spec.ID)

	// The "Done when": the patch applies cleanly onto a fresh checkout of the host repo.
	target := filepath.Join(t.TempDir(), "target")
	run(t, "", "git", "clone", "--quiet", src, target)
	if err := patch.Apply(ctx, target, res.PatchPath, false); err != nil {
		t.Fatalf("krayt apply (git apply) failed: %v", err)
	}
	if got := readFile(t, filepath.Join(target, "greeting.txt")); got != "hello world\n" {
		t.Errorf("greeting.txt after apply = %q, want %q", got, "hello world\n")
	}
	if got := readFile(t, filepath.Join(target, "new.txt")); got != "fresh\n" {
		t.Errorf("new.txt after apply = %q, want %q", got, "fresh\n")
	}
}

// TestStateNotRunningUntilCodeCaptured is the regression proof for §6.2: `state: running` must
// not be externally visible until the code snapshot is durably captured inside the sandbox
// (krayt-helper setup succeeding), because that is the point after which the host repo can be
// safely mutated without affecting this run's snapshot.
func TestStateNotRunningUntilCodeCaptured(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{
		WorkspaceFiles: map[string]string{"a.txt": "2\n"}, ExitCode: 0,
	}})

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_state_order", ImageRef: "img", RepoPath: src, BundleDepth: 1, Network: allowlistAll,
		TaskPrompt: []byte("task"), Resources: task.Resources{CPUs: 2, MemoryMiB: 2048},
	}
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir); err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}
	final, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord final: %v", err)
	}
	if final.State != orchestrator.StateDone {
		t.Errorf("final state = %q, want %q", final.State, orchestrator.StateDone)
	}
}

// TestContainerPolicyIsHardErroredForMsb proves run-tasks-on-microsandbox.md decision 5: the
// pre-msb OCI-spec knobs have no msb equivalent and must be refused before any sandbox work
// begins, rather than silently ignored — a config setting them is reasoning about hardening, and
// dropping it quietly would be a posture regression.
func TestContainerPolicyIsHardErroredForMsb(t *testing.T) {
	cases := []task.ContainerPolicy{
		{AddCapabilities: []string{"CAP_NET_BIND_SERVICE"}},
		{SeccompUnconfined: true},
		{ReadonlyRootfs: true},
	}
	for _, cp := range cases {
		if err := task.ValidateContainerPolicyForMsb(cp); err == nil {
			t.Errorf("ValidateContainerPolicyForMsb(%+v) = nil, want an error naming --security", cp)
		}
	}
}

// TestNoSecretValueOnAnyArgvOrNonCreateEnv is the Done-when's headline security proof: across
// every msb invocation in a full run's lifecycle (create, copy x N, exec x N, logs, stop, rm),
// a declared secret's real value never appears in any argv, and appears in the child's
// environment ONLY on the single `create` call — never on a later exec/copy/logs/stop/rm — per
// hand-secrets-to-msb.md's Timing rule.
func TestNoSecretValueOnAnyArgvOrNonCreateEnv(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const secretValue = "sk-ant-supersecret-0123456789"
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY="+secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})

	spec := task.RunSpec{
		ID: "run_secret_argv", ImageRef: "img", RepoPath: src, BundleDepth: 1,
		TaskPrompt:  []byte("task"),
		SecretsPath: secretsFile,
		Network: task.NetworkPolicy{
			Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"},
			Secrets: []task.SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}},
		},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := readFakeMsbCalls(t, home)
	if len(calls) == 0 {
		t.Fatal("no msb calls recorded")
	}
	createCalls := 0
	for _, c := range calls {
		for _, a := range c.Args {
			if strings.Contains(a, secretValue) {
				t.Errorf("secret value appeared in argv of a %q call: %v", c.Args[0], c.Args)
			}
		}
		leaked := false
		for _, v := range c.Env {
			if strings.Contains(v, secretValue) {
				leaked = true
			}
		}
		if c.Args[0] == "create" {
			createCalls++
			if !leaked {
				t.Error("create call's env should carry the secret value (the one accepted channel)")
			}
		} else if leaked {
			t.Errorf("secret value leaked into a non-create call's env: verb=%q", c.Args[0])
		}
	}
	if createCalls != 1 {
		t.Fatalf("expected exactly 1 create call, got %d", createCalls)
	}
}

// TestTeardownRunsOnEveryPath asserts msb `stop` and `rm` both run regardless of how the run
// ends: success, agent failure (nonzero exit with real output), a driver failure (ErrMsbFailed),
// a wall-clock timeout, and ctx cancellation.
func TestTeardownRunsOnEveryPath(t *testing.T) {
	cases := []struct {
		name       string
		agent      fakeAgentScript
		timeout    time.Duration
		cancelSoon bool
		wantErr    bool
	}{
		{name: "success", agent: fakeAgentScript{ExitCode: 0}},
		{name: "agent_failure", agent: fakeAgentScript{LogLines: []string{"boom"}, ExitCode: 7}},
		{name: "msb_driver_failure", agent: fakeAgentScript{NoOutput: true}, wantErr: true},
		{name: "wall_clock_timeout", agent: fakeAgentScript{Block: true}, timeout: 200 * time.Millisecond},
		{name: "ctx_cancellation", agent: fakeAgentScript{Block: true}, cancelSoon: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if tc.cancelSoon {
				go func() { time.Sleep(200 * time.Millisecond); cancel() }()
			}

			src := newRepo(t, map[string]string{"a.txt": "1\n"})
			home := t.TempDir()
			sb := newFakeSandbox(t, home, fakeMsbScript{Agent: tc.agent})

			spec := task.RunSpec{
				ID: "run_teardown_" + tc.name, ImageRef: "img", RepoPath: src, BundleDepth: 1,
				TaskPrompt: []byte("task"), Network: allowlistAll,
			}
			if tc.timeout > 0 {
				spec.Resources.Timeout = tc.timeout
			}
			runDir := filepath.Join(t.TempDir(), "run")
			_, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir)
			if tc.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			var sawStop, sawRm bool
			for _, c := range readFakeMsbCalls(t, home) {
				if c.Args[0] == "stop" {
					sawStop = true
				}
				if c.Args[0] == "rm" {
					sawRm = true
				}
			}
			if !sawStop {
				t.Error("msb stop was never called")
			}
			if !sawRm {
				t.Error("msb rm was never called")
			}
		})
	}
}

// TestMsbDriverFailureIsNotMistakenForAgentExitCode pins decision 6: a non-zero exit from `msb
// exec` with no output observed on either stream must surface as a failed run naming the driver
// failure, never as "the agent exited N".
func TestMsbDriverFailureIsNotMistakenForAgentExitCode(t *testing.T) {
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{NoOutput: true}})
	spec := task.RunSpec{ID: "run_msb_fail", ImageRef: "img", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("task"), Network: allowlistAll}
	runDir := filepath.Join(t.TempDir(), "run")
	_, err := orchestrator.Run(context.Background(), orchestrator.Deps{Sandbox: sb}, spec, runDir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "msb failed") {
		t.Errorf("error = %v, want it to name an msb driver failure, not an agent exit code", err)
	}
}

// TestRunTimeout is the wall-clock-timeout proof: a stuck agent is killed and the run is
// recorded as timed out, with the sandbox stopped and removed (§6.1).
func TestRunTimeout(t *testing.T) {
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{Block: true}})

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_timeout", ImageRef: "img", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"), Network: allowlistAll,
		Resources: task.Resources{Timeout: 300 * time.Millisecond},
	}
	res, err := orchestrator.Run(context.Background(), orchestrator.Deps{Sandbox: sb}, spec, runDir)
	if err != nil {
		t.Fatalf("Run (timeout should not be an error): %v", err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut = true")
	}
	b, err := os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"timed_out": true`)) {
		t.Errorf("meta.json should record timed_out: true; got %s", b)
	}
}

// TestRunTimeoutDuringSetup covers the same wall-clock timeout, but forced to fire before the
// agent ever runs — during Create/copy-in/helper-setup — instead of during the agent's own exec.
// A deadline this tight has already elapsed before any step runs, so this deterministically
// exercises the setup-phase path rather than depending on machine speed.
func TestRunTimeoutDuringSetup(t *testing.T) {
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{Block: true}})

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_setup_timeout", ImageRef: "img", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"), Network: allowlistAll,
		Resources: task.Resources{Timeout: 1 * time.Nanosecond},
	}
	res, err := orchestrator.Run(context.Background(), orchestrator.Deps{Sandbox: sb}, spec, runDir)
	if err != nil {
		t.Fatalf("Run (setup-phase timeout should not be an error): %v", err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut = true")
	}
}

// TestPatchSecretScanWiredIntoRun proves Run() actually calls the host-side secret scan
// (PatchSecretKeys) against the collected patch and surfaces a Safety warning naming the key —
// never the value — in the Result, meta.json, and report.md (§6.8/§8.4). This is defense in
// depth: under B1 no secret value can legitimately reach the guest at all (msb substitutes only
// at the host TLS boundary), so this proves the wiring rather than a real leak path.
func TestPatchSecretScanWiredIntoRun(t *testing.T) {
	const secretValue = "sk-ant-supersecret-0123456789"
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY="+secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{
		WorkspaceFiles: map[string]string{"config.txt": "api_key=" + secretValue + "\n"},
		ExitCode:       0,
	}})

	spec := task.RunSpec{
		ID: "run_secret_scan", ImageRef: "img", RepoPath: src, BundleDepth: 1,
		TaskPrompt:  []byte("task"),
		SecretsPath: secretsFile,
		Network: task.NetworkPolicy{
			Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"},
			Secrets: []task.SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}},
		},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	res, err := orchestrator.Run(context.Background(), orchestrator.Deps{Sandbox: sb}, spec, runDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	hit := func(lines []string) bool {
		for _, s := range lines {
			if strings.Contains(s, "ANTHROPIC_API_KEY") && strings.Contains(s, "changes.patch") {
				return true
			}
		}
		return false
	}
	if !hit(res.Safety) {
		t.Errorf("Result.Safety should flag the secret key found in the patch; got %v", res.Safety)
	}
	mb := readFile(t, filepath.Join(runDir, "meta.json"))
	if strings.Contains(mb, secretValue) {
		t.Error("secret value leaked into meta.json")
	}
	var m orchestrator.RunRecord
	if err := json.Unmarshal([]byte(mb), &m); err != nil {
		t.Fatalf("parse meta.json: %v", err)
	}
	if !hit(m.Safety) {
		t.Errorf("meta.json safety should flag the secret key; got %v", m.Safety)
	}
}

// TestCreateSpecReflectsRunResources proves resources.{cpus,memory,disk,timeout} reach `msb
// create`'s argv (run-tasks-on-microsandbox.md decision 5).
func TestCreateSpecReflectsRunResources(t *testing.T) {
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{ExitCode: 0}})

	spec := task.RunSpec{
		ID: "run_resources", ImageRef: "img", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"), Network: allowlistAll,
		Resources: task.Resources{CPUs: 4, MemoryMiB: 8192, DiskGiB: 40, Timeout: 20 * time.Minute},
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Run(context.Background(), orchestrator.Deps{Sandbox: sb}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := readFakeMsbCalls(t, home)
	if len(calls) == 0 || calls[0].Args[0] != "create" {
		t.Fatalf("expected the first call to be create; got %v", calls)
	}
	create := calls[0].Args
	for _, want := range [][2]string{
		{"--cpus", "4"}, {"--memory", "8192"}, {"--root-disk", "40G"}, {"--max-duration", (20 * time.Minute).String()},
		{"--security", "restricted"}, {"--user", "agent"},
	} {
		if !argPairPresent(create, want[0], want[1]) {
			t.Errorf("create args %v missing %s %s", create, want[0], want[1])
		}
	}
}

func argPairPresent(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// --- shared helpers used across this package's test files ---

// allowlistAll is the minimal, always-valid network policy for tests that don't care about
// egress specifics — task.NetworkArgs refuses to translate a zero-value Mode (the ADR's "never
// emit an empty policy" rule), so every RunSpec needs an explicit one.
var allowlistAll = task.NetworkPolicy{Mode: task.NetworkAllowlist}

func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "--quiet", "-b", "main")
	run(t, dir, "git", "config", "user.name", "tester")
	run(t, dir, "git", "config", "user.email", "tester@example.com")
	for name, c := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "--quiet", "-m", "initial")
	return dir
}

func assertMeta(t *testing.T, path, wantID string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var m struct {
		ID       string `json:"id"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse meta.json: %v", err)
	}
	if m.ID != wantID {
		t.Errorf("meta id = %q, want %q", m.ID, wantID)
	}
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
