package orchestrator_test

// Tests for `krayt shell`'s lifecycle (add-interactive-shell-session.md): Shell (fresh session)
// and AttachShell (re-entering a --keep session), against the same scriptable fake msb the rest
// of this package's tests use. The teardown-on-error-with-keep matrix (Done-when #4) is the one
// this file exists to prove — a leaked sandbox would come from exactly that matrix being wrong.

import (
	"context"
	"errors"
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
					if got := ttyWorkdir(c.Args); got != "/workspace" {
						t.Errorf("exec --tty --workdir = %q, want /workspace (args %v)", got, c.Args)
					}
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
	ttyCalls := 0
	for _, c := range readFakeMsbCalls(t, home) {
		if c.Args[0] == "stop" {
			sawStop = true
		}
		if c.Args[0] == "rm" {
			sawRm = true
		}
		if isTTYCall(c) {
			ttyCalls++
			if got := ttyWorkdir(c.Args); got != "/workspace" {
				t.Errorf("exec --tty --workdir = %q, want /workspace for the initial and re-attached session (args %v)", got, c.Args)
			}
		}
	}
	if sawStop || sawRm {
		t.Error("AttachShell must never call stop/rm — only `krayt stop` destroys a kept sandbox")
	}
	if ttyCalls != 2 {
		t.Errorf("saw %d `exec --tty` calls, want 2 (initial Shell + AttachShell)", ttyCalls)
	}
}

// TestAttachShellFailureLeavesSessionManageable proves a failed re-attach does not strand the
// sandbox. AttachShell never calls stop/rm on any path, so the sandbox outlives the failure — and
// a terminal `failed` record would then be a dead end: `krayt shell --attach` refuses anything
// that is not `kept`, `krayt stop` refuses a terminal record, and `krayt doctor` can only point
// at raw msb. The record must come back as `kept` (PID cleared, the error recorded) so both
// commands still reach it. The failure here is the realistic one: the terminal went away
// mid-session (SIGHUP/Ctrl-C), cancelling the context out from under the attach.
func TestAttachShellFailureLeavesSessionManageable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})

	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_attach_fail", src), runDir, true, nil); err != nil {
		t.Fatalf("Shell (initial, --keep): %v", err)
	}

	// Re-attach into a wedged shell, then kill the terminal.
	attachCtx, attachCancel := context.WithCancel(ctx)
	go func() { time.Sleep(200 * time.Millisecond); attachCancel() }()
	sb2 := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{Block: true}})
	if _, err := orchestrator.AttachShell(attachCtx, orchestrator.Deps{Sandbox: sb2}, runDir, "", nil); err == nil {
		t.Fatal("AttachShell returned nil error for a cancelled session")
	}
	attachCancel()

	final, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord after failed AttachShell: %v", err)
	}
	if final.State != orchestrator.StateKept {
		t.Errorf("State after a failed attach = %q, want %q — the sandbox is still running, so the record must stay attachable and stoppable", final.State, orchestrator.StateKept)
	}
	if final.Terminal() {
		t.Error("a failed attach left a terminal record; `krayt stop` refuses those, and nothing else can destroy the sandbox")
	}
	if final.Error == "" {
		t.Error("the failure was not recorded in the run record")
	}
	if final.PID != 0 {
		t.Errorf("PID = %d, want 0 — no process supervises the sandbox once the attach returned", final.PID)
	}
	for _, c := range readFakeMsbCalls(t, home) {
		if c.Args[0] == "stop" || c.Args[0] == "rm" {
			t.Errorf("AttachShell tore the sandbox down on an error path: %v", c.Args)
		}
	}
}

// TestAttachShellFailureWithRemovedSandboxIsTerminal is the other half: `kept` is only honest
// while the sandbox exists. When it was destroyed out from under krayt (`msb rm` by hand between
// attaches — usually the very reason the attach failed), there is nothing left to re-enter or
// stop, and the record must say so rather than advertise a session that cannot be resumed.
func TestAttachShellFailureWithRemovedSandboxIsTerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})

	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_attach_gone", src), runDir, true, nil); err != nil {
		t.Fatalf("Shell (initial, --keep): %v", err)
	}
	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	removeFakeSandbox(t, home, rec.SandboxName)

	attachCtx, attachCancel := context.WithCancel(ctx)
	go func() { time.Sleep(200 * time.Millisecond); attachCancel() }()
	sb2 := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{Block: true}})
	if _, err := orchestrator.AttachShell(attachCtx, orchestrator.Deps{Sandbox: sb2}, runDir, "", nil); err == nil {
		t.Fatal("AttachShell returned nil error for a cancelled session")
	}
	attachCancel()

	final, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord after failed AttachShell: %v", err)
	}
	if final.State != orchestrator.StateFailed {
		t.Errorf("State = %q, want failed — the sandbox is gone, so the session is not resumable", final.State)
	}
}

// TestAttachShellIsExclusiveAcrossProcesses proves the kept→running claim is atomic: the check
// and the transition are a read followed by a write, so without a lock two `krayt shell --attach
// <run-id>` in two terminals both see `kept`, both attach, overwrite each other's PID in the one
// record, and race each other's patch/report/meta writes on exit. The second attempt is refused
// with ErrAttachInProgress instead. The two attaches are goroutines here, but the lock they
// contend on is the OS's (flock/LockFileEx on a file in the run dir), which is what makes it hold
// across processes too.
func TestAttachShellIsExclusiveAcrossProcesses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})

	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := orchestrator.Shell(ctx, orchestrator.Deps{Sandbox: sb}, shellSpec("run_attach_excl", src), runDir, true, nil); err != nil {
		t.Fatalf("Shell (initial, --keep): %v", err)
	}

	// First attach: wedged in the shell, holding the session, until we cancel it.
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	sbBlocking := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{Block: true}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = orchestrator.AttachShell(firstCtx, orchestrator.Deps{Sandbox: sbBlocking}, runDir, "", nil)
	}()

	// Wait for it to have actually claimed the session (its record write to `running`).
	if !waitForState(t, runDir, "running") {
		cancelFirst()
		<-done
		t.Fatal("first attach never reached state running")
	}

	sb2 := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})
	_, err := orchestrator.AttachShell(ctx, orchestrator.Deps{Sandbox: sb2}, runDir, "", nil)
	if !errors.Is(err, orchestrator.ErrAttachInProgress) {
		t.Errorf("second concurrent attach err = %v, want ErrAttachInProgress", err)
	}

	cancelFirst()
	<-done

	// Once the first attach is gone, the session is claimable again.
	if !waitForState(t, runDir, orchestrator.StateKept) {
		t.Fatal("session never returned to kept after the first attach ended")
	}
	sb3 := newFakeSandbox(t, home, fakeMsbScript{Shell: fakeShellScript{ExitCode: 0}})
	if _, err := orchestrator.AttachShell(ctx, orchestrator.Deps{Sandbox: sb3}, runDir, "", nil); err != nil {
		t.Errorf("attach after the first one released the session: %v", err)
	}
}

// waitForState polls the run record until it reaches want, up to a few seconds.
func waitForState(t *testing.T, runDir, want string) bool {
	t.Helper()
	for i := 0; i < 200; i++ {
		if rec, err := orchestrator.ReadRecord(runDir); err == nil && rec.State == want {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// ttyWorkdir returns the --workdir value of one recorded `msb exec` argv, looking only at the
// flags before the `--` that starts the guest command; "" when absent.
func ttyWorkdir(args []string) string {
	for i := 0; i+1 < len(args) && args[i] != "--"; i++ {
		if args[i] == "--workdir" {
			return args[i+1]
		}
	}
	return ""
}
