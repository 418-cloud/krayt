package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
)

// setupFakeMsb points sandbox.NewClient() (via sandbox.BinEnv) at this test binary re-exec'd as
// msb (fakemsb_test.go) and writes script to a fresh $HOME, returning it so a test can inspect
// fakeCallsFile afterward.
func setupFakeMsb(t *testing.T, script fakeScript) string {
	t.Helper()
	if testBinPath == "" {
		t.Fatal("testBinPath not set — TestMain did not run (are tests being invoked oddly?)")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sandbox.BinEnv, testBinPath)
	writeFakeScript(t, home, script)
	return home
}

// callsFor returns every recorded call whose first argv token is verb.
func callsFor(t *testing.T, home, verb string) []fakeCall {
	t.Helper()
	var out []fakeCall
	for _, c := range readFakeCalls(t, home) {
		if len(c.Args) > 0 && c.Args[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

func TestImageLs(t *testing.T) {
	setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"images --format json": {ExitCode: 0, Stdout: `[` +
			`{"reference":"ghcr.io/x/agent:latest","size":1073741824},` +
			`{"reference":"ghcr.io/x/other:v1","size":2048}` +
			`]`},
	}})

	out := run(t, newImageLsCmd())
	for _, want := range []string{"REF", "SIZE", "ghcr.io/x/agent:latest", "1.0GiB", "ghcr.io/x/other:v1", "2.0KiB", "2 images"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output missing %q; got:\n%s", want, out)
		}
	}
}

func TestImageLsEmpty(t *testing.T) {
	setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"images --format json": {ExitCode: 0, Stdout: `[]`},
	}})
	out := run(t, newImageLsCmd())
	if !strings.Contains(out, "0 images") {
		t.Errorf("empty ls output = %q", out)
	}
}

func TestImagePull(t *testing.T) {
	setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"pull ghcr.io/x/agent:latest": {ExitCode: 0},
	}})
	out := run(t, newImagePullCmd(), "ghcr.io/x/agent:latest")
	if !strings.Contains(out, "pulled ghcr.io/x/agent:latest") {
		t.Errorf("pull output = %q", out)
	}
}

func TestImageRm(t *testing.T) {
	home := setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"rmi": {ExitCode: 0},
	}})
	out := run(t, newImageRmCmd(), "ghcr.io/x/agent:latest")
	if !strings.Contains(out, "removed ghcr.io/x/agent:latest") {
		t.Errorf("rm output = %q", out)
	}
	calls := callsFor(t, home, "rmi")
	if len(calls) != 1 {
		t.Fatalf("want 1 rmi call, got %d", len(calls))
	}
	want := []string{"rmi", "ghcr.io/x/agent:latest"}
	if !reflect.DeepEqual(calls[0].Args, want) {
		t.Errorf("rmi args = %v, want %v", calls[0].Args, want)
	}
}

func TestImageRmForce(t *testing.T) {
	home := setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"rmi": {ExitCode: 0},
	}})
	_ = run(t, newImageRmCmd(), "--force", "ghcr.io/x/agent:latest")
	calls := callsFor(t, home, "rmi")
	want := []string{"rmi", "--force", "ghcr.io/x/agent:latest"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, want) {
		t.Errorf("rmi --force args = %v, want %v", calls, want)
	}
}

func TestImageRmError(t *testing.T) {
	setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"rmi": {ExitCode: 1, Stderr: "no such image"},
	}})
	if err := execErr(newImageRmCmd(), "ghcr.io/x/nope:latest"); err == nil {
		t.Error("rm of a nonexistent ref should error")
	}
}

func TestImageRmCompletion(t *testing.T) {
	setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"images -q": {ExitCode: 0, Stdout: "ghcr.io/x/agent:latest\nghcr.io/x/other:v1\n"},
	}})
	cmd := newImageRmCmd()
	comps, dir := cmd.ValidArgsFunction(cmd, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", dir)
	}
	want := []string{"ghcr.io/x/agent:latest", "ghcr.io/x/other:v1"}
	if !reflect.DeepEqual(comps, want) {
		t.Errorf("rm completion = %v, want %v", comps, want)
	}

	if comps, dir := cmd.ValidArgsFunction(cmd, []string{"ghcr.io/x/agent:latest"}, ""); comps != nil || dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("second-arg completion = (%v, %v), want (nil, NoFileComp)", comps, dir)
	}
}

// TestImagePruneRetention covers decision 3's full policy: an image protected by a non-terminal
// run, one protected by the age window, one used but stale (beyond the window, no active run),
// and one with no run record at all (neither protection) — the two must be removed, the two must
// survive.
func TestImagePruneRetention(t *testing.T) {
	home := setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"images --format json": {ExitCode: 0, Stdout: `[` +
			`{"reference":"img:live","size":100},` +
			`{"reference":"img:recent","size":200},` +
			`{"reference":"img:stale","size":300},` +
			`{"reference":"img:orphan","size":400}` +
			`]`},
		"rmi":   {ExitCode: 0},
		"image": {ExitCode: 0},
	}})
	repo := t.TempDir()
	now := time.Now().UTC()

	seedRunRecord(t, repo, orchestrator.RunRecord{
		ID: "run_live", State: "running", ImageRef: "img:live",
		StartedAt: now.Format(time.RFC3339),
	})
	seedRunRecord(t, repo, orchestrator.RunRecord{
		ID: "run_recent", State: "done", ImageRef: "img:recent",
		StartedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
		EndedAt:   now.Add(-1 * time.Hour).Format(time.RFC3339),
	})
	seedRunRecord(t, repo, orchestrator.RunRecord{
		ID: "run_stale", State: "done", ImageRef: "img:stale",
		StartedAt: now.Add(-74 * time.Hour).Format(time.RFC3339),
		EndedAt:   now.Add(-72 * time.Hour).Format(time.RFC3339),
	})
	// img:orphan has no run record at all.

	out := run(t, newImagePruneCmd(), "--repo", repo)

	if !strings.Contains(out, "kept img:live (in use by run_live)") {
		t.Errorf("prune output missing the in-use keep; got:\n%s", out)
	}
	if !strings.Contains(out, "kept img:recent (used 1h ago)") {
		t.Errorf("prune output missing the age-window keep; got:\n%s", out)
	}
	if !strings.Contains(out, "removed img:stale") {
		t.Errorf("prune output should remove the stale, unreferenced-by-any-active-run image; got:\n%s", out)
	}
	if !strings.Contains(out, "removed img:orphan") {
		t.Errorf("prune output should remove the never-run image (neither protection applies); got:\n%s", out)
	}

	rmiCalls := callsFor(t, home, "rmi")
	if len(rmiCalls) != 2 {
		t.Fatalf("want 2 rmi calls (stale + orphan), got %d: %v", len(rmiCalls), rmiCalls)
	}
	gotRefs := map[string]bool{}
	for _, c := range rmiCalls {
		gotRefs[c.Args[len(c.Args)-1]] = true
	}
	for _, ref := range []string{"img:stale", "img:orphan"} {
		if !gotRefs[ref] {
			t.Errorf("rmi was not called for %s", ref)
		}
	}
	if calls := callsFor(t, home, "image"); len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []string{"image", "prune"}) {
		t.Errorf("want exactly one `msb image prune` call, got %v", calls)
	}
}

func TestImagePruneOlderThanZero(t *testing.T) {
	setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"images --format json": {ExitCode: 0, Stdout: `[` +
			`{"reference":"img:live","size":100},` +
			`{"reference":"img:recent","size":200}` +
			`]`},
		"rmi":   {ExitCode: 0},
		"image": {ExitCode: 0},
	}})
	repo := t.TempDir()
	now := time.Now().UTC()
	seedRunRecord(t, repo, orchestrator.RunRecord{
		ID: "run_live", State: "running", ImageRef: "img:live",
		StartedAt: now.Format(time.RFC3339),
	})
	seedRunRecord(t, repo, orchestrator.RunRecord{
		ID: "run_recent", State: "done", ImageRef: "img:recent",
		StartedAt: now.Add(-2 * time.Minute).Format(time.RFC3339),
		EndedAt:   now.Add(-1 * time.Minute).Format(time.RFC3339),
	})

	out := run(t, newImagePruneCmd(), "--repo", repo, "--older-than", "0s")
	if !strings.Contains(out, "kept img:live") {
		t.Errorf("in-use image must survive --older-than 0s; got:\n%s", out)
	}
	if !strings.Contains(out, "removed img:recent") {
		t.Errorf("recent-but-inactive image should be pruned by --older-than 0s; got:\n%s", out)
	}
}

func TestImagePruneAll(t *testing.T) {
	home := setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"images --format json": {ExitCode: 0, Stdout: `[` +
			`{"reference":"img:live","size":100},` +
			`{"reference":"img:recent","size":200}` +
			`]`},
		"rmi":   {ExitCode: 0},
		"image": {ExitCode: 0},
	}})
	repo := t.TempDir()
	now := time.Now().UTC()
	seedRunRecord(t, repo, orchestrator.RunRecord{
		ID: "run_live", State: "running", ImageRef: "img:live",
		StartedAt: now.Format(time.RFC3339),
	})

	out := run(t, newImagePruneCmd(), "--repo", repo, "--all")
	for _, want := range []string{"removed img:live", "removed img:recent"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all output missing %q; got:\n%s", want, out)
		}
	}
	if calls := callsFor(t, home, "rmi"); len(calls) != 2 {
		t.Errorf("--all should call msb rmi for every image (even in-use), got %d calls: %v", len(calls), calls)
	}
}

func TestImagePruneDryRun(t *testing.T) {
	home := setupFakeMsb(t, fakeScript{Responses: map[string]fakeResponse{
		"images --format json": {ExitCode: 0, Stdout: `[{"reference":"img:orphan","size":100}]`},
	}})
	repo := t.TempDir()

	out := run(t, newImagePruneCmd(), "--repo", repo, "--dry-run")
	if !strings.Contains(out, "would remove img:orphan") {
		t.Errorf("--dry-run output = %q", out)
	}
	if calls := callsFor(t, home, "rmi"); len(calls) != 0 {
		t.Errorf("--dry-run must not call msb rmi; got %v", calls)
	}
	if calls := callsFor(t, home, "image"); len(calls) != 0 {
		t.Errorf("--dry-run must not call msb image prune; got %v", calls)
	}
}
