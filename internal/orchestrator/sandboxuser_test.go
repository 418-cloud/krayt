package orchestrator_test

// Tests for running every sandbox as the image's own USER (resolveSandboxUser, KRAYT_SPEC.md
// §8.2), rather than the fixed `agent` user msb refuses to boot on images that lack one.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/task"
)

func strPtr(s string) *string { return &s }

// userFlag returns the --user value of one recorded msb argv, looking only at the flags before a
// `--` that starts a guest command; "" when absent.
func userFlag(args []string) string {
	for i := 0; i+1 < len(args) && args[i] != "--"; i++ {
		if args[i] == "--user" {
			return args[i+1]
		}
	}
	return ""
}

func TestRunUsesImageUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{
		ImageUser: strPtr("node"),
		Agent:     fakeAgentScript{ExitCode: 0},
	})
	runDir := filepath.Join(t.TempDir(), "run")
	_, err := orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, task.RunSpec{
		ID: "run_user_node", ImageRef: "img", RepoPath: newRepo(t, map[string]string{"a.txt": "1\n"}),
		BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		TranscriptDir: ".claude/projects",
	}, runDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawCreate, sawAgent, sawHomeProbe bool
	for _, c := range readFakeMsbCalls(t, home) {
		switch {
		case c.Args[0] == "create":
			sawCreate = true
			if got := userFlag(c.Args); got != "node" {
				t.Errorf("create --user = %q, want node (args %v)", got, c.Args)
			}
		case isAgentExecCall(c):
			sawAgent = true
			if got := userFlag(c.Args); got != "node" {
				t.Errorf("agent exec --user = %q, want node", got)
			}
		case c.Args[0] == "exec" && strings.Contains(strings.Join(c.Args, " "), "$HOME"):
			sawHomeProbe = true
			if got := userFlag(c.Args); got != "node" {
				t.Errorf("$HOME probe --user = %q, want node", got)
			}
		case c.Args[0] == "exec" && userFlag(c.Args) != "root" && userFlag(c.Args) != "node":
			t.Errorf("exec ran as %q, want either root (helper) or the image user: %v", userFlag(c.Args), c.Args)
		}
	}
	if !sawCreate || !sawAgent || !sawHomeProbe {
		t.Fatalf("sawCreate=%v sawAgent=%v sawHomeProbe=%v, want all true", sawCreate, sawAgent, sawHomeProbe)
	}
	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SandboxUser != "node" {
		t.Errorf("record sandbox_user = %q, want node", rec.SandboxUser)
	}
}

// TestRootOrUnsetImageUserRefusedBeforeCreate proves §8.2's "fails the run with a clear error and
// never launches" for both Run and Shell: no `create` is ever issued.
func TestRootOrUnsetImageUserRefusedBeforeCreate(t *testing.T) {
	cases := []struct {
		name, user, wantErr string
	}{
		{name: "unset", user: "", wantErr: "sets no USER"},
		{name: "root", user: "root", wantErr: "which is root"},
		{name: "uid0", user: "0:0", wantErr: "which is root"},
	}
	for _, tc := range cases {
		for _, mode := range []string{"run", "shell"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				home := t.TempDir()
				sb := newFakeSandbox(t, home, fakeMsbScript{ImageUser: strPtr(tc.user)})
				runDir := filepath.Join(t.TempDir(), "run")
				repo := newRepo(t, map[string]string{"a.txt": "1\n"})
				var err error
				if mode == "run" {
					_, err = orchestrator.Run(ctx, orchestrator.Deps{Sandbox: sb}, task.RunSpec{
						ID: "run_root", ImageRef: "img", RepoPath: repo,
						BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
					}, runDir)
				} else {
					_, err = orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_root", repo), runDir, false, nil)
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				for _, c := range readFakeMsbCalls(t, home) {
					if c.Args[0] == "create" {
						t.Fatalf("create was issued for a root/unset image: %v", c.Args)
					}
				}
				rec, rerr := orchestrator.ReadRecord(runDir)
				if rerr != nil {
					t.Fatal(rerr)
				}
				if rec.State != orchestrator.StateFailed || !strings.Contains(rec.Error, tc.wantErr) {
					t.Errorf("record state=%q error=%q, want failed with %q", rec.State, rec.Error, tc.wantErr)
				}
			})
		}
	}
}

// TestUncachedImageIsPulledBeforeInspect: msb inspects only its local cache, so an image that
// isn't there yet must be pulled and inspected again before Create.
func TestUncachedImageIsPulledBeforeInspect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{ImageNotCached: true, Shell: fakeShellScript{ExitCode: 0}})
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_pull", newRepo(t, map[string]string{"a.txt": "1\n"})), runDir, false, nil); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	var verbs []string
	for _, c := range readFakeMsbCalls(t, home) {
		verbs = append(verbs, c.Args[0])
		if c.Args[0] == "create" {
			break
		}
	}
	want := []string{"image", "pull", "image", "create"}
	if strings.Join(verbs, " ") != strings.Join(want, " ") {
		t.Errorf("msb calls up to create = %v, want %v", verbs, want)
	}
}

func TestPullFailureFailsBeforeCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{ImageNotCached: true, PullExitCode: 1})
	runDir := filepath.Join(t.TempDir(), "run")
	_, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_badimg", newRepo(t, map[string]string{"a.txt": "1\n"})), runDir, false, nil)
	if err == nil || !strings.Contains(err.Error(), "msb pull img") {
		t.Fatalf("err = %v, want the pull failure", err)
	}
	for _, c := range readFakeMsbCalls(t, home) {
		if c.Args[0] == "create" {
			t.Fatalf("create was issued after a failed pull: %v", c.Args)
		}
	}
}

// TestAttachShellUsesRecordedUser: a re-attach must exec as the user the sandbox was created
// with (recorded at create time), and a record written before sandbox_user existed falls back to
// `agent`, the user those sandboxes were created with.
func TestAttachShellUsesRecordedUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{ImageUser: strPtr("1000:1000"), Shell: fakeShellScript{ExitCode: 0}})
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_attach_user", src), runDir, true, nil); err != nil {
		t.Fatalf("Shell (--keep): %v", err)
	}

	// The re-attach's fake would report a different image user; the recorded one must win.
	sb2 := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})
	if _, err := orchestrator.AttachShell(ctx, orchestrator.Deps{Sandbox: sb2}, runDir, "", nil); err != nil {
		t.Fatalf("AttachShell: %v", err)
	}

	// Simulate a legacy record, then attach once more.
	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatal(err)
	}
	rec.SandboxUser = ""
	if err := orchestrator.WriteRecord(runDir, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.AttachShell(ctx, orchestrator.Deps{Sandbox: sb2}, runDir, "", nil); err != nil {
		t.Fatalf("AttachShell (legacy record): %v", err)
	}

	var ttyUsers []string
	for _, c := range readFakeMsbCalls(t, home) {
		if isTTYCall(c) {
			ttyUsers = append(ttyUsers, userFlag(c.Args))
		}
	}
	want := []string{"1000:1000", "1000:1000", "agent"}
	if strings.Join(ttyUsers, " ") != strings.Join(want, " ") {
		t.Errorf("tty exec users = %v, want %v (initial, re-attach, legacy re-attach)", ttyUsers, want)
	}
}
