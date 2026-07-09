package task_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/418-cloud/krayt/internal/task"
)

func TestParseSizes(t *testing.T) {
	mib := []struct {
		in   string
		want uint64
	}{{"4GiB", 4096}, {"512MiB", 512}, {"2048", 2048}, {"1GB", 1024}, {"1.5GiB", 1536}}
	for _, c := range mib {
		got, err := task.ParseMiB(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseMiB(%q) = %d,%v want %d", c.in, got, err, c.want)
		}
	}
	gib := []struct {
		in   string
		want uint64
	}{{"20GiB", 20}, {"20480MiB", 20}, {"10", 10}, {"2048MiB", 2}}
	for _, c := range gib {
		got, err := task.ParseGiB(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseGiB(%q) = %d,%v want %d", c.in, got, err, c.want)
		}
	}
	if _, err := task.ParseMiB("4TiB"); err == nil {
		t.Error("expected error for an unknown unit")
	}
	// Fractional values that would truncate must be rejected, not silently rounded.
	if _, err := task.ParseMiB("1.5MiB"); err == nil {
		t.Error("ParseMiB(1.5MiB) should be rejected (would truncate to 1)")
	}
	if _, err := task.ParseGiB("512MiB"); err == nil {
		t.Error("ParseGiB(512MiB) should be rejected (would truncate to a 0-GiB disk)")
	}
	if _, err := task.ParseGiB("1536MiB"); err == nil {
		t.Error("ParseGiB(1536MiB) should be rejected (1.5 GiB is not a whole GiB / multiple of 1024 MiB)")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "krayt.yaml")
	content := "image: my-agent:latest\n" +
		"include_dirty: true\n" +
		"network:\n  mode: allowlist\n  allow:\n    - api.anthropic.com\n" +
		"resources:\n  cpus: 3\n  memory: 8GiB\n  timeout: 45m\n" +
		"env:\n  LOG_LEVEL: debug\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := task.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Image != "my-agent:latest" {
		t.Errorf("image = %q", c.Image)
	}
	if c.IncludeDirty == nil || !*c.IncludeDirty {
		t.Error("include_dirty should be true")
	}
	if c.Network.Mode != "allowlist" || len(c.Network.Allow) != 1 {
		t.Errorf("network = %+v", c.Network)
	}
	if c.Resources.CPUs == nil || *c.Resources.CPUs != 3 || c.Resources.Memory != "8GiB" {
		t.Errorf("resources = %+v", c.Resources)
	}
	if c.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("env = %v", c.Env)
	}
}

func TestLoadConfigContainer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "krayt.yaml")
	content := "image: my-agent:latest\n" +
		"container:\n" +
		"  capabilities:\n    - net_bind_service\n    - CAP_CHOWN\n" +
		"  seccomp: unconfined\n" +
		"  readonly_rootfs: true\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := task.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(c.Container.Capabilities) != 2 || c.Container.Seccomp != "unconfined" {
		t.Errorf("container = %+v", c.Container)
	}
	// The pointer distinguishes an explicit value from unset.
	if c.Container.ReadonlyRootfs == nil || !*c.Container.ReadonlyRootfs {
		t.Errorf("readonly_rootfs = %v, want true", c.Container.ReadonlyRootfs)
	}
}

func TestLoadConfigContainerReadonlyUnsetVsFalse(t *testing.T) {
	dir := t.TempDir()
	// Unset: the pointer stays nil so the CLI can tell "not specified" from "false".
	unsetPath := filepath.Join(dir, "unset.yaml")
	if err := os.WriteFile(unsetPath, []byte("image: a:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := task.LoadConfig(unsetPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Container.ReadonlyRootfs != nil {
		t.Errorf("unset readonly_rootfs should be nil, got %v", *c.Container.ReadonlyRootfs)
	}
	// Explicit false: the pointer is non-nil pointing at false.
	falsePath := filepath.Join(dir, "false.yaml")
	if err := os.WriteFile(falsePath, []byte("image: a:latest\ncontainer:\n  readonly_rootfs: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = task.LoadConfig(falsePath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Container.ReadonlyRootfs == nil || *c.Container.ReadonlyRootfs {
		t.Errorf("explicit false should be non-nil false, got %v", c.Container.ReadonlyRootfs)
	}
}

// TestExampleConfigParses guards the shipped configs/krayt.yaml against the config schema +
// validators (KnownFields, seccomp mode, capability allow/deny-list) so the documented example
// can't drift out of sync with the code.
func TestExampleConfigParses(t *testing.T) {
	c, err := task.LoadConfig(filepath.Join("..", "..", "configs", "krayt.yaml"))
	if err != nil {
		t.Fatalf("example config does not parse: %v", err)
	}
	if _, err := task.ParseSeccompMode(c.Container.Seccomp); err != nil {
		t.Errorf("example container.seccomp invalid: %v", err)
	}
	if _, err := task.NormalizeCapabilities(c.Container.Capabilities); err != nil {
		t.Errorf("example container.capabilities invalid: %v", err)
	}
}

func TestLoadConfigRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("imagge: typo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := task.LoadConfig(path); err == nil {
		t.Error("expected an error for an unknown/typo'd key")
	}
}
