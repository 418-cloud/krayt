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
	"os"
	"path/filepath"
	"sync"

	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/protocol/pb"
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

	root   string // base dir for this run's workspace/task/output/bundle/image
	runner Runner // nil until wired; Start fails clearly without it

	mu         sync.Mutex
	bundlePath string // received git bundle (§6.7)
	imagePath  string // received OCI archive (§6.11)
	taskEnv    map[string]string
	baseline   string // recorded baseline commit, set during Start (§6.7)
}

// Option configures a Service.
type Option func(*Service)

// WithRunner sets the container Runner (the linux containerd runner in production, a fake
// in tests).
func WithRunner(r Runner) Option { return func(s *Service) { s.runner = r } }

// WithRoot sets the base directory for the run's working files. If unset, a temp dir is
// created lazily on first use.
func WithRoot(dir string) Option { return func(s *Service) { s.root = dir } }

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

// Start is the run spine (§6.5): ingest the bundle into the workspace and record the
// baseline (§6.7), run the user image via the Runner while streaming logs, then build the
// patch (+ optional commits bundle) into the output dir. The final RunEvent carries the
// terminal Status; the stream then closes.
func (s *Service) Start(req *pb.StartRequest, stream pb.GuestAgent_StartServer) error {
	ctx := stream.Context()
	s.mu.Lock()
	bundlePath, imagePath, env := s.bundlePath, s.imagePath, s.taskEnv
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
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("guest: create output dir: %w", err)
	}

	// Ingest: verify + clone the bundle, set identity, record + tag the baseline (§6.7).
	baseline, err := patch.Ingest(ctx, bundlePath, workspace, patch.DefaultIdentity)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.baseline = baseline
	s.mu.Unlock()

	// Run the agent, streaming logs to the host as they arrive.
	log := func(strm pb.LogLine_Stream, line []byte, ts int64) {
		_ = stream.Send(&pb.RunEvent{Kind: &pb.RunEvent_Log{Log: &pb.LogLine{
			Stream: strm, Line: line, TsUnixMs: ts,
		}}})
	}
	exitCode, runErr := s.runner.Run(ctx, RunConfig{
		ImageArchivePath: imagePath,
		ImageRef:         req.GetImageRef(),
		WorkspaceDir:     workspace,
		TaskPath:         filepath.Join(root, "task", "prompt.md"),
		OutputDir:        outputDir,
		Env:              env,
	}, log)
	if runErr != nil {
		// Infrastructure failure (import/create/start). Report it on the terminal Status.
		return stream.Send(&pb.RunEvent{Kind: &pb.RunEvent_Status{Status: &pb.Status{
			ExitCode: -1, Error: runErr.Error(),
		}}})
	}

	// Build the patch + optional reverse bundle from the recorded baseline (§6.7).
	if err := s.buildArtifacts(ctx, workspace, outputDir); err != nil {
		return stream.Send(&pb.RunEvent{Kind: &pb.RunEvent_Status{Status: &pb.Status{
			ExitCode: int32(exitCode), Error: err.Error(),
		}}})
	}
	return stream.Send(&pb.RunEvent{Kind: &pb.RunEvent_Status{Status: &pb.Status{
		ExitCode: int32(exitCode),
	}}})
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
