package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/418-cloud/krayt/internal/sandbox"
)

// trustWorkspaceScript adds $1 to the running user's own global git config as a safe.directory,
// once. /workspace is owned by root (krayt-helper clones it as root, then only relaxes its mode),
// so without this every git command the sandbox user runs there fails with "detected dubious
// ownership" (run_499009c0, on the published claude-code image, which has no
// krayt-agent-shellenv). An image without git has nothing to fix, and `--fixed-value --get-all`
// keeps a second run from adding a duplicate. It always prints something, because Exec reports a
// non-zero exit with no output as a driver failure.
const trustWorkspaceScript = `command -v git >/dev/null 2>&1 || { echo "no git"; exit 0; }
git config --global --fixed-value --get-all safe.directory "$1" >/dev/null 2>&1 && { echo "already trusted"; exit 0; }
git config --global --add safe.directory "$1" && echo "trusted" || { echo "krayt-git-safe-directory: git config --global failed" >&2; exit 1; }`

// trustWorkspaceForGit marks /workspace as a git safe.directory for the sandbox's own user, in
// that user's global git config and never as root. It runs as user, so it does not touch root's
// git: krayt-helper's git ignores global config (GIT_CONFIG_GLOBAL=/dev/null, internal/patch) and
// must keep refusing a .git the agent could have swapped for one it owns. That is why this is not
// a create-time GIT_CONFIG_* variable, which every exec, root's included, would inherit.
//
// It is image-agnostic, which is what makes the safe.directory line in the published images'
// krayt-agent-shellenv/entrypoint a redundant duplicate. It is best-effort, like config seeds:
// a failure prints one warning line to warnOut (nil discards it) and never fails the run or
// session.
func trustWorkspaceForGit(ctx context.Context, sb *sandbox.Client, name, user string, warnOut io.Writer) {
	var stderr bytes.Buffer
	res, err := sb.Exec(ctx, sandbox.ExecSpec{
		Name: name, User: user,
		Command: []string{"sh", "-c", trustWorkspaceScript, "sh", containerWorkspace},
		Stdout:  io.Discard,
		Stderr:  &stderr,
	})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(stderr.String()))
	}
	if err != nil && warnOut != nil {
		_, _ = fmt.Fprintf(warnOut, "warning: git safe.directory for %s: %v\n", containerWorkspace, err)
	}
}
