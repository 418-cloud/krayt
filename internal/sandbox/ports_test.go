package sandbox

import (
	"math"
	"testing"
)

// TestAskPortDistinctAndValid pins dial-ask-channel-over-vsock.md decision 3: the ask-bridge vsock
// port is its own constant, not merely a valid one, and must never collide with msb's reserved
// 123 or the invalid 0/math.MaxUint32.
func TestAskPortDistinctAndValid(t *testing.T) {
	for _, bad := range []uint32{0, 123, math.MaxUint32} {
		if AskPort == bad {
			t.Errorf("AskPort = %d, must not equal reserved/invalid value %d", AskPort, bad)
		}
	}
}

func TestAskSocketEnvIsVsockURL(t *testing.T) {
	want := "vsock://2:1026"
	if AskSocketEnv != want {
		t.Errorf("AskSocketEnv = %q, want %q", AskSocketEnv, want)
	}
}
