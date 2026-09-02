package ask

import (
	"errors"
	"testing"
)

// TestParseDialAddr pins dial-ask-channel-over-vsock.md decision 2: a bare path is unix,
// "vsock://cid:port" is vsock, and a malformed vsock:// value is ErrMalformedSocket — a usage
// error, not a silent fallback that would masquerade as "no human is available" (§6.13).
func TestParseDialAddr(t *testing.T) {
	t.Run("bare path is unix", func(t *testing.T) {
		addr, err := parseDialAddr("/run/krayt/ask.sock")
		if err != nil {
			t.Fatalf("parseDialAddr: %v", err)
		}
		if !addr.unix || addr.path != "/run/krayt/ask.sock" {
			t.Errorf("addr = %+v, want unix path", addr)
		}
	})

	t.Run("vsock URL", func(t *testing.T) {
		addr, err := parseDialAddr("vsock://2:5000")
		if err != nil {
			t.Fatalf("parseDialAddr: %v", err)
		}
		if addr.unix || addr.cid != 2 || addr.port != 5000 {
			t.Errorf("addr = %+v, want {cid:2 port:5000}", addr)
		}
	})

	for _, malformed := range []string{
		"vsock://",
		"vsock://2",
		"vsock://2:",
		"vsock://notacid:5000",
		"vsock://2:notaport",
		"vsock://2:5000:extra",
	} {
		t.Run("malformed "+malformed, func(t *testing.T) {
			_, err := parseDialAddr(malformed)
			if err == nil {
				t.Fatalf("parseDialAddr(%q) succeeded, want ErrMalformedSocket", malformed)
			}
			if !errors.Is(err, ErrMalformedSocket) {
				t.Errorf("parseDialAddr(%q) error = %v, want wrapping ErrMalformedSocket", malformed, err)
			}
		})
	}
}
