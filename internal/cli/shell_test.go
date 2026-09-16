package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
)

// newTestShellCmd mirrors newTestRunCmd (run_test.go): bind flags onto a real *cobra.Command with
// a context set, so runShellFresh/runShellAttach behave as they do under a real Execute().
func newTestShellCmd(f *runFlags, sf *shellFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "shell", RunE: func(*cobra.Command, []string) error { return nil }}
	bindShellFlags(cmd, f, sf)
	cmd.SetContext(context.Background())
	return cmd
}

// TestShellRequiresImageUnlessAttaching checks decision 1/host-side bullet's --image requirement:
// a fresh session needs one, but --attach never does (runShellAttach doesn't even look at it).
func TestShellRequiresImageUnlessAttaching(t *testing.T) {
	dir := t.TempDir()
	var f runFlags
	var sf shellFlags
	cmd := newTestShellCmd(&f, &sf)
	if err := cmd.ParseFlags([]string{"--repo", dir}); err != nil {
		t.Fatal(err)
	}
	err := runShell(cmd, &f, &sf)
	if err == nil || !strings.Contains(err.Error(), "--image is required") {
		t.Fatalf("runShell err = %v, want an --image-is-required error", err)
	}
}

// TestShellAttachSkipsImageRequirement proves --attach takes a different path entirely — it
// fails on "no such run" (there's no seeded record), never on a missing --image.
func TestShellAttachSkipsImageRequirement(t *testing.T) {
	dir := t.TempDir()
	var f runFlags
	var sf shellFlags
	cmd := newTestShellCmd(&f, &sf)
	if err := cmd.ParseFlags([]string{"--repo", dir, "--attach", "run_nonexistent"}); err != nil {
		t.Fatal(err)
	}
	err := runShell(cmd, &f, &sf)
	if err == nil {
		t.Fatal("expected an error attaching to a nonexistent run")
	}
	if strings.Contains(err.Error(), "--image is required") {
		t.Errorf("runShell err = %v, --attach must not require --image", err)
	}
}

// TestShellResourcePreflightRejectsOversizedRequest mirrors
// TestRunRunResourcePreflightRejectsOversizedRequest (run_test.go): a --memory request no real
// host could satisfy is refused before any sandbox work, same preflight `krayt run` uses.
func TestShellResourcePreflightRejectsOversizedRequest(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("resource preflight is a no-op off macOS by design (see resources_other.go)")
	}
	dir := t.TempDir()
	var f runFlags
	var sf shellFlags
	cmd := newTestShellCmd(&f, &sf)
	if err := cmd.ParseFlags([]string{
		"--image", "img:1", "--repo", dir, "--memory", "999999999999",
	}); err != nil {
		t.Fatal(err)
	}
	err := runShell(cmd, &f, &sf)
	if err == nil || !strings.Contains(err.Error(), "insufficient free memory") {
		t.Fatalf("runShell err = %v, want an insufficient-free-memory resource preflight error", err)
	}
}

// TestShellNoTaskRequired proves decision 13: unlike `krayt run`, a missing --task is not an
// error — pre-flight must fail (if at all) for an unrelated reason (the unrunnable msb
// pinMissingMsb pins), never "task prompt is empty"/"--task is required".
func TestShellNoTaskRequired(t *testing.T) {
	dir := t.TempDir()
	pinMissingMsb(t, dir)
	var f runFlags
	var sf shellFlags
	cmd := newTestShellCmd(&f, &sf)
	if err := cmd.ParseFlags([]string{"--image", "img:1", "--repo", dir, "--skip-resource-check"}); err != nil {
		t.Fatal(err)
	}
	err := runShell(cmd, &f, &sf)
	if err != nil && (strings.Contains(err.Error(), "task prompt is empty") || strings.Contains(err.Error(), "--task is required")) {
		t.Fatalf("runShell err = %v, want --task to be optional in shell mode (decision 13)", err)
	}
}

// TestShellNoTimeoutFlag proves decision 5: krayt shell registers no --timeout flag at all —
// unlike `krayt run`, where it's always present.
func TestShellNoTimeoutFlag(t *testing.T) {
	var f runFlags
	var sf shellFlags
	cmd := newTestShellCmd(&f, &sf)
	if fl := cmd.Flags().Lookup("timeout"); fl != nil {
		t.Errorf("krayt shell must not register --timeout (decision 5); found: %+v", fl)
	}
	for _, name := range []string{"detach", "on-question", "on-question-timeout", "agent", "transcript"} {
		if fl := cmd.Flags().Lookup(name); fl != nil {
			t.Errorf("krayt shell must not register --%s (decision 2/5/12); found: %+v", name, fl)
		}
	}
}

// TestShellResolvesAdapterSecretScope is the fix for the msb/decision-2 gap (KRAYT_SPEC.md §13's
// 2026-09-16 amendment): a krayt.yaml with `agent.adapter: claude-code` must get the SAME
// automatic network.inject scoping for CLAUDE_CODE_OAUTH_TOKEN that `krayt run` derives via
// applyAdapter — without it, ValidateNetworkPolicyForMsb rejects the run before it ever reaches
// the (unrelated, hardware-only) msb-missing error pinMissingMsb forces. Mirrors
// TestApplyAdapterScopesCredential (adapter_test.go) but through the real shell flag/config path.
func TestShellResolvesAdapterSecretScope(t *testing.T) {
	dir := t.TempDir()
	pinMissingMsb(t, dir)

	cfgYAML := "image: file-image:1\nagent:\n  adapter: claude-code\n" +
		"network:\n  mode: allowlist\n  allow: [api.anthropic.com]\n"
	cfgPath := filepath.Join(dir, "krayt.yaml")
	write(t, cfgPath, cfgYAML)

	secretsPath := filepath.Join(dir, "secrets.env")
	write(t, secretsPath, "CLAUDE_CODE_OAUTH_TOKEN=test-token\n")

	var f runFlags
	var sf shellFlags
	cmd := newTestShellCmd(&f, &sf)
	if err := cmd.ParseFlags([]string{
		"--repo", dir, "--config", cfgPath, "--secrets", secretsPath, "--skip-resource-check",
	}); err != nil {
		t.Fatal(err)
	}
	err := runShell(cmd, &f, &sf)
	if err != nil && strings.Contains(err.Error(), "network.inject") {
		t.Fatalf("runShell err = %v, want CLAUDE_CODE_OAUTH_TOKEN's network.inject scope resolved "+
			"automatically from agent.adapter, no hand-written entry needed", err)
	}
}

// TestShellNoAdapterConfiguredIsNoop proves the common case — no agent.adapter in krayt.yaml — is
// unaffected by wiring applyAdapterSecrets into runShellFresh: adapter.Get("") is the `none`
// adapter, whose Plan.Secrets is always empty, so nothing is merged into spec.Network.Secrets and
// an unrelated secrets-file key with no network.inject entry still fails exactly as it did before.
func TestShellNoAdapterConfiguredIsNoop(t *testing.T) {
	dir := t.TempDir()
	pinMissingMsb(t, dir)

	secretsPath := filepath.Join(dir, "secrets.env")
	write(t, secretsPath, "SOME_OTHER_SECRET=test-value\n")

	var f runFlags
	var sf shellFlags
	cmd := newTestShellCmd(&f, &sf)
	if err := cmd.ParseFlags([]string{
		"--image", "img:1", "--repo", dir, "--secrets", secretsPath, "--skip-resource-check",
	}); err != nil {
		t.Fatal(err)
	}
	err := runShell(cmd, &f, &sf)
	if err == nil || !strings.Contains(err.Error(), "network.inject") {
		t.Fatalf("runShell err = %v, want the ordinary no-adapter secrets-file-with-no-scope "+
			"rejection (network.inject) unaffected by the adapter secret-scoping change", err)
	}
}

// TestExecCommand checks the --exec convenience flag's translation into TTYExecSpec.Command
// (decision 2: acceptable as a convenience, never a per-agent launcher).
func TestExecCommand(t *testing.T) {
	if got := execCommand(""); got != nil {
		t.Errorf("execCommand(\"\") = %v, want nil (no override — msb attaches the default shell)", got)
	}
	want := []string{"sh", "-c", "go test ./..."}
	if got := execCommand("go test ./..."); !reflect.DeepEqual(got, want) {
		t.Errorf("execCommand(...) = %v, want %v", got, want)
	}
}

// TestCompleteShellAttachIDsFiltersToKeptShellRecords checks --attach's dynamic completion only
// offers live `kind: shell` sessions (state `kept`) — not a plain run, and not a shell record
// still mid-session (`running`, owned by another shell process already attached to it).
func TestCompleteShellAttachIDsFiltersToKeptShellRecords(t *testing.T) {
	repo := t.TempDir()
	seedShellRun(t, repo, "run_kept", orchestrator.StateKept)
	seedShellRun(t, repo, "run_running", orchestrator.StateRunning)
	seedRun(t, repo, "run_plain_done", "done") // an ordinary `krayt run`, no kind field at all

	var f runFlags
	var sf shellFlags
	cmd := newTestShellCmd(&f, &sf)
	if err := cmd.ParseFlags([]string{"--repo", repo}); err != nil {
		t.Fatal(err)
	}
	got, _ := completeShellAttachIDs(cmd, nil, "")
	if len(got) != 1 {
		t.Fatalf("completeShellAttachIDs = %v, want exactly 1 entry", got)
	}
	if !strings.HasPrefix(got[0], "run_kept\t") {
		t.Errorf("completeShellAttachIDs[0] = %q, want it to start with run_kept", got[0])
	}
}

// seedShellRun writes a minimal `kind: shell` run dir, mirroring seedRun (manage_test.go) but for
// a shell session's record shape.
func seedShellRun(t *testing.T, repo, id, state string) string {
	t.Helper()
	sd := filepath.Join(repo, ".krayt")
	runDir := orchestrator.RunDir(sd, id)
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"` + id + `","kind":"shell","state":"` + state + `","exit_code":0,"image_ref":"img:1","started_at":"2026-07-01T00:00:00Z","pid":0,"sandbox_name":"krayt-` + id + `"}`
	write(t, filepath.Join(runDir, "meta.json"), meta)
	return runDir
}
