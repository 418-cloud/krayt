package task_test

import (
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/task"
)

func TestNormalizeCapabilities(t *testing.T) {
	// Lowercase + missing CAP_ prefix are normalized; the result is sorted + de-duplicated.
	got, err := task.NormalizeCapabilities([]string{"net_bind_service", "CAP_CHOWN", "chown"})
	if err != nil {
		t.Fatalf("NormalizeCapabilities: %v", err)
	}
	want := []string{"CAP_CHOWN", "CAP_NET_BIND_SERVICE"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("normalized = %v, want %v", got, want)
	}

	// Empty / nil input keeps the drop-all default (nil, not an error).
	if got, err := task.NormalizeCapabilities(nil); err != nil || got != nil {
		t.Errorf("NormalizeCapabilities(nil) = %v,%v want nil,nil", got, err)
	}
}

func TestNormalizeCapabilitiesRejectsUnknown(t *testing.T) {
	if _, err := task.NormalizeCapabilities([]string{"CAP_NOT_A_REAL_CAP"}); err == nil {
		t.Error("expected an error for an unknown capability name")
	}
	// A typo that isn't a real cap must fail, not silently grant nothing.
	if _, err := task.NormalizeCapabilities([]string{"net_bnid_service"}); err == nil {
		t.Error("expected an error for a typo'd capability name")
	}
}

func TestNormalizeCapabilitiesRejectsDenylisted(t *testing.T) {
	// Every denylisted cap must be rejected even when explicitly requested, in any casing/prefix
	// form — these re-open the egress bypass or are broad escape primitives (§10).
	for _, c := range []string{
		"CAP_SETUID", "setgid", "cap_setpcap", "CAP_SYS_ADMIN",
		"net_admin", "CAP_NET_RAW", "CAP_DAC_READ_SEARCH", "cap_bpf", "CAP_SYS_PTRACE",
	} {
		if _, err := task.NormalizeCapabilities([]string{c}); err == nil {
			t.Errorf("capability %q should be rejected by the denylist", c)
		}
	}
}

func TestParseSeccompMode(t *testing.T) {
	for _, s := range []string{"", "default", "unconfined"} {
		if m, err := task.ParseSeccompMode(s); err != nil || string(m) != s {
			t.Errorf("ParseSeccompMode(%q) = %q,%v", s, m, err)
		}
	}
	if _, err := task.ParseSeccompMode("permissive"); err == nil {
		t.Error("ParseSeccompMode should reject an unknown mode")
	}
}
