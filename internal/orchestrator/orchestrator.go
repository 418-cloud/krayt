// Package orchestrator drives one run's lifecycle end to end (§7): provision the VM, push
// the image/code/task, start the container and stream logs, collect the artifact bundle,
// and guarantee teardown. It is OS-agnostic — it talks to the Provider seam and the gRPC
// control client only — so it is unit-tested against the fakeProvider with no real VM
// (§14). Secrets, the egress proxy, and wall-clock container-kill are Phase 3; this is the
// happy path.
package orchestrator

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/418-cloud/krayt/internal/controlclient"
	"github.com/418-cloud/krayt/internal/imagestore"
	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/secrets"
	"github.com/418-cloud/krayt/internal/task"
)

// bootTimeout bounds how long we wait for the guest-agent to answer Hello after Start
// (§11.6). The real wall-clock run timeout (spec.Resources.Timeout) is separate.
const bootTimeout = 60 * time.Second

// Deps are the host-side collaborators for a run. The Provider and BaseVM are OS-specific
// (the CLI supplies the vfkit provider + the pulled base image on macOS); Image is the
// user's agent image already acquired on the host (§6.11).
type Deps struct {
	Provider provider.Provider
	BaseVM   provider.VMSpec   // kernel/initrd/cmdline/rootfs base; resources overlaid from spec
	Image    *imagestore.Image // acquired user image; nil skips the image push (test/simple paths)
	LogOut   io.Writer         // live log sink when spec.Detach is false; may be nil

	// OnClient, if set, is invoked once the guest control client is connected with an
	// AnswerFunc that delivers a human answer to this run's guest (§6.13), and again with nil
	// as the run ends. The Manager uses it so Manager.Answer / `krayt answer` can resolve a
	// waiting run in-process without reaching for the transport.
	OnClient func(runID string, answer AnswerFunc)
}

// AnswerFunc delivers a human answer (or no-answer sentinel) to a waiting agent question via
// the guest Answer RPC (§6.13).
type AnswerFunc func(questionID, response string, noAnswer bool) error

// Result summarizes a completed run for the caller and `krayt` output.
type Result struct {
	RunDir        string
	ExitCode      int
	TimedOut      bool
	PatchPath     string   // path to changes.patch in the run dir
	CommitsBundle string   // path to commits.bundle if the agent committed, else ""
	Safety        []string // patch-lint findings, if any (§14 Phase 5), for a run-time warning
}

// Run executes the full lifecycle and writes artifacts under runDir (§7, §8.4). The VM is
// always destroyed before Run returns — on success, error, or context cancellation — via
// deferred teardown; the CLI maps SIGINT/SIGTERM to ctx cancellation so Ctrl-C still tears
// the VM down.
func Run(ctx context.Context, deps Deps, spec task.RunSpec, runDir string) (res *Result, err error) {
	if spec.Resources.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Resources.Timeout)
		defer cancel()
	}
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		return nil, fmt.Errorf("orchestrator: create run dir: %w", err)
	}

	// Publish run state to disk so `ls`/`attach`/`stop` observe it without any in-process
	// handle (§6.2). Written best-effort at each transition and finalized on return. The
	// static facts (task summary, network, resources, questions mode) are the §8.4 review
	// schema; the dynamic ones (timings, patch stats, questions, safety) are filled below.
	rec := RunRecord{
		ID: spec.ID, ImageRef: spec.ImageRef, RepoPath: spec.RepoPath,
		TaskSummary:  summarizeTask(spec.TaskPrompt),
		Network:      NetworkMeta{Mode: string(spec.Network.Mode), Allow: spec.Network.Allow},
		Resources:    ResourceMeta{CPUs: spec.Resources.CPUs, MemoryMiB: spec.Resources.MemoryMiB, DiskGiB: spec.Resources.DiskGiB, TimeoutSecs: int(spec.Resources.Timeout.Seconds())},
		QuestionMode: string(spec.Questions.Mode),
		State:        StateStarting, StartedAt: nowStamp(), PID: os.Getpid(),
	}
	_ = writeRecord(runDir, rec)
	defer func() {
		rec.EndedAt = nowStamp()
		rec.DurationSecs = durationSecs(rec.StartedAt, rec.EndedAt)
		switch {
		case err != nil:
			rec.State, rec.Error = StateFailed, err.Error()
		case res != nil && res.TimedOut:
			rec.State, rec.ExitCode, rec.TimedOut = StateTimedOut, res.ExitCode, true
		case res != nil:
			rec.State, rec.ExitCode = StateDone, res.ExitCode
		}
		rec.Questions = summarizeQuestions(runDir) // §6.13 Q&A summary for the review artifacts
		// Read any agent-written report.md before overwriting it with the canonical one (§8.4).
		notes := agentNotes(runDir)
		_ = writeRecord(runDir, rec)
		_ = writeReport(runDir, rec, notes)
	}()

	// 1. Provision the VM and guarantee teardown.
	vmSpec := deps.BaseVM
	vmSpec.ID = spec.ID
	if spec.Resources.CPUs > 0 {
		vmSpec.CPUs = spec.Resources.CPUs
	}
	if spec.Resources.MemoryMiB > 0 {
		vmSpec.MemoryMiB = spec.Resources.MemoryMiB
	}
	if spec.Resources.DiskGiB > 0 {
		vmSpec.DiskGiB = spec.Resources.DiskGiB
	}
	vm, err := deps.Provider.Create(ctx, vmSpec)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: create VM: %w", err)
	}
	defer func() {
		// Teardown is guaranteed; surface its error only if the run otherwise succeeded.
		if derr := vm.Destroy(context.WithoutCancel(ctx)); derr != nil && err == nil {
			err = fmt.Errorf("orchestrator: destroy VM: %w", derr)
		}
	}()
	if err := vm.Start(ctx); err != nil {
		return nil, fmt.Errorf("orchestrator: start VM: %w", err)
	}

	// 2. Connect + boot-readiness handshake (§11.6).
	client, err := controlclient.Dial(vm, provider.ControlPort)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: dial guest: %w", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.WaitReady(ctx, bootTimeout, 200*time.Millisecond); err != nil {
		// A run wall-clock timeout shorter than bootTimeout can also expire here, before any
		// push step runs — same class of gap as pushImage/pushCode/etc. below (§6.1).
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, err
	}

	// Record how to reach this run's guest (for cross-invocation `krayt answer`/`stop`) and
	// register the in-process answerer for the Manager (§6.13, §6.2).
	if cs, ok := vm.(controlSocketer); ok {
		rec.CtrlSocket = cs.ControlSocket()
	}
	if deps.OnClient != nil {
		deps.OnClient(spec.ID, func(qid, response string, noAnswer bool) error {
			ack, aerr := client.Agent.Answer(context.WithoutCancel(ctx), &pb.AnswerRequest{
				QuestionId: qid, Response: response, NoAnswer: noAnswer,
			})
			if aerr != nil {
				return aerr
			}
			// Ok=false means no such question was waiting — treat it as a failure, matching
			// the CLI `krayt answer` and the timeout path, so a stale/duplicate answer doesn't
			// silently report success (§6.13).
			if !ack.GetOk() {
				return fmt.Errorf("orchestrator: no pending question %q on run %q (already answered or timed out)", qid, spec.ID)
			}
			_ = RecordAnswer(runDir, qid, response, noAnswer) // complete the on-disk Q&A history (best-effort)
			return nil
		})
		defer deps.OnClient(spec.ID, nil)
	}
	// 3. Push inputs: image (incremental), code bundle, task, secrets. A wall-clock timeout
	// can expire mid-step here just as easily as during the container's run (e.g. a slow
	// `git bundle create` in pushCode outliving the budget) — isWallClockTimeout catches that
	// so it is reported the same clean way as a timeout during streamRun, not as a raw
	// killed-subprocess/context error (§6.1).
	if err := pushImage(ctx, client, deps.Image); err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, err
	}
	if err := pushCode(ctx, client, spec); err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, err
	}
	// The code snapshot is now fixed (§6.7) — from this point on it is safe for the host repo
	// to be mutated (checkout/commit/rebase) without affecting this run, so `running` becomes
	// externally visible only now, not before pushImage/pushCode (§6.2).
	rec.State = StateRunning
	_ = writeRecord(runDir, rec)
	if _, err := client.Agent.PushTask(ctx, &pb.TaskSpec{
		Prompt:            spec.TaskPrompt,
		Env:               spec.Env,
		AddCapabilities:   spec.Container.AddCapabilities,
		SeccompUnconfined: spec.Container.SeccompUnconfined,
		ReadonlyRootfs:    spec.Container.ReadonlyRootfs,
	}); err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, fmt.Errorf("orchestrator: push task: %w", err)
	}
	knownSecretKeys, err := pushSecrets(ctx, client, spec.SecretsPath)
	if err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, err
	}
	if err := setNetworkPolicy(ctx, client, spec.Network); err != nil {
		if isWallClockTimeout(ctx, err) {
			return earlyTimeoutResult(runDir), nil
		}
		return nil, err
	}

	// 4. Start the container and consume the event stream (logs, questions, terminal status).
	setState := func(st string) { rec.State = st; _ = writeRecord(runDir, rec) }
	exitCode, timedOut, err := streamRun(ctx, client, spec, deps.LogOut, runDir, setState)
	if err != nil {
		return nil, err
	}

	res = &Result{
		RunDir:    runDir,
		ExitCode:  exitCode,
		TimedOut:  timedOut,
		PatchPath: filepath.Join(runDir, "changes.patch"),
	}

	// Copy the guest's serial console log into the run's own (persistent) logs dir now, while
	// the VM is still alive — the deferred vm.Destroy above removes the VM's directory,
	// console.log included, before this function returns. This is the only place a
	// guest-agent- or proxyd-level failure (as opposed to a container-level one, already
	// covered by logs/agent.log) is visible at all; without copying it out here it is gone the
	// moment Run returns, unrecoverable by any caller no matter how soon they look.
	if _, consoleLog := vm.LogPaths(); consoleLog != "" {
		if b, rerr := os.ReadFile(consoleLog); rerr == nil {
			_ = os.WriteFile(ConsoleLogPath(runDir), b, 0o644)
		}
	}
	// 5. Collect artifacts into the run dir (§6.7, §8.4). On a wall-clock timeout the run
	// context is already dead, so skip collection and just record the timed-out run.
	if !timedOut {
		if err := collect(ctx, client, runDir); err != nil {
			return nil, err
		}
		if cb := filepath.Join(runDir, "commits.bundle"); fileExists(cb) {
			res.CommitsBundle = cb
		}
		// Diffstat + safety lint of the collected patch → meta.json/report.md (§8.4, §14). The
		// record is finalized by the deferred writer, which captures rec.Patch/rec.Safety.
		if st, serr := patch.Stat(ctx, res.PatchPath); serr == nil {
			rec.Patch = &PatchMeta{Path: st.Path, FilesChanged: st.FilesChanged, Insertions: st.Insertions, Deletions: st.Deletions}
		}
		if b, rerr := os.ReadFile(res.PatchPath); rerr == nil {
			for _, f := range patch.Lint(b) {
				rec.Safety = append(rec.Safety, f.Path+": "+f.Reason)
			}
		}
		// The guest cannot redact changes.patch (mutating hunks breaks `git apply`), so it
		// records which secret KEYS it found in the patch (values never leave the VM, §6.8);
		// surface each as a Safety warning for the human's pre-apply review (§8.4).
		for _, k := range secretPatchKeys(runDir, knownSecretKeys) {
			rec.Safety = append(rec.Safety, "changes.patch contains the value of secret "+k+" — review before applying")
		}
		res.Safety = rec.Safety
	}
	// Best-effort polite shutdown before the deferred Destroy; the terminal run state is
	// written by the deferred finalizer above.
	_, _ = client.Agent.Shutdown(context.WithoutCancel(ctx), &pb.ShutdownRequest{})
	return res, nil
}

// pushImage runs the incremental image transfer: ask which blobs the guest lacks, then
// stream only those (§6.11). For a fresh ephemeral VM the guest is missing everything.
func pushImage(ctx context.Context, client *controlclient.Client, img *imagestore.Image) error {
	if img == nil {
		return nil
	}
	digests, err := img.BlobDigests()
	if err != nil {
		return err
	}
	presence, err := client.Agent.QueryImageBlobs(ctx, &pb.BlobQuery{Digests: digests})
	if err != nil {
		return fmt.Errorf("orchestrator: query image blobs: %w", err)
	}
	only := map[string]bool{}
	for _, d := range presence.GetMissingDigests() {
		only[d] = true
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(img.WriteArchive(pw, only)) }()
	if err := client.PushImage(ctx, pr); err != nil {
		return fmt.Errorf("orchestrator: push image: %w", err)
	}
	return nil
}

// pushCode builds the self-contained bundle on the host and streams it (§6.7).
func pushCode(ctx context.Context, client *controlclient.Client, spec task.RunSpec) error {
	tmp, err := os.MkdirTemp("", "krayt-bundle-")
	if err != nil {
		return fmt.Errorf("orchestrator: temp bundle dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	bundle := filepath.Join(tmp, "repo.bundle")
	// Pass BundleDepth through literally: 0 = full history is the documented contract
	// (§6.1/§8.1), and CreateBundle treats depth<=0 as full history. The default of 1 is
	// applied at the CLI flag (and, in Phase 4, config resolution) — overriding 0 here would
	// silently defeat an explicit `--bundle-depth 0` request for full history.
	if err := patch.CreateBundle(ctx, spec.RepoPath, bundle, spec.BundleDepth, spec.IncludeDirty); err != nil {
		return err
	}
	f, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := client.PushCode(ctx, f); err != nil {
		return fmt.Errorf("orchestrator: push code: %w", err)
	}
	return nil
}

// pushSecrets loads the per-task secrets file (if any) and pushes it to the guest, which
// holds it in memory and materializes it on tmpfs at /run/secrets (§6.8). It returns the secret
// KEY NAMES (never values) the host loaded, so the caller can later cross-check anything the
// guest reports about them (secretPatchKeys) against a set the host determined independently of
// the guest/container.
func pushSecrets(ctx context.Context, client *controlclient.Client, secretsPath string) ([]string, error) {
	if secretsPath == "" {
		return nil, nil
	}
	values, err := secrets.Load(secretsPath)
	if err != nil {
		return nil, err
	}
	if _, err := client.Agent.PushSecrets(ctx, &pb.SecretsBundle{Values: values}); err != nil {
		return nil, fmt.Errorf("orchestrator: push secrets: %w", err)
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	return keys, nil
}

// setNetworkPolicy translates the task's egress policy to the proto and sends it (§6.6).
func setNetworkPolicy(ctx context.Context, client *controlclient.Client, np task.NetworkPolicy) error {
	mode := pb.NetworkPolicy_ALLOWLIST
	switch np.Mode {
	case task.NetworkFull:
		mode = pb.NetworkPolicy_FULL
	case task.NetworkNone:
		mode = pb.NetworkPolicy_NONE
	}
	if _, err := client.Agent.SetNetworkPolicy(ctx, &pb.NetworkPolicy{Mode: mode, Allow: np.Allow}); err != nil {
		return fmt.Errorf("orchestrator: set network policy: %w", err)
	}
	return nil
}

// isWallClockTimeout reports whether an error from any run-context-bound step — the Start
// stream, or an earlier push (image/code/task/secrets/network) — is the run's wall-clock
// timeout rather than a real failure. ctx.Err() can lag under load (the deadline timer may
// fire just after gRPC observes the expiry and RST_STREAMs the stream, or after a subprocess
// like `git bundle create` gets SIGKILLed by ctx's cancellation) — so we also accept a
// DeadlineExceeded RPC status (set atomically by gRPC at failure time) or a deadline that has
// already elapsed. A plain cancellation (Ctrl-C) is not a timeout and stays an error.
func isWallClockTimeout(ctx context.Context, err error) bool {
	if ctx.Err() == context.DeadlineExceeded || status.Code(err) == codes.DeadlineExceeded {
		return true
	}
	if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
		return true
	}
	return false
}

// earlyTimeoutResult builds the Result for a wall-clock timeout that fired before the
// container ever started — during the image/code/task/secrets/network push steps (§7 step 3).
// It mirrors the shape streamRun produces for a timeout during the container's run (§6.1), so
// both are reported identically: TimedOut, no error, sentinel exit code, nothing to collect.
func earlyTimeoutResult(runDir string) *Result {
	return &Result{
		RunDir:    runDir,
		ExitCode:  -1,
		TimedOut:  true,
		PatchPath: filepath.Join(runDir, "changes.patch"),
	}
}

// controlSocketer is implemented by a VM that exposes its host-side control socket path, so a
// run can record where a later `krayt answer`/`stop` should dial the guest (§6.2, §6.13). The
// fakeProvider does not implement it (in-process transport), leaving CtrlSocket empty.
type controlSocketer interface{ ControlSocket() string }

// streamRun starts the container and consumes RunEvents until the terminal Status. Log lines
// are appended to logs/agent.log and, when not detached, echoed to LogOut. An agent question
// (§6.13) drives the run to `waiting` (setState) with the Q&A persisted and a desktop
// notification; it is answered out of band by the guest Answer RPC (`krayt answer` dialing the
// guest, Manager.Answer, or the timeout below). In `fail` mode a question is sentinel-answered
// immediately so the run never blocks.
//
// A log line is NOT a resume signal — an agent can (and does) keep logging while blocked in
// ask_human — so resumption comes only from the guest "question resolved" RunEvent (§6.13),
// which fires however the question was answered (Answer RPC, a separate `krayt answer` process,
// or the timeout sentinel). The run is `waiting` while any question is outstanding and flips back
// to `running` when the last one resolves.
func streamRun(ctx context.Context, client *controlclient.Client, spec task.RunSpec, logOut io.Writer, runDir string, setState func(string)) (int, bool, error) {
	var timeoutSecs uint32
	if spec.Resources.Timeout > 0 {
		timeoutSecs = uint32(spec.Resources.Timeout.Seconds())
	}
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	stream, err := client.Agent.Start(streamCtx, &pb.StartRequest{ImageRef: spec.ImageRef, TimeoutSecs: timeoutSecs})
	if err != nil {
		return 0, false, fmt.Errorf("orchestrator: start: %w", err)
	}

	logFile, err := os.Create(filepath.Join(runDir, "logs", "agent.log"))
	if err != nil {
		return 0, false, fmt.Errorf("orchestrator: open log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	var aborted atomic.Bool
	var outstanding int // wait-mode questions awaiting an answer; run is `waiting` while > 0
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return 0, false, fmt.Errorf("orchestrator: start stream ended without a terminal status")
		}
		if err != nil {
			if aborted.Load() {
				return -1, false, fmt.Errorf("orchestrator: question timed out (abort policy, §6.13)")
			}
			// Wall-clock timeout: our run context expired, which canceled the stream. The
			// guest kills the container and the deferred Destroy tears the VM down (§6.1).
			if isWallClockTimeout(ctx, err) {
				return -1, true, nil
			}
			return 0, false, fmt.Errorf("orchestrator: stream recv: %w", err)
		}
		switch k := ev.GetKind().(type) {
		case *pb.RunEvent_Log:
			line := k.Log.GetLine()
			_, _ = logFile.Write(line)
			if !spec.Detach && logOut != nil {
				_, _ = logOut.Write(line)
			}
		case *pb.RunEvent_Status:
			st := k.Status
			if e := st.GetError(); e != "" {
				return int(st.GetExitCode()), st.GetTimedOut(), fmt.Errorf("orchestrator: run failed: %s", e)
			}
			return int(st.GetExitCode()), st.GetTimedOut(), nil
		case *pb.RunEvent_Question:
			q := k.Question
			if spec.Questions.Mode != task.QuestionWait {
				// fail mode (default): never block — sentinel immediately so the agent
				// proceeds autonomously (§6.13).
				_, _ = client.Agent.Answer(context.WithoutCancel(ctx), &pb.AnswerRequest{QuestionId: q.GetId(), NoAnswer: true})
				continue
			}
			outstanding++
			// Persist the question BEFORE announcing `waiting`, so any observer that sees the
			// waiting state — a test, or a cross-process `krayt answer` reading the newest
			// question — is guaranteed to find it on disk (§6.13). Otherwise there's a window
			// where the state is `waiting` but the question file isn't written yet.
			if err := writeQuestion(runDir, q); err != nil {
				return 0, false, err
			}
			setState(StateWaiting)
			notifyWaiting(filepath.Base(runDir), q.GetPrompt())
			if to := spec.Questions.Timeout; to > 0 {
				armQuestionTimeout(ctx, client, spec, runDir, q.GetId(), to, &aborted, streamCancel)
			}
		case *pb.RunEvent_Resolved:
			// A question was answered (§6.13). Resume only when the last outstanding one clears; a
			// Resolved with none outstanding is a fail-mode sentinel echo, so it's a no-op.
			if outstanding > 0 {
				if outstanding--; outstanding == 0 {
					setState(StateRunning)
				}
			}
		}
	}
}

// armQuestionTimeout schedules the per-question wait limit (§6.13). On expiry it probes with a
// no-answer sentinel: Ack.Ok reports whether the question was still pending, so a question the
// human already answered (possibly from another process) is never wrongly sentinel-echoed or
// aborted. Only a genuinely-still-pending question triggers the on-timeout action.
func armQuestionTimeout(ctx context.Context, client *controlclient.Client, spec task.RunSpec, runDir, qid string, to time.Duration, aborted *atomic.Bool, cancel context.CancelFunc) {
	time.AfterFunc(to, func() {
		ack, err := client.Agent.Answer(context.WithoutCancel(ctx), &pb.AnswerRequest{QuestionId: qid, NoAnswer: true})
		if err != nil || !ack.GetOk() {
			return // already answered/resolved, or transient failure — do not act
		}
		// The question was genuinely still pending at the deadline. The no-answer sentinel was
		// just delivered; record it in the history and, for `abort`, fail the whole run.
		_ = RecordAnswer(runDir, qid, "", true)
		if spec.Questions.OnTimeout == task.OnTimeoutAbort {
			aborted.Store(true)
			cancel()
		}
	})
}

// collect streams the guest's artifact tar and extracts it into the run dir (§8.4).
func collect(ctx context.Context, client *controlclient.Client, runDir string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(client.CollectArtifacts(ctx, pw)) }()
	if err := untar(pr, runDir); err != nil {
		return fmt.Errorf("orchestrator: extract artifacts: %w", err)
	}
	return nil
}

// untar extracts a tar stream into dir, creating parent directories as needed. Entry names
// are cleaned and constrained to dir so a malformed/hostile tar cannot escape it.
func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return err
		}
		if hdr.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // bounded by the guest run
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}

// safeJoin joins name onto dir, rejecting paths that would escape dir (path traversal).
func safeJoin(dir, name string) (string, error) {
	target := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || hasDotDotPrefix(rel) {
		return "", fmt.Errorf("orchestrator: unsafe artifact path %q", name)
	}
	return target, nil
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' && (rel[2] == filepath.Separator)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// secretScanFile is the guest's marker (§6.8) naming the secret KEYS whose value appears in
// changes.patch. The guest never writes the values, so the file is harmless in the run dir; the
// host turns each key into a Safety warning (§8.4).
const secretScanFile = "secret-scan.json"

// secretPatchKeys reads the guest's secret-scan.json (collected with the other artifacts) and
// returns the secret KEYS whose value the guest found in changes.patch. Absent/unreadable →
// none (a run with no secret in the patch, the common case).
//
// secret-scan.json lives in the guest's outputDir, which is rbind-mounted read-write into the
// (untrusted, §10) container as /output. The guest's own post-run scan is meant to be the
// authoritative last writer there, but as defense in depth this does not trust the file's
// contents outright: reported keys are filtered against knownKeys — the secret names the host
// itself loaded for this run (pushSecrets), independent of anything the guest/container
// reports — and deduplicated. So a malformed or maliciously planted file can at worst produce a
// false-positive warning naming a real configured secret, never an arbitrary, huge, or
// attacker-chosen string.
func secretPatchKeys(runDir string, knownKeys []string) []string {
	b, err := os.ReadFile(filepath.Join(runDir, secretScanFile))
	if err != nil {
		return nil
	}
	var s struct {
		PatchContainsSecretKeys []string `json:"patch_contains_secret_keys"`
	}
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	known := make(map[string]bool, len(knownKeys))
	for _, k := range knownKeys {
		known[k] = true
	}
	seen := make(map[string]bool, len(s.PatchContainsSecretKeys))
	var keys []string
	for _, k := range s.PatchContainsSecretKeys {
		if !known[k] || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys
}
