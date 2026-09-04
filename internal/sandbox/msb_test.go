package sandbox

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newFakeClient points a Client at this test binary (via testBinPath, set by TestMain) and
// writes script to HOME (also set to home). Callers must have already set HOME via t.Setenv.
func newFakeClient(t *testing.T, home string, script fakeScript) *Client {
	t.Helper()
	if testBinPath == "" {
		t.Fatal("testBinPath not set — TestMain did not run (are tests being invoked oddly?)")
	}
	writeFakeScript(t, home, script)
	return &Client{Bin: testBinPath}
}

func TestCreateSpecArgsStableOrderAndQuoting(t *testing.T) {
	spec := CreateSpec{
		Image:     "agent-image:latest",
		Name:      "krayt-run-1",
		User:      "agent",
		CPUs:      4,
		MemoryMiB: 2048,
		RootDisk:  "flat:/base.img,clone=auto",
		// msb reads only "<int><unit>", so a Go duration must never reach argv verbatim.
		MaxDuration: 90 * time.Minute,
		Env: []EnvVar{
			{Name: "FOO", Value: "bar"},
			{Name: "BAZ", Value: "qux=quux"},
		},
		Vsock: []VsockRoute{
			{HostPath: "/run/krayt/ask.sock", Port: 9000},
		},
		NetRules: []string{
			"deny@private",
			"allow@dns",
			"allow@api.anthropic.com",
		},
		NetDefault: "deny",
		Secrets: []SecretRef{
			{Name: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}},
			{Name: "GH_TOKEN", Hosts: []string{"api.github.com", "github.com"}},
		},
		TLSIntercept: true,
		TLSBypass:    []string{"passthrough.example.com"},
		Security:     "restricted",
		ExtraArgs:    []string{"--future-flag", "value"},
	}

	got := spec.Args()
	want := []string{
		"create", "agent-image:latest",
		"--name", "krayt-run-1",
		"--user", "agent",
		"--cpus", "4",
		"--memory", "2048M",
		"--root-disk", "flat:/base.img,clone=auto",
		"--max-duration", "5400s",
		"--env", "FOO=bar",
		"--env", "BAZ=qux=quux",
		"--vsock", "/run/krayt/ask.sock:9000",
		"--net-default", "deny",
		"--net-rule", "deny@private",
		"--net-rule", "allow@dns",
		"--net-rule", "allow@api.anthropic.com",
		"--secret", "ANTHROPIC_API_KEY@api.anthropic.com",
		"--secret", "GH_TOKEN@api.github.com,github.com",
		"--tls-intercept",
		"--tls-bypass", "passthrough.example.com",
		"--security", "restricted",
		"--future-flag", "value",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args() =\n%q\nwant\n%q", got, want)
	}

	// Every --net-rule and --secret token must be exactly ONE argv element — never shell-joined —
	// even though its value contains '@', ':' and ',', all shell-significant characters.
	for i, tok := range got {
		if tok == "--net-rule" || tok == "--secret" {
			next := got[i+1]
			if strings.Contains(next, " ") {
				t.Fatalf("token %q looks shell-joined, want a single argv element", next)
			}
		}
	}
}

func TestCreateSpecArgsOmitsZeroValues(t *testing.T) {
	got := CreateSpec{Image: "img"}.Args()
	want := []string{"create", "img"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args() = %q, want %q (every optional field should be omitted)", got, want)
	}
}

// TestCreateSpecArgsVsockOnlyWhenPopulated pins dial-ask-channel-over-vsock.md decision 9: the
// --vsock route is create-time policy decided above this package — CreateSpec.Args() only ever
// renders what it is given. An --on-question=fail run passes an empty Vsock and must get no
// flag; a wait run passes one route and must get exactly one — never zero (the channel silently
// missing) and never more than one (two channels sharing a number inviting the wrong one being
// reasoned about, decision 3).
func TestCreateSpecArgsVsockOnlyWhenPopulated(t *testing.T) {
	fail := CreateSpec{Image: "img"}.Args()
	for _, tok := range fail {
		if tok == "--vsock" {
			t.Fatalf("Args() for an empty Vsock (fail mode) emitted --vsock: %q", fail)
		}
	}

	wait := CreateSpec{Image: "img", Vsock: []VsockRoute{{HostPath: "/run/krayt/ask.sock", Port: AskPort}}}.Args()
	n := 0
	for _, tok := range wait {
		if tok == "--vsock" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("Args() for one Vsock route (wait mode) emitted %d --vsock flags, want 1: %q", n, wait)
	}
}

func TestCreateSpecArgsNoNet(t *testing.T) {
	got := CreateSpec{Image: "img", NoNet: true}.Args()
	want := []string{"create", "img", "--no-net"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args() = %q, want %q", got, want)
	}
}

func TestChildEnvAllowlistExact(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// The exact adversarial case the ADR/task exist to defeat: an operator with a cloud
	// backend/profile pinned in their shell, plus unrelated secrets that must never leak into
	// any msb child.
	t.Setenv("MSB_BACKEND", "cloud")
	t.Setenv("MSB_PROFILE", "prod")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-should-not-leak")

	c := newFakeClient(t, home, fakeScript{Default: fakeResponse{ExitCode: 0, Stdout: "msb 0.6.16\n"}})
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatalf("Version: %v", err)
	}

	call := lastFakeCall(t, home)
	env := call.Env

	if got := env["MSB_BACKEND"]; got != BackendLocal {
		t.Fatalf("child MSB_BACKEND = %q, want %q (krayt must pin this, never forward the parent's)", got, BackendLocal)
	}
	for _, leaked := range []string{"MSB_PROFILE", "ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY"} {
		if v, ok := env[leaked]; ok {
			t.Fatalf("child env leaked %s=%q — must not be forwarded", leaked, v)
		}
	}

	allowed := map[string]bool{"PATH": true, "HOME": true, "MSB_HOME": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "MSB_BACKEND": true}
	for k := range env {
		if !allowed[k] {
			t.Fatalf("child env carried unexpected key %q — allowlist is not closed", k)
		}
	}
	if env["HOME"] != home {
		t.Fatalf("child HOME = %q, want %q", env["HOME"], home)
	}
}

func TestChildEnvForwardsOptionalKeysWhenSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MSB_HOME", "/custom/msb-home")
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/custom.pem")

	c := newFakeClient(t, home, fakeScript{Default: fakeResponse{ExitCode: 0, Stdout: "msb 0.6.16\n"}})
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatalf("Version: %v", err)
	}

	env := lastFakeCall(t, home).Env
	if env["MSB_HOME"] != "/custom/msb-home" {
		t.Fatalf("MSB_HOME = %q, want forwarded value", env["MSB_HOME"])
	}
	if env["SSL_CERT_FILE"] != "/etc/ssl/custom.pem" {
		t.Fatalf("SSL_CERT_FILE = %q, want forwarded value", env["SSL_CERT_FILE"])
	}
}

func TestVersionParsesRealShapes(t *testing.T) {
	cases := []struct {
		raw     string
		want    Version
		wantErr bool
	}{
		{raw: "msb 0.6.16", want: Version{0, 6, 16, "msb 0.6.16"}},
		{raw: "microsandbox-cli 0.6.16 (a1b2c3d)", want: Version{0, 6, 16, "microsandbox-cli 0.6.16 (a1b2c3d)"}},
		{raw: "0.10.2", want: Version{0, 10, 2, "0.10.2"}},
		{raw: "no version here", wantErr: true},
		{raw: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseVersion(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseVersion(%q): want error, got %+v", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseVersion(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseVersion(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b Version
		want bool
	}{
		{Version{0, 6, 15, ""}, MinVersion, true},
		{Version{0, 6, 16, ""}, MinVersion, false},
		{Version{0, 6, 17, ""}, MinVersion, false},
		{Version{0, 5, 99, ""}, MinVersion, true},
		{Version{1, 0, 0, ""}, MinVersion, false},
	}
	for _, tc := range cases {
		if got := tc.a.Less(tc.b); got != tc.want {
			t.Errorf("%s.Less(%s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestExecStreamsAndSeparatesStdoutStderr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"exec": {ExitCode: 0, Stderr: "stderr-marker\n", EchoStdin: true},
	}})

	var stdout, stderr bytes.Buffer
	res, err := c.Exec(context.Background(), ExecSpec{
		Name:    "sbx",
		Command: []string{"cat"},
		Stdin:   strings.NewReader("hello-from-host"),
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if stdout.String() != "hello-from-host" {
		t.Fatalf("stdout = %q, want the piped stdin echoed back — proves a real pipe, not the terminal", stdout.String())
	}
	if stderr.String() != "stderr-marker\n" {
		t.Fatalf("stderr = %q, want stderr-marker only — stdout/stderr must stay separated", stderr.String())
	}
	if strings.Contains(stdout.String(), "stderr-marker") {
		t.Fatalf("stderr leaked into stdout: %q", stdout.String())
	}

	call := lastFakeCall(t, home)
	if !slicesContain(call.Args, "--stream") {
		t.Fatalf("argv %v missing --stream", call.Args)
	}
}

func TestExecNonZeroWithOutputMapsToExitCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"exec": {ExitCode: 7, Stdout: "agent produced this before failing\n"},
	}})

	var stdout bytes.Buffer
	res, err := c.Exec(context.Background(), ExecSpec{Name: "sbx", Command: []string{"false"}, Stdout: &stdout})
	if err != nil {
		t.Fatalf("Exec: %v (want a normal ExecResult, not an error — output was observed)", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
}

func TestExecNonZeroNoOutputMapsToErrMsbFailed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"exec": {ExitCode: 1}, // msb's own anyhow-error shape: exit 1, nothing ever streamed
	}})

	_, err := c.Exec(context.Background(), ExecSpec{Name: "sbx", Command: []string{"whatever"}})
	if !errors.Is(err, ErrMsbFailed) {
		t.Fatalf("Exec err = %v, want ErrMsbFailed (no output was observed, so exit 1 cannot be trusted as the agent's own code)", err)
	}
}

func TestContextReportsBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// msb 0.6.16's actual output, read from `msb context --format json` on macOS/aarch64.
	t.Run("local", func(t *testing.T) {
		c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
			"context": {ExitCode: 0, Stdout: `{"kind":"local","source":"MSB_BACKEND"}`},
		}})
		info, err := c.Context(context.Background())
		if err != nil {
			t.Fatalf("Context: %v", err)
		}
		if !info.IsLocal() {
			t.Fatalf("IsLocal() = false for backend %q, want true", info.Backend)
		}
		if info.Source != "MSB_BACKEND" {
			t.Errorf("Source = %q, want MSB_BACKEND — it is what shows krayt's pin reached msb", info.Source)
		}
	})

	t.Run("cloud is not local", func(t *testing.T) {
		c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
			"context": {ExitCode: 0, Stdout: `{"kind":"cloud","source":"MSB_PROFILE"}`},
		}})
		info, err := c.Context(context.Background())
		if err != nil {
			t.Fatalf("Context: %v", err)
		}
		if info.IsLocal() {
			t.Fatalf("IsLocal() = true for backend %q, want false", info.Backend)
		}
	})

	// The pre-0.6.16 guess. Kept working deliberately: the fallback keys are there to absorb a
	// rename, and a fallback with no test is a fallback nobody knows is broken.
	t.Run("legacy backend key still parses", func(t *testing.T) {
		c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
			"context": {ExitCode: 0, Stdout: `{"backend":"local"}`},
		}})
		info, err := c.Context(context.Background())
		if err != nil {
			t.Fatalf("Context: %v", err)
		}
		if !info.IsLocal() {
			t.Fatalf("IsLocal() = false for legacy shape, want true")
		}
	})

	// No recognised key: fail closed with an empty Backend, but keep the raw bytes so the caller
	// can show the operator what msb printed (doctor.go's contextCheck does).
	t.Run("unrecognised schema fails closed but keeps Raw", func(t *testing.T) {
		const raw = `{"some_future_field":"local"}`
		c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
			"context": {ExitCode: 0, Stdout: raw},
		}})
		info, err := c.Context(context.Background())
		if err != nil {
			t.Fatalf("Context: %v", err)
		}
		if info.IsLocal() {
			t.Fatalf("IsLocal() = true for an unrecognised schema, want false")
		}
		if info.Backend != "" {
			t.Errorf("Backend = %q, want empty", info.Backend)
		}
		if !strings.Contains(string(info.Raw), "some_future_field") {
			t.Errorf("Raw = %q, want msb's original output preserved", info.Raw)
		}
	})
}

func TestTeardownRunsUnderAlreadyCancelledContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Default: fakeResponse{ExitCode: 0}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before Stop/Remove are even called

	if err := c.Stop(ctx, "sbx"); err != nil {
		t.Fatalf("Stop under a cancelled context: %v (teardown must still run)", err)
	}
	if err := c.Remove(ctx, "sbx"); err != nil {
		t.Fatalf("Remove under a cancelled context: %v (teardown must still run)", err)
	}

	calls := readFakeCalls(t, home)
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls, want 2 (stop, rm)", len(calls))
	}
	if calls[0].Args[0] != "stop" || calls[1].Args[0] != "rm" {
		t.Fatalf("calls = %v, want [stop ...] then [rm ...]", calls)
	}
	if !slicesContain(calls[1].Args, "--force") {
		t.Fatalf("rm args %v missing --force", calls[1].Args)
	}
}

func TestTeardownStillBoundedByATimeout(t *testing.T) {
	// Not a behavioral test of the timeout firing (that would need a slow fake and a slow
	// test) — just documents/pins that teardownTimeout is a real, positive bound, since Stop/
	// Remove's context.WithoutCancel alone never expires on its own.
	if teardownTimeout <= 0 {
		t.Fatalf("teardownTimeout = %v, want > 0", teardownTimeout)
	}
	if teardownTimeout < time.Second {
		t.Fatalf("teardownTimeout = %v, suspiciously short", teardownTimeout)
	}
}

func TestSystemLogsRendersArgsAndReturnsStdout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"logs": {ExitCode: 0, Stdout: `{"source":"system","line":"boot ok"}` + "\n"},
	}})

	out, err := c.SystemLogs(context.Background(), "krayt-run-1")
	if err != nil {
		t.Fatalf("SystemLogs: %v", err)
	}
	if !strings.Contains(string(out), "boot ok") {
		t.Errorf("SystemLogs output = %q, want it to contain the fake's stdout", out)
	}
	call := lastFakeCall(t, home)
	want := []string{"logs", "--source", "system", "--json", "krayt-run-1"}
	if !reflect.DeepEqual(call.Args, want) {
		t.Errorf("SystemLogs args = %v, want %v", call.Args, want)
	}
}

func TestSystemLogsErrorsIncludeStderr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"logs": {ExitCode: 1, Stderr: "sandbox not found"},
	}})
	_, err := c.SystemLogs(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "sandbox not found") {
		t.Errorf("SystemLogs error = %v, want it to surface stderr", err)
	}
}

func TestImagesParsesToleratesFieldNameVariants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"images": {ExitCode: 0, Stdout: `[` +
			`{"reference":"ghcr.io/x/agent:latest","size":1048576},` +
			`{"name":"ghcr.io/x/other:v1","size_bytes":2048}` +
			`]`},
	}})

	imgs, err := c.Images(context.Background())
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	want := []ImageInfo{
		{Ref: "ghcr.io/x/agent:latest", SizeB: 1048576},
		{Ref: "ghcr.io/x/other:v1", SizeB: 2048},
	}
	if len(imgs) != len(want) {
		t.Fatalf("Images = %+v, want %d entries", imgs, len(want))
	}
	for i, w := range want {
		if imgs[i].Ref != w.Ref || imgs[i].SizeB != w.SizeB {
			t.Errorf("Images[%d] = %+v, want %+v", i, imgs[i], w)
		}
	}
	call := lastFakeCall(t, home)
	want2 := []string{"images", "--format", "json"}
	if !reflect.DeepEqual(call.Args, want2) {
		t.Errorf("Images args = %v, want %v", call.Args, want2)
	}
}

func TestImageRefsSplitsLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"images": {ExitCode: 0, Stdout: "ghcr.io/x/agent:latest\nghcr.io/x/other:v1\n\n"},
	}})

	refs, err := c.ImageRefs(context.Background())
	if err != nil {
		t.Fatalf("ImageRefs: %v", err)
	}
	want := []string{"ghcr.io/x/agent:latest", "ghcr.io/x/other:v1"}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("ImageRefs = %v, want %v", refs, want)
	}
	call := lastFakeCall(t, home)
	wantArgs := []string{"images", "-q"}
	if !reflect.DeepEqual(call.Args, wantArgs) {
		t.Errorf("ImageRefs args = %v, want %v", call.Args, wantArgs)
	}
}

func TestRmiRendersArgs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"rmi": {ExitCode: 0},
	}})

	if err := c.Rmi(context.Background(), "ghcr.io/x/agent:latest", false); err != nil {
		t.Fatalf("Rmi: %v", err)
	}
	want := []string{"rmi", "ghcr.io/x/agent:latest"}
	if call := lastFakeCall(t, home); !reflect.DeepEqual(call.Args, want) {
		t.Errorf("Rmi args = %v, want %v", call.Args, want)
	}

	if err := c.Rmi(context.Background(), "ghcr.io/x/agent:latest", true); err != nil {
		t.Fatalf("Rmi --force: %v", err)
	}
	wantForce := []string{"rmi", "--force", "ghcr.io/x/agent:latest"}
	if call := lastFakeCall(t, home); !reflect.DeepEqual(call.Args, wantForce) {
		t.Errorf("Rmi --force args = %v, want %v", call.Args, wantForce)
	}
}

func TestRmiErrorIncludesStderr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"rmi": {ExitCode: 1, Stderr: "image in use"},
	}})
	err := c.Rmi(context.Background(), "ghcr.io/x/agent:latest", false)
	if err == nil || !strings.Contains(err.Error(), "image in use") {
		t.Errorf("Rmi error = %v, want it to surface stderr", err)
	}
}

func TestImagePruneRendersArgs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Responses: map[string]fakeResponse{
		"image": {ExitCode: 0},
	}})
	if err := c.ImagePrune(context.Background()); err != nil {
		t.Fatalf("ImagePrune: %v", err)
	}
	want := []string{"image", "prune"}
	if call := lastFakeCall(t, home); !reflect.DeepEqual(call.Args, want) {
		t.Errorf("ImagePrune args = %v, want %v", call.Args, want)
	}
}

func slicesContain(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestMsbDurationIsSingleUnit pins the rendering msb's parser actually accepts. msb takes one
// integer plus one unit; time.Duration.String()'s composite form ("30m0s", "1h0m0s") makes it
// exit with "invalid digit found in string" before it opens its database, so every duration
// krayt puts on argv is whole seconds.
func TestMsbDurationIsSingleUnit(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{30 * time.Minute, "1800s"},     // Duration.String() would say "30m0s"
		{time.Hour, "3600s"},            // Duration.String() would say "1h0m0s"
		{90 * time.Minute, "5400s"},     // Duration.String() would say "1h30m0s"
		{1500 * time.Millisecond, "2s"}, // rounds up; never truncates toward "0s"
		{time.Millisecond, "1s"},        // a sub-second limit stays a limit
	}
	for _, tc := range cases {
		if got := msbDuration(tc.in); got != tc.want {
			t.Errorf("msbDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
