package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/provider/fake"
	"github.com/418-cloud/krayt/internal/proxy"
	"github.com/418-cloud/krayt/internal/secrets"
	"github.com/418-cloud/krayt/internal/task"
)

// TestSpawnEgressProxyRealChildProcess spawns the REAL `krayt __egress-proxy` child — via this
// package's TestMain re-exec-self trick (climit_test.go's runEgressHelper) — with a genuine
// fd-3 unix listener from the fake provider, and drives it exactly as krayt-vsock-forward
// would: dial the socket and send proxy-style HTTP requests. This is what proves fd-passing,
// argv (--mode/--allow), and the real child process's allow/deny decision all work end to end;
// it deliberately does not fake it with an in-process proxy.Serve stand-in
// (move-egress-proxy-to-host.md §Tests).
//
// It cannot assert a genuinely reachable upstream: this task's own hard SSRF block (§2)
// refuses loopback/private ranges in EVERY mode, and any listener this test process can stand
// up is necessarily loopback (httptest.Server) or a private CI-container address — there is no
// reachable address left to prove real internet connectivity against, offline, and this is an
// "Offline (required)" test that must not depend on live network. What IS provable, and is
// exactly what distinguishes the two decisions the proxy makes, is response *shape*: an
// ALLOWED host gets PAST the L7 allowlist check and is refused only by the SSRF guard (a
// distinct 403 body, "blocked address range") — never the L7 "blocked by the network policy"
// body a DENIED host gets immediately, before any dial is even attempted. Real (not simulated)
// upstream reachability is exercised on hardware by hack/netprobe (HUMAN_TODO.md).
func TestSpawnEgressProxyRealChildProcess(t *testing.T) {
	if os.Getenv(EgressProxyBinEnv) == "" {
		t.Skip("EgressProxyBinEnv not set — this package's TestMain (climit_test.go) sets it; run via `go test`")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()
	allowedHost := srv.Listener.Addr().(*net.TCPAddr).IP.String() // loopback — deliberately SSRF-guard territory, see above

	ctx := context.Background()
	p := fake.New()
	vm, err := p.Create(ctx, provider.VMSpec{ID: "run_egress_child"})
	if err != nil {
		t.Fatalf("fake provider Create: %v", err)
	}
	defer func() { _ = vm.Destroy(ctx) }()

	lis, err := vm.ListenEgress(ctx, provider.EgressPort)
	if err != nil {
		t.Fatalf("ListenEgress: %v", err)
	}
	sockPath := lis.Addr().String()

	runDir := t.TempDir()
	np := task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{allowedHost}}
	ep, err := spawnEgressProxy(ctx, lis, np, "run_egress_child", runDir, "")
	if err != nil {
		t.Fatalf("spawnEgressProxy: %v", err)
	}
	defer ep.stop()

	get := func(t *testing.T, host string) (int, string) {
		t.Helper()
		proxyURL, _ := url.Parse("http://proxy.invalid:0")
		client := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL), // makes the client send an absolute-URI proxy request
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath) // stand in for krayt-vsock-forward
				},
			},
		}
		resp, err := client.Get("http://" + host + "/")
		if err != nil {
			t.Fatalf("GET %s via egress proxy child: %v", host, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	t.Run("allowed host passes L7, refused only by the SSRF guard", func(t *testing.T) {
		status, body := get(t, allowedHost)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
		if !strings.Contains(body, "blocked address range") {
			t.Errorf("body = %q, want the SSRF-guard message (proves it passed the L7 allow check)", body)
		}
	})

	t.Run("non-allowlisted host is refused at L7, before any dial", func(t *testing.T) {
		status, body := get(t, "evil.example.invalid")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
		if !strings.Contains(body, "blocked by the network policy") {
			t.Errorf("body = %q, want the L7-policy message", body)
		}
	})
}

// TestSpawnEgressProxyTeardown proves stop() actually ends the child process (not just
// disconnects) and that the run's socket-cleanup unlink happens exactly once, via the
// provider's own Destroy — not a double-unlink race between this package and the provider.
func TestSpawnEgressProxyTeardown(t *testing.T) {
	if os.Getenv(EgressProxyBinEnv) == "" {
		t.Skip("EgressProxyBinEnv not set — this package's TestMain (climit_test.go) sets it; run via `go test`")
	}
	ctx := context.Background()
	p := fake.New()
	vm, err := p.Create(ctx, provider.VMSpec{ID: "run_egress_teardown"})
	if err != nil {
		t.Fatalf("fake provider Create: %v", err)
	}
	lis, err := vm.ListenEgress(ctx, provider.EgressPort)
	if err != nil {
		t.Fatalf("ListenEgress: %v", err)
	}

	runDir := t.TempDir()
	ep, err := spawnEgressProxy(ctx, lis, task.NetworkPolicy{Mode: task.NetworkNone}, "run_egress_teardown", runDir, "")
	if err != nil {
		t.Fatalf("spawnEgressProxy: %v", err)
	}
	pid := ep.cmd.Process.Pid

	ep.stop()
	select {
	case <-ep.waited:
	default:
		t.Fatal("stop() returned before the child process exited")
	}
	// The process is gone: signaling it now must fail (ESRCH), not succeed. Signal(0) does no
	// actual signaling — it only probes whether the pid still names a live, signalable process.
	if proc, err := os.FindProcess(pid); err == nil {
		if err := proc.Signal(syscall.Signal(0)); err == nil {
			t.Errorf("pid %d still appears alive after stop()", pid)
		}
	}

	// Destroying the VM must not error even though the egress socket file may already be gone
	// (stop() doesn't unlink it — the provider's per-run socket dir owns that, §4) or already
	// unlinked by an earlier Destroy — either way this must be a clean, single teardown.
	if err := vm.Destroy(ctx); err != nil {
		t.Errorf("vm.Destroy after egress proxy teardown: %v", err)
	}
}

// TestSpawnEgressProxySecretNeverInArgvEnvOrOutput proves a secret value resolved for
// network.inject[].set — delivered to the child on stdin (§2b of
// add-tls-mitm-credential-injection.md) — never appears in the REAL child process's argv or
// environ (read from /proc, not just the exec.Cmd we constructed) or its full captured
// stdout/stderr, matching the "Child config" test requirement verbatim.
func TestSpawnEgressProxySecretNeverInArgvEnvOrOutput(t *testing.T) {
	if os.Getenv(EgressProxyBinEnv) == "" {
		t.Skip("EgressProxyBinEnv not set — this package's TestMain (climit_test.go) sets it; run via `go test`")
	}
	const secret = "sk-ant-PLANTED-0123456789-do-not-leak"
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	p := fake.New()
	vm, err := p.Create(ctx, provider.VMSpec{ID: "run_secret_argv"})
	if err != nil {
		t.Fatalf("fake provider Create: %v", err)
	}
	defer func() { _ = vm.Destroy(ctx) }()
	lis, err := vm.ListenEgress(ctx, provider.EgressPort)
	if err != nil {
		t.Fatalf("ListenEgress: %v", err)
	}

	np := task.NetworkPolicy{
		Mode: task.NetworkAllowlist, Allow: []string{"api.example.com"}, MITM: true,
		Inject: []task.InjectRule{{
			Host: "api.example.com", Strip: []string{"x-api-key"},
			Set: map[string]string{"x-api-key": "ANTHROPIC_API_KEY"},
		}},
	}
	runDir := t.TempDir()
	ep, err := spawnEgressProxy(ctx, lis, np, "run_secret_argv", runDir, secretsFile)
	if err != nil {
		t.Fatalf("spawnEgressProxy: %v", err)
	}
	pid := ep.cmd.Process.Pid

	// argv, as this process actually constructed it for exec — the real OS-level argv can only
	// be a subset/rearrangement of this, so if it's clean here it's clean on the wire.
	for _, a := range ep.cmd.Args {
		if strings.Contains(a, secret) {
			t.Errorf("secret leaked into argv: %v", ep.cmd.Args)
		}
	}
	// env, as constructed — nil means full-inherit of THIS test process's own environment, which
	// never carries the secret either (it only ever lives in the secrets file + stdin payload).
	for _, e := range ep.cmd.Env {
		if strings.Contains(e, secret) {
			t.Errorf("secret leaked into env: %v", e)
		}
	}
	// Stronger, real-process assertions on Linux: read the actual argv/environ of the live child
	// from /proc, not just what this process intended to pass.
	if runtime.GOOS == "linux" {
		pidStr := strconv.Itoa(pid)
		if b, err := os.ReadFile("/proc/" + pidStr + "/cmdline"); err == nil && strings.Contains(string(b), secret) {
			t.Errorf("secret leaked into the real child's /proc cmdline")
		}
		if b, err := os.ReadFile("/proc/" + pidStr + "/environ"); err == nil && strings.Contains(string(b), secret) {
			t.Errorf("secret leaked into the real child's /proc environ")
		}
	}

	ep.stop()
	if strings.Contains(string(ep.out.Bytes()), secret) {
		t.Errorf("secret leaked into the child's full stdout/stderr output: %q", ep.out.Bytes())
	}
}

// TestEgressProxyWriteLogRedactsSecrets exercises writeLog — the REAL function that persists
// proxy.log (§9) — directly rather than through a live network flow: making the actual child
// process leak a secret-bearing query string deterministically, offline, without a reachable
// upstream is not practical (and this task's own hard SSRF block, §2, means no address this
// sandbox can stand up is even dialable by the real proxy — see
// TestSpawnEgressProxyRealChildProcess's doc comment for the same constraint). syncBuffer is a
// generic io.Writer sink regardless of what fills it, so seeding it with the shape of a real
// leaked log line — the case §9 calls out by name, a plain-HTTP forward whose URL carries a
// token in the query string — and asserting on writeLog's actual output is a direct, faithful
// test of the real redaction mechanism, not a mock of it.
func TestEgressProxyWriteLogRedactsSecrets(t *testing.T) {
	runDir := t.TempDir()
	secret := "sk-ant-supersecret-0123456789"
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ep := &egressProxy{runDir: runDir, secretsPath: secretsFile, out: &syncBuffer{}}
	denialReason := "upstream dial failed: dial tcp: lookup evil.invalid: no such host"
	_, _ = ep.out.Write([]byte(
		"krayt-egress-proxy: CONNECT evil.invalid:443: " + denialReason + "\n" +
			"krayt-egress-proxy: GET api.example.com: upstream request failed: " +
			"dial tcp 203.0.113.5:443: i/o timeout (url=http://api.example.com/v1?token=" + secret + ")\n",
	))

	ep.writeLog()

	b, err := os.ReadFile(ProxyLogPath(runDir))
	if err != nil {
		t.Fatalf("read proxy.log: %v", err)
	}
	if !strings.Contains(string(b), denialReason) {
		t.Errorf("proxy.log missing the denial reason for a blocked host; got %q", b)
	}
	if strings.Contains(string(b), secret) {
		t.Errorf("proxy.log contains the raw secret value; got %q", b)
	}
	if !strings.Contains(string(b), secrets.RedactionMarker) {
		t.Errorf("proxy.log does not show the redaction marker in place of the secret; got %q", b)
	}
}

// TestBuildEgressStdinConfigAppliesSetPrefix covers the host side of the shape-translation wire
// format (task.InjectRule.SetPrefix): the auth SCHEME is folded into the resolved value here, so the
// proxy child receives one exact string to set and never learns what a scheme is.
func TestBuildEgressStdinConfigAppliesSetPrefix(t *testing.T) {
	secret := "sk-ant-oat01-token-value"
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("CLAUDE_CODE_OAUTH_TOKEN="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	np := task.NetworkPolicy{
		Mode: task.NetworkAllowlist, Allow: []string{"api.example.com"}, MITM: true,
		Inject: []task.InjectRule{{
			Host:      "api.example.com",
			Strip:     []string{"x-api-key", "authorization"},
			Set:       map[string]string{"authorization": "CLAUDE_CODE_OAUTH_TOKEN"},
			SetPrefix: map[string]string{"authorization": "Bearer "},
		}},
	}

	b, err := buildEgressStdinConfig(np, secretsFile)
	if err != nil {
		t.Fatalf("buildEgressStdinConfig: %v", err)
	}
	var cfg proxy.StdinConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.Inject) != 1 {
		t.Fatalf("want one rule, got %+v", cfg.Inject)
	}
	if got, want := cfg.Inject[0].Set["authorization"], "Bearer "+secret; got != want {
		t.Errorf("resolved authorization = %q, want %q", got, want)
	}
}

// TestBuildEgressStdinConfigPrefixDoesNotMaskAnEmptySecret guards the fail-closed path: prefixing an
// empty resolved value would hand the proxy a plausible-looking "Bearer " and send the request
// upstream unauthenticated, defeating internal/proxy's empty-value check (§7). The value must stay
// empty so that check still fires.
func TestBuildEgressStdinConfigPrefixDoesNotMaskAnEmptySecret(t *testing.T) {
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("CLAUDE_CODE_OAUTH_TOKEN=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	np := task.NetworkPolicy{
		Mode: task.NetworkAllowlist, Allow: []string{"api.example.com"}, MITM: true,
		Inject: []task.InjectRule{{
			Host:      "api.example.com",
			Set:       map[string]string{"authorization": "CLAUDE_CODE_OAUTH_TOKEN"},
			SetPrefix: map[string]string{"authorization": "Bearer "},
		}},
	}

	b, err := buildEgressStdinConfig(np, secretsFile)
	if err != nil {
		t.Fatalf("buildEgressStdinConfig: %v", err)
	}
	var cfg proxy.StdinConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cfg.Inject[0].Set["authorization"]; got != "" {
		t.Errorf("resolved authorization = %q, want empty so the proxy's fail-closed check fires", got)
	}
}
