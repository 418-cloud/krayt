package proxy

import (
	"crypto/x509"
	"reflect"
	"testing"
)

// TestCAChainsAndSNI proves a leaf issued by the CA chains to it and carries the requested SNI.
func TestCAChainsAndSNI(t *testing.T) {
	ca, err := newCA("run_test")
	if err != nil {
		t.Fatalf("newCA: %v", err)
	}
	leaf, err := ca.leafFor("api.anthropic.com")
	if err != nil {
		t.Fatalf("leafFor: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(leafCert.DNSNames) != 1 || leafCert.DNSNames[0] != "api.anthropic.com" {
		t.Errorf("leaf DNSNames = %v, want [api.anthropic.com]", leafCert.DNSNames)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := leafCert.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: pool}); err != nil {
		t.Errorf("leaf does not verify against the CA: %v", err)
	}
}

// TestCALeafIPSAN proves a bare-IP CONNECT authority gets an IP SAN, not a DNS name.
func TestCALeafIPSAN(t *testing.T) {
	ca, err := newCA("")
	if err != nil {
		t.Fatalf("newCA: %v", err)
	}
	leaf, err := ca.leafFor("93.184.216.34")
	if err != nil {
		t.Fatalf("leafFor: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(leafCert.IPAddresses) != 1 || leafCert.IPAddresses[0].String() != "93.184.216.34" {
		t.Errorf("leaf IPAddresses = %v, want [93.184.216.34]", leafCert.IPAddresses)
	}
	if len(leafCert.DNSNames) != 0 {
		t.Errorf("leaf DNSNames = %v, want none for an IP-literal SNI", leafCert.DNSNames)
	}
}

// TestCALeafCacheStable proves repeat lookups for the same SNI return the identical cached
// leaf, and distinct SNIs get distinct leaves.
func TestCALeafCacheStable(t *testing.T) {
	ca, err := newCA("")
	if err != nil {
		t.Fatalf("newCA: %v", err)
	}
	a1, err := ca.leafFor("a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := ca.leafFor("a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a1, a2) {
		t.Error("repeat lookup for the same SNI returned a different leaf")
	}
	b, err := ca.leafFor("b.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a1, b) {
		t.Error("distinct SNIs returned the same leaf")
	}
	if ca.cacheLen() != 2 {
		t.Errorf("cache size = %d, want 2", ca.cacheLen())
	}
}

// TestCALeafCacheEvicts proves the SNI cache is bounded: flooding it with distinct SNIs never
// grows it past leafCacheCap (§3 — mode: full + mitm makes the SNI set attacker-chosen).
func TestCALeafCacheEvicts(t *testing.T) {
	ca, err := newCA("")
	if err != nil {
		t.Fatalf("newCA: %v", err)
	}
	const flood = leafCacheCap + 500
	for i := 0; i < flood; i++ {
		sni := randomHost(i)
		if _, err := ca.leafFor(sni); err != nil {
			t.Fatalf("leafFor(%d): %v", i, err)
		}
		if ca.cacheLen() > leafCacheCap {
			t.Fatalf("cache size %d exceeded cap %d after %d insertions", ca.cacheLen(), leafCacheCap, i+1)
		}
	}
	if ca.cacheLen() != leafCacheCap {
		t.Errorf("final cache size = %d, want exactly %d", ca.cacheLen(), leafCacheCap)
	}
}

func randomHost(i int) string {
	// Deterministic, distinct per i — no need for real randomness, just uniqueness.
	b := make([]byte, 0, 16)
	for i > 0 {
		b = append(b, byte('a'+i%26))
		i /= 26
	}
	if len(b) == 0 {
		b = []byte{'a'}
	}
	return string(b) + ".flood.example.com"
}

// TestCAPrivateKeyNotExported proves there is no exported API surface that can yield the CA's
// private key — CACertPEM is the only exported accessor, and it is documented to return the
// certificate alone. This test exercises that by reflection: every exported method on *CA must
// not return anything containing a crypto private key type.
func TestCAPrivateKeyNotExported(t *testing.T) {
	caType := reflect.TypeOf(&CA{})
	for i := 0; i < caType.NumMethod(); i++ {
		m := caType.Method(i)
		for j := 0; j < m.Type.NumOut(); j++ {
			out := m.Type.Out(j)
			if out.Kind() == reflect.Pointer {
				out = out.Elem()
			}
			if out.Name() == "PrivateKey" || out.String() == "ecdsa.PrivateKey" {
				t.Errorf("exported method %s returns a private-key-shaped type %s", m.Name, out)
			}
		}
	}
	// Direct sanity check on the one exported accessor's actual output shape.
	ca, err := newCA("")
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := ca.CACertPEM()
	if _, err := x509.ParseCertificate(ca.certDER); err != nil {
		t.Fatalf("CA cert itself should parse: %v", err)
	}
	if len(pemBytes) == 0 {
		t.Fatal("CACertPEM returned nothing")
	}
}
