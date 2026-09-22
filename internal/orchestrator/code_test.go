package orchestrator_test

// Tests for `krayt code`'s lifecycle (add-vscode-remote-ssh-session.md), against the same
// scriptable fake msb the rest of this package uses. No msb, no sshd, no ssh, no VS Code: the
// session's own transport (`sshd -i` over `msb exec`) is exercised at the CLI layer
// (internal/cli/code_test.go); what this file proves is that the session boots, puts its SSH
// material in the right place, blocks, and still produces a patch on the way out.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sshsession"
	"github.com/418-cloud/krayt/internal/task"
)

func codeSpec(id, repoPath string) task.RunSpec {
	return task.RunSpec{
		ID: id, ImageRef: "img", RepoPath: repoPath, BundleDepth: 1, Network: allowlistAll,
		Resources: task.Resources{CPUs: 2, MemoryMiB: 2048},
	}
}

// TestCodeSessionCreatesCopiesSetsUpAndCollects is the full fake-msb lifecycle: create → copy in →
// helper setup → SSH material in place → session live → interrupted → changes.patch collected.
// It also pins the placement rule that keeps the material out of the deliverable: every SSH file
// lands under /.krayt, never under /workspace, so none of it can appear in the patch a human is
// asked to review.
func TestCodeSessionCreatesCopiesSetsUpAndCollects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{})

	runDir := filepath.Join(t.TempDir(), "run")
	stateDir := t.TempDir()
	var session orchestrator.CodeSession
	res, err := orchestrator.Code(ctx, orchestrator.Deps{Sandbox: sb}, codeSpec("run_code_e2e", src), runDir,
		orchestrator.CodeOptions{
			StateDir: stateDir, KraytExe: "/usr/local/bin/krayt",
			OnReady: func(s orchestrator.CodeSession) error {
				session = s
				// Stand in for the human editing over SSH: the fake sandbox's /workspace is a real
				// directory, and krayt-helper finish diffs it for real.
				ws := filepath.Join(sandboxRoot(home, "krayt-run_code_e2e"), "workspace")
				if werr := os.WriteFile(filepath.Join(ws, "greeting.txt"), []byte("hello from vscode\n"), 0o644); werr != nil {
					return werr
				}
				// …then end the session, exactly as Ctrl-C would.
				cancel()
				return nil
			},
		})
	if err != nil {
		t.Fatalf("orchestrator.Code: %v", err)
	}
	if res.Kept {
		t.Error("Kept = true, want false (no --keep)")
	}

	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if rec.EffectiveKind() != orchestrator.KindCode {
		t.Errorf("Kind = %q, want %q", rec.EffectiveKind(), orchestrator.KindCode)
	}
	if rec.State != orchestrator.StateDone {
		t.Errorf("State = %q, want %q — an interrupted code session ended normally, it did not fail", rec.State, orchestrator.StateDone)
	}
	if rec.Patch == nil || rec.Patch.FilesChanged != 1 {
		t.Errorf("Patch = %+v, want a 1-file diffstat", rec.Patch)
	}
	patchBytes, err := os.ReadFile(res.PatchPath)
	if err != nil {
		t.Fatalf("read changes.patch: %v", err)
	}
	if !strings.Contains(string(patchBytes), "hello from vscode") {
		t.Errorf("changes.patch does not carry the session's edit:\n%s", patchBytes)
	}
	for _, leaked := range []string{"id_ed25519", "host_ed25519", "authorized_keys", "sshd_config", "BEGIN OPENSSH"} {
		if strings.Contains(string(patchBytes), leaked) {
			t.Errorf("changes.patch mentions %q — SSH material must live under %s, never in /workspace",
				leaked, sshsession.GuestDir)
		}
	}

	// The host half: both private keys on the host, 0600, and a per-run ssh config.
	for _, name := range []string{
		sshsession.ClientKeyFile, sshsession.HostKeyFile, sshsession.KnownHostsFile,
		sshsession.SSHDConfigFile, sshsession.AuthorizedKeysFile, sshsession.ClientConfigFile,
	} {
		if _, serr := os.Stat(filepath.Join(runDir, sshsession.DirName, name)); serr != nil {
			t.Errorf("run dir missing ssh/%s: %v", name, serr)
		}
	}

	// The guest half: exactly the three files sshd reads, under /.krayt/ssh. The CLIENT private
	// key must never be among them — it stays on the host, which is the point of generating both
	// halves there.
	guestSSH := filepath.Join(sandboxRoot(home, "krayt-run_code_e2e"), sshsession.GuestDir)
	entries, err := os.ReadDir(guestSSH)
	if err != nil {
		t.Fatalf("read guest %s: %v", sshsession.GuestDir, err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{sshsession.HostKeyFile, sshsession.AuthorizedKeysFile, sshsession.SSHDConfigFile} {
		if !got[want] {
			t.Errorf("guest %s missing %s (have %v)", sshsession.GuestDir, want, got)
		}
	}
	for _, unwanted := range []string{sshsession.ClientKeyFile, sshsession.KnownHostsFile, sshsession.ClientConfigFile} {
		if got[unwanted] {
			t.Errorf("guest %s contains %s — that half never leaves the host", sshsession.GuestDir, unwanted)
		}
	}
	// /run/sshd, sshd's privilege-separation directory, created per session because /run is a tmpfs.
	if _, serr := os.Stat(filepath.Join(sandboxRoot(home, "krayt-run_code_e2e"), "run", "sshd")); serr != nil {
		t.Errorf("guest /run/sshd not created: %v", serr)
	}

	// The connection block the CLI prints.
	if session.Alias != "krayt-run_code_e2e" {
		t.Errorf("Alias = %q, want krayt-run_code_e2e", session.Alias)
	}
	if !strings.Contains(session.SSHCommand, "ssh -F") || !strings.Contains(session.SSHCommand, session.Alias) {
		t.Errorf("SSHCommand = %q, want a ready-to-paste `ssh -F … <alias>`", session.SSHCommand)
	}
	if want := "vscode-remote://ssh-remote+krayt-run_code_e2e/workspace"; session.VSCodeURI != want {
		t.Errorf("VSCodeURI = %q, want %q", session.VSCodeURI, want)
	}
	if session.IncludeLine != "Include "+sshsession.AggregateConfigPath(stateDir) {
		t.Errorf("IncludeLine = %q, want the one-time aggregate Include", session.IncludeLine)
	}

	// Teardown fired (no --keep).
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
					t.Error("Code must never pass --max-duration — a human is sitting in this session")
				}
				if a == "--vsock" {
					t.Error("Code must never pass --vsock (no ask_human channel in a session)")
				}
			}
		}
	}
	if !sawStop || !sawRm {
		t.Errorf("sawStop=%v sawRm=%v, want both true (no --keep)", sawStop, sawRm)
	}
}

// TestCodeKeepLeavesSandboxAndAlias: --keep behaves exactly as `krayt shell --keep` does — the
// sandbox survives, the record goes `kept` (not Terminal), the PID is cleared, and the session's
// ssh alias stays in the aggregate config so the human can keep connecting to it.
func TestCodeKeepLeavesSandboxAndAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	home := t.TempDir()
	sb := newFakeSandbox(t, home, fakeMsbScript{})

	stateDir := t.TempDir()
	runDir := orchestrator.RunDir(stateDir, "run_code_keep")
	res, err := orchestrator.Code(ctx, orchestrator.Deps{Sandbox: sb}, codeSpec("run_code_keep", src), runDir,
		orchestrator.CodeOptions{
			Keep: true, StateDir: stateDir, KraytExe: "/usr/local/bin/krayt",
			OnReady: func(orchestrator.CodeSession) error { cancel(); return nil },
		})
	if err != nil {
		t.Fatalf("orchestrator.Code: %v", err)
	}
	if !res.Kept {
		t.Error("Kept = false, want true")
	}
	for _, c := range readFakeMsbCalls(t, home) {
		if c.Args[0] == "stop" || c.Args[0] == "rm" {
			t.Errorf("teardown ran (%v) for a clean --keep exit — the sandbox must survive", c.Args)
		}
	}
	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != orchestrator.StateKept || rec.PID != 0 || rec.Terminal() {
		t.Errorf("record state=%q pid=%d terminal=%v, want kept/0/false", rec.State, rec.PID, rec.Terminal())
	}

	b, err := os.ReadFile(sshsession.AggregateConfigPath(stateDir))
	if err != nil {
		t.Fatalf("read aggregate ssh config: %v", err)
	}
	if !strings.Contains(string(b), filepath.Join(runDir, sshsession.DirName, sshsession.ClientConfigFile)) {
		t.Errorf("aggregate config does not Include the kept session:\n%s", b)
	}
}

// TestCodeTeardownOnFailedCreate: the teardown discipline Shell established — registered before
// Create is attempted, so a failure there cannot leak a sandbox even under --keep.
func TestCodeTeardownOnFailedCreate(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(keepSuffix(keep), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			src := newRepo(t, map[string]string{"a.txt": "1\n"})
			home := t.TempDir()
			sb := newFakeSandbox(t, home, fakeMsbScript{CreateExitCode: 1})

			runDir := filepath.Join(t.TempDir(), "run")
			_, err := orchestrator.Code(ctx, orchestrator.Deps{Sandbox: sb}, codeSpec("run_code_fail", src), runDir,
				orchestrator.CodeOptions{Keep: keep, StateDir: t.TempDir(), KraytExe: "/usr/local/bin/krayt"})
			if err == nil {
				t.Fatal("expected an error from a failed create")
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
			if !sawStop || !sawRm {
				t.Errorf("keep=%v: sawStop=%v sawRm=%v, want both true — a failed create must never leak a sandbox", keep, sawStop, sawRm)
			}
			rec, rerr := orchestrator.ReadRecord(runDir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if rec.State != orchestrator.StateFailed {
				t.Errorf("State = %q, want %q", rec.State, orchestrator.StateFailed)
			}
		})
	}
}

// TestNoIngressFlagsEmittedByCode is decision 17's guard, asserted rather than inspected: in every
// network mode, `krayt code`'s `msb create` argv publishes no port and does not relax ingress. The
// whole design exists because there is no verified host→guest TCP path into an msb sandbox — if
// this ever fails, the transport has quietly changed and §6.6's security position with it.
func TestNoIngressFlagsEmittedByCode(t *testing.T) {
	for _, mode := range []task.NetworkMode{task.NetworkAllowlist, task.NetworkFull, task.NetworkNone} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			src := newRepo(t, map[string]string{"a.txt": "1\n"})
			home := t.TempDir()
			sb := newFakeSandbox(t, home, fakeMsbScript{})

			spec := codeSpec("run_code_net", src)
			spec.Network = task.NetworkPolicy{Mode: mode}
			runDir := filepath.Join(t.TempDir(), "run")
			if _, err := orchestrator.Code(ctx, orchestrator.Deps{Sandbox: sb}, spec, runDir,
				orchestrator.CodeOptions{
					StateDir: t.TempDir(), KraytExe: "/usr/local/bin/krayt",
					OnReady: func(orchestrator.CodeSession) error { cancel(); return nil },
				}); err != nil {
				t.Fatalf("orchestrator.Code: %v", err)
			}

			var create []string
			for _, c := range readFakeMsbCalls(t, home) {
				if c.Args[0] == "create" {
					create = c.Args
				}
			}
			if create == nil {
				t.Fatal("no `msb create` observed")
			}
			// No port/publish/forward flag of any spelling.
			for i, a := range create {
				for _, forbidden := range []string{"--port", "--publish", "--expose", "--forward", "-p", "--ingress"} {
					if a == forbidden || strings.HasPrefix(a, forbidden+"=") {
						t.Errorf("create argv[%d] = %q — krayt code publishes no port (decision 17): %v", i, a, create)
					}
				}
			}
			// Ingress is either not mentioned (the deny-default modes need no separate flag) or
			// explicitly denied — never allowed.
			for i, a := range create {
				if a != "--net-default-ingress" {
					continue
				}
				if i+1 >= len(create) || create[i+1] != "deny" {
					t.Errorf("--net-default-ingress = %v, want deny: %v", create[i+1:], create)
				}
			}
		})
	}
}

// TestKindCodeRoundTripsThroughState: the new kind survives a write/read of meta.json, reads as a
// session kind, and — for a kept session — is deliberately not Terminal, since its sandbox is
// still alive. Absent `kind` still means `run`, so no existing run dir needs migrating.
func TestKindCodeRoundTripsThroughState(t *testing.T) {
	runDir := t.TempDir()
	if err := orchestrator.WriteRecord(runDir, orchestrator.RunRecord{
		ID: "run_kind", Kind: orchestrator.KindCode, State: orchestrator.StateKept,
		SandboxName: "krayt-run_kind",
	}); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if rec.Kind != orchestrator.KindCode || rec.EffectiveKind() != orchestrator.KindCode {
		t.Errorf("Kind = %q / EffectiveKind = %q, want %q", rec.Kind, rec.EffectiveKind(), orchestrator.KindCode)
	}
	if rec.Terminal() {
		t.Error("a kept code session must not be Terminal() — its sandbox is still alive")
	}
	b, err := os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kind": "code"`) {
		t.Errorf("meta.json does not carry the kind:\n%s", b)
	}

	for kind, want := range map[string]bool{
		orchestrator.KindCode:  true,
		orchestrator.KindShell: true,
		orchestrator.KindRun:   false,
	} {
		if got := orchestrator.IsSessionKind(kind); got != want {
			t.Errorf("IsSessionKind(%q) = %v, want %v", kind, got, want)
		}
	}

	// A record from before either session kind existed still reads as a run.
	legacy := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacy, "meta.json"), []byte(`{"id":"run_old","state":"done"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := orchestrator.ReadRecord(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if old.EffectiveKind() != orchestrator.KindRun {
		t.Errorf("EffectiveKind of a pre-kind record = %q, want %q", old.EffectiveKind(), orchestrator.KindRun)
	}
}
