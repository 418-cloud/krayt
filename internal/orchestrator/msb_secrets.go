package orchestrator

import (
	"fmt"
	"os"

	"github.com/418-cloud/krayt/internal/secrets"
)

// PatchSecretKeys is the host-side counterpart to secretPatchKeys (orchestrator.go), for an msb
// run (hand-secrets-to-msb.md decision 6). Under B1 no secret value ever enters the guest — it
// travels only krayt's own memory -> msb's child env (§6.8) — so there is nothing for a guest-side
// scan to find, and add-krayt-guest-helper.md's guest scanner requirement drops out entirely. The
// host already holds every secret value AND already copied changes.patch out of the sandbox, so
// it scans its own copy directly — strictly more trustworthy than trusting a guest report, since
// the agent inside the sandbox never runs this scanner and cannot tamper with it.
//
// It reuses secrets.ScanKeys rather than defining a second notion of "this looks like the
// value" — the same substring-over-the-whole-buffer matching §6.8's (pre-msb) guest scan and the
// artifact redactor both already use. values is the map the host loaded from secrets.env
// (pushSecrets, or its msb-era equivalent); values never appear in the returned key list, only
// their names.
//
// Not called anywhere yet — run-tasks-on-microsandbox.md wires it in at the msb cut-over,
// replacing secretPatchKeys's guest-report cross-check the way this function's doc comment
// describes.
func PatchSecretKeys(patchPath string, values map[string]string) ([]string, error) {
	b, err := os.ReadFile(patchPath)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: read %s: %w", patchPath, err)
	}
	return secrets.ScanKeys(values, b), nil
}
