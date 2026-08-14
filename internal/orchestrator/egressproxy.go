package orchestrator

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/418-cloud/krayt/internal/proxy"
	"github.com/418-cloud/krayt/internal/secrets"
	"github.com/418-cloud/krayt/internal/task"
)

// EgressProxyBinEnv is the swap seam move-egress-proxy-to-host.md §4 promises: set it to the
// path of a replacement egress-proxy binary (e.g. a future memory-safe reimplementation, §6.6)
// instead of re-execing the running krayt binary as `krayt __egress-proxy`. The replacement
// must honor the same contract — --mode/--allow/--dns flags in, a listener on fd 3, logs on
// stdout/stderr — which is what keeps this a real swap seam and not just a name. Exported so
// this package's own tests can point it at a re-exec'd test-binary-as-helper-process (the
// classic Go pattern, see climit_test.go's TestMain) instead of mocking spawnEgressProxy.
const EgressProxyBinEnv = "KRAYT_EGRESS_PROXY_BIN"

// egressProxyKillWait bounds how long stop() waits for the child to exit after Kill — the
// same 2-second kill/drain shape internal/guest/proxy/controller_linux.go used before this
// task moved the L7 proxy off the guest, now living host-side instead.
const egressProxyKillWait = 2 * time.Second

// egressProxyStartupWait bounds how long spawnEgressProxy waits to notice an immediate child
// exit (bad flags, a KRAYT_EGRESS_PROXY_BIN path that doesn't exist, …) before declaring the
// spawn successful. No readiness handshake is needed beyond this: the PARENT created and
// bound the fd-3 listener, so guest connections queue in the kernel accept backlog even
// before the child calls Accept — a benefit of fd-passing over handing the child a path to
// bind itself (§4).
const egressProxyStartupWait = 150 * time.Millisecond

// maxProxyLog caps how much of the egress proxy child's collected output persists to
// proxy.log, mirroring maxConsoleLog's tail-keeping shape below.
const maxProxyLog = 1 << 20 // 1 MiB

// egressProxy is one run's host-side egress allowlist proxy child process (`krayt
// __egress-proxy`, or a KRAYT_EGRESS_PROXY_BIN override) — see spawnEgressProxy.
type egressProxy struct {
	cmd    *exec.Cmd
	waited chan struct{}
	out    *syncBuffer

	runDir      string
	secretsPath string

	// caCertPEM is the child's ephemeral MITM CA public certificate (§5,
	// add-tls-mitm-credential-injection.md), reported back over fd 4 — empty when
	// network.mitm is false (the only case that's valid; spawnEgressProxy fails the run outright
	// if MITM is enabled and no valid cert arrives, since a MITM child without a trusted CA is
	// not equivalent to mitm-off). The caller pushes this into the guest's NetworkPolicy so the
	// container's TLS stack can trust MITM'd connections.
	caCertPEM []byte
}

// caHandshakeWait bounds how long spawnEgressProxy additionally waits, after confirming the
// child didn't exit immediately, to receive its CA cert over fd 4, when network.mitm is enabled.
// Cert generation + a single small write is sub-millisecond in practice; this only needs to be
// generous enough to absorb scheduling noise under load.
const caHandshakeWait = 2 * time.Second

// maxCACertPEMBytes bounds the fd-4 read for the child's CA-cert report. An ECDSA P-256
// self-signed cert PEM is a few hundred bytes; this is generous headroom, not a real limit. The
// child is the process most directly exposed to adversarial network input (§6.6.1), so this read
// is bounded and the result validated rather than trusted, the same "treat as hostile" posture
// §6 of add-tls-mitm-credential-injection.md applies to guest input.
const maxCACertPEMBytes = 8 << 10

// spawnEgressProxy starts the host-side egress allowlist/MITM proxy for one run (§4, §6, and
// §2/§4 of add-tls-mitm-credential-injection.md): it duplicates lis's fd for the child, execs it
// with that dup as fd 3 and a CA-cert-report pipe as fd 4, writes the injection/passthrough
// config (with any secrets-file key names already resolved to values) to the child's stdin and
// closes it, and captures its stdout/stderr for later redaction into proxy.log (writeLog, §9).
// The caller must have created lis via vm.ListenEgress, after Create and before vm.Start, and
// must call stop() on the returned egressProxy as part of the run's teardown.
//
// A failure here — including the child exiting within egressProxyStartupWait of Start — is a
// fail-fast run error: the run must never boot a VM whose only egress path is already dead.
func spawnEgressProxy(ctx context.Context, lis net.Listener, np task.NetworkPolicy, runID, runDir, secretsPath string) (*egressProxy, error) {
	f, err := takeListenerFD(lis)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: egress proxy listener: %w", err)
	}
	defer func() { _ = f.Close() }() // the child inherits its own dup at Start; see takeListenerFD

	caR, caW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("orchestrator: egress proxy CA-cert pipe: %w", err)
	}
	defer func() { _ = caR.Close() }()

	stdinPayload, err := buildEgressStdinConfig(np, secretsPath)
	if err != nil {
		_ = caW.Close()
		return nil, err
	}

	args := []string{"--mode", string(np.Mode), "--allow", strings.Join(np.Allow, ",")}
	if np.MITM {
		args = append(args, "--mitm")
		// --run-id only means anything to MITM mode (it's folded into the ephemeral CA's CN,
		// §5) and is a NEW flag a KRAYT_EGRESS_PROXY_BIN replacement built against the
		// pre-add-tls-mitm-credential-injection.md contract won't recognize. Passing it
		// unconditionally would break that replacement's mitm:false invocation even though
		// mitm:false promises zero behavior change (add-tls-mitm-credential-injection.md
		// Constraints) — so it rides along only when --mitm does.
		if runID != "" {
			args = append(args, "--run-id", runID)
		}
	}
	var cmd *exec.Cmd
	if bin := os.Getenv(EgressProxyBinEnv); bin != "" {
		cmd = exec.CommandContext(ctx, bin, args...)
	} else {
		self, err := os.Executable()
		if err != nil {
			_ = caW.Close()
			return nil, fmt.Errorf("orchestrator: resolve krayt executable for egress proxy: %w", err)
		}
		cmd = exec.CommandContext(ctx, self, append([]string{"__egress-proxy"}, args...)...)
	}
	cmd.ExtraFiles = []*os.File{f, caW}
	cmd.Stdin = bytes.NewReader(stdinPayload)
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out

	if err := cmd.Start(); err != nil {
		_ = caW.Close()
		return nil, fmt.Errorf("orchestrator: start egress proxy: %w", err)
	}
	_ = caW.Close() // this process's own copy; the child owns its independent dup at fd 4 from here

	caCh := make(chan []byte, 1)
	go func() {
		// Bounded: a compromised or merely buggy child could otherwise stream fd 4 until the
		// caller times out, forcing unbounded allocation in this process. The +1 lets a
		// legitimate cert (well under the cap) read cleanly while still detecting oversized
		// output (len(b) > maxCACertPEMBytes below) instead of silently truncating it into
		// something that happens to parse.
		b, _ := io.ReadAll(io.LimitReader(caR, maxCACertPEMBytes+1))
		caCh <- b
	}()

	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()

	ep := &egressProxy{cmd: cmd, waited: waited, out: out, runDir: runDir, secretsPath: secretsPath}
	select {
	case <-waited:
		ep.writeLog()
		return nil, fmt.Errorf("orchestrator: egress proxy exited immediately at startup; see %s", ProxyLogPath(runDir))
	case <-time.After(egressProxyStartupWait):
	}
	if np.MITM {
		// Unlike mitm:false (where no CA is the correct, unchanged behavior), a MITM child that
		// never reports a valid CA is NOT equivalent to mitm-off: the child still terminates TLS
		// and presents certificates the guest has no way to trust, while the injected
		// credential has already been withheld from SecretsBundle (§2). Proceeding here would
		// silently strand the run without its credential, so treat a missing/oversized/
		// unparseable cert as the same class of fail-fast startup error as an immediate child
		// exit above — the child is killed and its log persisted either way.
		var b []byte
		select {
		case b = <-caCh:
		case <-time.After(caHandshakeWait):
			ep.stop()
			return nil, fmt.Errorf("orchestrator: egress proxy did not report its MITM CA cert within %s; see %s",
				caHandshakeWait, ProxyLogPath(runDir))
		}
		if len(b) > maxCACertPEMBytes || !isCACertPEM(b) {
			ep.stop()
			return nil, fmt.Errorf("orchestrator: egress proxy reported no usable MITM CA cert; see %s", ProxyLogPath(runDir))
		}
		ep.caCertPEM = b
	}
	return ep, nil
}

// isCACertPEM reports whether b is a single well-formed PEM-encoded X.509 certificate — the
// shape CACertPEM() (internal/proxy) produces. Bounds what spawnEgressProxy will accept from the
// fd-4 channel to an actual certificate, not just "some bytes within the size cap".
func isCACertPEM(b []byte) bool {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	_, err := x509.ParseCertificate(block.Bytes)
	return err == nil
}

// buildEgressStdinConfig builds the JSON document written to the egress proxy child's stdin
// (§2b): the passthrough list plus every inject rule, with each rule's `set` resolved from a
// secrets-file key NAME to its actual VALUE — the only place that resolution happens, so
// internal/proxy never needs to know what a "secrets file" is. Pre-flight validation
// (task.ValidateNetworkPolicy, run before any VM boots) already guarantees every referenced key
// exists, so a lookup miss here would be a programming error, not a user-facing one.
func buildEgressStdinConfig(np task.NetworkPolicy, secretsPath string) ([]byte, error) {
	cfg := proxy.StdinConfig{Passthrough: np.Passthrough}
	if len(np.Inject) > 0 {
		values, err := secrets.Load(secretsPath)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: load secrets for network.inject: %w", err)
		}
		for _, r := range np.Inject {
			set := make(map[string]string, len(r.Set))
			for header, key := range r.Set {
				set[header] = values[key]
			}
			pr := proxy.InjectRule{Host: r.Host, Strip: r.Strip, Set: set, SetLiteral: r.SetLiteral}
			if r.Refresh != nil {
				pr.Refresh = &proxy.RefreshRule{
					Host: r.Refresh.Host, PathPrefix: r.Refresh.PathPrefix,
					ResponseTokenFields: r.Refresh.ResponseTokenFields,
				}
			}
			cfg.Inject = append(cfg.Inject, pr)
		}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: marshal egress proxy stdin config: %w", err)
	}
	return b, nil
}

// stop kills the egress proxy child, waits (bounded) for it to exit, and persists its
// redacted output to proxy.log. Safe to call on a nil *egressProxy (a spawn that failed
// before returning one).
func (p *egressProxy) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.waited:
	case <-time.After(egressProxyKillWait):
	}
	p.writeLog()
}

// writeLog redacts and persists the egress proxy child's collected stdout/stderr as proxy.log
// (§9): timestamps, hostnames, allow/deny verdicts, dial errors — never request/response
// bodies. This is the FIRST host-side redaction path in krayt (amending §6.8's "all in the
// guest" claim): for plain-HTTP forwards the proxy sees full URLs, which can carry a token in
// a query string. Same fail-closed rule as writeConsoleLog: if the secret values can't be
// loaded to redact against, the file is dropped rather than risked in the clear.
func (p *egressProxy) writeLog() {
	b := p.out.Bytes() // already capped to maxProxyLog by syncBuffer.Write
	if p.secretsPath != "" {
		values, err := secrets.Load(p.secretsPath)
		if err != nil {
			return // couldn't confirm what to scrub against; fail closed, write nothing
		}
		b = secrets.NewRedactor(secrets.Values(values)).Redact(b)
	}
	_ = os.WriteFile(ProxyLogPath(p.runDir), b, 0o644)
}

// takeListenerFD duplicates lis's underlying fd for fd-passing to the child (§4). The dup is
// independent of lis's own fd, so closing one never affects the other.
//
// The gotcha: (*net.UnixListener).Close() unlinks the socket path by default. This process
// never Accepts on lis (only the child does, via its own dup at fd 3) and closes its copy
// immediately after handing the dup to the child — but if that Close unlinked the path, a
// guest connection racing the handoff would find the socket gone. So we disable
// unlink-on-close here and leave the file on disk; it is removed only when the provider tears
// down the VM's per-run socket dir (vfkit/firecracker Destroy), which is also what makes it
// safe to never explicitly unlink it ourselves.
func takeListenerFD(lis net.Listener) (*os.File, error) {
	ul, ok := lis.(*net.UnixListener)
	if !ok {
		return nil, fmt.Errorf("egress listener is a %T, not *net.UnixListener", lis)
	}
	f, err := ul.File() // a NEW dup, independent of ul's own fd
	if err != nil {
		return nil, fmt.Errorf("dup egress listener fd: %w", err)
	}
	ul.SetUnlinkOnClose(false)
	_ = ul.Close() // this process never Accepts; the child does, via its own dup of f
	return f, nil
}

// syncBuffer is a concurrency-safe io.Writer sink for the egress proxy child's combined
// stdout/stderr. exec.Cmd already serializes writes when Stdout and Stderr are the same
// value, but a mutex here makes that safe by construction rather than by relying on it.
//
// It retains only the last maxProxyLog bytes AS WRITES ARRIVE, not just when writeLog later
// reads it back — an upstream that generates unbounded failure output over a long run must not
// be able to grow this buffer (and this process's memory) without bound just because the
// on-disk artifact is capped.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if extra := w.buf.Len() - maxProxyLog; extra > 0 {
		w.buf.Next(extra) // drop the oldest bytes, keeping the tail
	}
	return n, err
}

func (w *syncBuffer) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}
