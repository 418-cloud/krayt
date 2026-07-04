package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/task"
)

// TestApplyAdapterAuthGate proves the run's host-side pre-flight rejects an ambiguous
// credential set before any VM boots (§6.14).
func TestApplyAdapterAuthGate(t *testing.T) {
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY=a\nCLAUDE_CODE_OAUTH_TOKEN=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &task.RunSpec{SecretsPath: secretsFile, Questions: task.QuestionsPolicy{Mode: task.QuestionFail}}
	err := applyAdapter(spec, "claude-code")
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected exactly-one auth error; got %v", err)
	}
}

// TestApplyAdapterWiresAsk proves a valid single credential passes and, in wait mode, the
// krayt-ask front-end is wired into the container env (§6.13), without clobbering user env.
func TestApplyAdapterWiresAsk(t *testing.T) {
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY=a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &task.RunSpec{
		SecretsPath: secretsFile,
		Env:         map[string]string{"LOG_LEVEL": "debug"},
		Questions:   task.QuestionsPolicy{Mode: task.QuestionWait},
	}
	if err := applyAdapter(spec, "claude-code"); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if spec.Env["KRAYT_ASK_SOCKET"] != guest.ContainerAskSocket {
		t.Errorf("KRAYT_ASK_SOCKET = %q, want %q", spec.Env["KRAYT_ASK_SOCKET"], guest.ContainerAskSocket)
	}
	if spec.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("adapter clobbered user env: %v", spec.Env)
	}

	// fail mode: no front-end wiring.
	spec2 := &task.RunSpec{SecretsPath: secretsFile, Questions: task.QuestionsPolicy{Mode: task.QuestionFail}}
	if err := applyAdapter(spec2, "claude-code"); err != nil {
		t.Fatalf("applyAdapter fail-mode: %v", err)
	}
	if _, wired := spec2.Env["KRAYT_ASK_SOCKET"]; wired {
		t.Errorf("fail mode should not wire krayt-ask; env = %v", spec2.Env)
	}
}
