//go:build !linux

package askclient

import (
	"errors"
	"testing"
)

// TestDialVsockUnsupportedOffLinux pins dial_vsock_other.go's contract: on a platform where the
// vsock:// transport has no meaning, dialVsock fails with an error wrapping ErrVsockUnsupported
// (not a bare error), so a caller can tell "this platform can't dial vsock" apart from "the
// bridge is unreachable" and fail loudly instead of falling back to the no-answer sentinel
// (§6.13).
func TestDialVsockUnsupportedOffLinux(t *testing.T) {
	_, err := dialVsock(2, 5000)
	if err == nil {
		t.Fatal("dialVsock succeeded, want ErrVsockUnsupported")
	}
	if !errors.Is(err, ErrVsockUnsupported) {
		t.Errorf("dialVsock error = %v, want wrapping ErrVsockUnsupported", err)
	}
}
