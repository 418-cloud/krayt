package orchestrator

import (
	"strings"
	"testing"
)

func TestValidateSandboxUser(t *testing.T) {
	cases := []struct {
		user    string
		wantErr string // "" means accepted
	}{
		{user: "agent"},
		{user: "node"},
		{user: "1000"},
		{user: "1000:1000"},
		{user: "agent:agent"},
		{user: "1000:0"}, // non-root user in the root group is still a non-root uid
		{user: "", wantErr: "sets no USER"},
		{user: "  ", wantErr: "sets no USER"},
		{user: ":1000", wantErr: "sets no USER"},
		{user: "root", wantErr: "which is root"},
		{user: "root:root", wantErr: "which is root"},
		{user: "0", wantErr: "which is root"},
		{user: "0:0", wantErr: "which is root"},
		{user: "00", wantErr: "which is root"},
		{user: "0:1000", wantErr: "which is root"},
	}
	for _, tc := range cases {
		t.Run(tc.user, func(t *testing.T) {
			err := validateSandboxUser("img:1", tc.user)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateSandboxUser(%q) = %v, want accepted", tc.user, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validateSandboxUser(%q) accepted, want an error containing %q", tc.user, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("validateSandboxUser(%q) = %v, want it to contain %q", tc.user, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "img:1") {
				t.Errorf("error %q does not name the image", err)
			}
		})
	}
}

func TestEffectiveSandboxUserFallsBackForLegacyRecords(t *testing.T) {
	if got := (RunRecord{}).EffectiveSandboxUser(); got != "agent" {
		t.Errorf("legacy record EffectiveSandboxUser = %q, want agent", got)
	}
	if got := (RunRecord{SandboxUser: "node"}).EffectiveSandboxUser(); got != "node" {
		t.Errorf("EffectiveSandboxUser = %q, want node", got)
	}
}
