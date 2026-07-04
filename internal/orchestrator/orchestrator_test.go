package orchestrator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/imagestore"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider/fake"
	"github.com/418-cloud/krayt/internal/secrets"
	"github.com/418-cloud/krayt/internal/task"
)

// TestEndToEndRun is the automated proof of the Phase 2 "Done when" (§14 test strategy):
// against the in-process fakeProvider, `krayt`'s orchestrator drives the real bundle →
// clone → baseline → diff → collect path, a stand-in agent (the fake Runner) edits one
// file, and the resulting changes.patch applies cleanly back onto the host repo. The
// container runtime is the only simulated piece; everything else is production code. The
// real trivial-image-on-hardware run is the build-tagged integration test + a HUMAN_TODO.
func TestEndToEndRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A host repo to sandbox.
	src := newRepo(t, map[string]string{
		"greeting.txt": "hello\n",
		"keep.txt":     "unchanged\n",
	})

	// A minimal user image acquired on the host (exercises the PushImage path).
	img := minimalImage(ctx, t)

	// Stand-in agent: edits one tracked file + adds a new one, without committing.
	runner := &editingRunner{edits: map[string]string{
		"greeting.txt": "hello world\n",
		"new.txt":      "fresh\n",
	}}
	guestRoot := t.TempDir()
	p := &fake.Provider{Register: func(s *grpc.Server) {
		pb.RegisterGuestAgentServer(s, guest.NewService(
			guest.WithRunner(runner),
			guest.WithRoot(guestRoot),
		))
	}}

	var logs bytes.Buffer
	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID:          "run_e2e",
		ImageRef:    "latest",
		RepoPath:    src,
		BundleDepth: 1,
		TaskPrompt:  []byte("edit the greeting"),
		Resources:   task.Resources{CPUs: 2, MemoryMiB: 2048},
	}

	res, err := orchestrator.Run(ctx, orchestrator.Deps{Provider: p, Image: img, LogOut: &logs}, spec, runDir)
	if err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}

	// Artifacts + logs landed in the run dir.
	patchBytes, err := os.ReadFile(res.PatchPath)
	if err != nil || len(patchBytes) == 0 {
		t.Fatalf("changes.patch missing/empty: err=%v len=%d", err, len(patchBytes))
	}
	if !bytes.Contains(logs.Bytes(), []byte("agent starting")) {
		t.Errorf("live log did not include agent output; got %q", logs.String())
	}
	assertMeta(t, filepath.Join(runDir, "meta.json"), spec.ID)

	// The "Done when": the patch applies cleanly onto a fresh checkout of the host repo.
	target := filepath.Join(t.TempDir(), "target")
	run(t, "", "git", "clone", "--quiet", src, target)
	if err := patch.Apply(ctx, target, res.PatchPath, false); err != nil {
		t.Fatalf("krayt apply (git apply) failed: %v", err)
	}
	if got := readFile(t, filepath.Join(target, "greeting.txt")); got != "hello world\n" {
		t.Errorf("greeting.txt after apply = %q, want %q", got, "hello world\n")
	}
	if got := readFile(t, filepath.Join(target, "new.txt")); got != "fresh\n" {
		t.Errorf("new.txt after apply = %q, want %q", got, "fresh\n")
	}
}

// editingRunner is a stand-in for the containerd runner: it simulates the agent by writing
// known edits into the workspace and emitting a couple of log lines (§14).
type editingRunner struct{ edits map[string]string }

func (r *editingRunner) Version() string { return "fake-containerd" }

func (r *editingRunner) Run(_ context.Context, cfg guest.RunConfig, log guest.LogFunc) (int, error) {
	log(pb.LogLine_STDOUT, []byte("agent starting\n"), time.Now().UnixMilli())
	for name, contentStr := range r.edits {
		p := filepath.Join(cfg.WorkspaceDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return 1, err
		}
		if err := os.WriteFile(p, []byte(contentStr), 0o644); err != nil {
			return 1, err
		}
	}
	log(pb.LogLine_STDOUT, []byte("agent done\n"), time.Now().UnixMilli())
	return 0, nil
}

// TestSecretsRedactedInLogs is the Phase 3 "secrets never appear in logs/artifacts" proof:
// a secret reaches the container (mounted at /run/secrets), but when the agent prints it the
// guest redacts it before the line is streamed, so it is absent from the live log, the
// persisted agent.log, and meta.json (§6.8).
func TestSecretsRedactedInLogs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	img := minimalImage(ctx, t)

	const secretVal = "sk-ant-supersecret-0123456789"
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY="+secretVal+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mounted string
	runner := &secretRunner{secret: secretVal, onRun: func(cfg guest.RunConfig) {
		if cfg.SecretsDir != "" {
			b, _ := os.ReadFile(filepath.Join(cfg.SecretsDir, "ANTHROPIC_API_KEY"))
			mounted = string(b)
		}
	}}
	guestRoot := t.TempDir()
	p := &fake.Provider{Register: func(s *grpc.Server) {
		pb.RegisterGuestAgentServer(s, guest.NewService(guest.WithRunner(runner), guest.WithRoot(guestRoot)))
	}}

	var logs bytes.Buffer
	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_secrets", ImageRef: "latest", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"), SecretsPath: secretsFile,
	}
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Provider: p, Image: img, LogOut: &logs}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The agent could read the real secret (mounted at /run/secrets)…
	if mounted != secretVal {
		t.Errorf("secret not mounted for the agent: got %q", mounted)
	}
	// …and, since the container runs as a NON-ROOT uid (§8.2) while the guest writes the tmpfs as
	// root, the dir must be traversable and the file world-readable or the agent can't read its
	// credential (the exit-78 bug the claude-code image hit).
	secDir := filepath.Join(guestRoot, "secrets")
	if di, err := os.Stat(secDir); err != nil {
		t.Fatalf("stat secrets dir: %v", err)
	} else if di.Mode().Perm()&0o001 == 0 {
		t.Errorf("secrets dir mode %v not traversable by others; a non-root container can't reach /run/secrets (§8.2)", di.Mode().Perm())
	}
	if fi, err := os.Stat(filepath.Join(secDir, "ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("stat secret file: %v", err)
	} else if fi.Mode().Perm()&0o004 == 0 {
		t.Errorf("secret file mode %v not readable by others; a non-root container can't read it (§8.2/§6.14)", fi.Mode().Perm())
	}
	// …but it must not survive anywhere krayt records output.
	if bytes.Contains(logs.Bytes(), []byte(secretVal)) {
		t.Error("secret value leaked into the live log stream")
	}
	if !bytes.Contains(logs.Bytes(), []byte(secrets.RedactionMarker)) {
		t.Errorf("expected a redaction marker in the logs; got %q", logs.String())
	}
	for _, f := range []string{"logs/agent.log", "meta.json"} {
		b, err := os.ReadFile(filepath.Join(runDir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if bytes.Contains(b, []byte(secretVal)) {
			t.Errorf("secret value leaked into %s", f)
		}
	}
}

// TestRunTimeout is the wall-clock-timeout proof: a stuck agent is killed and the run is
// recorded as timed out, with the VM torn down (§6.1).
func TestRunTimeout(t *testing.T) {
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	img := minimalImage(context.Background(), t)

	guestRoot := t.TempDir()
	p := &fake.Provider{Register: func(s *grpc.Server) {
		pb.RegisterGuestAgentServer(s, guest.NewService(guest.WithRunner(blockingRunner{}), guest.WithRoot(guestRoot)))
	}}

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_timeout", ImageRef: "latest", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"),
		Resources:  task.Resources{Timeout: 300 * time.Millisecond},
	}
	res, err := orchestrator.Run(context.Background(), orchestrator.Deps{Provider: p, Image: img}, spec, runDir)
	if err != nil {
		t.Fatalf("Run (timeout should not be an error): %v", err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut = true")
	}
	b, err := os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"timed_out": true`)) {
		t.Errorf("meta.json should record timed_out: true; got %s", b)
	}
}

// secretRunner simulates an agent that reads the mounted secret and (carelessly) logs it.
type secretRunner struct {
	secret string
	onRun  func(guest.RunConfig)
}

func (r *secretRunner) Version() string { return "fake" }
func (r *secretRunner) Run(_ context.Context, cfg guest.RunConfig, log guest.LogFunc) (int, error) {
	if r.onRun != nil {
		r.onRun(cfg)
	}
	log(pb.LogLine_STDOUT, []byte("debug: ANTHROPIC_API_KEY="+r.secret+" (oops)\n"), time.Now().UnixMilli())
	return 0, nil
}

// blockingRunner never finishes on its own; it returns only when the run context is
// canceled (the wall-clock timeout).
type blockingRunner struct{}

func (blockingRunner) Version() string { return "fake" }
func (blockingRunner) Run(ctx context.Context, _ guest.RunConfig, _ guest.LogFunc) (int, error) {
	<-ctx.Done()
	return -1, ctx.Err()
}

// --- helpers ---

func minimalImage(ctx context.Context, t *testing.T) *imagestore.Image {
	t.Helper()
	src := memory.New()
	cfg := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, []byte(`{"architecture":"arm64","os":"linux"}`))
	if err := src.Push(ctx, cfg, bytes.NewReader([]byte(`{"architecture":"arm64","os":"linux"}`))); err != nil {
		t.Fatal(err)
	}
	layer := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayer, []byte("layer"))
	if err := src.Push(ctx, layer, bytes.NewReader([]byte("layer"))); err != nil {
		t.Fatal(err)
	}
	manifestBlob, _ := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfg,
		Layers:    []ocispec.Descriptor{layer},
	})
	mdesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestBlob)
	if err := src.Push(ctx, mdesc, bytes.NewReader(manifestBlob)); err != nil {
		t.Fatal(err)
	}
	if err := src.Tag(ctx, mdesc, "latest"); err != nil {
		t.Fatal(err)
	}
	img, err := imagestore.Acquire(ctx, src, "latest", t.TempDir())
	if err != nil {
		t.Fatalf("acquire image: %v", err)
	}
	return img
}

func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "--quiet", "-b", "main")
	run(t, dir, "git", "config", "user.name", "tester")
	run(t, dir, "git", "config", "user.email", "tester@example.com")
	for name, c := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "--quiet", "-m", "initial")
	return dir
}

func assertMeta(t *testing.T, path, wantID string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var m struct {
		ID       string `json:"id"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse meta.json: %v", err)
	}
	if m.ID != wantID {
		t.Errorf("meta id = %q, want %q", m.ID, wantID)
	}
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
