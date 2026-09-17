package orchestrator_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/task"
)

func isGitTrustCall(c fakeCall) bool {
	return len(c.Args) > 0 && c.Args[0] == "exec" && containsArgSubstring(c.Args, "krayt-git-safe-directory")
}

// TestGitTrustRunsAsSandboxUserBeforeTheAgent: Run and Shell both mark /workspace as a git
// safe.directory, as the image's own user (never root), after the workspace exists and before the
// agent or the human gets it.
func TestGitTrustRunsAsSandboxUserBeforeTheAgent(t *testing.T) {
	for _, mode := range []string{"run", "shell"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			id := "run_gittrust_" + mode
			home := t.TempDir()
			sb := newFakeSandbox(t, home, fakeMsbScript{
				ImageUser: strPtr("node"),
				Agent:     fakeAgentScript{ExitCode: 0},
				Shell:     fakeShellScript{ExitCode: 0},
			})
			runDir := filepath.Join(t.TempDir(), "run")
			repo := newRepo(t, map[string]string{"a.txt": "1\n"})
			var err error
			if mode == "run" {
				_, err = orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, task.RunSpec{
					ID: id, ImageRef: "img", RepoPath: repo, BundleDepth: 1,
					TaskPrompt: []byte("t"), Network: allowlistAll,
				}, runDir)
			} else {
				_, err = orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec(id, repo), runDir, false, nil)
			}
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}

			got, rerr := os.ReadFile(filepath.Join(home, "state", "krayt-"+id, fakeGitTrustFile))
			if rerr != nil || strings.TrimSpace(string(got)) != "/workspace" {
				t.Fatalf("trusted dir = %q (%v), want /workspace", got, rerr)
			}

			setupIdx, trustIdx, workIdx := -1, -1, -1
			for i, c := range readFakeMsbCalls(t, home) {
				switch {
				case containsArgSubstring(c.Args, "krayt-helper") && hasArg(c.Args, "setup"):
					setupIdx = i
				case isGitTrustCall(c):
					trustIdx = i
					if got := userFlag(c.Args); got != "node" {
						t.Errorf("git trust ran as %q, want the image user node", got)
					}
				case isAgentExecCall(c) || isTTYCall(c):
					workIdx = i
				}
			}
			if setupIdx == -1 || trustIdx == -1 || workIdx == -1 || setupIdx >= trustIdx || trustIdx >= workIdx {
				t.Errorf("call order setup=%d trust=%d agent/tty=%d, want setup < trust < agent/tty", setupIdx, trustIdx, workIdx)
			}
		})
	}
}

// TestGitTrustFailureOnlyWarns: like a config seed, a failed git trust step prints one warning and
// never fails the session.
func TestGitTrustFailureOnlyWarns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{FailGitTrust: true, Shell: fakeShellScript{ExitCode: 0}})
	var warn bytes.Buffer
	runDir := filepath.Join(t.TempDir(), "run")
	res, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb, Warn: &warn}, shellSpec("run_gittrust_fail", newRepo(t, map[string]string{"a.txt": "1\n"})), runDir, false, nil)
	if err != nil || res == nil {
		t.Fatalf("Shell failed because of the git trust step: %v", err)
	}
	if !strings.Contains(warn.String(), "warning: git safe.directory for /workspace") {
		t.Errorf("warnings = %q, want the git safe.directory warning", warn.String())
	}
}

// TestAttachShellDoesNotRepeatGitTrust: the setting lives in the user's $HOME, which a kept
// sandbox still has.
func TestAttachShellDoesNotRepeatGitTrust(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_gittrust_attach", newRepo(t, map[string]string{"a.txt": "1\n"})), runDir, true, nil); err != nil {
		t.Fatalf("Shell (--keep): %v", err)
	}
	before := 0
	for _, c := range readFakeMsbCalls(t, home) {
		if isGitTrustCall(c) {
			before++
		}
	}
	if _, err := orchestrator.AttachShell(ctx, orchestrator.Deps{Sandbox: sb}, runDir, "", nil); err != nil {
		t.Fatalf("AttachShell: %v", err)
	}
	after := 0
	for _, c := range readFakeMsbCalls(t, home) {
		if isGitTrustCall(c) {
			after++
		}
	}
	if before != 1 || after != 1 {
		t.Errorf("git trust calls: %d after Shell, %d after AttachShell; want 1 and 1", before, after)
	}
}
