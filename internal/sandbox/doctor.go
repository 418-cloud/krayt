package sandbox

import (
	"context"
	"fmt"
)

// CheckResult is one host-prerequisite check, shaped for internal/cli's `krayt doctor` (§13) to
// render — deliberately a plain struct rather than importing internal/cli's own checkResult type,
// since this package must not import anything above it.
type CheckResult struct {
	Name     string
	OK       bool
	Optional bool
	Detail   string
}

// installHint is shown whenever msb cannot be found at all, so every check that depends on it
// gives the same actionable next step instead of repeating prose.
const installHint = "install from https://github.com/superradcompany/microsandbox " +
	"(or point " + BinEnv + " at an existing install)"

// DoctorChecks returns the msb prerequisite checks for `krayt doctor`: msb resolved (via PATH or
// BinEnv), its version against MinVersion, MSB_BACKEND=local resolution, and msb's own `msb
// doctor` passthrough — in that order, since each of the first three short-circuits the rest once
// msb can't be found at all.
//
// Every check here is MANDATORY (add-msb-sandbox-driver.md's original Optional:true was
// deliberately temporary, for the additive-only period before anything called this package —
// run-tasks-on-microsandbox.md wires `krayt run` through msb and deletes the vfkit/firecracker
// checks, so a host without a healthy msb install can no longer run anything and `krayt doctor`
// must fail loudly rather than warn quietly).
func DoctorChecks(ctx context.Context) []CheckResult {
	bin, err := resolveBin()
	if err != nil {
		skip := "skipped — msb not found"
		return []CheckResult{
			{Name: "msb found (PATH or " + BinEnv + ")", Detail: "msb not found — " + installHint},
			{Name: fmt.Sprintf("msb --version >= %s", MinVersion), Detail: skip},
			{Name: "msb context reports local backend", Detail: skip},
			{Name: "msb doctor", Detail: skip},
		}
	}

	c := &Client{Bin: bin}
	return []CheckResult{
		{Name: "msb found (PATH or " + BinEnv + ")", OK: true, Detail: bin},
		versionCheck(ctx, c),
		contextCheck(ctx, c),
		passthroughDoctorCheck(ctx, c),
	}
}

func versionCheck(ctx context.Context, c *Client) CheckResult {
	name := fmt.Sprintf("msb --version >= %s", MinVersion)
	v, err := c.Version(ctx)
	if err != nil {
		return CheckResult{Name: name, Detail: "msb --version failed: " + err.Error()}
	}
	if v.Less(MinVersion) {
		return CheckResult{Name: name, Detail: fmt.Sprintf(
			"msb %s is below krayt's minimum supported version %s (raw: %q) — upgrade msb", v, MinVersion, v.Raw)}
	}
	return CheckResult{Name: name, OK: true, Detail: v.String()}
}

func contextCheck(ctx context.Context, c *Client) CheckResult {
	const name = "msb context reports local backend"
	info, err := c.Context(ctx)
	if err != nil {
		return CheckResult{Name: name, Detail: "msb context --format json failed: " + err.Error()}
	}
	if !info.IsLocal() {
		backend := info.Backend
		if backend == "" {
			backend = "(unknown — could not identify a backend field in msb's json output)"
		}
		return CheckResult{Name: name, Detail: fmt.Sprintf(
			"resolved backend is %q, not %q — krayt pins %s=%s on every invocation, so this "+
				"indicates the pin is not taking effect; every `krayt run` will still refuse to "+
				"start until this is fixed", backend, BackendLocal, backendEnvKey, BackendLocal)}
	}
	return CheckResult{Name: name, OK: true, Detail: "backend=" + info.Backend}
}

func passthroughDoctorCheck(ctx context.Context, c *Client) CheckResult {
	const name = "msb doctor"
	out, stderr, err := c.runCaptured(ctx, []string{"doctor"})
	detail := firstNonEmpty(out, stderr)
	if err != nil {
		return CheckResult{Name: name, Detail: "msb doctor reported problems: " + detail}
	}
	return CheckResult{Name: name, OK: true, Detail: detail}
}
