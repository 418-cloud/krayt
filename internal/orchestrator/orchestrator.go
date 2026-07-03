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
	PatchPath     string // path to changes.patch in the run dir
	CommitsBundle string // path to commits.bundle if the agent committed, else ""
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
	// handle (§6.2). Written best-effort at each transition and finalized on return.
	rec := RunRecord{
		ID: spec.ID, ImageRef: spec.ImageRef, RepoPath: spec.RepoPath,
		State: StateStarting, StartedAt: nowStamp(), PID: os.Getpid(),
	}
	_ = writeRecord(runDir, rec)
	defer func() {
		rec.EndedAt = nowStamp()
		switch {
		case err != nil:
			rec.State, rec.Error = StateFailed, err.Error()
		case res != nil && res.TimedOut:
			rec.State, rec.ExitCode, rec.TimedOut = StateTimedOut, res.ExitCode, true
		case res != nil:
			rec.State, rec.ExitCode = StateDone, res.ExitCode
		}
		_ = writeRecord(runDir, rec)
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
		return nil, err
	}

	// Record how to reach this run's guest (for cross-invocation `krayt answer`/`stop`) and
	// register the in-process answerer for the Manager (§6.13, §6.2).
	if cs, ok := vm.(controlSocketer); ok {
		rec.CtrlSocket = cs.ControlSocket()
	}
	if deps.OnClient != nil {
		deps.OnClient(spec.ID, func(qid, response string, noAnswer bool) error {
			_, aerr := client.Agent.Answer(context.WithoutCancel(ctx), &pb.AnswerRequest{
				QuestionId: qid, Response: response, NoAnswer: noAnswer,
			})
			return aerr
		})
		defer deps.OnClient(spec.ID, nil)
	}
	rec.State = StateRunning
	_ = writeRecord(runDir, rec)

	// 3. Push inputs: image (incremental), code bundle, task, secrets.
	if err := pushImage(ctx, client, deps.Image); err != nil {
		return nil, err
	}
	if err := pushCode(ctx, client, spec); err != nil {
		return nil, err
	}
	if _, err := client.Agent.PushTask(ctx, &pb.TaskSpec{Prompt: spec.TaskPrompt, Env: spec.Env}); err != nil {
		return nil, fmt.Errorf("orchestrator: push task: %w", err)
	}
	if err := pushSecrets(ctx, client, spec.SecretsPath); err != nil {
		return nil, err
	}
	if err := setNetworkPolicy(ctx, client, spec.Network); err != nil {
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
	// 5. Collect artifacts into the run dir (§6.7, §8.4). On a wall-clock timeout the run
	// context is already dead, so skip collection and just record the timed-out run.
	if !timedOut {
		if err := collect(ctx, client, runDir); err != nil {
			return nil, err
		}
		if cb := filepath.Join(runDir, "commits.bundle"); fileExists(cb) {
			res.CommitsBundle = cb
		}
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
// holds it in memory and materializes it on tmpfs at /run/secrets (§6.8).
func pushSecrets(ctx context.Context, client *controlclient.Client, secretsPath string) error {
	if secretsPath == "" {
		return nil
	}
	values, err := secrets.Load(secretsPath)
	if err != nil {
		return err
	}
	if _, err := client.Agent.PushSecrets(ctx, &pb.SecretsBundle{Values: values}); err != nil {
		return fmt.Errorf("orchestrator: push secrets: %w", err)
	}
	return nil
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

// isWallClockTimeout reports whether a Start-stream error is the run's wall-clock timeout
// rather than a real failure. ctx.Err() can lag the stream teardown under load — the deadline
// timer may fire just after gRPC observes the expiry and RST_STREAMs the stream — so we also
// accept a DeadlineExceeded RPC status (set atomically by gRPC at failure time) or a deadline
// that has already elapsed. A plain cancellation (Ctrl-C) is not a timeout and stays an error.
func isWallClockTimeout(ctx context.Context, err error) bool {
	if ctx.Err() == context.DeadlineExceeded || status.Code(err) == codes.DeadlineExceeded {
		return true
	}
	if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
		return true
	}
	return false
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
// The run stays `waiting` until it finishes: a log line is NOT a resume signal — an agent can
// (and does) keep logging while blocked in ask_human — so we do not infer resumption from the
// stream. Precise `waiting`→`running` on answer needs a guest "question resolved" RunEvent
// (§6.13, a Phase-5 protocol addition); until then a resolved run simply shows `waiting` until
// its terminal state.
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
			setState(StateWaiting)
			if err := writeQuestion(runDir, q); err != nil {
				return 0, false, err
			}
			notifyWaiting(filepath.Base(runDir), q.GetPrompt())
			if to := spec.Questions.Timeout; to > 0 {
				armQuestionTimeout(ctx, client, spec, q.GetId(), to, &aborted, streamCancel)
			}
		}
	}
}

// armQuestionTimeout schedules the per-question wait limit (§6.13). On expiry it probes with a
// no-answer sentinel: Ack.Ok reports whether the question was still pending, so a question the
// human already answered (possibly from another process) is never wrongly sentinel-echoed or
// aborted. Only a genuinely-still-pending question triggers the on-timeout action.
func armQuestionTimeout(ctx context.Context, client *controlclient.Client, spec task.RunSpec, qid string, to time.Duration, aborted *atomic.Bool, cancel context.CancelFunc) {
	time.AfterFunc(to, func() {
		ack, err := client.Agent.Answer(context.WithoutCancel(ctx), &pb.AnswerRequest{QuestionId: qid, NoAnswer: true})
		if err != nil || !ack.GetOk() {
			return // already answered/resolved, or transient failure — do not act
		}
		// The question was genuinely still pending at the deadline. The no-answer sentinel was
		// just delivered; `abort` additionally fails the whole run.
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
