// Package orchestrator drives one run's lifecycle end to end (§7): rent an msb sandbox, copy the
// code bundle and task in, run the guest helper (root) to set up the patch baseline, run the
// agent (as the sandbox's non-root user) with its logs streamed, run the helper again to build
// the patch, copy the artifacts out, and guarantee teardown. It is OS-agnostic — it talks to the
// internal/sandbox msb driver only — so it is unit-tested against a scriptable fake `msb`
// binary, not a real sandbox (§14; run-tasks-on-microsandbox.md superseded the fakeProvider seam
// this package's tests used before the ADR option B1 migration).
package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/418-cloud/krayt/internal/askbridge"
	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/sandbox"
	"github.com/418-cloud/krayt/internal/sandbox/guestbin"
	"github.com/418-cloud/krayt/internal/secrets"
	"github.com/418-cloud/krayt/internal/task"
)

// sandboxAgentUser is the non-root user krayt's agent images run as (§8.2 — enforced, not just
// convention) and the `msb create --user`/`msb exec --user` value the agent's own exec uses. The
// guest helper always execs as root instead (add-krayt-guest-helper.md's privilege separation).
const sandboxAgentUser = "agent"

// sandboxSecurity is msb's `--security` profile every krayt sandbox is created with. Fixed, not
// user-configurable: P2 (probe-microsandbox-feasibility.md, 2026-08-30) confirmed `msb exec
// --user root` still works under `--security restricted` with a root-only path staying unreadable
// to an `--user agent` exec, so the guest helper keeps BOTH the restricted profile and its own
// privilege separation rather than trading one for the other (run-tasks-on-microsandbox.md
// decision, carrying add-krayt-guest-helper.md's finding forward).
const sandboxSecurity = "restricted"

// Container-contract paths (§8.2) the guest helper and the agent's entrypoint both read/write.
// containerPatchGit is new under msb: previously a guest-agent-managed temp dir, now a fixed
// in-sandbox path outside /workspace and /output, matching guestbin.GuestRoot's own reasoning
// (never mistaken for a collected artifact).
const (
	containerWorkspace  = "/workspace"
	containerTaskFile   = "/task/prompt.md"
	containerOutput     = "/output"
	containerBundlePath = "/tmp/repo.bundle"
	containerPatchGit   = guestbin.GuestRoot + "/patchgit"
	containerAskBinPath = "/usr/local/bin/krayt-ask" // §8.2's fixed, documented path

	// containerEntrypoint is the one command every agent image exposes (§8.2, add-*-agent-image.md
	// tasks): "/usr/local/bin/krayt-agent-entrypoint", uniform across every published image so this
	// package needs no per-adapter command table.
	containerEntrypoint = "/usr/local/bin/krayt-agent-entrypoint"
)

// Deps are the host-side collaborators for a run. Sandbox is the msb driver — the one thing this
// package is not OS-specific about, since internal/sandbox itself has no build tags.
type Deps struct {
	Sandbox *sandbox.Client
	LogOut  io.Writer // live log sink when spec.Detach is false; may be nil

	// OnClient, if set, is invoked once a run's answerer is ready (immediately, since msb has no
	// boot handshake this package waits on) with an AnswerFunc that delivers a human answer to
	// this run (§6.13), and again with nil as the run ends. The Manager uses it so
	// Manager.Answer / `krayt answer` can resolve a waiting run in-process. Named identically to
	// the pre-msb Deps field it replaces; its meaning ("a way to answer this run") is unchanged.
	OnClient func(runID string, answer AnswerFunc)
}

// AnswerFunc delivers a human answer (or no-answer sentinel) to a waiting agent question (§6.13).
type AnswerFunc func(questionID, response string, noAnswer bool) error

// Result summarizes a completed run for the caller and `krayt` output.
type Result struct {
	RunDir        string
	ExitCode      int
	TimedOut      bool
	PatchPath     string   // path to changes.patch in the run dir
	CommitsBundle string   // path to commits.bundle if the agent committed, else ""
	Safety        []string // patch-lint findings, if any, for a run-time warning
}

// sandboxName derives the msb sandbox name from the run id (run-tasks-on-microsandbox.md decision
// 8): one sandbox per run, named so an orphaned "krayt-*" sandbox is recognizable as krayt's own.
func sandboxName(runID string) string { return "krayt-" + runID }

// Run executes the full lifecycle and writes artifacts under runDir (§7, §8.4). The sandbox is
// always stopped and removed before Run returns — on success, error, or context cancellation —
// via a deferred teardown that runs regardless of how far setup got; the CLI maps SIGINT/SIGTERM
// to ctx cancellation so Ctrl-C still tears the sandbox down.
func Run(ctx context.Context, deps Deps, spec task.RunSpec, runDir string) (res *Result, err error) {
	if spec.Resources.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Resources.Timeout)
		defer cancel()
	}
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		return nil, fmt.Errorf("orchestrator: create run dir: %w", err)
	}

	// Resolve secrets up front: every declared secret must be network-scoped (already enforced
	// pre-flight by task.ValidateNetworkPolicyForMsb before Run is ever called), so specs and
	// secretValues below are the complete secret picture for this run.
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

	name := sandboxName(spec.ID)
	rec := RunRecord{
		ID: spec.ID, ImageRef: spec.ImageRef, RepoPath: spec.RepoPath,
		TaskSummary: summarizeTask(spec.TaskPrompt),
		Network: NetworkMeta{
			// Under msb, TLS interception turns on automatically the moment any secret is
			// declared (docs/adr-microsandbox-sandbox-layer.md correction 1) — MITM here reports
			// exactly that, and InjectedKeys names every secret substituted host-side, which
			// under B1 is every declared secret (there is no other delivery channel, §6.8).
			Mode: string(spec.Network.Mode), Allow: spec.Network.Allow,
			MITM: hasSecrets, InjectedKeys: secretKeyNames,
		},
		Resources:    ResourceMeta{CPUs: spec.Resources.CPUs, MemoryMiB: spec.Resources.MemoryMiB, DiskGiB: spec.Resources.DiskGiB, TimeoutSecs: int(spec.Resources.Timeout.Seconds())},
		QuestionMode: string(spec.Questions.Mode),
		State:        StateStarting, StartedAt: nowStamp(), PID: os.Getpid(),
		SandboxName: name,
	}
	_, _ = writeRecord(runDir, rec)
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
		case res != nil && res.TimedOut:
			rec.State, rec.ExitCode, rec.TimedOut = StateTimedOut, res.ExitCode, true
		case res != nil:
			rec.State, rec.ExitCode = StateDone, res.ExitCode
		}
		rec.Questions = summarizeQuestions(runDir)
		notes := agentNotes(runDir)
		metaDigest, _ := writeRecord(runDir, rec)
		_ = writeReport(runDir, rec, notes, metaDigest)
	}()

	// Teardown + boot/system diagnostics, guaranteed on EVERY path (run-tasks-on-microsandbox.md
	// decision 8) — registered before Create is even attempted, so a failure at any later step,
	// including Create itself failing partway through, still stops and removes whatever msb
	// created. SystemLogs is captured first (ordered before rm, decision 7): it is msb's
	// replacement for the pre-msb console log, including the reconstructed boot-error block msb
	// prepends when a sandbox never finished starting.
	defer func() {
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

	// 1b. Wire the ask_human channel (§6.13) BEFORE Create: --vsock is a create-time-only flag
	// (msb requires a restart to add one to a running sandbox), so the route must exist before
	// msb ever starts the sandbox. Only for a --on-question=wait run — in `fail` mode no --vsock
	// route is emitted at all, so krayt-ask inside the container simply fails to dial and its CLI
	// front-end maps that straight to the no-answer sentinel; there is no separate in-process
	// "fail mode" branch to maintain here the way the pre-msb Start-stream loop needed one.
	var vsockRoutes []sandbox.VsockRoute
	var streamCancel context.CancelFunc // set just before the agent Exec call; referenced by the question-timeout closure below
	var aborted atomic.Bool
	var outstandingQuestions atomic.Int32
	setState := func(st string) { rec.State = st; _, _ = writeRecord(runDir, rec) }

	if spec.Questions.Mode == task.QuestionWait {
		askDir := filepath.Join(runDir, "ask")
		lis, lerr := askbridge.Listen(askDir)
		if lerr != nil {
			return nil, fmt.Errorf("orchestrator: listen ask bridge: %w", lerr)
		}
		defer func() { _ = lis.Close() }()

		var redactor *secrets.Redactor
		if len(secretValues) > 0 {
			redactor = secrets.NewRedactor(secrets.Values(secretValues))
		}
		var bridge *askbridge.Bridge
		bridge = askbridge.NewBridge(func(id, prompt string, choices []string) error {
			if redactor != nil {
				prompt = string(redactor.Redact([]byte(prompt)))
				choices = redactChoices(redactor, choices)
			}
			if err := writeQuestionRecord(runDir, QuestionRecord{ID: id, Prompt: prompt, Choices: choices, AskedAt: nowStamp()}); err != nil {
				return err
			}
			outstandingQuestions.Add(1)
			setState(StateWaiting)
			notifyWaiting(filepath.Base(runDir), prompt)
			if to := spec.Questions.Timeout; to > 0 {
				armQuestionTimeout(bridge, runDir, id, to, spec.Questions.OnTimeout, &aborted, &streamCancel)
			}
			return nil
		})
		bridge.OnResolved(func(string) {
			if outstandingQuestions.Add(-1) <= 0 {
				setState(StateRunning)
			}
		})

		bridgeCtx, bridgeCancel := context.WithCancel(ctx)
		defer bridgeCancel()
		go func() { _ = askbridge.Serve(bridgeCtx, lis, bridge) }()

		answerFunc := func(qid, response string, noAnswer bool) error {
			if !bridge.Answer(qid, response, noAnswer) {
				return fmt.Errorf("orchestrator: no pending question %q on run %q (already answered or timed out)", qid, spec.ID)
			}
			_ = RecordAnswer(runDir, qid, response, noAnswer)
			return nil
		}
		ctlSocket, stopCtl, cerr := serveRunControl(askDir, answerFunc)
		if cerr != nil {
			return nil, fmt.Errorf("orchestrator: %w", cerr)
		}
		defer stopCtl()
		rec.CtrlSocket = ctlSocket
		if deps.OnClient != nil {
			deps.OnClient(spec.ID, answerFunc)
			defer deps.OnClient(spec.ID, nil)
		}

		vsockRoutes = []sandbox.VsockRoute{{HostPath: filepath.Join(askDir, "ask.sock"), Port: sandbox.AskPort}}
		mergeEnv(&spec, map[string]string{"KRAYT_ASK_SOCKET": sandbox.AskSocketEnv})
	}
	_, _ = writeRecord(runDir, rec)

	// 1. Create (rent) the sandbox: image, --secret (names only; values in secretEnv), --vsock
	// only when wiring above populated it, --security restricted, resources, --max-duration. The
	// run's own context.WithTimeout (above) is belt-and-braces alongside --max-duration
	// (run-tasks-on-microsandbox.md decision 5): the ctx is what makes teardown deterministic,
	// --max-duration is what stops a wedged guest outliving it.
	createSpec := sandbox.CreateSpec{
		Image: spec.ImageRef, Name: name, User: sandboxAgentUser,
		CPUs: spec.Resources.CPUs, MemoryMiB: spec.Resources.MemoryMiB, DiskGiB: spec.Resources.DiskGiB,
		MaxDuration: spec.Resources.Timeout,
		Env:         envVarsFromMap(spec.Env),
		Vsock:       vsockRoutes,
		Secrets:     secretRefs,
		Security:    sandboxSecurity,
		ExtraArgs:   netArgs,
	}
	if err := deps.Sandbox.Create(ctx, createSpec, secretEnv); err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, fmt.Errorf("orchestrator: create sandbox: %w", err)
	}

	// 2. Copy in: the git bundle, the task prompt, and the two embedded guest binaries.
	tmp, err := os.MkdirTemp("", "krayt-msb-")
	if err != nil {
		return nil, fmt.Errorf("orchestrator: temp copy-in dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	bundlePath := filepath.Join(tmp, "repo.bundle")
	// BundleDepth passes through literally: 0 = full history (§6.1/§8.1); CreateBundle treats
	// depth<=0 as full history.
	br, err := patch.CreateBundle(ctx, spec.RepoPath, bundlePath, spec.BundleDepth, spec.IncludeDirty)
	if err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, err
	}
	bundleDigest, err := digestFile(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: digest bundle: %w", err)
	}
	rec.Provenance = &ProvenanceMeta{
		HeadSHA: br.HeadSHA, BundleSHA: br.BundleSHA,
		BundleDepth: spec.BundleDepth, IncludeDirty: spec.IncludeDirty,
		BundleDigest: bundleDigest.String(),
	}
	_, _ = writeRecord(runDir, rec)

	promptPath := filepath.Join(tmp, "prompt.md")
	if err := os.WriteFile(promptPath, spec.TaskPrompt, 0o644); err != nil {
		return nil, fmt.Errorf("orchestrator: write task prompt: %w", err)
	}
	helperLocal, err := writeEmbeddedBinary(tmp, guestbin.HelperName)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}
	askLocal, err := writeEmbeddedBinary(tmp, guestbin.AskName)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	copies := [...][2]string{
		{bundlePath, name + ":" + containerBundlePath},
		{promptPath, name + ":" + containerTaskFile},
		{helperLocal, name + ":" + guestbin.GuestPath(guestbin.HelperName)},
		{askLocal, name + ":" + containerAskBinPath},
	}
	for _, c := range copies {
		if err := deps.Sandbox.Copy(ctx, c[0], c[1]); err != nil {
			if isWallClockTimeout(ctx, err) {
				return earlyTimeoutResult(runDir), nil
			}
			return nil, fmt.Errorf("orchestrator: copy %s: %w", c[1], err)
		}
	}
	// Defensive: msb copy's mode-preservation is not a pinned contract, so make sure both
	// binaries are actually executable before exec-ing either of them.
	if _, err := execCapture(ctx, deps.Sandbox, name, "root",
		[]string{"chmod", "+x", guestbin.GuestPath(guestbin.HelperName), containerAskBinPath}); err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, fmt.Errorf("orchestrator: chmod copied binaries: %w", err)
	}

	// 3. Exec the helper as root: clone the bundle into /workspace, tag krayt-baseline, snapshot
	// the root-only patch-git, then relax /workspace for the agent user
	// (add-krayt-guest-helper.md's privilege-separation ordering).
	setupOut, err := execCapture(ctx, deps.Sandbox, name, "root", []string{
		guestbin.GuestPath(guestbin.HelperName), "setup",
		"--bundle", containerBundlePath, "--workspace", containerWorkspace,
		"--patch-git", containerPatchGit, "--agent-user", sandboxAgentUser,
	})
	if err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		// decision 6: a driver failure (ErrMsbFailed) must surface as a failed run, never as
		// "the agent/helper exited 1" — errors.Is sees through execCapture's %w wrapping.
		return nil, fmt.Errorf("orchestrator: krayt-helper setup: %w", err)
	}
	var setupResult struct {
		Baseline string `json:"baseline"`
	}
	if jerr := json.Unmarshal(setupOut, &setupResult); jerr != nil || setupResult.Baseline == "" {
		return nil, fmt.Errorf("orchestrator: parse krayt-helper setup output %q: %v", setupOut, jerr)
	}

	// The code snapshot is now durably captured inside the sandbox (cloned from the bundle,
	// baseline tagged) — only now is it safe for the host repo to be mutated without affecting
	// this run, so `running` becomes externally visible here, matching the pre-msb rule (§6.2).
	rec.State = StateRunning
	_, _ = writeRecord(runDir, rec)

	// 4. Exec the agent as the sandbox's non-root user, streamed to the run's log sink.
	logFile, err := os.Create(filepath.Join(runDir, "logs", "agent.log"))
	if err != nil {
		return nil, fmt.Errorf("orchestrator: open log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	writers := []io.Writer{logFile}
	if !spec.Detach && deps.LogOut != nil {
		writers = append(writers, deps.LogOut)
	}
	logWriter := io.MultiWriter(writers...)

	streamCtx, cancelStream := context.WithCancel(ctx)
	streamCancel = cancelStream
	defer cancelStream()

	execResult, execErr := deps.Sandbox.Exec(streamCtx, sandbox.ExecSpec{
		Name: name, User: sandboxAgentUser, Command: []string{containerEntrypoint},
		Stdout: logWriter, Stderr: logWriter,
	})

	var exitCode int
	var timedOut bool
	switch {
	case execErr != nil && aborted.Load():
		return nil, fmt.Errorf("orchestrator: question timed out (abort policy, §6.13)")
	case execErr != nil && isWallClockTimeout(ctx, execErr):
		timedOut, exitCode = true, -1
	case errors.Is(execErr, sandbox.ErrMsbFailed):
		return nil, fmt.Errorf("orchestrator: %w", execErr)
	case execErr != nil:
		return nil, fmt.Errorf("orchestrator: run agent: %w", execErr)
	default:
		exitCode = execResult.ExitCode
	}

	res = &Result{RunDir: runDir, ExitCode: exitCode, TimedOut: timedOut, PatchPath: filepath.Join(runDir, "changes.patch")}
	if timedOut {
		// The run context is already dead; skip helper finish/collection, matching the pre-msb
		// contract for a wall-clock timeout during the container's run.
		return res, nil
	}

	// 5. Exec the helper again as root: diff against the baseline, assemble /output.
	if _, err := execCapture(ctx, deps.Sandbox, name, "root", []string{
		guestbin.GuestPath(guestbin.HelperName), "finish",
		"--workspace", containerWorkspace, "--patch-git", containerPatchGit,
		"--baseline", setupResult.Baseline, "--out", containerOutput,
	}); err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, fmt.Errorf("orchestrator: krayt-helper finish: %w", err)
	}

	// 6. Copy out /output/* (§6.7, §8.4).
	if err := collectOutput(ctx, deps.Sandbox, name, runDir); err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, err
	}
	if cb := filepath.Join(runDir, "commits.bundle"); fileExists(cb) {
		res.CommitsBundle = cb
	}

	// 7. Host: diffstat + safety lint + secret-value scan of the collected patch (§8.4, §14) —
	// none of this is an exec; the host already holds the patch bytes and every secret value.
	if st, serr := patch.Stat(ctx, res.PatchPath); serr == nil {
		rec.Patch = &PatchMeta{Path: st.Path, FilesChanged: st.FilesChanged, Insertions: st.Insertions, Deletions: st.Deletions}
	}
	if b, rerr := os.ReadFile(res.PatchPath); rerr == nil {
		for _, f := range patch.Lint(b) {
			rec.Safety = append(rec.Safety, f.Path+": "+f.Reason)
		}
	}
	if len(secretValues) > 0 {
		if keys, kerr := PatchSecretKeys(res.PatchPath, secretValues); kerr == nil {
			for _, k := range keys {
				rec.Safety = append(rec.Safety, "changes.patch contains the value of secret "+k+" — review before applying")
			}
		}
	}
	res.Safety = rec.Safety
	return res, nil
}

// envVarsFromMap renders a non-secret env map into CreateSpec's []EnvVar, sorted for
// deterministic argv (matching CreateSpec.Args()' own determinism guarantee).
func envVarsFromMap(m map[string]string) []sandbox.EnvVar {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]sandbox.EnvVar, len(keys))
	for i, k := range keys {
		out[i] = sandbox.EnvVar{Name: k, Value: m[k]}
	}
	return out
}

// mergeEnv adds every key in additions to spec.Env that spec.Env doesn't already set — mirrors
// internal/cli's own mergeEnv (adapter plan env), used here so a run's own explicit env always
// wins over the ask-socket wiring this package adds.
func mergeEnv(spec *task.RunSpec, additions map[string]string) {
	if len(additions) == 0 {
		return
	}
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	for k, v := range additions {
		if _, set := spec.Env[k]; !set {
			spec.Env[k] = v
		}
	}
}

// writeEmbeddedBinary writes the embedded guest binary named name (guestbin.HelperName or
// guestbin.AskName) for the host's own architecture — under msb the guest architecture always
// equals the host's (libkrun runs a same-arch VM, guestbin's own doc comment) — to a temp file
// with the executable bit set, returning its path for Copy.
func writeEmbeddedBinary(dir, name string) (string, error) {
	b, err := guestbin.Binary(name, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o755); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	return path, nil
}

// digestFile computes the canonical digest of a file's bytes without loading it all into memory.
func digestFile(path string) (digest.Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return digest.Canonical.FromReader(f)
}

// execCapture runs one msb exec to completion, capturing stdout/stderr into buffers rather than
// streaming — used for the short, JSON-in/JSON-out helper invocations and the defensive chmod,
// as opposed to the long-lived, streamed agent exec. A non-zero exit (including ErrMsbFailed) is
// returned as an error carrying stderr, with errors.Is-visibility into ErrMsbFailed preserved
// through the %w wrap.
func execCapture(ctx context.Context, sb *sandbox.Client, name, user string, cmd []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	result, err := sb.Exec(ctx, sandbox.ExecSpec{Name: name, User: user, Command: cmd, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%w (stderr: %s)", err, firstNonEmptyTrimmed(stderr.String(), stdout.String()))
	}
	if result.ExitCode != 0 {
		return stdout.Bytes(), fmt.Errorf("exit %d (stderr: %s)", result.ExitCode, firstNonEmptyTrimmed(stderr.String(), stdout.String()))
	}
	return stdout.Bytes(), nil
}

func firstNonEmptyTrimmed(ss ...string) string {
	for _, s := range ss {
		if t := bytesTrimSpace(s); t != "" {
			return t
		}
	}
	return "(no output)"
}

func bytesTrimSpace(s string) string { return string(bytes.TrimSpace([]byte(s))) }

// collectOutput copies /output/* out of the sandbox into runDir (§6.7, §8.4). msb's Copy uses
// docker-cp syntax; docker itself is inconsistent about whether copying a directory nests it one
// level (dest/output/...) or flattens it (dest/...) depending on whether the destination already
// exists, so this tolerates either shape rather than assuming one.
func collectOutput(ctx context.Context, sb *sandbox.Client, name, runDir string) error {
	tmp, err := os.MkdirTemp(runDir, ".output-tmp-")
	if err != nil {
		return fmt.Errorf("orchestrator: create output staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := sb.Copy(ctx, name+":"+containerOutput, tmp); err != nil {
		return fmt.Errorf("orchestrator: copy output: %w", err)
	}
	src := tmp
	if nested := filepath.Join(tmp, "output"); dirExists(nested) {
		src = nested
	}
	return moveTreeContents(src, runDir)
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// moveTreeContents moves every top-level entry of src into dst, overwriting any same-named
// entry already there. src is expected to be on the same filesystem as dst (collectOutput stages
// its temp dir under runDir itself for exactly this reason), so this is a plain rename per entry.
func moveTreeContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("orchestrator: read collected output: %w", err)
	}
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		if err := os.RemoveAll(to); err != nil {
			return fmt.Errorf("orchestrator: replace %s: %w", to, err)
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("orchestrator: move %s: %w", e.Name(), err)
		}
	}
	return nil
}

// maxConsoleLog bounds how much of msb's system/boot diagnostics writeConsoleLog persists per
// run — the container is untrusted (§10), and a run's wall-clock timeout has no fixed ceiling,
// so nothing bounds how much a hostile or merely broken guest could otherwise cause msb to log.
const maxConsoleLog = 1 << 20 // 1 MiB

// writeConsoleLog persists msb's `logs --source system --json` output — boot/system diagnostics,
// including the reconstructed error block msb prepends when a sandbox never finished starting
// (run-tasks-on-microsandbox.md decision 7, replacing the pre-msb guest serial console) — into
// the run's logs dir, redacted against the task's secrets. Same fail-closed rule as before: if
// the secret values can't be confirmed, nothing is written rather than risking one in the clear.
func writeConsoleLog(b []byte, runDir string, secretValues map[string]string) {
	if len(b) == 0 {
		return
	}
	if len(b) > maxConsoleLog {
		b = b[len(b)-maxConsoleLog:]
	}
	if len(secretValues) > 0 {
		b = secrets.NewRedactor(secrets.Values(secretValues)).Redact(b)
	}
	_ = os.WriteFile(ConsoleLogPath(runDir), b, 0o644)
}

// redactChoices applies r to each choice string, same as a question's prompt — an agent could in
// principle put a secret value inside an offered choice, not just the prompt text.
func redactChoices(r *secrets.Redactor, choices []string) []string {
	if len(choices) == 0 {
		return choices
	}
	out := make([]string, len(choices))
	for i, c := range choices {
		out[i] = string(r.Redact([]byte(c)))
	}
	return out
}

// isWallClockTimeout reports whether an error from any ctx-bound step (Create, Copy, Exec) is
// the run's wall-clock timeout rather than a real failure. ctx.Err() can lag under load (the
// deadline timer may fire just after a subprocess is SIGKILLed by ctx's cancellation) — so a
// deadline that has already elapsed is accepted too. A plain cancellation (Ctrl-C) is not a
// timeout and stays an error.
func isWallClockTimeout(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
		return true
	}
	return false
}

// earlyTimeoutResult builds the Result for a wall-clock timeout that fired before the agent ever
// ran — during Create/copy-in/helper-setup. It mirrors the shape a timeout during the agent's own
// exec produces, so both are reported identically: TimedOut, no error, sentinel exit code,
// nothing to collect.
func earlyTimeoutResult(runDir string) *Result {
	return &Result{RunDir: runDir, ExitCode: -1, TimedOut: true, PatchPath: filepath.Join(runDir, "changes.patch")}
}

// armQuestionTimeout schedules the per-question wait limit (§6.13). On expiry it delivers the
// no-answer sentinel directly to the bridge — unblocking the sandbox's still-pending Ask call —
// and records it; Bridge.Answer is itself idempotent-safe (a no-op if the question was already
// answered by a human first, since the human's answer already consumed the pending channel). For
// `abort` it also cancels the agent's exec via *streamCancel (a pointer so it can be armed before
// the agent's own exec has actually started the real context it will cancel).
func armQuestionTimeout(bridge *askbridge.Bridge, runDir, qid string, to time.Duration, onTimeout task.QuestionTimeoutAction, aborted *atomic.Bool, streamCancel *context.CancelFunc) {
	time.AfterFunc(to, func() {
		if !bridge.Answer(qid, "", true) {
			return // already answered (by a human, or a previous timeout) — nothing to do
		}
		_ = RecordAnswer(runDir, qid, "", true)
		if onTimeout == task.OnTimeoutAbort {
			aborted.Store(true)
			if cancel := *streamCancel; cancel != nil {
				cancel()
			}
		}
	})
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
