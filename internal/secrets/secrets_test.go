package secrets_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/418-cloud/krayt/internal/secrets"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	content := "# a comment\n" +
		"\n" +
		"ANTHROPIC_API_KEY=sk-ant-123\n" +
		"export TOKEN=\"quoted value\"\n" +
		"  SPACED  =  trimmed  \n" +
		"SINGLE='single quoted'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := secrets.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-123",
		"TOKEN":             "quoted value",
		"SPACED":            "trimmed",
		"SINGLE":            "single quoted",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestLoadRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(path, []byte("NOEQUALS\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Load(path); err == nil {
		t.Fatal("expected error for a line without '='")
	}
}

func TestRedactor(t *testing.T) {
	r := secrets.NewRedactor([]string{"sk-ant-123", "short", "", "sk-ant-123-extended"})
	// Longest-first ordering: the extended value must be redacted as a whole, not leave a
	// dangling suffix.
	in := []byte("using sk-ant-123-extended and sk-ant-123 plus short here")
	out := r.Redact(in)
	if bytes.Contains(out, []byte("sk-ant-123")) {
		t.Errorf("secret value leaked through redaction: %s", out)
	}
	if !bytes.Contains(out, []byte(secrets.RedactionMarker)) {
		t.Errorf("no redaction marker in %s", out)
	}
	if bytes.Contains(out, []byte("short")) {
		t.Errorf("short secret not redacted: %s", out)
	}
}

func TestRedactorEmpty(t *testing.T) {
	r := secrets.NewRedactor(nil)
	in := []byte("nothing to redact")
	if got := r.Redact(in); !bytes.Equal(got, in) {
		t.Errorf("empty redactor changed input: %s", got)
	}
}

func TestScanKeys(t *testing.T) {
	vals := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-123",
		"TOKEN":             "tok-xyz",
		"UNUSED":            "never-present",
		"EMPTY":             "", // ignored: an empty value would match everywhere
	}

	// A full value present in the buffer reports exactly its key; other keys stay silent.
	got := secrets.ScanKeys(vals, []byte("config: key=sk-ant-123 here"))
	if len(got) != 1 || got[0] != "ANTHROPIC_API_KEY" {
		t.Fatalf("ScanKeys = %v, want [ANTHROPIC_API_KEY]", got)
	}

	// Two values present → both keys, sorted for determinism.
	got = secrets.ScanKeys(vals, []byte("tok-xyz and sk-ant-123"))
	if len(got) != 2 || got[0] != "ANTHROPIC_API_KEY" || got[1] != "TOKEN" {
		t.Fatalf("ScanKeys = %v, want [ANTHROPIC_API_KEY TOKEN]", got)
	}

	// No value present → no keys; the empty-valued key never matches.
	if got := secrets.ScanKeys(vals, []byte("nothing sensitive here")); got != nil {
		t.Fatalf("ScanKeys = %v, want nil", got)
	}

	// Documented limitation: a value that never appears whole is not detected (mirrors Redact).
	if got := secrets.ScanKeys(vals, []byte("sk-ant-\n123 split across a boundary")); got != nil {
		t.Fatalf("ScanKeys on a split value = %v, want nil (whole-value scan only)", got)
	}
}
