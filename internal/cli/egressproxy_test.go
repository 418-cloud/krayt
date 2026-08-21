package cli

import (
	"testing"

	"github.com/418-cloud/krayt/internal/proxy"
)

// TestEnvEnabled pins the one rule that matters for KRAYT_PROXY_LOG_REQUESTS: only an explicit
// affirmative turns the request-observation log on, so `=0` or `=false` never enables it by
// accident — a mistake that would quietly start persisting every host and path a run visited.
func TestEnvEnabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{" yes ", true},
		{"on", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"maybe", false},
	}
	for _, tc := range tests {
		t.Setenv(proxy.LogRequestsEnv, tc.value)
		if got := envEnabled(proxy.LogRequestsEnv); got != tc.want {
			t.Errorf("envEnabled(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
	t.Run("unset", func(t *testing.T) {
		if envEnabled("KRAYT_DEFINITELY_NOT_SET_ANYWHERE") {
			t.Error("envEnabled of an unset variable = true, want false")
		}
	})
}
