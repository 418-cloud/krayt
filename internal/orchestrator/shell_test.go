package orchestrator_test

// Tests for `krayt shell`'s lifecycle (add-interactive-shell-session.md): Shell (fresh session)
// and AttachShell (re-entering a --keep session), against the same scriptable fake msb the rest
// of this package's tests use. The teardown-on-error-with-keep matrix (Done-when #4) is the one
// this file exists to prove — a leaked sandbox would come from exactly that matrix being wrong.

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/task"
)

func shellSpec(id, repoPath string) task.RunSpec {
	return task.RunSpec{
		ID: id, ImageRef: "img", RepoPath: repoPath, BundleDepth: 1, Network: allowlistAll,
		Resources: task.Resources{CPUs: 2, MemoryMiB: 2048},
	}
}

// TestShellEndToEnd proves the happy path: a session edits the workspace, exits cleanly, and the
// edit shows up in changes.patch — same deliverable a run produces (decision 8) — while the
// sandbox is torn down since --keep was not given (decision 3's default).
func TestShellEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{
		WorkspaceFiles: map[string]string{"greeting.txt": "hello world\n"},
		ExitCode:       0,
	}})

	runDir := filepath.Join(t.TempDir(), "run")
	res, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_shell_e2e", src), runDir, false, nil)
	if err != nil {
		t.Fatalf("orchestrator.Shell: %v", err)
	}
	if res.Kept {
		t.Error("Kept = true, want false (no --keep)")
	}
	assertMeta(t, filepath.Join(runDir, "meta.json"), "run_shell_e2e")

	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if rec.EffectiveKind() != orchestrator.KindShell {
		t.Errorf("Kind = %q, want %q", rec.EffectiveKind(), orchestrator.KindShell)
	}
	if rec.State != orchestrator.StateDone {
		t.Errorf("State = %q, want %q", rec.State, orchestrator.StateDone)
	}
	if rec.Patch == nil || rec.Patch.FilesChanged != 1 {
		t.Errorf("Patch = %+v, want a 1-file diffstat", rec.Patch)
	}

	var sawStop, sawRm bool
	for _, c := range readFakeMsbCalls(t, home) {
		switch c.Args[0] {
		case "stop":
			sawStop = true
		case "rm":
			sawRm = true
		case "create":
			for _, a := range c.Args {
				if a == "--max-duration" {
					t.Error("Shell must never pass --max-duration (decision 5)")
				}
				if a == "--vsock" {
					t.Error("Shell must never pass --vsock (decision 12: no ask_human channel)")
				}
			}
		}
	}
	if !sawStop || !sawRm {
		t.Errorf("sawStop=%v sawRm=%v, want both true (no --keep)", sawStop, sawRm)
	}
}

// TestShellExecTTYCommand pins ttyCommand's fallback (shell.go): a real `msb exec --tty` with an
// empty Command re-execs the image's ENTRYPOINT rather than a shell for any image that sets one
// without an image-level Shell — every published krayt agent image included — discovered against
// real hardware (2026-09-16, HUMAN_TODO.md). Shell/AttachShell must therefore never hand msb an
// empty Command: with no execCmd (no `--exec`) they resolve an explicit login shell themselves;
// with one (the `--exec` convenience flag), it must pass through untouched.
func TestShellExecTTYCommand(t *testing.T) {
	cases := []struct {
		name    string
		execCmd []string
		want    []string
	}{
		{
			name: "no --exec: an explicit shell, never an empty Command",
			want: []string{"/bin/sh", "-c",
				`sh_bin="${SHELL:-/bin/bash}"; [ -x "$sh_bin" ] || sh_bin=/bin/sh; exec "$sh_bin" -l`},
		},
		{
			name:    "--exec given: passed through unchanged",
			execCmd: []string{"sh", "-c", "go test ./..."},
			want:    []string{"sh", "-c", "go test ./..."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
			home := t.TempDir()
			sb := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})

			runDir := filepath.Join(t.TempDir(), "run")
			if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_tty_cmd", src), runDir, false, tc.execCmd); err != nil {
				t.Fatalf("orchestrator.Shell: %v", err)
			}

			var gotTTYExec bool
			for _, c := range readFakeMsbCalls(t, home) {
				if len(c.Args) >= 2 && c.Args[0] == "exec" && c.Args[1] == "--tty" {
					gotTTYExec = true
					i := len(c.Args)
					for j, a := range c.Args {
						if a == "--" {
							i = j + 1
							break
						}
					}
					got := c.Args[i:]
					if !reflect.DeepEqual(got, tc.want) {
						t.Errorf("exec --tty command = %v, want %v", got, tc.want)
					}
				}
			}
			if !gotTTYExec {
				t.Fatal("no `exec --tty` call observed")
			}
		})
	}
}

// TestShellTeardownKeepMatrix is the Done-when #4 proof: without --keep, the sandbox is torn down
// on every path including a failure before the shell ever attached; with --keep, teardown is
// skipped ONLY when the session reached a clean exit — every error path still tears down, keep or
// not, so a --keep run that never got the human a shell cannot leak a sandbox nobody can find.
func TestShellTeardownKeepMatrix(t *testing.T) {
	cases := []struct {
		name           string
		createExitCode int
		shell          fakeShellScript
		cancelSoon     bool
		wantErr        bool
	}{
		{name: "clean_exit", shell: fakeShellScript{ExitCode: 0}},
		{name: "clean_exit_nonzero_shell_status", shell: fakeShellScript{ExitCode: 1}}, // `exit 1` inside bash is still a clean Shell() return
		{name: "create_failure", createExitCode: 1, wantErr: true},
		{name: "ctx_cancellation_before_attach_returns", shell: fakeShellScript{Block: true}, cancelSoon: true, wantErr: true},
	}
	for _, tc := range cases {
		for _, keep := range []bool{false, true} {
			t.Run(tc.name+keepSuffix(keep), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if tc.cancelSoon {
					go func() { time.Sleep(200 * time.Millisecond); cancel() }()
				}

				src := newRepo(t, map[string]string{"a.txt": "1\n"})
				home := t.TempDir()
				sb := newFakeSandbox(t, home, fakeMsbScript{
					Shell: tc.shell, CreateExitCode: tc.createExitCode,
				})

				runDir := filepath.Join(t.TempDir(), "run")
				res, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_td", src), runDir, keep, nil)
				if tc.wantErr && err == nil {
					t.Fatal("expected an error, got nil")
				}
				if !tc.wantErr && err != nil {
					t.Fatalf("unexpected error: %v", err)
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

				wantTeardown := !keep || tc.wantErr // skipped only for keep + a clean exit
				if wantTeardown && (!sawStop || !sawRm) {
					t.Errorf("keep=%v case=%q: sawStop=%v sawRm=%v, want both true (teardown must fire)", keep, tc.name, sawStop, sawRm)
				}
				if !wantTeardown && (sawStop || sawRm) {
					t.Errorf("keep=%v case=%q: sawStop=%v sawRm=%v, want both false (kept sandbox must survive)", keep, tc.name, sawStop, sawRm)
				}
				if !wantTeardown {
					if res == nil || !res.Kept {
						t.Errorf("keep=%v case=%q: res.Kept = %+v, want true", keep, tc.name, res)
					}
					rec, rerr := orchestrator.ReadRecord(runDir)
					if rerr != nil {
						t.Fatalf("ReadRecord: %v", rerr)
					}
					if rec.State != orchestrator.StateKept {
						t.Errorf("State = %q, want %q", rec.State, orchestrator.StateKept)
					}
					if rec.PID != 0 {
						t.Errorf("PID = %d, want 0 (no process outlives a --keep session's own exit)", rec.PID)
					}
					if rec.Terminal() {
						t.Error("a kept record must not report Terminal() == true — its sandbox is still alive")
					}
				}
			})
		}
	}
}

func keepSuffix(keep bool) string { //nolint:revive // test helper, name mirrors the table field it stringifies
	if keep {
		return "/keep"
	}
	return "/no-keep"
}

// TestAttachShellReEntersKeptSession proves decision 4: `krayt shell --attach <run-id>` re-enters
// the same sandbox (no Create, no copy-in, no helper setup — AttachShell issues neither), a
// second edit still lands in changes.patch, and the session ends up `kept` again afterward
// (nothing tears the sandbox down on a clean re-attach exit either — only `krayt stop` does,
// decision 3).
func TestAttachShellReEntersKeptSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{
		WorkspaceFiles: map[string]string{"a.txt": "2\n"}, ExitCode: 0,
	}})

	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_attach", src), runDir, true, nil); err != nil {
		t.Fatalf("Shell (initial, --keep): %v", err)
	}
	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord after initial Shell: %v", err)
	}
	if rec.State != orchestrator.StateKept {
		t.Fatalf("State after initial Shell = %q, want %q", rec.State, orchestrator.StateKept)
	}

	// Re-attach: a second edit, on top of the first.
	sb2 := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{
		WorkspaceFiles: map[string]string{"b.txt": "new\n"}, ExitCode: 0,
	}})
	res, err := orchestrator.AttachShell(ctx, orchestrator.Deps{Sandbox: sb2}, runDir, "", nil)
	if err != nil {
		t.Fatalf("AttachShell: %v", err)
	}
	if !res.Kept {
		t.Error("AttachShell result Kept = false, want true")
	}

	final, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord after AttachShell: %v", err)
	}
	if final.State != orchestrator.StateKept {
		t.Errorf("State after AttachShell = %q, want %q (re-attach must not tear the sandbox down)", final.State, orchestrator.StateKept)
	}
	if final.Patch == nil || final.Patch.FilesChanged != 2 {
		t.Errorf("Patch after AttachShell = %+v, want both edits reflected (2 files)", final.Patch)
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
	if sawStop || sawRm {
		t.Error("AttachShell must never call stop/rm — only `krayt stop` destroys a kept sandbox")
	}
}
