//go:build darwin

package cli

import (
	"errors"
	"strings"
	"testing"
)

// These are smoke tests, not fixed-value tests: this machine has a real vm_stat and a real
// filesystem, so we can only assert the measurement functions return plausible values, not
// exact ones.

func TestFreeMemoryMiB(t *testing.T) {
	got, err := freeMemoryMiB()
	if err != nil {
		t.Fatalf("freeMemoryMiB: %v", err)
	}
	if got == 0 {
		t.Errorf("freeMemoryMiB() = 0, want > 0")
	}
}

func TestFreeDiskGiBAt(t *testing.T) {
	t.Run("real temp dir", func(t *testing.T) {
		dir := t.TempDir()
		got, err := freeDiskGiBAt(func() (string, error) { return dir, nil })
		if err != nil {
			t.Fatalf("freeDiskGiBAt: %v", err)
		}
		if got == 0 {
			t.Errorf("freeDiskGiBAt() = 0, want > 0")
		}
	})

	t.Run("dirFn error propagates", func(t *testing.T) {
		want := errors.New("boom")
		_, err := freeDiskGiBAt(func() (string, error) { return "", want })
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("freeDiskGiBAt() err = %v, want it to wrap %v", err, want)
		}
	})
}
