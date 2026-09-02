package provider

import (
	"math"
	"testing"
)

// TestAskPortDistinctAndValid pins dial-ask-channel-over-vsock.md decision 3: the ask-bridge vsock
// port is its own constant, not merely a valid one — two channels sharing a number invite the
// wrong one being reasoned about — and it must never collide with msb's reserved 123 or the
// invalid 0/math.MaxUint32.
func TestAskPortDistinctAndValid(t *testing.T) {
	for _, bad := range []uint32{0, 123, math.MaxUint32} {
		if AskPort == bad {
			t.Errorf("AskPort = %d, must not equal reserved/invalid value %d", AskPort, bad)
		}
	}
	if AskPort == ControlPort {
		t.Errorf("AskPort = %d collides with ControlPort", AskPort)
	}
	if AskPort == EgressPort {
		t.Errorf("AskPort = %d collides with EgressPort", AskPort)
	}
}
