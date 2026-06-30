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
