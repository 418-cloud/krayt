package proxy

// Package-internal: the ephemeral per-run CA and its bounded SNI-keyed leaf cache
// (add-tls-mitm-credential-injection.md §3). Stdlib crypto only (crypto/tls, crypto/x509,
// crypto/ecdsa) — no new dependency.
//
// The CA is generated once per proxy process (one process per run, §6.6) and lives in memory
// only: it is never written to host disk, and there is no exported path that can serialize its
// private key — CACertPEM returns the public certificate alone.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"
)

// leafCacheCap bounds the SNI->leaf cache. Under mode: full + mitm the set of SNIs is
// attacker-chosen (the guest picks what host to CONNECT to) and unbounded, so an uncapped map is
// a guest-triggerable host memory-growth path (§3). A simple random-eviction cap is sufficient —
// this is a performance cache, not a security boundary, so eviction policy doesn't need LRU
// precision.
const leafCacheCap = 1024

// caValidity is the ephemeral CA's certificate lifetime: generous enough to outlive any single
// run's wall-clock timeout, short enough that a CA nobody explicitly revoked doesn't linger.
// There is no persistence across runs to make a longer lifetime meaningful — a fresh CA is
// generated every time the proxy process starts (§6.6 "ephemeral per-run CA").
const caValidity = 24 * time.Hour

// leafValidity is each leaf certificate's lifetime — likewise scoped to comfortably outlive one
// run, never persisted or reused across proxy processes.
const leafValidity = 24 * time.Hour

// CA is krayt's ephemeral per-run MITM certificate authority (§3). Generated in memory at proxy
// startup, discarded at teardown, never written to disk. The zero value is not usable; use
// newCA.
type CA struct {
	cert    *x509.Certificate
	certDER []byte
	key     *ecdsa.PrivateKey // NEVER exported; CACertPEM returns only the public certificate

	runID string

	mu    sync.Mutex
	cache map[string]*tls.Certificate
	order []string // insertion order, for random-ish eviction when the cache is full
}

// newCA generates a fresh, in-memory ECDSA P-256 self-signed CA for one run (§3). runID (may be
// empty) is folded into the CN purely for operator legibility in a TLS chain viewer — it carries
// no security meaning.
func newCA(runID string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate CA key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName(runID), Organization: []string{"krayt (ephemeral, per-run)"}},
		NotBefore:             now.Add(-5 * time.Minute), // clock-skew slack
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("mitm: create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse CA certificate: %w", err)
	}
	return &CA{
		cert: cert, certDER: der, key: key, runID: runID,
		cache: make(map[string]*tls.Certificate),
	}, nil
}

func caCommonName(runID string) string {
	if runID == "" {
		return "krayt ephemeral MITM CA"
	}
	return "krayt ephemeral MITM CA (" + runID + ")"
}

// CACertPEM returns the CA's PUBLIC certificate, PEM-encoded — never the private key. This is
// the only exported way to get bytes out of a CA, by construction: there is no method that can
// return c.key or anything derived from it.
func (c *CA) CACertPEM() []byte {
	return pemEncode("CERTIFICATE", c.certDER)
}

// leafFor returns a cached or freshly-generated ECDSA P-256 leaf certificate for sni, signed by
// the CA, with DNSNames: [sni] (or an IP SAN if sni is an IP literal — the CONNECT authority can
// be either). Bounded per leafCacheCap (§3): a cache miss when full evicts one arbitrary entry
// before inserting, rather than growing without bound.
func (c *CA) leafFor(sni string) (*tls.Certificate, error) {
	c.mu.Lock()
	if leaf, ok := c.cache[sni]; ok {
		c.mu.Unlock()
		return leaf, nil
	}
	c.mu.Unlock()

	leaf, err := c.generateLeaf(sni)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.cache[sni]; ok {
		// Lost a race with a concurrent generator for the same SNI; keep whichever is already
		// cached so repeat lookups are stable (§ tests: "cache returns the same leaf for repeat SNI").
		return existing, nil
	}
	if len(c.cache) >= leafCacheCap {
		c.evictLocked()
	}
	c.cache[sni] = leaf
	c.order = append(c.order, sni)
	return leaf, nil
}

// evictLocked drops the oldest-inserted entry. Called with c.mu held.
func (c *CA) evictLocked() {
	if len(c.order) == 0 {
		return
	}
	victim := c.order[0]
	c.order = c.order[1:]
	delete(c.cache, victim)
}

// cacheLen reports the current leaf cache size (test seam).
func (c *CA) cacheLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.cache)
}

// generateLeaf creates a new leaf certificate + key for sni, signed by the CA. Not
// cache-aware — callers go through leafFor.
func (c *CA) generateLeaf(sni string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate leaf key for %q: %w", sni, err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: sni},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(sni); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{sni}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("mitm: sign leaf for %q: %w", sni, err)
	}
	return &tls.Certificate{Certificate: [][]byte{der, c.certDER}, PrivateKey: key}, nil
}

// tlsConfigFor returns a tls.Config whose GetCertificate always returns the leaf for authority,
// regardless of the ClientHello's SNI (which may be empty for a bare-IP CONNECT authority, or
// may legitimately differ from a client that mis-set it) — the CONNECT authority the allowlist
// already approved is the only value this code trusts (§4).
func (c *CA) tlsConfigFor(authority string) *tls.Config {
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c.leafFor(authority)
		},
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	}
}

func randSerial() (*big.Int, error) {
	upper := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, upper)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate serial: %w", err)
	}
	return n, nil
}

func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}
