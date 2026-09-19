package orchestrator

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/418-cloud/krayt/internal/sandbox"
)

// resolveSandboxUser returns the user a run's or shell session's sandbox runs as: the image's own
// USER (§8.2), read with `msb image inspect` (pulling the image first if it isn't cached yet).
// krayt used to force a fixed `agent` user, which msb 0.6.16 refuses to boot on any image without
// a user of exactly that name (`agentd: init failed: exec session error: guest user not found:
// agent`, run_985a6aae) — including krayt's own gemini-cli image, which runs as `node`.
func resolveSandboxUser(ctx context.Context, sb *sandbox.Client, imageRef string) (string, error) {
	user, err := sb.ImageUser(ctx, imageRef)
	if err != nil {
		return "", fmt.Errorf("orchestrator: read image user: %w", err)
	}
	if err := validateSandboxUser(imageRef, user); err != nil {
		return "", err
	}
	return user, nil
}

// validateSandboxUser refuses an image whose USER is unset or root (§8.2: enforced, not just a
// convention). user is an OCI USER value — `name`, `uid`, `name:group` or `uid:gid` — which msb's
// --user accepts unchanged. Only the user half decides root-ness. A non-root NAME that the image's
// /etc/passwd maps to uid 0 is not detectable from here without reading the guest's passwd; that
// is an accepted residual.
func validateSandboxUser(imageRef, user string) error {
	name, _, _ := strings.Cut(user, ":")
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return fmt.Errorf("orchestrator: image %s sets no USER, so it would run as root; krayt requires a non-root USER (§8.2) — add one to its Dockerfile, e.g. `RUN useradd --create-home agent` then `USER agent`", imageRef)
	case name == "root" || isZeroUID(name):
		return fmt.Errorf("orchestrator: image %s has USER %q, which is root; krayt requires a non-root USER (§8.2)", imageRef, user)
	}
	return nil
}

func isZeroUID(s string) bool {
	uid, err := strconv.ParseUint(s, 10, 32)
	return err == nil && uid == 0
}
