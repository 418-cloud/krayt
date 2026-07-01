package task_test

import (
	"testing"

	"github.com/418-cloud/krayt/internal/task"
)

func TestParseQuestionMode(t *testing.T) {
	for _, s := range []string{"fail", "wait"} {
		if m, err := task.ParseQuestionMode(s); err != nil || string(m) != s {
			t.Errorf("ParseQuestionMode(%q) = %q,%v", s, m, err)
		}
	}
	if _, err := task.ParseQuestionMode("maybe"); err == nil {
		t.Error("ParseQuestionMode should reject an unknown mode")
	}
}

func TestParseQuestionTimeoutAction(t *testing.T) {
	for _, s := range []string{"sentinel", "abort"} {
		if a, err := task.ParseQuestionTimeoutAction(s); err != nil || string(a) != s {
			t.Errorf("ParseQuestionTimeoutAction(%q) = %q,%v", s, a, err)
		}
	}
	if _, err := task.ParseQuestionTimeoutAction("panic"); err == nil {
		t.Error("ParseQuestionTimeoutAction should reject an unknown action")
	}
}

func TestParseNetworkMode(t *testing.T) {
	for _, s := range []string{"allowlist", "full", "none"} {
		if m, err := task.ParseNetworkMode(s); err != nil || string(m) != s {
			t.Errorf("ParseNetworkMode(%q) = %q,%v", s, m, err)
		}
	}
	if _, err := task.ParseNetworkMode("open"); err == nil {
		t.Error("ParseNetworkMode should reject an unknown mode")
	}
}
