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
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
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
		containerd.WithNewSpec(buildSpecOpts(cfg, image)...),
	)
	if err != nil {
		// A root image (uid 0) fails here via withEnforceNonRoot; the runner surfaces it as an
		// infrastructure error (§8.2, §10). Other create failures land here too.
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

	// Wait for the container to exit, or kill it on context cancel/deadline — the
	// wall-clock timeout kills the container, then the host tears down the VM (§6.1).
	var status containerd.ExitStatus
	select {
	case status = <-exitCh:
	case <-ctx.Done():
		killCtx := context.WithoutCancel(ctx)
		_ = task.Kill(killCtx, syscall.SIGKILL)
		// Drain the exit so the killed task is reaped; the exit code is irrelevant here.
		select {
		case <-exitCh:
		case <-time.After(5 * time.Second):
		}
		_ = outW.Close()
		_ = errW.Close()
		return -1, ctx.Err()
	}
	_ = outW.Close()
	_ = errW.Close()
	code, _, err := status.Result()
	if err != nil {
		return -1, fmt.Errorf("runner: exit result: %w", err)
	}
	return int(code), nil
}

// buildSpecOpts assembles the ordered OCI spec options for the untrusted agent container so it
// runs with least privilege (§6.10, §10). Order matters: WithImageConfig first (it resolves the
// image's USER and default caps/env), the security opts after (they read/overwrite what the image
// config set — enforce non-root off the resolved USER, drop all caps, clear ambient), then the
// host network namespace last so the container shares the VM's netns and the egress lock applies
// (§6.6). Factored out (and split from securitySpecOpts) so the security decisions are unit-tested
// against an in-memory spec without a containerd daemon (§14).
func buildSpecOpts(cfg guest.RunConfig, image oci.Image) []oci.SpecOpts {
	opts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithProcessCwd(guest.ContainerWorkspace),
		oci.WithEnv(envSlice(cfg.Env)),
		oci.WithMounts(contractMounts(cfg)),
	}
	opts = append(opts, securitySpecOpts(cfg)...)
	// Run in the VM's own network namespace (no new netns), so the container reaches the egress
	// proxy on the VM's loopback and the nftables output lock applies to its sockets — the VM
	// boundary is the network boundary (§6.6).
	opts = append(opts, oci.WithHostNamespace(specs.NetworkNamespace))
	return opts
}

// securitySpecOpts is the least-privilege half of the spec (§10): it depends only on the task
// policy in cfg, not on the image, so it applies cleanly to a seeded *specs.Spec in tests.
//   - withEnforceNonRoot fails the run if the image would run as uid 0 (§8.2) — the enforcement
//     that, together with dropped CAP_SETUID/SETGID, makes the skuid-based egress lock unbypassable.
//   - WithCapabilities(cfg.AddCapabilities) drops ALL capabilities when the opt-in list is empty
//     (Bounding/Effective/Permitted/Inheritable), or grants exactly the validated opt-in set.
//   - withClearAmbient zeroes the ambient set belt-and-suspenders, regardless of the image config.
//   - the containerd default seccomp profile is applied unless the task opts out.
//   - the rootfs is read-only only on explicit opt-in (writable tmpfs /tmp + /run come from
//     contractMounts so they precede the /run/secrets bind).
//
// NoNewPrivileges stays the containerd default (true); nothing here relaxes it.
func securitySpecOpts(cfg guest.RunConfig) []oci.SpecOpts {
	opts := []oci.SpecOpts{
		withEnforceNonRoot(),
		oci.WithCapabilities(cfg.AddCapabilities),
		withClearAmbient(),
	}
	if !cfg.SeccompUnconfined {
		opts = append(opts, seccomp.WithDefaultProfile())
	}
	if cfg.ReadonlyRootfs {
		opts = append(opts, oci.WithRootFSReadonly())
	}
	return opts
}

// withEnforceNonRoot fails container creation when the image would run as root (uid 0), covering
// both an explicit `USER root` and an unset USER (which WithImageConfig leaves at the uid-0
// default). krayt requires non-root because egress and secret confinement depend on it: a uid-0
// process could setuid to proxyd even with caps dropped in some kernels, and some agents refuse
// uid 0 anyway (§8.2). Must run AFTER WithImageConfig, which resolves s.Process.User from the
// image config.
func withEnforceNonRoot() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if s.Process == nil {
			return fmt.Errorf("krayt: image has no process config; cannot verify non-root user")
		}
		if s.Process.User.UID == 0 {
			return fmt.Errorf("krayt: image runs as root (uid 0); set a non-root USER in the image (§8.2) — egress and secret confinement require non-root")
		}
		return nil
	}
}

// withClearAmbient explicitly empties the ambient capability set so no capability is
// inheritable-ambient regardless of what the image config or WithCapabilities left behind (§10).
func withClearAmbient() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if s.Process != nil && s.Process.Capabilities != nil {
			s.Process.Capabilities.Ambient = nil
		}
		return nil
	}
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
	var mounts []specs.Mount
	// Read-only rootfs (§8.2 opt-in) still needs writable /tmp and /run. These ephemeral tmpfs
	// mounts MUST precede the /run/secrets + ask-socket binds below, or a tmpfs mounted on /run
	// afterwards would shadow them. /run drops exec (nothing legitimately execs from it); /tmp
	// keeps exec because agents run helper scripts from it (verified against the krayt-dev image).
	// Both drop suid/dev. This is only ever a tmpfs over an empty system dir, never a blanket
	// tmpfs over a populated one (§10).
	if cfg.ReadonlyRootfs {
		mounts = append(mounts,
			specs.Mount{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777"}},
			specs.Mount{Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "noexec", "mode=0755"}},
		)
	}
	mounts = append(mounts,
		specs.Mount{Destination: guest.ContainerWorkspace, Type: "bind", Source: cfg.WorkspaceDir, Options: []string{"rbind", "rw"}},
		specs.Mount{Destination: "/task", Type: "bind", Source: taskDir, Options: []string{"rbind", "ro"}},
		specs.Mount{Destination: guest.ContainerOutput, Type: "bind", Source: cfg.OutputDir, Options: []string{"rbind", "rw"}},
	)
	// Secrets are bind-mounted read-only from the guest's tmpfs secrets dir (§6.8).
	if cfg.SecretsDir != "" {
		mounts = append(mounts, specs.Mount{
			Destination: guest.ContainerSecrets, Type: "bind", Source: cfg.SecretsDir,
			Options: []string{"rbind", "ro"},
		})
	}
	// The agent → human question bridge socket (§6.13); the front-ends (MCP server / krayt-ask)
	// inside the container connect to it. Empty when the guest could not open it.
	if cfg.AskSocket != "" {
		mounts = append(mounts, specs.Mount{
			Destination: guest.ContainerAskSocket, Type: "bind", Source: cfg.AskSocket,
			Options: []string{"rbind", "rw"},
		})
	}
	// The krayt-ask CLI front-end, mounted read-only onto the container PATH so any agent can
	// shell out to `krayt-ask` (§6.13). Empty when the binary isn't shipped alongside the agent.
	if cfg.AskBinary != "" {
		mounts = append(mounts, specs.Mount{
			Destination: guest.ContainerAskBin, Type: "bind", Source: cfg.AskBinary,
			Options: []string{"rbind", "ro"},
		})
	}
	return mounts
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
