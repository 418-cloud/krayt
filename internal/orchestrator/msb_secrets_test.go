package orchestrator

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPatchSecretKeysFindsMatchingKeysOnly(t *testing.T) {
	dir := t.TempDir()
	patchPath := filepath.Join(dir, "changes.patch")
	content := "diff --git a/config.py b/config.py\n" +
		"+API_KEY = \"sk-ant-the-real-value\"\n" +
		"+OTHER = \"unrelated text\"\n"
	if err := os.WriteFile(patchPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	values := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-the-real-value", // present in the patch
		"GH_TOKEN":          "ghp-not-in-the-patch",  // absent
	}
	got, err := PatchSecretKeys(patchPath, values)
	if err != nil {
		t.Fatalf("PatchSecretKeys: %v", err)
	}
	want := []string{"ANTHROPIC_API_KEY"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PatchSecretKeys = %v, want %v", got, want)
	}
}

func TestPatchSecretKeysNoMatchIsEmpty(t *testing.T) {
	dir := t.TempDir()
	patchPath := filepath.Join(dir, "changes.patch")
	if err := os.WriteFile(patchPath, []byte("diff --git a/README.md b/README.md\n+hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := PatchSecretKeys(patchPath, map[string]string{"GH_TOKEN": "ghp-never-appears"})
	if err != nil {
		t.Fatalf("PatchSecretKeys: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("PatchSecretKeys = %v, want empty", got)
	}
}

func TestPatchSecretKeysMissingFileErrors(t *testing.T) {
	_, err := PatchSecretKeys(filepath.Join(t.TempDir(), "does-not-exist.patch"), map[string]string{"K": "v"})
	if err == nil {
		t.Fatal("expected an error for a missing patch file")
	}
}

func TestPatchSecretKeysIgnoresEmptyValues(t *testing.T) {
	dir := t.TempDir()
	patchPath := filepath.Join(dir, "changes.patch")
	if err := os.WriteFile(patchPath, []byte("+anything at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A key with an empty value must never match everything (secrets.ScanKeys's own contract).
	got, err := PatchSecretKeys(patchPath, map[string]string{"EMPTY_SECRET": ""})
	if err != nil {
		t.Fatalf("PatchSecretKeys: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("PatchSecretKeys with an empty value = %v, want empty", got)
	}
}
