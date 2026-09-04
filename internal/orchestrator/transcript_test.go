package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/task"
)

const transcriptGuestDir = ".claude/projects"

func transcriptFiles() map[string]string {
	return map[string]string{
		"session-abc.jsonl": `{"type":"user","text":"go"}` + "\n" +
			`{"type":"tool_use","name":"Bash","input":{"command":"echo $GH_TOKEN"}}` + "\n" +
			`{"type":"tool_result","content":"$MSB_GH_TOKEN"}` + "\n",
	}
}

func readTranscript(t *testing.T, runDir string) string {
	t.Helper()
	dir := orchestrator.TranscriptDirPath(runDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("no transcript at %s: %v", dir, err)
	}
	var sb strings.Builder
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		sb.Write(b)
	}
	return sb.String()
}

// TestTranscriptCapturedOnCleanRun is the baseline: opted in, the transcript reaches the run dir
// and carries the tool calls that stdout never shows.
func TestTranscriptCapturedOnCleanRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{
		Agent:           fakeAgentScript{ExitCode: 0},
		TranscriptFiles: transcriptFiles(),
	})
	runDir := filepath.Join(t.TempDir(), "run")
	_, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, task.RunSpec{
		ID: "run_tr_ok", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		TranscriptDir: transcriptGuestDir,
	}, runDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := readTranscript(t, runDir); !strings.Contains(got, `"tool_use"`) {
		t.Errorf("transcript missing the tool calls that are the whole reason to capture it: %q", got)
	}
}

// TestTranscriptCapturedWhenAgentExecFails is the regression that matters. On a driver failure the
// run returns before krayt-helper finish and collectOutput ever run, so anything riding the normal
// collection path is lost — which is precisely the run you need a transcript for. Capture lives in
// the teardown defer to survive this.
func TestTranscriptCapturedWhenAgentExecFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{
		// NoOutput makes the agent exec exit non-zero with nothing on either stream, which the
		// driver classifies as ErrMsbFailed — the run returns before finish/collectOutput.
		Agent:           fakeAgentScript{NoOutput: true},
		TranscriptFiles: transcriptFiles(),
	})
	runDir := filepath.Join(t.TempDir(), "run")
	_, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, task.RunSpec{
		ID: "run_tr_fail", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		TranscriptDir: transcriptGuestDir,
	}, runDir)
	if err == nil {
		t.Fatal("expected the run to fail; this test is about what survives a failure")
	}
	if _, serr := os.Stat(filepath.Join(runDir, "changes.patch")); serr == nil {
		t.Fatal("collectOutput ran; this test no longer exercises the path it was written for")
	}
	if got := readTranscript(t, runDir); !strings.Contains(got, `"tool_use"`) {
		t.Errorf("transcript not captured on the failure path: %q", got)
	}
}

// TestTranscriptRedactsSecretValues: a transcript records what the agent read and printed, so it
// is the artifact most likely to quote a secret. Unlike changes.patch (scanned but never rewritten,
// because mutating it would break git apply) this one is rewritten.
func TestTranscriptRedactsSecretValues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const secretValue = "sk-ant-super-secret-value-0123456789"
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY="+secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{
		Agent: fakeAgentScript{ExitCode: 0},
		TranscriptFiles: map[string]string{
			"session.jsonl": `{"type":"tool_result","content":"key is ` + secretValue + `"}` + "\n",
		},
	})
	runDir := filepath.Join(t.TempDir(), "run")
	_, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, task.RunSpec{
		ID: "run_tr_redact", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"),
		Network:     task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}, Secrets: []task.SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}}},
		SecretsPath: secretsFile, TranscriptDir: transcriptGuestDir,
	}, runDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := readTranscript(t, runDir)
	if strings.Contains(got, secretValue) {
		t.Error("the captured transcript contains a raw secret value")
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected the value replaced by the redaction marker, got %q", got)
	}
}

// TestTranscriptNotCapturedWhenNotOptedIn: the default. An empty spec.TranscriptDir must mean no
// directory AND no work — no $HOME probe, no copy.
func TestTranscriptNotCapturedWhenNotOptedIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{
		Agent:           fakeAgentScript{ExitCode: 0},
		TranscriptFiles: transcriptFiles(),
	})
	runDir := filepath.Join(t.TempDir(), "run")
	_, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, task.RunSpec{
		ID: "run_tr_off", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		// TranscriptDir deliberately empty.
	}, runDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, serr := os.Stat(orchestrator.TranscriptDirPath(runDir)); !os.IsNotExist(serr) {
		t.Error("a transcript was captured without the run opting in")
	}
	for _, c := range readFakeMsbCalls(t, home) {
		if strings.Contains(strings.Join(c.Args, " "), "$HOME") {
			t.Error("the $HOME probe ran even though no transcript was requested")
		}
	}
}

// TestTranscriptAbsentInGuestIsNotAnError: an inferred adapter path that does not match the image,
// or an agent that never started, must cost a transcript and nothing else.
func TestTranscriptAbsentInGuestIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{
		Agent: fakeAgentScript{ExitCode: 0},
		// No TranscriptFiles: the guest path will not exist, so msb copy exits non-zero.
	})
	runDir := filepath.Join(t.TempDir(), "run")
	res, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, task.RunSpec{
		ID: "run_tr_missing", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		TranscriptDir: transcriptGuestDir,
	}, runDir)
	if err != nil {
		t.Fatalf("a missing transcript failed the whole run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if _, serr := os.Stat(orchestrator.TranscriptDirPath(runDir)); !os.IsNotExist(serr) {
		t.Error("an empty transcript directory was created for a guest that had none")
	}
}

// TestTranscriptCapturedOnWallClockTimeout covers the other path that returns before
// collectOutput. A timeout also cancels the run's ctx, so this additionally proves the capture's
// context.WithoutCancel is doing its job — with the run ctx it would fail instantly and silently.
func TestTranscriptCapturedOnWallClockTimeout(t *testing.T) {
	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{
		Agent:           fakeAgentScript{Block: true},
		TranscriptFiles: transcriptFiles(),
	})
	runDir := filepath.Join(t.TempDir(), "run")
	res, err := orchestrator.Run(context.Background(), orchestrator.Deps{Sandbox: sb}, task.RunSpec{
		ID: "run_tr_timeout", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		Resources:     task.Resources{Timeout: 300 * time.Millisecond},
		TranscriptDir: transcriptGuestDir,
	}, runDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("expected a timed-out run; this test is about capture after the ctx is dead")
	}
	if got := readTranscript(t, runDir); !strings.Contains(got, `"tool_use"`) {
		t.Errorf("transcript not captured on the timeout path (WithoutCancel not applied?): %q", got)
	}
}
