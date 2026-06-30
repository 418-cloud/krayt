// Package proxy is the in-guest egress allowlist proxy of §6.6: a small HTTP/HTTPS CONNECT
// forward proxy that checks each request's host against the per-task policy. It is the L7
// half of the egress control; the L3 nftables lock (firewall_linux.go) makes it
// unbypassable by dropping all egress except loopback and the proxy's own uid.
//
// The proxy logic here is OS-agnostic and unit-tested directly. The implementation is
// deliberately behind the Factory seam so it can be swapped for elazarl/goproxy or any
// other backend without touching the guest wiring (the user chose hand-rolled for v1).
package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Policy modes (§6.6). Mirrors the proto NetworkPolicy.Mode / task.NetworkMode as strings.
const (
	ModeAllowlist = "allowlist" // default: only listed hosts may be reached
	ModeFull      = "full"      // allow everything (nftables also opened; explicit opt-in)
	ModeNone      = "none"      // deny everything
)

// Policy is the per-task egress policy the proxy enforces (§6.6).
type Policy struct {
	Mode  string
	Allow []string
}

// Factory builds the forward-proxy handler for a policy. This is the swap seam: HandRolled
// is the default; a goproxy-based factory (or any other) can replace it by matching this
// signature and being passed to Serve.
type Factory func(Policy) http.Handler

// dialTimeout bounds how long the proxy waits to connect to an allowed upstream.
const dialTimeout = 30 * time.Second

// Serve runs a forward proxy built by factory (HandRolled if nil) on lis until ctx is
// canceled. It is what cmd/krayt-proxy runs as the dedicated proxyd uid (§6.6).
func Serve(ctx context.Context, lis net.Listener, p Policy, factory Factory) error {
	if factory == nil {
		factory = HandRolled
	}
	srv := &http.Server{Handler: factory(p), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: serve: %w", err)
	}
	return nil
}

// HandRolled is the default allowlist forward proxy: it tunnels CONNECT (HTTPS) and
// forwards plain HTTP, allowing a request only if its host passes the policy (§6.6).
func HandRolled(p Policy) http.Handler {
	return newHandler(p, http.DefaultTransport)
}

// newHandler builds the proxy with an injectable transport (tests pass a fake so the
// forward path needs no real network).
func newHandler(p Policy, rt http.RoundTripper) *handler {
	allow := make(map[string]bool, len(p.Allow))
	for _, a := range p.Allow {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			allow[a] = true
		}
	}
	return &handler{mode: p.Mode, allow: allow, transport: rt}
}

type handler struct {
	mode      string
	allow     map[string]bool
	transport http.RoundTripper
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	if !h.allowed(host) {
		http.Error(w, "krayt: egress to "+host+" is blocked by the network policy", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		h.connect(w, r)
		return
	}
	h.forward(w, r)
}

// allowed applies the policy to a bare host (no port).
func (h *handler) allowed(host string) bool {
	switch h.mode {
	case ModeFull:
		return true
	case ModeNone:
		return false
	default: // allowlist
		return h.allow[strings.ToLower(host)]
	}
}

// connect tunnels an HTTPS CONNECT to the (already allowed) target, copying bytes both ways.
func (h *handler) connect(w http.ResponseWriter, r *http.Request) {
	upstream, err := net.DialTimeout("tcp", r.Host, dialTimeout)
	if err != nil {
		http.Error(w, "krayt: upstream dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "krayt: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		_ = upstream.Close()
		_ = client.Close()
		return
	}
	go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
}

// forward proxies a plain-HTTP request to the (already allowed) target.
func (h *handler) forward(w http.ResponseWriter, r *http.Request) {
	r.RequestURI = "" // must be cleared before re-sending as a client request
	resp, err := h.transport.RoundTrip(r)
	if err != nil {
		http.Error(w, "krayt: upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// requestHost extracts the bare hostname (no port) a request targets. For CONNECT the
// authority is in r.Host; for a plain proxied request it is in the absolute r.URL.
func requestHost(r *http.Request) string {
	host := r.Host
	if r.Method != http.MethodConnect && r.URL != nil && r.URL.Host != "" {
		host = r.URL.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
