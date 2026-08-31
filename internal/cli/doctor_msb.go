package cli

import (
	"context"
	"time"

	"github.com/418-cloud/krayt/internal/sandbox"
)

// msbDoctorTimeout bounds the msb checks: each spawns a real subprocess (--version, `context
// --format json`, `msb doctor`), and doctor must not hang indefinitely if one of them does.
const msbDoctorTimeout = 15 * time.Second

// msbChecks bridges sandbox.DoctorChecks (add-msb-sandbox-driver.md) into this package's
// checkResult, appended to commonChecks. See sandbox.DoctorChecks' doc comment for why every one
// of these is currently optional.
func msbChecks() []checkResult {
	ctx, cancel := context.WithTimeout(context.Background(), msbDoctorTimeout)
	defer cancel()

	results := sandbox.DoctorChecks(ctx)
	out := make([]checkResult, 0, len(results))
	for _, r := range results {
		out = append(out, checkResult{name: r.Name, ok: r.OK, optional: r.Optional, detail: r.Detail})
	}
	return out
}
