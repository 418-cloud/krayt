package orchestrator

// This file is the session lifecycle for `krayt code` (add-vscode-remote-ssh-session.md) — an SSH
// remote-dev session a real editor (VS Code Remote-SSH, Cursor, JetBrains Gateway) or plain
// `ssh`/`scp`/`rsync` opens on /workspace inside the sandbox. It is `Shell`'s sibling, not a mode
// of it: the same shared prologue (copyInputs → helperSetup → applyConfigSeeds →
// trustWorkspaceForGit) and the same finishAndCollect tail, with the tty attach replaced by SSH
// material copied in and a block until the human ends the session.
//
// It opens NO ingress and publishes NO port. There is no verified host→guest TCP path into an msb
// sandbox — `--vsock` is guest→host and create-time only, ingress is denied in every network mode,
// and `CreateSpec` has no port field at all (§6.6) — so the transport is `sshd -i` (inetd mode:
// one connection on stdin/stdout, then exit) reached through an `ssh` ProxyCommand that pipes
// bytes through `msb exec --stream` (`krayt code --stdio`, internal/cli/code_stdio.go). That is
// also why this is NOT the guest daemon §6.13 forbids: nothing listens inside the sandbox, ever.
// `sshd -i` is exec'd per connection, argv-in, and exits with the connection — the same stateless
// shape as krayt-helper.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/418-cloud/krayt/internal/sandbox"
	"github.com/418-cloud/krayt/internal/sandbox/guestbin"
	"github.com/418-cloud/krayt/internal/secrets"
	"github.com/418-cloud/krayt/internal/sshsession"
	"github.com/418-cloud/krayt/internal/task"
)

// sshdRunDir is sshd's privilege-separation directory. It must exist before `sshd -i` runs, and
// creating it in the image's Dockerfile is NOT sufficient: /run is commonly a tmpfs, so whatever
// the image built there is gone by the time the sandbox boots. krayt creates it per session.
const sshdRunDir = "/run/sshd"

// codeCollectTimeout bounds the krayt-helper finish + copy-out that runs AFTER the session's own
// context is already cancelled (the human pressed Ctrl-C, which is the normal way a `krayt code`
// session ends). Without a fresh, detached deadline that work would either inherit a dead context
// and collect nothing, or hang forever on a wedged sandbox with the human already waiting.
const codeCollectTimeout = 5 * time.Minute

// CodeOptions carries what a code session needs beyond a RunSpec — three host-side facts a shell
// session has no equivalent of (the state dir holding the aggregate ssh config, the krayt binary
// the generated ProxyCommand invokes, and a way to tell the caller the session is reachable),
// plus Keep, which behaves exactly as `krayt shell --keep` does.
type CodeOptions struct {
	// Keep leaves the sandbox running when the session ends, destroyed only by `krayt stop`.
	Keep bool
	// StateDir is the repo's `.krayt` directory; the stable aggregate ssh config lives at
	// <StateDir>/ssh/config. Empty skips the aggregate entirely (the per-run config still works
	// with `ssh -F`).
	StateDir string
	// KraytExe is the absolute path to this krayt binary, baked into the generated ProxyCommand.
	// ssh — and VS Code's own ssh — runs with an environment krayt does not control, so PATH is not
	// something the ProxyCommand may rely on.
	KraytExe string
	// OnReady is called once, after the sandbox is up and the ssh material is in place, with
	// everything the human needs to connect. `krayt code` prints it; a test asserts on it. An
	// error returned here fails the session (and tears the sandbox down), since a session nobody
	// can be told how to reach is not a session.
	OnReady func(CodeSession) error
}

// CodeSession is the connection information for a live `krayt code` session — printed by the CLI
// (decision 13: krayt prints, it never launches an editor).
type CodeSession struct {
	RunID string
	Alias string // `krayt-<run-id>`, the ssh alias and the msb sandbox name both
	User  string // the sandbox's own user, which is also the SSH login user

	ConfigPath    string // <runDir>/ssh/config — usable directly with `ssh -F`
	AggregatePath string // <stateDir>/ssh/config — the stable file ~/.ssh/config Includes; "" if none
	SSHCommand    string // a ready-to-paste `ssh -F … krayt-<id>`
	IncludeLine   string // the one-time line the human adds to ~/.ssh/config themselves
	VSCodeURI     string // `vscode-remote://ssh-remote+krayt-<id>/workspace`
}

// CodeResult summarizes one finished `krayt code` session.
type CodeResult struct {
	RunDir        string
	PatchPath     string
	CommitsBundle string
	Safety        []string
	Session       CodeSession
	Kept          bool
}

// Code drives a `krayt code` session end to end. Steps 1-3b are Shell's, verbatim, through the
// same shared helpers. Where Shell attaches a tty, Code:
//
//  1. generates ephemeral ed25519 key material into <runDir>/ssh/ (decision 10);
//  2. copies the host key, authorized_keys and sshd_config to /.krayt/ssh — deliberately outside
//     /workspace and /output, so none of it can land in changes.patch;
//  3. creates /run/sshd and re-applies root ownership + 0600 on the host key (sshd refuses a
//     group- or world-readable host key regardless of StrictModes, and `msb copy`'s mode
//     preservation is not a pinned contract);
//  4. writes the per-run ssh config, refreshes the aggregate, and hands both to OnReady;
//  5. blocks until the session is interrupted, then collects and (unless Keep) tears down.
//
// Teardown discipline matches Shell exactly — registered before Create is attempted, firing on
// every path except a clean exit under Keep — so a failed create, a failed setup, or a killed
// terminal cannot leak a sandbox.
func Code(ctx context.Context, deps Deps, spec task.RunSpec, runDir string, opts CodeOptions) (res *CodeResult, err error) {
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		return nil, fmt.Errorf("orchestrator: create run dir: %w", err)
	}
	// Registered FIRST so it runs LAST (defers are LIFO): the aggregate lists live sessions, and
	// whether this one still counts is decided by the record-writing defer below.
	defer func() { _ = RefreshAggregateSSHConfig(opts.StateDir) }()

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
		ID: spec.ID, ImageRef: spec.ImageRef, RepoPath: spec.RepoPath, Kind: KindCode,
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
		case opts.Keep && cleanExit:
			// Same as Shell: no process outlives a --keep session, so PID no longer names anything
			// `krayt stop` could signal — it stops the sandbox by name instead.
			rec.State, rec.PID = StateKept, 0
		case res != nil:
			rec.State = StateDone
		}
		metaDigest, _ := writeRecord(runDir, rec)
		notes := agentNotes(runDir)
		_ = writeReport(runDir, rec, notes, metaDigest)
	}()

	createFailed := false
	defer func() {
		if opts.Keep && cleanExit {
			return
		}
		if out, lerr := deps.Sandbox.SystemLogs(ctx, name); lerr == nil || len(out) > 0 {
			if writeConsoleLog(out, runDir, secretValues) && createFailed {
				err = pointCreateErrorAtConsoleLog(err, name, ConsoleLogPath(runDir))
			}
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

	// 1. Create. Like Shell: no --max-duration (a human is sitting in this session), no --vsock
	// (no ask_human channel), and — decision 17 — nothing that publishes a port or touches
	// ingress. netArgs is task.NetworkArgs' output unmodified.
	user, err := resolveSandboxUser(ctx, deps.Sandbox, spec.ImageRef)
	if err != nil {
		return nil, err
	}
	recMu.Lock()
	rec.SandboxUser = user
	_, _ = writeRecord(runDir, rec)
	recMu.Unlock()

	// Generated before Create, though written to the run dir either way: keygen is pure and
	// offline, so failing here costs nothing, whereas failing after Create would mean tearing a
	// booted sandbox back down for a reason that had nothing to do with it.
	material, err := sshsession.GenerateSession(user, spec.ID)
	if err != nil {
		return nil, err
	}
	sshDir := filepath.Join(runDir, sshsession.DirName)
	if err := sshsession.Write(sshDir, material); err != nil {
		return nil, err
	}

	createSpec := sandbox.CreateSpec{
		Image: spec.ImageRef, Name: name, User: user,
		CPUs: spec.Resources.CPUs, MemoryMiB: spec.Resources.MemoryMiB, DiskGiB: spec.Resources.DiskGiB,
		Env:       envVarsFromMap(spec.Env),
		Secrets:   secretRefs,
		Security:  sandboxSecurity,
		ExtraConf: spec.ExtraConf,
		ExtraArgs: netArgs,
	}
	if err := deps.Sandbox.Create(ctx, createSpec, secretEnv); err != nil {
		createFailed = true
		return nil, fmt.Errorf("orchestrator: create sandbox: %w", err)
	}

	// 2. Copy in the git bundle, krayt-helper and (if given) the task prompt. No krayt-ask.
	cir, err := copyInputs(ctx, deps.Sandbox, name, spec, false)
	if err != nil {
		return nil, err
	}
	recMu.Lock()
	rec.Provenance = &cir.Provenance
	_, _ = writeRecord(runDir, rec)
	recMu.Unlock()

	// 3. krayt-helper setup as root.
	baseline, err := helperSetup(ctx, deps.Sandbox, name, user)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: krayt-helper setup: %w", err)
	}

	recMu.Lock()
	rec.State = StateRunning
	_, _ = writeRecord(runDir, rec)
	recMu.Unlock()

	// 3b/3c. Same as Shell: seed the adapter's first-run config and make /workspace's root-owned
	// .git usable by the session's user — an agent started in a VS Code terminal needs both
	// exactly as much as one started in `krayt shell` does.
	applyConfigSeeds(ctx, deps.Sandbox, name, user, spec.ConfigSeeds, deps.Warn)
	trustWorkspaceForGit(ctx, deps.Sandbox, name, user, deps.Warn)

	// 4. Put the guest's half of the SSH material in place.
	if err := installSSHMaterial(ctx, deps.Sandbox, name, sshDir); err != nil {
		return nil, err
	}

	// 5. Write the per-run ssh config, refresh the aggregate, and tell the caller how to connect.
	session, err := publishSession(runDir, spec.RepoPath, opts, material)
	if err != nil {
		return nil, err
	}
	if opts.OnReady != nil {
		if err := opts.OnReady(session); err != nil {
			return nil, err
		}
	}

	// 6. Block. Unlike Shell, krayt is not in the data path at all from here: each `ssh` the human
	// (or VS Code) starts runs its own `krayt code --stdio` ProxyCommand against this sandbox.
	// This process exists only to own the sandbox's lifetime, so the session ends when it is
	// interrupted — Ctrl-C, `krayt stop`'s SIGTERM, or a closed terminal.
	<-ctx.Done()

	// 7. Collect on a DETACHED context: the cancellation above is the normal way this session
	// ends, so the patch has to be derived after it, not abandoned because of it.
	collectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), codeCollectTimeout)
	defer cancel()
	fres, ferr := finishAndCollect(collectCtx, deps.Sandbox, name, runDir, baseline, secretValues)
	if ferr != nil {
		return nil, fmt.Errorf("orchestrator: %w", ferr)
	}

	cleanExit = true
	res = &CodeResult{
		RunDir: runDir, PatchPath: fres.PatchPath, CommitsBundle: fres.CommitsBundle,
		Safety: fres.Safety, Session: session, Kept: opts.Keep,
	}
	recMu.Lock()
	rec.Patch = fres.Patch
	rec.Safety = fres.Safety
	recMu.Unlock()
	return res, nil
}

// installSSHMaterial copies the guest's half of the session material into /.krayt/ssh and fixes it
// up in place. Three separate concerns, each with its own reason:
//
//   - /.krayt/ssh and /run/sshd are created first. `msb copy` does not create a missing parent
//     (copyInputs' own comment), and /run/sshd is sshd's privilege-separation directory, which
//     cannot be baked into the image because /run is typically a tmpfs.
//   - Only the three files sshd itself reads are copied. Neither private client key nor
//     known_hosts ever enters the sandbox: the client half stays on the host, which is the whole
//     point of generating both halves there.
//   - Ownership and modes are re-applied afterwards. sshd refuses a host key that is group- or
//     world-readable regardless of StrictModes, and msb's mode preservation across `copy` is not
//     a pinned contract.
func installSSHMaterial(ctx context.Context, sb *sandbox.Client, name, sshDir string) error {
	// A cheap structural check rather than a comment: sshsession spells the guest path itself, so
	// it stays a pure package, and this is where the two definitions are held to agree.
	if !strings.HasPrefix(sshsession.GuestDir, guestbin.GuestRoot+"/") {
		return fmt.Errorf("orchestrator: sshsession.GuestDir %q is outside %q — the SSH material must "+
			"live under krayt's own guest root, never in /workspace or /output", sshsession.GuestDir, guestbin.GuestRoot)
	}
	if _, err := execCapture(ctx, sb, name, "root", []string{"mkdir", "-p", sshsession.GuestDir, sshdRunDir}); err != nil {
		return fmt.Errorf("orchestrator: create guest ssh directories: %w", err)
	}
	copies := []copySpec{
		{filepath.Join(sshDir, sshsession.HostKeyFile), sshsession.GuestHostKeyPath},
		{filepath.Join(sshDir, sshsession.AuthorizedKeysFile), sshsession.GuestAuthorizedKeysPath},
		{filepath.Join(sshDir, sshsession.SSHDConfigFile), sshsession.GuestSSHDConfigPath},
	}
	for _, c := range copies {
		dst := name + ":" + c.guest
		if err := sb.Copy(ctx, c.local, dst); err != nil {
			return fmt.Errorf("orchestrator: copy %s: %w", dst, err)
		}
	}
	if _, err := execCapture(ctx, sb, name, "root", []string{"chown", "-R", "root:root", sshsession.GuestDir}); err != nil {
		return fmt.Errorf("orchestrator: chown %s: %w", sshsession.GuestDir, err)
	}
	if _, err := execCapture(ctx, sb, name, "root", []string{"chmod", "0700", sshsession.GuestDir}); err != nil {
		return fmt.Errorf("orchestrator: chmod %s: %w", sshsession.GuestDir, err)
	}
	if _, err := execCapture(ctx, sb, name, "root", []string{"chmod", "0600",
		sshsession.GuestHostKeyPath, sshsession.GuestAuthorizedKeysPath, sshsession.GuestSSHDConfigPath}); err != nil {
		return fmt.Errorf("orchestrator: chmod guest ssh material: %w", err)
	}
	return nil
}

// publishSession writes <runDir>/ssh/config, refreshes <stateDir>/ssh/config, and assembles the
// CodeSession the CLI prints. krayt writes only inside `.krayt/` (decision 12) — ~/.ssh/config is
// the human's to edit, with the one `Include` line this returns.
func publishSession(runDir, repoPath string, opts CodeOptions, m *sshsession.Material) (CodeSession, error) {
	sshDir := filepath.Join(runDir, sshsession.DirName)
	cfg := sshsession.ClientConfig{
		RunID: m.RunID, User: m.User, Dir: sshDir,
		KraytExe: opts.KraytExe, RepoPath: repoPath,
	}
	if err := sshsession.WriteClientConfig(sshDir, cfg); err != nil {
		return CodeSession{}, err
	}
	configPath := filepath.Join(sshDir, sshsession.ClientConfigFile)
	alias := sshsession.Alias(m.RunID)
	session := CodeSession{
		RunID: m.RunID, Alias: alias, User: m.User,
		ConfigPath: configPath,
		SSHCommand: fmt.Sprintf("ssh -F %q %s", configPath, alias),
		VSCodeURI:  sshsession.VSCodeURI(alias, containerWorkspace),
	}
	if opts.StateDir != "" {
		if err := RefreshAggregateSSHConfig(opts.StateDir); err != nil {
			return CodeSession{}, err
		}
		session.AggregatePath = sshsession.AggregateConfigPath(opts.StateDir)
		session.IncludeLine = "Include " + session.AggregatePath
	}
	return session, nil
}

// RefreshAggregateSSHConfig rewrites <stateDir>/ssh/config from scratch to `Include` exactly the
// `krayt code` sessions whose sandbox is plausibly still alive — newest first, matching `krayt
// ls`'s own order. Rewriting rather than appending is what keeps a finished session's alias from
// lingering in the human's ssh config after its sandbox is gone.
//
// "Plausibly alive" is deliberately generous: `starting`/`running`/`waiting` (a session in flight)
// and `kept` (one `--keep` left running). It never consults msb — this runs on the teardown path
// of a session that may have just been Ctrl-C'd, and an `msb ls` there would be a subprocess
// nobody is waiting for. A stale Include is harmless: ssh reads the file, tries the alias, and
// fails to connect, which is exactly what a dead session should do.
func RefreshAggregateSSHConfig(stateDir string) error {
	if stateDir == "" {
		return nil
	}
	recs, err := List(stateDir)
	if err != nil {
		return err
	}
	var configs []string
	for _, r := range recs {
		if r.EffectiveKind() != KindCode || r.Terminal() {
			continue
		}
		p := filepath.Join(RunDir(stateDir, r.ID), sshsession.DirName, sshsession.ClientConfigFile)
		if !fileExists(p) {
			continue
		}
		configs = append(configs, p)
	}
	_, err = sshsession.WriteAggregateConfig(stateDir, configs)
	return err
}

// PruneAggregateSSHConfig is RefreshAggregateSSHConfig for the management commands (`krayt stop`,
// `krayt rm`) that end or delete a session someone may have `Include`d: it drops that session's
// alias from the aggregate so `ssh krayt-<id>` stops half-working, but it does NOT create the
// aggregate for a repo that has never run `krayt code` — those commands have no business writing
// an ssh config into a repo that never asked for one.
func PruneAggregateSSHConfig(stateDir string) error {
	if stateDir == "" || !fileExists(sshsession.AggregateConfigPath(stateDir)) {
		return nil
	}
	return RefreshAggregateSSHConfig(stateDir)
}
