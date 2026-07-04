// Package guest holds the guest-agent that runs inside the micro-VM. The OS-specific
// wiring (vsock listener, the containerd Runner, nftables/egress proxy) lives in
// build-tagged files and injected dependencies; this file is the OS-agnostic control-server
// logic, so the same code backs the real linux agent and the fakeProvider loopback in
// host-side unit tests (§14 test strategy).
//
// One VM serves exactly one run, so the Service holds single-run state (received bundle,
// image archive, task) guarded by a mutex against concurrent gRPC handlers.
package guest

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/418-cloud/krayt/internal/guest/ask"
	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/secrets"
)

// Version is the guest-agent version reported in the Hello handshake (§6.5).
const Version = "0.0.0-dev"

// chunkSize bounds each protocol Chunk; keep streams flowing without buffering a whole
// artifact in memory on either side (§6.5).
const chunkSize = 1 << 20 // 1 MiB

// Artifact file names written under the run's output dir and streamed by CollectArtifacts.
const (
	fileChangesPatch  = "changes.patch"
	fileCommitsBundle = "commits.bundle"
)

// Service implements pb.GuestAgentServer for one run (§6.5). The Runner is the only piece
// that needs a real OS/runtime; everything else (bundle ingest, baseline, diff, artifact
// streaming) is OS-agnostic.
type Service struct {
	pb.UnimplementedGuestAgentServer

	root       string  // base dir for this run's workspace/task/output/bundle/image
	secretsDir string  // where /run/secrets contents are materialized; tmpfs in the real guest
	runner     Runner  // nil until wired; Start fails clearly without it
	network    Network // nil in tests; the linux egress proxy + firewall controller (§6.6)

	mu         sync.Mutex
	bundlePath string // received git bundle (§6.7)
	imagePath  string // received OCI archive (§6.11)
	taskEnv    map[string]string
	secrets    map[string]string // received secrets, held in memory only (§6.8)
	netPolicy  NetworkPolicy     // received network policy (§6.6)
	baseline   string            // recorded baseline commit, set during Start (§6.7)
	bridge     *ask.Bridge       // active run's question bridge; Answer routes to it (§6.13)
}

// eventSender serializes Sends on the Start stream: the runner's stdout/stderr forwarders and
// the question pusher all emit RunEvents concurrently, and grpc.ServerStream.Send is not safe
// for concurrent use.
type eventSender struct {
	mu     sync.Mutex
	stream pb.GuestAgent_StartServer
}

func (e *eventSender) send(ev *pb.RunEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stream.Send(ev)
}

// Option configures a Service.
type Option func(*Service)

// WithRunner sets the container Runner (the linux containerd runner in production, a fake
// in tests).
func WithRunner(r Runner) Option { return func(s *Service) { s.runner = r } }

// WithRoot sets the base directory for the run's working files. If unset, a temp dir is
// created lazily on first use.
func WithRoot(dir string) Option { return func(s *Service) { s.root = dir } }

// WithSecretsDir sets where secret values are materialized for the /run/secrets mount. The
// real guest points this at a tmpfs path so secrets never touch persistent disk (§6.8); if
// unset they go under the run root (fine for tests).
func WithSecretsDir(dir string) Option { return func(s *Service) { s.secretsDir = dir } }

// WithNetwork sets the egress controller (the linux proxy+firewall in production; unset in
// tests, where no real egress occurs).
func WithNetwork(n Network) Option { return func(s *Service) { s.network = n } }

// NewService returns a guest control service ready to register on a gRPC server.
func NewService(opts ...Option) *Service {
	s := &Service{}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Hello performs the handshake + version negotiation (§6.5).
func (s *Service) Hello(_ context.Context, _ *pb.HelloRequest) (*pb.HelloResponse, error) {
	var cv string
	if s.runner != nil {
		cv = s.runner.Version()
	}
	return &pb.HelloResponse{AgentVersion: Version, ContainerdVersion: cv}, nil
}

// QueryImageBlobs reports which of the host's blobs the guest is missing (§6.11). A fresh
// ephemeral VM has an empty content store, so every queried blob is missing; the host then
// streams them all via PushImage. A warm-pool / persistent store could answer more
// selectively later — the protocol already supports it.
func (s *Service) QueryImageBlobs(_ context.Context, req *pb.BlobQuery) (*pb.BlobPresence, error) {
	return &pb.BlobPresence{MissingDigests: req.GetDigests()}, nil
}

// PushImage receives the OCI archive stream and stores it for the Runner to import (§6.11).
func (s *Service) PushImage(stream pb.GuestAgent_PushImageServer) error {
	root, err := s.ensureRoot()
	if err != nil {
		return err
	}
	dest := filepath.Join(root, "image.tar")
	if err := recvToFile(stream, dest); err != nil {
		return fmt.Errorf("guest: receive image: %w", err)
	}
	s.mu.Lock()
	s.imagePath = dest
	s.mu.Unlock()
	return stream.SendAndClose(&pb.Ack{Ok: true})
}

// PushCode receives the git bundle stream and stores it as a file (you cannot clone from a
// pipe, §6.7); the clone happens in Start.
func (s *Service) PushCode(stream pb.GuestAgent_PushCodeServer) error {
	root, err := s.ensureRoot()
	if err != nil {
		return err
	}
	dest := filepath.Join(root, "repo.bundle")
	if err := recvToFile(stream, dest); err != nil {
		return fmt.Errorf("guest: receive code bundle: %w", err)
	}
	s.mu.Lock()
	s.bundlePath = dest
	s.mu.Unlock()
	return stream.SendAndClose(&pb.Ack{Ok: true})
}

// PushTask injects the task prompt at the contract path and records non-secret env (§6.5).
func (s *Service) PushTask(_ context.Context, req *pb.TaskSpec) (*pb.Ack, error) {
	root, err := s.ensureRoot()
	if err != nil {
		return nil, err
	}
	taskPath := filepath.Join(root, "task", "prompt.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		return nil, fmt.Errorf("guest: create task dir: %w", err)
	}
	if err := os.WriteFile(taskPath, req.GetPrompt(), 0o644); err != nil {
		return nil, fmt.Errorf("guest: write task: %w", err)
	}
	s.mu.Lock()
	s.taskEnv = req.GetEnv()
	s.mu.Unlock()
	return &pb.Ack{Ok: true}, nil
}

// SetNetworkPolicy records the per-task egress policy (§6.6); it is applied at Start by the
// Network controller (start the allowlist proxy + nftables lock).
func (s *Service) SetNetworkPolicy(_ context.Context, req *pb.NetworkPolicy) (*pb.Ack, error) {
	var mode string
	switch req.GetMode() {
	case pb.NetworkPolicy_FULL:
		mode = NetFull
	case pb.NetworkPolicy_NONE:
		mode = NetNone
	default:
		mode = NetAllowlist
	}
	s.mu.Lock()
	s.netPolicy = NetworkPolicy{Mode: mode, Allow: req.GetAllow()}
	s.mu.Unlock()
	return &pb.Ack{Ok: true}, nil
}

// PushSecrets receives the per-task secrets and holds them in memory only (§6.8). They are
// materialized to the tmpfs secrets dir at Start and used to build the log redactor; they
// are never written to persistent disk or the RunEvent stream.
func (s *Service) PushSecrets(_ context.Context, req *pb.SecretsBundle) (*pb.Ack, error) {
	for k := range req.GetValues() {
		if k == "" || strings.ContainsAny(k, "/\\") || k == "." || k == ".." {
			return nil, fmt.Errorf("guest: invalid secret key %q", k)
		}
	}
	s.mu.Lock()
	s.secrets = req.GetValues()
	s.mu.Unlock()
	return &pb.Ack{Ok: true}, nil
}

// Start is the run spine (§6.5): ingest the bundle into the workspace and record the
// baseline (§6.7), run the user image via the Runner while streaming logs, then build the
// patch (+ optional commits bundle) into the output dir. The final RunEvent carries the
// terminal Status; the stream then closes.
func (s *Service) Start(req *pb.StartRequest, stream pb.GuestAgent_StartServer) error {
	ctx := stream.Context()
	s.mu.Lock()
	bundlePath, imagePath, env, secretVals, netPolicy := s.bundlePath, s.imagePath, s.taskEnv, s.secrets, s.netPolicy
	s.mu.Unlock()

	if s.runner == nil {
		return fmt.Errorf("guest: no runner configured")
	}
	if bundlePath == "" {
		return fmt.Errorf("guest: Start before PushCode")
	}
	root, err := s.ensureRoot()
	if err != nil {
		return err
	}
	workspace := filepath.Join(root, "workspace")
	outputDir := filepath.Join(root, "output")
	if err := os.MkdirAll(outputDir, 0o777); err != nil {
		return fmt.Errorf("guest: create output dir: %w", err)
	}
	// The container runs non-root (§8.2) and writes report.md/changes.patch into /output, so it
	// must be writable by other uids; chmod explicitly so a non-022 umask can't leave it root-only.
	if err := os.Chmod(outputDir, 0o777); err != nil {
		return fmt.Errorf("guest: chmod output dir: %w", err)
	}

	// Materialize secrets to the (tmpfs) secrets dir and build the redactor (§6.8).
	secretsDir, err := s.writeSecrets(secretVals)
	if err != nil {
		return err
	}
	redactor := secrets.NewRedactor(secrets.Values(secretVals))

	// Ingest: verify + clone the bundle, set identity, record + tag the baseline (§6.7).
	baseline, err := patch.Ingest(ctx, bundlePath, workspace, patch.DefaultIdentity)
	if err != nil {
		return err
	}
	// Ingest clones the bundle as root; relax the tree so the non-root container can edit it
	// (§8.2). .git stays root-owned, so the guest's own git (run as root) is unaffected.
	if err := makeContainerWritable(workspace); err != nil {
		return fmt.Errorf("guest: make workspace writable: %w", err)
	}
	s.mu.Lock()
	s.baseline = baseline
	s.mu.Unlock()

	// Bring up the egress proxy + nftables lock and inject HTTP(S)_PROXY into the container
	// (§6.6). The controller is linux-only; without it (tests) the container just inherits
	// the task env.
	runEnv := env
	if s.network != nil {
		proxyEnv, err := s.network.Apply(ctx, netPolicy)
		if err != nil {
			return fmt.Errorf("guest: apply network policy: %w", err)
		}
		runEnv = mergeEnv(env, proxyEnv)
	}

	// All RunEvents go through es so the log forwarders and the question pusher can Send
	// concurrently without racing (§6.13).
	es := &eventSender{stream: stream}

	// Wire the agent → human question bridge (§6.13): questions become RunEvent.Question on
	// this stream; the host resolves them via the Answer RPC. The bridge is reachable two
	// ways — the container connects to a unix socket (AskSocket, used by the Phase-5
	// front-ends) and, for in-process fake runners, RunConfig.Ask calls it directly.
	bridge := ask.NewBridge(func(id, prompt string, choices []string) error {
		return es.send(&pb.RunEvent{Kind: &pb.RunEvent_Question{Question: &pb.Question{
			Id: id, Prompt: prompt, Choices: choices,
		}}})
	})
	s.mu.Lock()
	s.bridge = bridge
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.bridge = nil
		s.mu.Unlock()
	}()

	// Best-effort ask socket for the container front-ends; if bind is unavailable the direct
	// RunConfig.Ask handle still serves in-process runners.
	askSocket := ""
	if ln, lerr := net.Listen("unix", filepath.Join(root, "ask.sock")); lerr == nil {
		askSocket = filepath.Join(root, "ask.sock")
		// Connecting to a unix socket needs write permission on it; the container is non-root
		// (§8.2), so make the socket connectable by other uids (§6.13).
		_ = os.Chmod(askSocket, 0o777)
		go func() { _ = ask.Serve(ctx, ln, bridge) }()
	}

	// Run the agent, streaming logs to the host as they arrive — redacted in the guest so
	// secret values never cross the wire in logs (§6.8).
	log := func(strm pb.LogLine_Stream, line []byte, ts int64) {
		_ = es.send(&pb.RunEvent{Kind: &pb.RunEvent_Log{Log: &pb.LogLine{
			Stream: strm, Line: redactor.Redact(line), TsUnixMs: ts,
		}}})
	}
	exitCode, runErr := s.runner.Run(ctx, RunConfig{
		ImageArchivePath: imagePath,
		ImageRef:         req.GetImageRef(),
		WorkspaceDir:     workspace,
		TaskPath:         filepath.Join(root, "task", "prompt.md"),
		OutputDir:        outputDir,
		SecretsDir:       secretsDir,
		Env:              runEnv,
		AskSocket:        askSocket,
		Ask:              bridge.Ask,
	}, log)

	// The run context being done means the host aborted us — normally the wall-clock timeout
	// (§6.1), possibly a disconnect. The guest cannot reliably tell which, and must NOT check
	// for context.DeadlineExceeded: gRPC delivers the server-side context as context.Canceled
	// on a client-deadline expiry (verified), racing the propagated grpc-timeout deadline, so
	// DeadlineExceeded would miss real timeouts. Any cancellation means "the stream is dead,
	// stop reporting and bail." The host is authoritative for the timed_out label — it checks
	// DeadlineExceeded on its own context in the orchestrator.
	timedOut := ctx.Err() != nil
	if runErr != nil && !timedOut {
		// Infrastructure failure (import/create/start). Report it on the terminal Status.
		return es.send(&pb.RunEvent{Kind: &pb.RunEvent_Status{Status: &pb.Status{
			ExitCode: -1, Error: runErr.Error(),
		}}})
	}

	// Build the patch + optional reverse bundle from the recorded baseline (§6.7).
	if err := s.buildArtifacts(ctx, workspace, outputDir); err != nil && !timedOut {
		return es.send(&pb.RunEvent{Kind: &pb.RunEvent_Status{Status: &pb.Status{
			ExitCode: int32(exitCode), Error: err.Error(),
		}}})
	}
	return es.send(&pb.RunEvent{Kind: &pb.RunEvent_Status{Status: &pb.Status{
		ExitCode: int32(exitCode), TimedOut: timedOut,
	}}})
}

// Answer resolves an outstanding agent question (§6.13). The host calls it (directly, or via
// `krayt answer` dialing the guest) with the human's response or a no-answer sentinel; it
// routes to the active run's bridge. Ok=false means no such question is waiting — a duplicate
// or late answer, which is a harmless no-op.
func (s *Service) Answer(_ context.Context, req *pb.AnswerRequest) (*pb.Ack, error) {
	s.mu.Lock()
	b := s.bridge
	s.mu.Unlock()
	if b == nil {
		return &pb.Ack{Ok: false}, nil
	}
	return &pb.Ack{Ok: b.Answer(req.GetQuestionId(), req.GetResponse(), req.GetNoAnswer())}, nil
}

// writeSecrets materializes each secret as a file under the secrets dir (0600), so the
// runner can bind-mount it at /run/secrets (§6.8). Returns the dir, or "" when there are no
// secrets. The dir is tmpfs in the real guest (WithSecretsDir); under the run root in tests.
func (s *Service) writeSecrets(vals map[string]string) (string, error) {
	if len(vals) == 0 {
		return "", nil
	}
	dir := s.secretsDir
	if dir == "" {
		dir = filepath.Join(s.root, "secrets")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("guest: create secrets dir: %w", err)
	}
	// The guest-agent runs as root, but the container runs as a NON-ROOT uid (Claude Code and
	// others refuse uid 0, §8.2) and must read its credential from this tmpfs (§6.14). So the
	// dir must be traversable and the files readable by other uids — mirrors how Kubernetes/Docker
	// mount secrets (0644). The exposure is bounded: the VM is single-container and ephemeral, and
	// this container is the credential's intended consumer. Chmod explicitly so a non-022 umask on
	// the guest init can't leave them root-only. Secrecy on the host disk + log redaction (§6.8)
	// are unaffected — this is only the in-VM tmpfs.
	if err := os.Chmod(dir, 0o755); err != nil {
		return "", fmt.Errorf("guest: chmod secrets dir: %w", err)
	}
	for k, v := range vals {
		p := filepath.Join(dir, k)
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			return "", fmt.Errorf("guest: write secret %s: %w", k, err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			return "", fmt.Errorf("guest: chmod secret %s: %w", k, err)
		}
	}
	return dir, nil
}

// makeContainerWritable relaxes a directory tree so the NON-ROOT container uid (§8.2) can read,
// traverse, and write it. The guest runs as root and ingests /workspace root-owned, but the
// agent runs non-root (Claude Code and others refuse uid 0) and must edit the tree. Exposure is
// bounded to the ephemeral single-container VM. The .git owner is left as root, so the guest's
// own git (run as root) still works; the agent's git needs `safe.directory`, set by the
// adapter/entrypoint.
func makeContainerWritable(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() | 0o066 // group + other read/write
		if d.IsDir() {
			mode |= 0o111 // …and traversable
		}
		return os.Chmod(p, mode)
	})
}

// mergeEnv overlays add onto a copy of base (proxy env wins over task env).
func mergeEnv(base, add map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(add))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range add {
		out[k] = v
	}
	return out
}

// buildArtifacts writes changes.patch and, if the agent committed, commits.bundle into the
// output dir (§6.7).
func (s *Service) buildArtifacts(ctx context.Context, workspace, outputDir string) error {
	diff, err := patch.Diff(ctx, workspace, patch.BaselineTag)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, fileChangesPatch), diff, 0o644); err != nil {
		return fmt.Errorf("guest: write patch: %w", err)
	}
	if _, err := patch.BundleCommits(ctx, workspace, patch.BaselineTag, filepath.Join(outputDir, fileCommitsBundle)); err != nil {
		return err
	}
	return nil
}

// CollectArtifacts streams the output dir back to the host as a tar (§6.5, §6.7): the
// guest already built changes.patch (+ optional commits.bundle, + an agent-written
// report.md if present). The host extracts it into the run dir.
func (s *Service) CollectArtifacts(_ *pb.CollectRequest, stream pb.GuestAgent_CollectArtifactsServer) error {
	root, err := s.ensureRoot()
	if err != nil {
		return err
	}
	outputDir := filepath.Join(root, "output")
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarDir(outputDir, pw)) }()
	return sendFromReader(stream, pr)
}

// Shutdown is acknowledged; the host then destroys the VM (§6.5). Nothing persists.
func (s *Service) Shutdown(_ context.Context, _ *pb.ShutdownRequest) (*pb.Ack, error) {
	return &pb.Ack{Ok: true}, nil
}

// ensureRoot lazily creates the run's base dir if WithRoot was not supplied.
func (s *Service) ensureRoot() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root != "" {
		if err := os.MkdirAll(s.root, 0o755); err != nil {
			return "", fmt.Errorf("guest: create root: %w", err)
		}
		return s.root, nil
	}
	dir, err := os.MkdirTemp("", "krayt-run-")
	if err != nil {
		return "", fmt.Errorf("guest: temp root: %w", err)
	}
	s.root = dir
	return dir, nil
}

// recvToFile reassembles a client-streamed Chunk sequence into dest, never buffering the
// whole stream in memory (§6.5).
func recvToFile[T interface {
	Recv() (*pb.Chunk, error)
}](stream T, dest string) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return f.Close()
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(chunk.GetData()); err != nil {
			return err
		}
	}
}

// sendFromReader streams r to the host as Chunks (§6.5).
func sendFromReader(stream pb.GuestAgent_CollectArtifactsServer, r io.Reader) error {
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.Chunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// tarDir writes a tar of dir's contents to w. A missing dir yields an empty tar (a run that
// produced no artifacts is valid, not an error).
func tarDir(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return tw.Close()
	}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// /output is written by untrusted agent code, so archive only directories and
		// regular files; skip symlinks and other special files. A symlink would otherwise
		// get a 0-byte symlink header while os.Open follows the link, and io.Copy of the
		// target would fail the whole collection with ErrWriteTooLong (the tar writer
		// rejects the body, so it cannot exfiltrate the target — but skipping is correct).
		if !d.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
