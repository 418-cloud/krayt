//go:build linux

package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestApplyCACertNoop proves an empty CA cert (network.mitm: false) leaves env untouched and
// writes no file — the mitm:false byte-identical guarantee's guest-side half.
func TestApplyCACertNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "ca.crt")
	env := map[string]string{"HTTP_PROXY": "http://127.0.0.1:3128"}
	if err := applyCACert(nil, path, env); err != nil {
		t.Fatalf("applyCACert: %v", err)
	}
	if len(env) != 1 {
		t.Errorf("env gained keys for an empty CA cert: %v", env)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("applyCACert wrote a file for an empty CA cert: %v", err)
	}
}

// TestApplyCACertWritesFileAndEnv proves a non-empty CA cert is written to the contract path
// (world-readable, since it's public) and the KRAYT_CA_CERT contract var plus the three
// best-effort trust-store vars are all set to it.
func TestApplyCACertWritesFileAndEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "ca.crt")
	cert := []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n")
	env := map[string]string{}
	if err := applyCACert(cert, path, env); err != nil {
		t.Fatalf("applyCACert: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written cert: %v", err)
	}
	if string(got) != string(cert) {
		t.Errorf("written cert = %q, want %q", got, cert)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("cert file mode = %v, want 0644 (public)", fi.Mode().Perm())
	}
	for _, key := range []string{"KRAYT_CA_CERT", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"} {
		if env[key] != path {
			t.Errorf("env[%s] = %q, want %q", key, env[key], path)
		}
	}
}
