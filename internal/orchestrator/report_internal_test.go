package orchestrator

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
	if err := writeReport(dir, rec, "agent notes\x1b[0m"); err != nil {
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
