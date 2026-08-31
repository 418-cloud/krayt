// Package sandbox is the ONLY place in krayt that knows the `msb` (microsandbox) CLI exists —
// the same containment rule internal/provider holds for the hypervisor (§6.3). It drives msb as
// a subprocess over argv, stdio and its `--format json` / `--json` output, per ADR option B1
// (docs/adr-microsandbox-sandbox-layer.md, "Integration path: CLI or SDK"): not the Go SDK, which
// is a cgo dlopen bridge that would cost CGO_ENABLED=0 and the single-Linux-runner cross-build
// without buying independence from the msb binary — the SDK downloads it too.
//
// This package is OS-agnostic (no build tags), has no cgo, and builds argv from typed structs —
// it takes no krayt.yaml vocabulary and carries no lifecycle policy. Which flags a run deserves
// is decided above this package; this package only knows how to ask msb to do what it's told.
package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// BinEnv is the swap/test seam (add-msb-sandbox-driver.md decision 3), mirroring
// orchestrator.EgressProxyBinEnv: when set, it replaces the resolved `msb` path. This is how
// tests point the driver at a fake without mocking it, and it is the documented escape hatch for
// an operator whose msb install is not on PATH.
const BinEnv = "KRAYT_MSB_BIN"

// BackendLocal is the only backend value krayt ever allows a run to observe. See childEnv and
// Context.
const BackendLocal = "local"

// backendEnvKey is the environment variable msb resolves its backend from, ranked above
// MSB_PROFILE and the active_profile saved in ~/.microsandbox/config.json (§6.15). krayt pins it
// on every invocation — see childEnv.
const backendEnvKey = "MSB_BACKEND"

// childEnvKeys is the COMPLETE set of environment variables forwarded to the msb child, each
// copied only if this process already has it set, never invented — the exact discipline
// internal/orchestrator/egressproxy.go's egressProxyChildEnvKeys uses for the same reason: an
// unset cmd.Env would run the child with os.Environ(), handing it whatever secrets the operator
// happened to have exported (an ANTHROPIC_API_KEY, an AWS credential, a stray MSB_PROFILE=prod)
// when they typed `krayt run`. Anything added here must come with a comment saying why msb
// genuinely needs it. MSB_BACKEND is handled separately by childEnv — it is never read out of
// this process's environment, only ever set to BackendLocal (see the package doc and §6.15).
var childEnvKeys = []string{
	"PATH", // basic process hygiene
	"HOME", // msb resolves its own runtime state under $HOME/.microsandbox
	// msb's documented state-dir override; forwarded only when the operator set it, never
	// fabricated — krayt does not choose a state dir on the operator's behalf.
	"MSB_HOME",
	// Needed to verify upstream registry/API certificates on distributions where Go's and
	// Rust's root pools are only discoverable through them. Forwarded when the operator set
	// them; never fabricated.
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
}

// childEnv materializes childEnvKeys against this process's environment, dropping unset ones,
// then pins backendEnvKey to BackendLocal unconditionally — NOT forwarded from this process's own
// MSB_BACKEND, if it happens to have one. That is the point (add-msb-sandbox-driver.md decision
// 5): an operator who has ever `export MSB_BACKEND=cloud`, or who has a cloud active_profile
// saved from an unrelated session, must not have that silently reroute a run to microsandbox's
// hosted service — MSB_BACKEND outranks both. The result is always non-nil so it can be assigned
// to exec.Cmd.Env without silently meaning "inherit everything" (nil means os.Environ(); an empty
// non-nil slice means an empty environment).
func childEnv() []string {
	env := make([]string, 0, len(childEnvKeys)+1)
	for _, k := range childEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, backendEnvKey+"="+BackendLocal)
	return env
}

// resolveBin finds the msb binary: BinEnv wins if set, else exec.LookPath("msb").
func resolveBin() (string, error) {
	if bin := os.Getenv(BinEnv); bin != "" {
		return bin, nil
	}
	path, err := exec.LookPath("msb")
	if err != nil {
		return "", fmt.Errorf("sandbox: msb not found on PATH (set %s to override): %w", BinEnv, err)
	}
	return path, nil
}

// Client drives the msb CLI for one host. It is stateless and safe for concurrent use; every
// method spawns its own process.
type Client struct {
	Bin string // resolved msb path; BinEnv wins, else exec.LookPath("msb")
}

// NewClient resolves the msb binary (BinEnv, else PATH) and returns a Client for it.
func NewClient() (*Client, error) {
	bin, err := resolveBin()
	if err != nil {
		return nil, err
	}
	return &Client{Bin: bin}, nil
}

// command builds an *exec.Cmd for one msb invocation: the run's context.Context (killed with it,
// add-msb-sandbox-driver.md decision 6) and the closed child-env allowlist, never os.Environ().
func (c *Client) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = childEnv()
	return cmd
}

// runCaptured runs an msb subcommand to completion, capturing stdout/stderr separately.
func (c *Client) runCaptured(ctx context.Context, args []string) (stdout, stderr []byte, err error) {
	cmd := c.command(ctx, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// Version runs `msb --version` and parses the result.
func (c *Client) Version(ctx context.Context) (Version, error) {
	out, stderr, err := c.runCaptured(ctx, []string{"--version"})
	if err != nil {
		return Version{}, fmt.Errorf("sandbox: msb --version: %w (%s)", err, firstNonEmpty(stderr, out))
	}
	return parseVersion(strings.TrimSpace(string(out)))
}

// ContextInfo is the parsed result of `msb context --format json`. The exact JSON shape is not
// pinned by the ADR or msb's docs beyond the presence of a backend indicator, so parsing checks
// a small set of plausible key names rather than a single fixed field — see parseContext.
type ContextInfo struct {
	Backend string
	Raw     json.RawMessage
}

// IsLocal reports whether the resolved backend is BackendLocal — the assertion
// add-msb-sandbox-driver.md decision 5 requires krayt make before starting any run.
func (i ContextInfo) IsLocal() bool { return i.Backend == BackendLocal }

// Context runs `msb context --format json` under krayt's own pinned child env (backendEnvKey is
// always BackendLocal here — see childEnv) and reports what backend it resolves to. Callers must
// report the resolved backend either way, not just on failure: an operator with a cloud profile
// should see krayt overriding it, not silently benefit from it (decision 5).
func (c *Client) Context(ctx context.Context) (ContextInfo, error) {
	out, stderr, err := c.runCaptured(ctx, []string{"context", "--format", "json"})
	if err != nil {
		return ContextInfo{}, fmt.Errorf("sandbox: msb context --format json: %w (%s)", err, firstNonEmpty(stderr, out))
	}
	return parseContext(out)
}

// parseContext is deliberately tolerant: it looks for a handful of plausible key names for the
// backend indicator rather than assuming one exact schema, since that schema is unverified
// against msb's source (probe-microsandbox-feasibility.md's outstanding probes do not cover it,
// and neither blocks this task). If none match, Backend is empty — which IsLocal correctly
// reports as false, and callers fail closed on that (decision 5's "a pin that is never checked
// is a comment").
func parseContext(raw []byte) (ContextInfo, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ContextInfo{}, fmt.Errorf("sandbox: parse msb context json: %w", err)
	}
	info := ContextInfo{Raw: raw}
	for _, key := range []string{"backend", "active_backend", "current_backend"} {
		v, ok := m[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			info.Backend = s
			break
		}
	}
	return info, nil
}

func firstNonEmpty(bs ...[]byte) string {
	for _, b := range bs {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return "(no output)"
}

// EnvVar is one `--env NAME=VALUE` pair for CreateSpec.
type EnvVar struct {
	Name  string
	Value string
}

// VsockRoute is one `--vsock HOST_PATH:PORT` route for CreateSpec — the guest->host channel
// ask_human and the krayt-ask helper ride (dial-ask-channel-over-vsock.md).
type VsockRoute struct {
	HostPath string
	Port     uint32
}

// SecretRef is one `--secret NAME@HOST[,HOST...]` declaration. It never carries a value: msb
// itself rejects an inline NAME=VALUE@HOST on both `create` and `modify` (the secret's real value
// travels only in cmd.Env — see hand-secrets-to-msb.md, which owns deciding what belongs here).
type SecretRef struct {
	Name  string
	Hosts []string
}

// CreateSpec carries every `msb create` argument as typed fields. Render argv with Args(), a pure
// function with no I/O, so the whole surface is unit-testable without spawning anything.
type CreateSpec struct {
	Image string // positional image ref
	Name  string
	User  string

	CPUs      int    // 0 means omit --cpus (let msb choose its default)
	MemoryMiB uint64 // 0 means omit --memory
	DiskGiB   uint64 // 0 means omit --root-disk's size component

	// RootDisk is the raw --root-disk value (e.g. "flat:...,clone=auto" per
	// warm-start-msb-sandboxes.md); opaque to this package. Empty means omit the flag.
	RootDisk string

	MaxDuration time.Duration // 0 means omit --max-duration

	Env   []EnvVar
	Vsock []VsockRoute

	// NetRules are pre-built `--net-rule` tokens (e.g. "allow@api.anthropic.com",
	// "deny@private") — translate-network-policy-to-msb.md's job to build, never this
	// package's to interpret. Each becomes exactly one argv element.
	NetRules          []string
	NetDefault        string // --net-default value; empty means omit
	NetDefaultEgress  string // --net-default-egress value; empty means omit
	NetDefaultIngress string // --net-default-ingress value; empty means omit
	NoNet             bool   // --no-net; NetRules must be empty when set (translator's job to enforce)

	Secrets      []SecretRef
	TLSIntercept bool     // --tls-intercept
	TLSBypass    []string // --tls-bypass HOST, one per entry

	Security string // --security value (e.g. "restricted"); empty means omit

	// ExtraArgs is the one open escape hatch (add-msb-extra-conf-escape-hatch.md), appended
	// verbatim after everything else this function renders.
	ExtraArgs []string
}

// Args renders CreateSpec into `msb create` argv, in a stable order, with every value its own
// argv element — never shell-joined. This is a pure function: no I/O, safe to unit test
// exhaustively without spawning anything.
func (s CreateSpec) Args() []string {
	args := []string{"create", s.Image}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(s.CPUs))
	}
	if s.MemoryMiB > 0 {
		args = append(args, "--memory", strconv.FormatUint(s.MemoryMiB, 10))
	}
	if s.RootDisk != "" {
		args = append(args, "--root-disk", s.RootDisk)
	} else if s.DiskGiB > 0 {
		args = append(args, "--root-disk", strconv.FormatUint(s.DiskGiB, 10)+"G")
	}
	if s.MaxDuration > 0 {
		args = append(args, "--max-duration", s.MaxDuration.String())
	}
	for _, e := range s.Env {
		args = append(args, "--env", e.Name+"="+e.Value)
	}
	for _, v := range s.Vsock {
		args = append(args, "--vsock", v.HostPath+":"+strconv.FormatUint(uint64(v.Port), 10))
	}
	if s.NoNet {
		args = append(args, "--no-net")
	}
	if s.NetDefault != "" {
		args = append(args, "--net-default", s.NetDefault)
	}
	if s.NetDefaultEgress != "" {
		args = append(args, "--net-default-egress", s.NetDefaultEgress)
	}
	if s.NetDefaultIngress != "" {
		args = append(args, "--net-default-ingress", s.NetDefaultIngress)
	}
	for _, r := range s.NetRules {
		args = append(args, "--net-rule", r)
	}
	for _, sec := range s.Secrets {
		args = append(args, "--secret", sec.Name+"@"+strings.Join(sec.Hosts, ","))
	}
	if s.TLSIntercept {
		args = append(args, "--tls-intercept")
	}
	for _, h := range s.TLSBypass {
		args = append(args, "--tls-bypass", h)
	}
	if s.Security != "" {
		args = append(args, "--security", s.Security)
	}
	args = append(args, s.ExtraArgs...)
	return args
}

// Create runs `msb create` and returns its output combined (msb create's own stdout is not
// documented as structured; callers needing the sandbox name already supplied it via
// CreateSpec.Name).
func (c *Client) Create(ctx context.Context, spec CreateSpec) error {
	_, stderr, err := c.runCaptured(ctx, spec.Args())
	if err != nil {
		return fmt.Errorf("sandbox: msb create: %w (%s)", err, firstNonEmpty(stderr))
	}
	return nil
}

// ErrMsbFailed distinguishes "msb itself could not start/run the command" from "the guest command
// ran and returned a non-zero exit code". `msb exec` propagates the guest's exit code via
// std::process::exit, while msb's OWN failures surface as an anyhow error that also exits 1 — so
// exit 1 alone is ambiguous. Exec resolves this structurally: a non-zero msb exit with no output
// observed on either stream is treated as a driver failure, not an agent exit code (see Exec's
// doc comment for the exact heuristic and its limits).
var ErrMsbFailed = errors.New("sandbox: msb failed to run the command")

// ExecSpec carries one `msb exec --stream` invocation.
type ExecSpec struct {
	Name    string   // sandbox name
	User    string   // --user override; empty means msb's default
	Command []string // argv after `--`

	// Stdin is piped to the child incrementally. msb exec --stream requires stdin to be a
	// pipe, not a terminal (exec.rs bails otherwise) — Exec always gives the child a real
	// pipe, defaulting to an empty reader when Stdin is nil, rather than leaving it to
	// inherit whatever this process's own stdin happens to be.
	Stdin io.Reader
	// Stdout/Stderr receive the guest's streamed output, kept separate end to end. Nil means
	// discard.
	Stdout io.Writer
	Stderr io.Writer
}

// ExecResult is the outcome of a successful (non-driver-failed) Exec: the guest command's own
// exit code, exactly as msb propagated it.
type ExecResult struct {
	ExitCode int
}

// Exec runs `msb exec --stream` (add-msb-sandbox-driver.md, "Streaming"): the default
// non-interactive mode buffers the entire output until the command exits, which is unusable for
// krayt's live log streaming, so --stream is always passed. Stdin is always an explicit pipe (see
// ExecSpec.Stdin), and stdout/stderr are wired to separate writers so the two streams never mix.
//
// Exit-code ambiguity is resolved by observation: if the process exits non-zero and NEITHER
// stream ever received a byte, nothing indicates the guest command actually started, so this
// returns ErrMsbFailed rather than guessing it was the agent's own exit code. Any observed output
// is taken as evidence the command ran, and the exit code is trusted as the guest's. This is a
// structural placeholder, not a wire-level guarantee — add-krayt-guest-helper.md's helper and
// agent execs additionally write their real exit status somewhere krayt can read unambiguously;
// this heuristic is what the driver layer alone can do without that.
func (c *Client) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	args := []string{"exec"}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	args = append(args, "--stream", spec.Name, "--")
	args = append(args, spec.Command...)

	cmd := c.command(ctx, args...)

	stdin := spec.Stdin
	if stdin == nil {
		stdin = bytes.NewReader(nil)
	}
	cmd.Stdin = stdin // never the terminal/this process's own stdin — always an explicit pipe

	var observed countingWriter
	stdout := spec.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := spec.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stdout = io.MultiWriter(stdout, &observed)
	cmd.Stderr = io.MultiWriter(stderr, &observed)

	err := cmd.Run()
	if err == nil {
		return ExecResult{ExitCode: 0}, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return ExecResult{}, fmt.Errorf("sandbox: msb exec: %w", err)
	}
	if observed.n.Load() == 0 {
		return ExecResult{}, fmt.Errorf("%w: exit %d, no output observed", ErrMsbFailed, exitErr.ExitCode())
	}
	return ExecResult{ExitCode: exitErr.ExitCode()}, nil
}

// countingWriter counts bytes written, used by Exec to detect whether the child ever produced
// any output at all. Stdout and stderr are copied by two separate goroutines inside
// (*exec.Cmd).Start, so writes here must be safe for concurrent use.
type countingWriter struct{ n atomic.Int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n.Add(int64(len(p)))
	return len(p), nil
}

// Copy runs `msb copy`, using docker-cp syntax: `./local sandbox:/path` and back.
func (c *Client) Copy(ctx context.Context, from, to string) error {
	_, stderr, err := c.runCaptured(ctx, []string{"copy", from, to})
	if err != nil {
		return fmt.Errorf("sandbox: msb copy %s %s: %w (%s)", from, to, err, firstNonEmpty(stderr))
	}
	return nil
}

// LogEntry is one JSON-Lines record from `msb logs --json`. Stream tags which stream the line
// came from (the "s" field per the ADR); Raw retains the whole decoded line for anything else a
// caller needs, since the rest of the schema is not pinned by anything this package can verify
// offline.
type LogEntry struct {
	Stream string
	Raw    json.RawMessage
}

// Logs runs `msb logs --json [--follow] <name>` and streams decoded JSON-Lines entries on the
// returned channel, which is closed when the process exits, the scanner hits EOF, or ctx is
// done. A send error is reported on the returned error channel; at most one error is ever sent.
func (c *Client) Logs(ctx context.Context, name string, follow bool) (<-chan LogEntry, <-chan error) {
	entries := make(chan LogEntry)
	errs := make(chan error, 1)

	args := []string{"logs", "--json"}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, name)
	cmd := c.command(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		close(entries)
		errs <- fmt.Errorf("sandbox: msb logs stdout pipe: %w", err)
		return entries, errs
	}
	cmd.Stderr = io.Discard

	go func() {
		defer close(entries)
		if err := cmd.Start(); err != nil {
			errs <- fmt.Errorf("sandbox: msb logs: %w", err)
			return
		}
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var tagged struct {
				Stream string `json:"s"`
			}
			_ = json.Unmarshal(line, &tagged) // best-effort; Raw always carries the full line
			raw := append(json.RawMessage(nil), line...)
			select {
			case entries <- LogEntry{Stream: tagged.Stream, Raw: raw}:
			case <-ctx.Done():
				_ = cmd.Process.Kill()
				return
			}
		}
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			errs <- fmt.Errorf("sandbox: msb logs: %w", err)
		}
	}()

	return entries, errs
}

// teardownTimeout bounds how long Stop/Remove wait once given an out-of-band context (see their
// doc comments) — teardown must not hang forever just because the run's own context never gets
// cancelled again after the run itself already ended.
const teardownTimeout = 30 * time.Second

// Stop runs `msb stop <name>`. It always runs with context.WithoutCancel(ctx) (bounded by
// teardownTimeout), because teardown must still happen when the caller's context is the run
// context that just got cancelled or timed out — that is very often WHY Stop is being called.
func (c *Client) Stop(ctx context.Context, name string) error {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
	defer cancel()
	_, stderr, err := c.runCaptured(tctx, []string{"stop", name})
	if err != nil {
		return fmt.Errorf("sandbox: msb stop %s: %w (%s)", name, err, firstNonEmpty(stderr))
	}
	return nil
}

// Remove runs `msb rm --force <name>`. Same out-of-band-context treatment as Stop, for the same
// reason.
func (c *Client) Remove(ctx context.Context, name string) error {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
	defer cancel()
	_, stderr, err := c.runCaptured(tctx, []string{"rm", "--force", name})
	if err != nil {
		return fmt.Errorf("sandbox: msb rm --force %s: %w (%s)", name, err, firstNonEmpty(stderr))
	}
	return nil
}

// Pull runs `msb pull <ref>` (image acquisition, for retire-vm-image-pipeline.md).
func (c *Client) Pull(ctx context.Context, ref string) error {
	_, stderr, err := c.runCaptured(ctx, []string{"pull", ref})
	if err != nil {
		return fmt.Errorf("sandbox: msb pull %s: %w (%s)", ref, err, firstNonEmpty(stderr))
	}
	return nil
}
