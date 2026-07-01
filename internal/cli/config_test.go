package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestConfigPrecedence checks defaults → file → flags: the file supplies values, an explicit
// flag overrides the file, and unset flags fall back to the file/defaults (§8.3).
func TestConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "krayt.yaml")
	content := "image: file-image:1\n" +
		"task: ./file-task.md\n" +
		"resources:\n  cpus: 7\n  memory: 8GiB\n  timeout: 45m\n" +
		"network:\n  mode: full\n" +
		"env:\n  FOO: bar\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var f runFlags
	cmd := &cobra.Command{Use: "run", RunE: func(*cobra.Command, []string) error { return nil }}
	bindRunFlags(cmd, &f)
	// image comes from the flag (overrides file); task + resources come from the file.
	if err := cmd.ParseFlags([]string{"--config", cfgPath, "--image", "flag-image:2"}); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig(cmd, &f); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}

	if f.image != "flag-image:2" {
		t.Errorf("image = %q, want the flag value (flags win)", f.image)
	}
	if f.taskFile != "./file-task.md" {
		t.Errorf("task = %q, want the file value", f.taskFile)
	}
	if f.cpus != 7 {
		t.Errorf("cpus = %d, want 7 from file", f.cpus)
	}
	if f.memory != 8192 {
		t.Errorf("memory = %d MiB, want 8192 (8GiB) from file", f.memory)
	}
	if f.timeout != 45*time.Minute {
		t.Errorf("timeout = %s, want 45m from file", f.timeout)
	}
	if f.netMode != "full" {
		t.Errorf("net = %q, want full from file", f.netMode)
	}
	if f.env["FOO"] != "bar" {
		t.Errorf("env = %v, want FOO=bar from file", f.env)
	}
}

// TestConfigFlagWinsOverFile confirms an explicit flag beats the file value for the same key.
func TestConfigFlagWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "krayt.yaml")
	if err := os.WriteFile(cfgPath, []byte("resources:\n  cpus: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var f runFlags
	cmd := &cobra.Command{Use: "run", RunE: func(*cobra.Command, []string) error { return nil }}
	bindRunFlags(cmd, &f)
	if err := cmd.ParseFlags([]string{"--config", cfgPath, "--cpus", "9"}); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig(cmd, &f); err != nil {
		t.Fatal(err)
	}
	if f.cpus != 9 {
		t.Errorf("cpus = %d, want 9 (flag overrides file)", f.cpus)
	}
}
