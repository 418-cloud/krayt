// Package sandbox is the ONLY place in krayt that knows the `msb` (microsandbox) CLI exists —
// the same containment rule the pre-msb provider package held for the hypervisor (§6.3). It drives msb as
// a subprocess over argv, stdio and its `--format json` / `--json` output, per ADR option B1
// (docs/adr-microsandbox-sandbox-layer.md, "Integration path: CLI or SDK"): not the Go SDK, which
// is a cgo dlopen bridge that would cost CGO_ENABLED=0 and the single-Linux-runner cross-build
// without buying independence from the msb binary — the SDK downloads it too.
//
// This package is OS-agnostic (no build tags), has no cgo, and builds argv from typed structs —
// it takes no krayt.yaml vocabulary and carries no lifecycle policy. Which flags a run deserves
// is decided above this package; this package only knows how to ask msb to do what it's told.
//
// The one deliberate exception is task.SecretSpec (hand-secrets-to-msb.md's "What to build"): it
// is not krayt.yaml vocabulary in the sense above (no YAML shape, no policy) but a bare (key,
// hosts) pair — SecretArgs and SecretEnv render it into the two channels a secret actually
// travels, argv and env, and keeping both pure functions here next to CreateSpec.Args() is what
// makes their "never a value on argv" / "only the declared keys in env" properties testable
// without spawning anything, the same reason CreateSpec.Args() lives here rather than above this
// package.
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
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/418-cloud/krayt/internal/task"
)

// BinEnv is the swap/test seam (add-msb-sandbox-driver.md decision 3): when set, it replaces the
// resolved `msb` path. This is how tests point the driver at a fake without mocking it, and it
// is the documented escape hatch for an operator whose msb install is not on PATH.
const BinEnv = "KRAYT_MSB_BIN"

// BackendLocal is the only backend value krayt ever allows a run to observe. See childEnv and
// Context.
const BackendLocal = "local"

// backendEnvKey is the environment variable msb resolves its backend from, ranked above
// MSB_PROFILE and the active_profile saved in ~/.microsandbox/config.json (§6.15). krayt pins it
// on every invocation — see childEnv.
const backendEnvKey = "MSB_BACKEND"

// childEnvKeys is the COMPLETE set of environment variables forwarded to the msb child, each
// copied only if this process already has it set, never invented — an
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

// reservedChildEnvKeys is childEnvKeys plus backendEnvKey — the complete set of variable names
// childEnv() itself may set on the msb child. A secret sharing one of these names would append a
// second entry for the same name onto cmd.Env (commandWithEnv appends secretEnv after childEnv()),
// and which of the two duplicate entries an arbitrary msb build honors is not something this
// package controls — at worst silently overriding the MSB_BACKEND=local pin (decision 5). SecretEnv
// rejects the collision at construction time instead.
var reservedChildEnvKeys = func() map[string]bool {
	m := make(map[string]bool, len(childEnvKeys)+1)
	for _, k := range childEnvKeys {
		m[k] = true
	}
	m[backendEnvKey] = true
	return m
}()

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
	return c.commandWithEnv(ctx, nil, args...)
}

// commandWithEnv is command plus extraEnv appended on top of the closed allowlist — the ONLY seam
// a secret value ever travels through (Create, via secretEnv; hand-secrets-to-msb.md's Timing
// rule). Every other Client method goes through command, which passes nil here, so nothing but
// Create can ever hand the msb child anything beyond childEnv()'s fixed allowlist.
func (c *Client) commandWithEnv(ctx context.Context, extraEnv []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = append(childEnv(), extraEnv...)
	return cmd
}

// runCaptured runs an msb subcommand to completion, capturing stdout/stderr separately.
func (c *Client) runCaptured(ctx context.Context, args []string) (stdout, stderr []byte, err error) {
	return c.runCapturedWithEnv(ctx, nil, args)
}

// runCapturedWithEnv is runCaptured plus extraEnv — see commandWithEnv.
func (c *Client) runCapturedWithEnv(ctx context.Context, extraEnv []string, args []string) (stdout, stderr []byte, err error) {
	cmd := c.commandWithEnv(ctx, extraEnv, args...)
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

// ContextInfo is the parsed result of `msb context --format json`. msb 0.6.16 emits
// {"kind":"local","source":"default"} — `kind` carries the backend, `source` names what selected
// it — but the shape is not pinned by the ADR or msb's docs, so parsing stays tolerant of other
// key names (see parseContext).
type ContextInfo struct {
	Backend string
	// Source is msb's own account of what selected the backend ("default", "MSB_BACKEND", a
	// profile name…). krayt appends MSB_BACKEND=local to every invocation (childEnv), so a
	// healthy host reports "MSB_BACKEND" here — direct evidence the pin reached the child, which
	// Backend alone cannot give: "local" is also msb's default. Reported, never gated on; msb is
	// beta and a local backend reached by default is not a failure.
	Source string
	Raw    json.RawMessage
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

// parseContext reads the backend indicator out of `msb context --format json`. `kind` is msb's
// actual field name, read from msb 0.6.16 on macOS/aarch64:
//
//	$ msb context --format json
//	{"kind":"local","source":"default"}
//
// The three names after it are kept as fallbacks. They are NOT observed field names — they were
// this function's original guesses, made when the schema was unverified, and the guess was wrong:
// every one of them missed `kind`, so a correctly-pinned host failed `krayt doctor`'s backend
// check with an empty Backend. Keeping them costs nothing and covers a future rename; `kind` goes
// first because it is the one name actually confirmed against msb.
//
// If none match, Backend is empty — which IsLocal correctly reports as false, and callers fail
// closed on that (decision 5's "a pin that is never checked is a comment"). Raw is retained so
// that a caller can show the operator what msb actually printed rather than only that no key
// matched; the empty-Backend path is exactly where the schema has drifted before.
func parseContext(raw []byte) (ContextInfo, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ContextInfo{}, fmt.Errorf("sandbox: parse msb context json: %w", err)
	}
	info := ContextInfo{Raw: raw}
	for _, key := range []string{"kind", "backend", "active_backend", "current_backend"} {
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
	if v, ok := m["source"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			info.Source = s
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

// SecretArgs renders one `--secret NAME@HOST[,HOST...]` flag per spec, deterministically ordered
// by key regardless of input order — so the same set of secrets always produces byte-identical
// argv, matching CreateSpec.Args()'s own determinism guarantee. It carries no value, by
// construction: task.SecretSpec has no field to hold one (hand-secrets-to-msb.md's "What to
// build") — see SecretEnv for the one channel that does.
func SecretArgs(specs []task.SecretSpec) []string {
	sorted := append([]task.SecretSpec(nil), specs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	args := make([]string, 0, len(sorted)*2)
	for _, s := range sorted {
		args = append(args, "--secret", s.Key+"@"+strings.Join(s.Hosts, ","))
	}
	return args
}

// SecretEnv returns the KEY=VALUE entries to append to the msb child's environment (Create's
// secretEnv parameter), for EXACTLY the keys specs declare — never every key in the secrets file,
// so a secret nobody scoped to a host never leaves the host process at all. It errors on a
// declared key vals lacks: pre-flight refusal beats a run that fails thirty seconds in,
// unauthenticated, against an allowed host — the same rule krayt.yaml's own comment already
// states for the pre-msb shape.
func SecretEnv(specs []task.SecretSpec, vals map[string]string) ([]string, error) {
	env := make([]string, 0, len(specs))
	seen := make(map[string]bool, len(specs))
	for _, s := range specs {
		if reservedChildEnvKeys[s.Key] {
			return nil, fmt.Errorf("sandbox: secret key %q collides with a reserved msb child-env variable", s.Key)
		}
		if seen[s.Key] {
			return nil, fmt.Errorf("sandbox: secret key %q is declared more than once", s.Key)
		}
		seen[s.Key] = true
		v, ok := vals[s.Key]
		if !ok {
			return nil, fmt.Errorf("sandbox: secrets file has no value for declared key %q", s.Key)
		}
		env = append(env, s.Key+"="+v)
	}
	return env, nil
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
		// msb parses --memory as a size with an explicit unit suffix ("512M", "1G"); a bare
		// integer is accepted but its unit is msb's business, not ours. Always spell the M.
		args = append(args, "--memory", strconv.FormatUint(s.MemoryMiB, 10)+"M")
	}
	if s.RootDisk != "" {
		args = append(args, "--root-disk", s.RootDisk)
	} else if s.DiskGiB > 0 {
		args = append(args, "--root-disk", strconv.FormatUint(s.DiskGiB, 10)+"G")
	}
	if s.MaxDuration > 0 {
		args = append(args, "--max-duration", msbDuration(s.MaxDuration))
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

// msbDuration renders a Go duration the way msb's parser reads one. msb accepts exactly one
// integer followed by one unit ("30s", "5m", "1h") — time.Duration.String() emits the composite
// form ("30m0s", "1h0m0s"), which msb rejects with "invalid digit found in string" before it even
// opens its database. Whole seconds are the one form that is always expressible, so everything is
// rendered as "<n>s", rounded up so a sub-second limit never collapses to "0s" (which msb would
// read as no limit at all — the opposite of what the caller asked for).
func msbDuration(d time.Duration) string {
	secs := int64(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	return strconv.FormatInt(secs, 10) + "s"
}

// Create runs `msb create`, with secretEnv appended to the child's environment on top of the
// usual closed allowlist. secretEnv is the ONE place a secret value is ever handed to an msb
// child (hand-secrets-to-msb.md's Timing rule): msb reads a --secret's value "at start time", and
// `msb create` is the invocation that starts the sandbox, so this is the only Client method that
// accepts it at all — Exec, Copy, Logs and every other method call command/runCaptured, which
// always pass nil extra env, structurally rather than by convention (msb_test.go asserts this on
// Exec directly). Build secretEnv with SecretEnv; CreateSpec.Secrets ([]SecretRef, names + hosts,
// never values) is rendered into argv by CreateSpec.Args() itself, same as SecretArgs renders a
// []task.SecretSpec.
func (c *Client) Create(ctx context.Context, spec CreateSpec, secretEnv []string) error {
	_, stderr, err := c.runCapturedWithEnv(ctx, secretEnv, spec.Args())
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

// Args renders ExecSpec into `msb exec --stream` argv, in the exact order Exec issues it — a
// pure function, mirroring CreateSpec.Args(), so the exec argv surface is unit-testable without
// spawning anything. It has no way to carry a secret value structurally: ExecSpec carries no env
// field at all (hand-secrets-to-msb.md's Timing rule — a secret's value is set once, on whichever
// invocation starts the sandbox, never on a later exec against it) and Stdin/Stdout/Stderr are
// I/O plumbing this function never touches.
func (s ExecSpec) Args() []string {
	args := []string{"exec"}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	args = append(args, "--stream", s.Name, "--")
	args = append(args, s.Command...)
	return args
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
	cmd := c.command(ctx, spec.Args()...)

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
				_ = cmd.Wait()
				return
			}
		}
		scanErr := scanner.Err()
		waitErr := cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		if scanErr != nil {
			errs <- fmt.Errorf("sandbox: msb logs: scan: %w", scanErr)
			return
		}
		if waitErr != nil {
			errs <- fmt.Errorf("sandbox: msb logs: %w", waitErr)
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

// SystemLogs runs `msb logs --source system --json <name>` and returns its raw stdout — msb's
// boot/system diagnostics (run-tasks-on-microsandbox.md decision 7), as opposed to Logs' live
// guest-log stream. This is the msb-era replacement for the old provider.VM.LogPaths' console
// log: when a sandbox fails to start, `msb logs` prepends a reconstructed error block from
// boot-error.json, which is exactly the evidence a boot failure's console output used to carry.
// Captured non-streaming (the whole point is a diagnostics snapshot, not a live tail), and
// tolerant of a nonexistent/never-started sandbox — the caller persists whatever came back,
// even on error, since msb's own stderr on a failed lookup is itself diagnostic. Like Stop/
// Remove, it always runs under context.WithoutCancel(ctx) (bounded by teardownTimeout): it is
// called from the same deferred teardown path, which must still capture diagnostics when the
// run's own context is what just expired or was cancelled.
func (c *Client) SystemLogs(ctx context.Context, name string) ([]byte, error) {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
	defer cancel()
	out, stderr, err := c.runCaptured(tctx, []string{"logs", "--source", "system", "--json", name})
	if err != nil {
		return out, fmt.Errorf("sandbox: msb logs --source system: %w (%s)", err, firstNonEmpty(stderr))
	}
	return out, nil
}

// Pull runs `msb pull <ref>` (image acquisition, for retire-vm-image-pipeline.md).
func (c *Client) Pull(ctx context.Context, ref string) error {
	_, stderr, err := c.runCaptured(ctx, []string{"pull", ref})
	if err != nil {
		return fmt.Errorf("sandbox: msb pull %s: %w (%s)", ref, err, firstNonEmpty(stderr))
	}
	return nil
}

// ImageInfo is one parsed entry from `msb images --format json` (retire-vm-image-pipeline.md
// decision 2: `krayt image ls` is a thin render of this). Like ContextInfo, msb's JSON schema
// here is not pinned by the ADR or its docs, so field extraction is tolerant of a few plausible
// names per attribute and Raw always retains the whole decoded entry as a fallback.
type ImageInfo struct {
	Ref   string // best-effort reference this image was pulled/tagged under
	SizeB int64  // best-effort size in bytes; 0 if none of the candidate keys are present
	Raw   json.RawMessage
}

// Images runs `msb images --format json` and parses the result.
func (c *Client) Images(ctx context.Context) ([]ImageInfo, error) {
	out, stderr, err := c.runCaptured(ctx, []string{"images", "--format", "json"})
	if err != nil {
		return nil, fmt.Errorf("sandbox: msb images --format json: %w (%s)", err, firstNonEmpty(stderr, out))
	}
	return parseImages(out)
}

// parseImages reads `msb images --format json`'s array of image entries, tolerant of a few
// plausible field names per attribute — see ImageInfo.
func parseImages(raw []byte) ([]ImageInfo, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("sandbox: parse msb images json: %w", err)
	}
	out := make([]ImageInfo, 0, len(entries))
	for _, e := range entries {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(e, &m); err != nil {
			continue
		}
		info := ImageInfo{Raw: e}
		for _, key := range []string{"reference", "ref", "name", "image", "repository"} {
			v, ok := m[key]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err == nil && s != "" {
				info.Ref = s
				break
			}
		}
		for _, key := range []string{"size", "size_bytes", "sizeBytes"} {
			v, ok := m[key]
			if !ok {
				continue
			}
			var n int64
			if err := json.Unmarshal(v, &n); err == nil {
				info.SizeB = n
				break
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// ImageRefs runs `msb images -q`, msb's quiet/reference-only listing, and returns one trimmed,
// non-empty reference per line — the source shell completion uses for `krayt image rm`
// (retire-vm-image-pipeline.md decision 4/Done-when), cheaper than parsing the full JSON shape
// just to offer completions.
func (c *Client) ImageRefs(ctx context.Context) ([]string, error) {
	out, stderr, err := c.runCaptured(ctx, []string{"images", "-q"})
	if err != nil {
		return nil, fmt.Errorf("sandbox: msb images -q: %w (%s)", err, firstNonEmpty(stderr, out))
	}
	var refs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			refs = append(refs, line)
		}
	}
	return refs, nil
}

// Rmi runs `msb rmi <ref>`, optionally with --force — msb's own `--force` allows removing an
// image a sandbox still references (retire-vm-image-pipeline.md decision 2). The image is
// identified by reference, not a krayt-owned digest: msb's store is ref-keyed (decision 4).
func (c *Client) Rmi(ctx context.Context, ref string, force bool) error {
	args := []string{"rmi"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, ref)
	_, stderr, err := c.runCaptured(ctx, args)
	if err != nil {
		return fmt.Errorf("sandbox: msb rmi %s: %w (%s)", ref, err, firstNonEmpty(stderr))
	}
	return nil
}

// ImagePrune runs `msb image prune` — msb's own sweep of images unused by any sandbox or indexed
// snapshot (retire-vm-image-pipeline.md decision 3). krayt's own age/in-use retention runs first,
// via Rmi against the refs it decides to remove; this is the final sweep for whatever msb's store
// still considers dangling afterward. msb's prune has no age policy of its own, which is exactly
// why krayt keeps its own on top rather than calling only this.
func (c *Client) ImagePrune(ctx context.Context) error {
	_, stderr, err := c.runCaptured(ctx, []string{"image", "prune"})
	if err != nil {
		return fmt.Errorf("sandbox: msb image prune: %w (%s)", err, firstNonEmpty(stderr))
	}
	return nil
}
