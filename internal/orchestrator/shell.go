package orchestrator

// This file is the shell lifecycle for `krayt shell` (add-interactive-shell-session.md) — a
// separate, human-driven session, not the headless autonomous path Run drives. It reuses Run's
// shared prologue helpers (copyInputs, helperSetup, finishAndCollect, and the sandboxName/
// sandboxAgentUser/sandboxSecurity/container-path constants defined in orchestrator.go) rather
// than duplicating them, and departs from Run in exactly the ways the task's decisions require:
// no context.WithTimeout / --max-duration (decision 5), no ask_human wiring (decision 12), no
// adapter-driven launcher, krayt-ask wiring, or transcript capture (decision 2), and conditional
// teardown driven by --keep (decision 3) instead of unconditional teardown on every path.
//
// Decision 2 is about what happens INSIDE the sandbox — shell never runs an agent process or
// wires it up, a human does that by hand. It says nothing about the host-side secret-scoping
// pre-flight msb requires: since 2026-09-16 (KRAYT_SPEC.md §13 amendment),
// internal/cli/shell.go's runShellFresh still resolves agent.adapter's Plan.Secrets (via
// applyAdapterSecrets) before ValidateNetworkPolicyForMsb, purely so the same krayt.yaml that
// auto-scopes a credential for `krayt run` does not force a human to hand-write the identical
// network.inject entry just to start that agent themselves inside the shell. Shell.go itself
// (this file) is unaffected — RunSpec arrives with Network.Secrets already resolved by the CLI
// layer, same as every other field.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/sandbox"
	"github.com/418-cloud/krayt/internal/secrets"
	"github.com/418-cloud/krayt/internal/task"
)

// StateKept is the terminal-ISH state (deliberately NOT Terminal(), see RunRecord.Terminal) of a
// `krayt shell --keep` session after the human exits: the sandbox is still alive, nothing
// supervises it any more (PID is cleared), and it is re-entered with `krayt shell --attach
// <run-id>` or destroyed with `krayt stop <run-id>` (decisions 3, 4). A record in this state is
// exactly what `krayt doctor`'s orphan check (decision 6) expects to find still tracking a live
// "krayt-*" sandbox — the orphan case is a sandbox with NO such record, not this one.
const StateKept = "kept"

// defaultShellCommand is what a `krayt shell` session execs when the human didn't ask for
// something else (--exec). Decision 10's original design left TTYExecSpec.Command empty and
// trusted msb's documented "attaches to the default shell" behavior, specifically to avoid krayt
// hardcoding a shell path (KRAYT_SPEC.md §6.15). Hardware verification (2026-09-16,
// HUMAN_TODO.md's "krayt shell" entry, "Verify first" #4) found that assumption wrong for every
// published krayt agent image: each sets ENTRYPOINT ["krayt-agent-entrypoint"] and no image-level
// Shell, and against that shape `msb exec --tty` with no Command re-execs the ENTRYPOINT instead
// of a shell — a real session showed the headless entrypoint's own "task file /task/prompt.md not
// found" and exited 66 before a human ever saw a prompt. So krayt now resolves the shell itself,
// in the order a human would expect: $SHELL if the exec'd user has one set and it's executable,
// else /bin/bash (every published agent image ships it, and it's the agent user's own login
// shell), else /bin/sh (present on virtually any Linux image, for a user-supplied --image that
// ships neither). /bin/sh itself is assumed present — true of every mainstream Linux base image
// and the one thing this whole fallback chain has no further fallback for.
var defaultShellCommand = []string{"/bin/sh", "-c",
	`sh_bin="${SHELL:-/bin/bash}"; [ -x "$sh_bin" ] || sh_bin=/bin/sh; exec "$sh_bin" -l`}

// ttyCommand returns execCmd unchanged when the human asked for something specific (--exec), and
// defaultShellCommand otherwise — the one place both Shell and AttachShell decide what an empty
// --exec actually runs.
func ttyCommand(execCmd []string) []string {
	if len(execCmd) == 0 {
		return defaultShellCommand
	}
	return execCmd
}

// ShellResult summarizes one `krayt shell` (or `--attach`) session for the caller and `krayt`
// output.
type ShellResult struct {
	RunDir        string
	ExitCode      int
	PatchPath     string
	CommitsBundle string
	Safety        []string
	Kept          bool // true when the sandbox survived teardown — attached with `krayt shell --attach <run-id>`
}

// Shell drives a fresh `krayt shell` session end to end: create the sandbox, copy the repo
// snapshot in, run krayt-helper setup as root (§7 steps 1-2-4-5-6, via copyInputs/helperSetup —
// see the package doc), attach an interactive tty in place of Run's agent exec (decision 10),
// then krayt-helper finish + collect on the way out (decision 8, via finishAndCollect) before
// conditionally tearing the sandbox down.
//
// Teardown fires on every path EXCEPT keep==true with a clean exit (decision 3) — registered
// before Create is even attempted, exactly like Run's, so a failed Create, a failed krayt-helper
// setup, a tty-attach error, or ctx cancellation (Ctrl-C, a killed terminal) all still stop and
// remove whatever msb created, even under --keep. Only a session that reaches the very end
// without error sets cleanExit, which is what lets a --keep session survive.
func Shell(ctx context.Context, deps Deps, spec task.RunSpec, runDir string, keep bool, execCmd []string) (res *ShellResult, err error) {
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		return nil, fmt.Errorf("orchestrator: create run dir: %w", err)
	}

	specs := spec.Network.Secrets
	var secretValues map[string]string
	if spec.SecretsPath != "" {
		secretValues, err = secrets.Load(spec.SecretsPath)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: load secrets: %w", err)
		}
	}
	secretKeyNames := make([]string, 0, len(specs))
	for _, s := range specs {
		secretKeyNames = append(secretKeyNames, s.Key)
	}
	sort.Strings(secretKeyNames)
	hasSecrets := len(specs) > 0

	var extraConfMeta *ExtraConfMeta
	if spec.ExtraConf != "" {
		d, derr := digestFile(spec.ExtraConf)
		if derr != nil {
			return nil, fmt.Errorf("orchestrator: read sandbox.extra_conf: %w", derr)
		}
		extraConfMeta = &ExtraConfMeta{Path: spec.ExtraConf, Digest: d.String()}
	}

	name := sandboxName(spec.ID)
	rec := RunRecord{
		ID: spec.ID, ImageRef: spec.ImageRef, RepoPath: spec.RepoPath, Kind: KindShell,
		TaskSummary: summarizeTask(spec.TaskPrompt),
		Network: NetworkMeta{
			Mode: string(spec.Network.Mode), Allow: spec.Network.Allow,
			MITM: hasSecrets, InjectedKeys: secretKeyNames,
		},
		Resources: ResourceMeta{CPUs: spec.Resources.CPUs, MemoryMiB: spec.Resources.MemoryMiB, DiskGiB: spec.Resources.DiskGiB},
		State:     StateStarting, StartedAt: nowStamp(), PID: os.Getpid(),
		SandboxName: name,
		ExtraConf:   extraConfMeta,
	}
	var recMu sync.Mutex
	persistRec := func() { recMu.Lock(); _, _ = writeRecord(runDir, rec); recMu.Unlock() }
	persistRec()

	cleanExit := false
	defer func() {
		recMu.Lock()
		defer recMu.Unlock()
		rec.EndedAt = nowStamp()
		rec.DurationSecs = durationSecs(rec.StartedAt, rec.EndedAt)
		switch {
		case err != nil:
			if cause := context.Cause(ctx); cause != nil &&
				!errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
				err = cause
			}
			rec.State, rec.Error = StateFailed, err.Error()
		case keep && cleanExit:
			// No process outlives a --keep session's own exit (that is the point of --keep), so
			// PID no longer names anything `krayt stop` could signal — decision 3's re-entry model
			// depends on `krayt stop` recognizing that and stopping the sandbox directly by name.
			rec.State, rec.ExitCode, rec.PID = StateKept, res.ExitCode, 0
		case res != nil:
			rec.State, rec.ExitCode = StateDone, res.ExitCode
		}
		metaDigest, _ := writeRecord(runDir, rec)
		notes := agentNotes(runDir)
		_ = writeReport(runDir, rec, notes, metaDigest)
	}()

	// Teardown, EXCEPT a --keep session that ended cleanly (decision 3) — see the doc comment
	// above. Registered before Create so it fires on every earlier failure regardless of --keep.
	defer func() {
		if keep && cleanExit {
			return
		}
		if out, lerr := deps.Sandbox.SystemLogs(ctx, name); lerr == nil || len(out) > 0 {
			writeConsoleLog(out, runDir, secretValues)
		}
		_ = deps.Sandbox.Stop(ctx, name)
		_ = deps.Sandbox.Remove(ctx, name)
	}()

	netArgs, nerr := task.NetworkArgs(spec.Network, hasSecrets)
	if nerr != nil {
		return nil, fmt.Errorf("orchestrator: %w", nerr)
	}
	secretRefs := make([]sandbox.SecretRef, len(specs))
	for i, s := range specs {
		secretRefs[i] = sandbox.SecretRef{Name: s.Key, Hosts: s.Hosts}
	}
	secretEnv, serr := sandbox.SecretEnv(specs, secretValues)
	if serr != nil {
		return nil, fmt.Errorf("orchestrator: %w", serr)
	}

	// 1. Create (rent) the sandbox. No --max-duration (decision 5: no wall-clock budget for a
	// human-driven session) and no --vsock route (decision 12: no ask_human channel in shell mode).
	createSpec := sandbox.CreateSpec{
		Image: spec.ImageRef, Name: name, User: sandboxAgentUser,
		CPUs: spec.Resources.CPUs, MemoryMiB: spec.Resources.MemoryMiB, DiskGiB: spec.Resources.DiskGiB,
		Env:       envVarsFromMap(spec.Env),
		Secrets:   secretRefs,
		Security:  sandboxSecurity,
		ExtraConf: spec.ExtraConf,
		ExtraArgs: netArgs,
	}
	if err := deps.Sandbox.Create(ctx, createSpec, secretEnv); err != nil {
		return nil, fmt.Errorf("orchestrator: create sandbox: %w", err)
	}

	// 2. Copy in: the git bundle, krayt-helper, and (decision 13) the task prompt only if one was
	// given. No krayt-ask (decision 12).
	cir, err := copyInputs(ctx, deps.Sandbox, name, spec, false)
	if err != nil {
		return nil, err
	}
	recMu.Lock()
	rec.Provenance = &cir.Provenance
	_, _ = writeRecord(runDir, rec)
	recMu.Unlock()

	// 3. Exec the helper as root: clone the bundle into /workspace, tag krayt-baseline, snapshot
	// the root-only patch-git, then relax /workspace for the agent user.
	baseline, err := helperSetup(ctx, deps.Sandbox, name)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: krayt-helper setup: %w", err)
	}

	// The code snapshot is now durably captured inside the sandbox — matching Run's own rule
	// (§6.2): `running` means "safe to mutate the host repo now".
	recMu.Lock()
	rec.State = StateRunning
	_, _ = writeRecord(runDir, rec)
	recMu.Unlock()

	// 3b. Seed each selected adapter's first-run guest config (§6.14 "First-run state",
	// seed-agent-first-run-config.md) — as the agent user, before the human ever gets a shell, so
	// an agent started by hand authenticates without hitting onboarding or an auth dialog.
	// Best-effort: never fails the session. AttachShell/PatchLiveShell never seed (decision 5):
	// the sandbox already exists and the human may have changed these files since.
	applyConfigSeeds(ctx, deps.Sandbox, name, spec.ConfigSeeds, deps.Warn)

	// 4. Attach an interactive tty in place of Run's agent exec (decision 10) — msb owns the pty
	// from here on; this call blocks until the human exits the shell (or, with --exec, until the
	// given command finishes). ttyCommand supplies defaultShellCommand when execCmd is empty —
	// see its doc comment for why krayt picks the shell explicitly rather than leaving this to msb.
	execResult, execErr := deps.Sandbox.ExecTTY(ctx, sandbox.TTYExecSpec{Name: name, User: sandboxAgentUser, Command: ttyCommand(execCmd)})
	if execErr != nil {
		return nil, fmt.Errorf("orchestrator: attach shell: %w", execErr)
	}

	// 5-7. Exec the helper again as root, diff against the baseline, assemble + collect /output,
	// then host-side diffstat + safety lint + secret-value scan (decision 8: a session produces a
	// patch, same as a run).
	fres, ferr := finishAndCollect(ctx, deps.Sandbox, name, runDir, baseline, secretValues)
	if ferr != nil {
		return nil, fmt.Errorf("orchestrator: %w", ferr)
	}

	cleanExit = true
	res = &ShellResult{
		RunDir: runDir, ExitCode: execResult.ExitCode, PatchPath: fres.PatchPath,
		CommitsBundle: fres.CommitsBundle, Safety: fres.Safety, Kept: keep,
	}
	recMu.Lock()
	rec.Patch = fres.Patch
	rec.Safety = fres.Safety
	recMu.Unlock()
	return res, nil
}

// AttachShell re-enters a live, kept `krayt shell --keep` session by its run id (decision 4) — no
// Create, no copy-in, no helper setup: the sandbox and its /workspace already exist exactly as
// the human left them. On exit it runs krayt-helper finish + collect again, same as Shell's own
// tail, so a second round of edits folds into changes.patch too (decision 8). It never calls
// Stop/Remove on ANY path, including its own errors: decision 3 puts sole authority to destroy a
// kept sandbox in `krayt stop <run-id>`, so an attach that fails (say, the sandbox was removed by
// hand between attaches) must not make that worse by tearing down something a human might still
// want to inspect with `msb logs`/`msb exec`.
//
// secretsPath is optional: RunRecord persists only secret KEY NAMES (never values, §8.4), so
// re-scanning the refreshed patch for a leaked secret VALUE on this second pass needs the
// secrets file supplied again — via the same --secrets flag `krayt shell` already has. Empty
// means the scan is skipped for this attach, same as an ordinary run with no secrets.
func AttachShell(ctx context.Context, deps Deps, runDir, secretsPath string, execCmd []string) (res *ShellResult, err error) {
	rec, rerr := ReadRecord(runDir)
	if rerr != nil {
		return nil, fmt.Errorf("orchestrator: read run record: %w", rerr)
	}
	if rec.EffectiveKind() != KindShell {
		return nil, fmt.Errorf("orchestrator: run %q is not a shell session (kind=%q)", rec.ID, rec.EffectiveKind())
	}
	if rec.State != StateKept {
		return nil, fmt.Errorf("orchestrator: run %q is not a kept, attachable shell session (state=%q, want %q)", rec.ID, rec.State, StateKept)
	}
	if rec.SandboxName == "" {
		return nil, fmt.Errorf("orchestrator: run %q has no recorded sandbox name", rec.ID)
	}
	name := rec.SandboxName

	var secretValues map[string]string
	if secretsPath != "" {
		secretValues, err = secrets.Load(secretsPath)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: load secrets: %w", err)
		}
	}

	rec.PID = os.Getpid()
	rec.State = StateRunning
	if _, werr := writeRecord(runDir, rec); werr != nil {
		return nil, fmt.Errorf("orchestrator: %w", werr)
	}

	cleanExit := false
	defer func() {
		rec.EndedAt = nowStamp()
		rec.DurationSecs = durationSecs(rec.StartedAt, rec.EndedAt)
		switch {
		case err != nil:
			if cause := context.Cause(ctx); cause != nil &&
				!errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
				err = cause
			}
			rec.State, rec.Error = StateFailed, err.Error()
		case cleanExit:
			rec.State, rec.ExitCode, rec.PID = StateKept, res.ExitCode, 0
		}
		metaDigest, _ := writeRecord(runDir, rec)
		notes := agentNotes(runDir)
		_ = writeReport(runDir, rec, notes, metaDigest)
	}()

	execResult, execErr := deps.Sandbox.ExecTTY(ctx, sandbox.TTYExecSpec{Name: name, User: sandboxAgentUser, Command: ttyCommand(execCmd)})
	if execErr != nil {
		return nil, fmt.Errorf("orchestrator: attach shell: %w", execErr)
	}

	fres, ferr := finishAndCollect(ctx, deps.Sandbox, name, runDir, patch.BaselineTag, secretValues)
	if ferr != nil {
		return nil, fmt.Errorf("orchestrator: %w", ferr)
	}

	cleanExit = true
	res = &ShellResult{
		RunDir: runDir, ExitCode: execResult.ExitCode, PatchPath: fres.PatchPath,
		CommitsBundle: fres.CommitsBundle, Safety: fres.Safety, Kept: true,
	}
	rec.Patch = fres.Patch
	rec.Safety = fres.Safety
	return res, nil
}

// PatchLiveShell re-derives changes.patch from a still-running (not yet `kept`) shell session's
// current workspace state, without ending the session — `krayt patch <run-id>` against a live
// `kind: shell` record (decision 9). It is host-side copy-out plus a guest-side krayt-helper
// finish, exactly like the tail of Shell/AttachShell, and is safe to call repeatedly: each call
// re-derives the patch from whatever is in /workspace at that moment. It does not touch rec.State
// or PID — the interactive session (Shell/AttachShell, running in a different process/terminal)
// owns those.
func PatchLiveShell(ctx context.Context, deps Deps, runDir string) (string, error) {
	rec, err := ReadRecord(runDir)
	if err != nil {
		return "", fmt.Errorf("orchestrator: read run record: %w", err)
	}
	if rec.SandboxName == "" {
		return "", fmt.Errorf("orchestrator: run %q has no recorded sandbox name", rec.ID)
	}
	fres, err := finishAndCollect(ctx, deps.Sandbox, rec.SandboxName, runDir, patch.BaselineTag, nil)
	if err != nil {
		return "", fmt.Errorf("orchestrator: %w", err)
	}
	return fres.PatchPath, nil
}
