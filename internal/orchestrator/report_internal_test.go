package orchestrator

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

// TestWriteReportSanitizesFields is the regression for the report-injection review finding: no
// externally-sourced field — the network allow list (from an untrusted krayt.yaml), the image
// ref, or agent notes — may carry a raw terminal escape into report.md, which a human cats.
func TestWriteReportSanitizesFields(t *testing.T) {
	dir := t.TempDir()
	rec := RunRecord{
		ID:       "run_x",
		ImageRef: "img\x1b[31m",
		Network:  NetworkMeta{Mode: "allowlist", Allow: []string{"ok.com", "evil\x1b[2J"}},
		Safety:   []string{"path\x1b[1m: modifies a git hook"},
		State:    StateDone,
	}
	if err := writeReport(dir, rec, "agent notes\x1b[0m", digest.FromString("meta")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, reportName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.IndexByte(b, 0x1b) >= 0 {
		t.Errorf("report.md contains a raw ESC byte from an unsanitized field:\n%q", b)
	}
}

// TestWriteReportProvenanceOptional asserts the ## Provenance section is absent when no provenance
// was captured (mirroring the optional ## Safety/## Questions sections) and present, with both
// digests and the explicit consistency-check label, when it was (§8.4, decision 5).
func TestWriteReportProvenanceOptional(t *testing.T) {
	metaDigest := digest.FromString("meta.json bytes")

	// nil provenance → no section.
	dir := t.TempDir()
	if err := writeReport(dir, RunRecord{ID: "run_a", State: StateDone}, "", metaDigest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, reportName)); strings.Contains(string(b), "## Provenance") {
		t.Errorf("report.md has a Provenance section with nil rec.Provenance:\n%s", b)
	}

	// Captured provenance → section with commit line, both digests, and the consistency-check label.
	dir2 := t.TempDir()
	rec := RunRecord{
		ID:    "run_b",
		State: StateDone,
		Provenance: &ProvenanceMeta{
			HeadSHA: "abc123", BundleSHA: "def456", BundleDepth: 1, IncludeDirty: true,
			BundleDigest: "sha256:beef",
		},
	}
	if err := writeReport(dir2, rec, "", metaDigest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir2, reportName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Provenance",
		"- Commit: abc123  (bundle: def456, depth: 1, dirty: yes)",
		"- Bundle digest: sha256:beef",
		"- Metadata digest (consistency check, not a signature): " + metaDigest.String(),
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report.md missing %q; got:\n%s", want, b)
		}
	}
	// The consistency-check framing must never read as a security/tamper guarantee.
	for _, forbidden := range []string{"tamper", "signature:", "integrity guarantee", "authenticat"} {
		if strings.Contains(strings.ToLower(string(b)), forbidden) {
			t.Errorf("report.md provenance implies more than a consistency check (%q):\n%s", forbidden, b)
		}
	}
}
