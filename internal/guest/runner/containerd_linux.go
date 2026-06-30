//go:build linux

// Package runner is the guest-side containerd Runner (§6.10): it imports the user's OCI
// image (pre-loaded over vsock as an archive, §6.11) into containerd's content store and
// runs it as a single container with the contract mounts/env (§8.2), streaming logs back.
//
// This file is //go:build linux and drives a real containerd daemon, so it cannot run or
// be unit-tested in a cloud agent — it is exercised only by the real-VM integration test
// (build tag `integration`) on a Mac/CI, mirroring the Phase 1 vfkit boot test. The
// OS-agnostic guest.Service (bundle ingest, baseline, diff) is tested in-process against a
// fake runner instead (§14). Treat this code as unverified until the integration run.
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/protocol/pb"
)

// namespace isolates krayt's containerd objects from anything else on the daemon.
const namespace = "krayt"

// containerID / snapshotID are fixed because there is exactly one container per VM (§6.10).
const (
	containerID = "krayt-run"
	snapshotID  = "krayt-run-snapshot"
)

// Runner drives containerd over its local gRPC socket via the native Go client (§6.10).
type Runner struct {
	client    *containerd.Client
	cdVersion string // cached containerd version for the Hello handshake (§6.5)
}

// New connects to containerd at socket (e.g. /run/containerd/containerd.sock) and caches
// its version. The guest-agent constructs one Runner at startup and reuses it for the run.
func New(socket string) (*Runner, error) {
	client, err := containerd.New(socket)
	if err != nil {
		return nil, fmt.Errorf("runner: connect containerd at %s: %w", socket, err)
	}
	r := &Runner{client: client}
	ctx := namespaces.WithNamespace(context.Background(), namespace)
	if v, err := client.Version(ctx); err == nil {
		r.cdVersion = v.Version
	}
	return r, nil
}

// Close releases the containerd client connection.
func (r *Runner) Close() error { return r.client.Close() }

// Version reports the containerd version (§6.5).
func (r *Runner) Version() string { return r.cdVersion }

// Run imports the pre-loaded image archive and runs the container with the contract
// mounts/env, forwarding stdout/stderr to log as they are produced, and returns the
// container's exit code (§6.10). A non-nil error is an infrastructure failure (import/
// create/start), kept distinct from a non-zero exit code.
func (r *Runner) Run(ctx context.Context, cfg guest.RunConfig, log guest.LogFunc) (int, error) {
	ctx = namespaces.WithNamespace(ctx, namespace)

	image, err := r.importImage(ctx, cfg)
	if err != nil {
		return -1, err
	}
	if err := image.Unpack(ctx, ""); err != nil {
		return -1, fmt.Errorf("runner: unpack image: %w", err)
	}

	container, err := r.client.NewContainer(ctx, containerID,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(snapshotID, image),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithProcessCwd(guest.ContainerWorkspace),
			oci.WithEnv(envSlice(cfg.Env)),
			oci.WithMounts(contractMounts(cfg)),
		),
	)
	if err != nil {
		return -1, fmt.Errorf("runner: create container: %w", err)
	}
	defer func() { _ = container.Delete(ctx, containerd.WithSnapshotCleanup) }()

	// Pipe stdout/stderr through goroutines that forward raw reads to the host.
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	go forward(outR, pb.LogLine_STDOUT, log)
	go forward(errR, pb.LogLine_STDERR, log)

	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, outW, errW)))
	if err != nil {
		return -1, fmt.Errorf("runner: create task: %w", err)
	}
	defer func() { _, _ = task.Delete(ctx) }()

	// Wait must be set up before Start so we never miss a fast exit.
	exitCh, err := task.Wait(ctx)
	if err != nil {
		return -1, fmt.Errorf("runner: wait setup: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		return -1, fmt.Errorf("runner: start task: %w", err)
	}

	status := <-exitCh
	_ = outW.Close()
	_ = errW.Close()
	code, _, err := status.Result()
	if err != nil {
		return -1, fmt.Errorf("runner: exit result: %w", err)
	}
	return int(code), nil
}

// importImage imports the OCI archive into containerd's content store and resolves the
// runnable image. containerd verifies blob digests on import, giving the same integrity
// guarantee as the base image (§6.11).
func (r *Runner) importImage(ctx context.Context, cfg guest.RunConfig) (containerd.Image, error) {
	f, err := os.Open(cfg.ImageArchivePath)
	if err != nil {
		return nil, fmt.Errorf("runner: open image archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	imgs, err := r.client.Import(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("runner: import image: %w", err)
	}
	if len(imgs) == 0 {
		return nil, fmt.Errorf("runner: image archive contained no images")
	}
	// Prefer the image whose name matches the requested ref; fall back to the first.
	name := imgs[0].Name
	for _, im := range imgs {
		if im.Name == cfg.ImageRef {
			name = im.Name
			break
		}
	}
	image, err := r.client.GetImage(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("runner: resolve imported image %q: %w", name, err)
	}
	return image, nil
}

// contractMounts bind-mounts the run's working directories to the container contract paths
// (§8.2). The task dir (containing prompt.md) is mounted read-only.
func contractMounts(cfg guest.RunConfig) []specs.Mount {
	taskDir := parentDir(cfg.TaskPath)
	return []specs.Mount{
		{Destination: guest.ContainerWorkspace, Type: "bind", Source: cfg.WorkspaceDir, Options: []string{"rbind", "rw"}},
		{Destination: "/task", Type: "bind", Source: taskDir, Options: []string{"rbind", "ro"}},
		{Destination: guest.ContainerOutput, Type: "bind", Source: cfg.OutputDir, Options: []string{"rbind", "rw"}},
	}
}

// forward copies r to the host as log lines until EOF; pipe closure ends it cleanly.
func forward(r io.Reader, stream pb.LogLine_Stream, log guest.LogFunc) {
	br := bufio.NewReader(r)
	buf := make([]byte, 32*1024)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			line := make([]byte, n)
			copy(line, buf[:n])
			log(stream, line, time.Now().UnixMilli())
		}
		if err != nil {
			return
		}
	}
}

// envSlice flattens an env map to KEY=VALUE entries for oci.WithEnv.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// parentDir returns the directory containing path (used to mount the task dir).
func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}

// compile-time check that Runner satisfies the guest.Runner seam.
var _ guest.Runner = (*Runner)(nil)
