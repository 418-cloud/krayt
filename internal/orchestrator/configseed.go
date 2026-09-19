package orchestrator

// This file is §7's new step (seed-agent-first-run-config.md, KRAYT_SPEC.md §6.14 "First-run
// state"): before the agent exec (Run) or the tty attach (Shell), fill in each selected adapter's
// first-run config file inside the guest — Claude Code's hasCompletedOnboarding/
// customApiKeyResponses, Gemini CLI's security.auth.selectedType — so an agent (or a human
// starting one by hand inside `krayt shell`) authenticates without hitting onboarding or an auth
// dialog, in ANY image with the CLI installed, not just the published ones.
//
// Every guest step here runs as the sandbox's own non-root user (the image's USER, §8.2), never
// root (decision 4): file ownership then
// comes out right (the agent rewrites these files constantly), and no root process writes through
// a path the image controls. It is best-effort (decision 6): a failure never fails the run or
// session, just prints one warning line and moves on to the next seed.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/418-cloud/krayt/internal/configseed"
	"github.com/418-cloud/krayt/internal/sandbox"
	"github.com/418-cloud/krayt/internal/task"
)

// dirEnvNameRE bounds ConfigSeed.DirEnv before it is interpolated into a shell script's text
// (decision 4.1) — the one place a seed's guest-supplied value cannot travel as a positional
// argument, because sh has no positional-argument mechanism for "expand the variable named by
// this argument". Anchored to shell-identifier characters only, so nothing it admits can break
// out of the `${NAME:-$HOME}` expansion it's spliced into.
var dirEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validSeedPath enforces adapter.ConfigSeed.Path's documented contract — "clean, relative, and
// carries no '..' element" — at the one place it is actually joined to a base directory. Today
// every Path is a compile-time constant in internal/adapter, so nothing can currently violate
// it; this is what keeps a future adapter's `../settings.json` typo from silently writing
// outside the resolved config dir instead of failing loudly, the same way an out-of-contract
// DirEnv already does.
func validSeedPath(p string) bool {
	if p == "" || path.IsAbs(p) || p != path.Clean(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// maxConfigSeedFile bounds the existing guest file applyConfigSeed reads before merging into it
// (decision 4.2) — the guest is untrusted (§10), so nothing else bounds how large a file a
// hostile or merely strange image could hand back.
const maxConfigSeedFile = 1 << 20 // 1 MiB

// applyConfigSeeds seeds every one of spec's first-run config files, in order, against the given
// sandbox — §7's new step, shared by Run and Shell (decision 5). warnOut receives one line per
// seed that failed, naming the guest path and the reason (decision 6); nil discards them. A
// failure on one seed does not stop the rest from being attempted.
func applyConfigSeeds(ctx context.Context, sb *sandbox.Client, name, user string, seeds []task.ConfigSeed, warnOut io.Writer) {
	for _, seed := range seeds {
		if err := applyConfigSeed(ctx, sb, name, user, seed); err != nil {
			warnConfigSeed(warnOut, seed, err)
		}
	}
}

func warnConfigSeed(out io.Writer, seed task.ConfigSeed, err error) {
	if out == nil {
		return
	}
	base := "$HOME"
	if seed.DirEnv != "" {
		base = "${" + seed.DirEnv + ":-$HOME}"
	}
	_, _ = fmt.Fprintf(out, "warning: config seed %s/%s: %v\n", base, seed.Path, err)
}

// applyConfigSeed resolves one seed's base directory, reads its current guest file (if any),
// merges seed.Defaults into it (configseed.Apply, decision 3), and writes the result back only if
// the merge actually changed something (decision 4.4).
func applyConfigSeed(ctx context.Context, sb *sandbox.Client, name, user string, seed task.ConfigSeed) error {
	if seed.DirEnv != "" && !dirEnvNameRE.MatchString(seed.DirEnv) {
		return fmt.Errorf("invalid DirEnv %q", seed.DirEnv)
	}
	if !validSeedPath(seed.Path) {
		return fmt.Errorf("invalid Path %q: must be clean, relative, and free of %q", seed.Path, "..")
	}
	base := guestBaseDir(ctx, sb, name, user, seed.DirEnv)
	if base == "" {
		return fmt.Errorf("resolve base directory (DirEnv=%q)", seed.DirEnv)
	}
	guestPath := path.Join(base, seed.Path)

	existing, err := readSeedFile(ctx, sb, name, user, guestPath)
	if err != nil {
		return fmt.Errorf("%s: read: %w", guestPath, err)
	}
	merged, changed, err := configseed.Apply(existing, seed.Defaults)
	if err != nil {
		return fmt.Errorf("%s: %w", guestPath, err)
	}
	if !changed {
		return nil
	}
	if err := writeSeedFile(ctx, sb, name, user, guestPath, merged); err != nil {
		return fmt.Errorf("%s: write: %w", guestPath, err)
	}
	return nil
}

// capBuffer caps how much of an exec's stdout readSeedFile keeps, discarding (not erroring) past
// the limit and remembering that it did — decision 4.2's read cap.
type capBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil
	}
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		c.over = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.over = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// seedMissingExit is the exit status readScript uses for "the file is not there", distinct from
// any status `cat` itself can produce (1 or 2) — the one thing decision 4.2's "a missing file
// means {}" may NOT be conflated with is a file that exists but could not be read.
const seedMissingExit = 3

// readScript tests for the file's existence itself instead of inferring it from a failed `cat`.
// The two are not equivalent: a config that exists but is unreadable by the sandbox user (a
// root-owned 0600 file in a user-writable directory, say) would otherwise read as `{}`, merge
// cleanly, and get replaced by writeSeedFile's `mv -f` — silently overwriting a file krayt is
// contractually forbidden to overwrite. Absence exits seedMissingExit; any other failure keeps
// `cat`'s own non-zero status and is reported as an error, so the seed is skipped with a warning.
// The missing branch writes to stderr deliberately: sandbox.Exec's ErrMsbFailed heuristic treats
// a non-zero exit with no output on EITHER stream as "the command never ran", and a missing file
// is the common case, not a driver failure.
const readScript = `p=$1
if [ ! -e "$p" ]; then echo "krayt-seed-read: no such file" >&2; exit 3; fi
cat "$p"`

// readSeedFile execs readScript as user and returns the file's current bytes. A missing file is
// decision 4.2's "{}": nil bytes, no error. An existing-but-unreadable file — or any other
// failure — is an error, which applyConfigSeeds turns into one warning line and no write.
func readSeedFile(ctx context.Context, sb *sandbox.Client, name, user, guestPath string) ([]byte, error) {
	cb := &capBuffer{limit: maxConfigSeedFile}
	var stderr bytes.Buffer
	res, err := sb.Exec(ctx, sandbox.ExecSpec{
		Name: name, User: user,
		Command: []string{"sh", "-c", readScript, "sh", guestPath},
		Stdout:  cb,
		Stderr:  &stderr,
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode == seedMissingExit {
		return nil, nil // missing: {} per decision 4.2
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(stderr.String()))
	}
	if cb.over {
		return nil, fmt.Errorf("exceeds %d bytes, skipping", maxConfigSeedFile)
	}
	return cb.buf.Bytes(), nil
}

// writeSeedFile execs a small shell script, as user, that mkdir -p's guestPath's parent,
// writes the merged bytes (carried on ExecSpec.Stdin) to a temp file in that same directory, then
// mv -f's it over guestPath (decision 4.4) — atomic from any concurrent reader's point of view,
// and never a partial file if the write is interrupted. Every path travels as a positional shell
// argument (`sh -c '…' sh "$1" "$2" "$3"`), never interpolated into the script text, since
// guestPath is derived from a guest-controlled DirEnv value that could contain anything.
//
// The script opens with `umask 077` because the replacement is a NEW inode: the temp file is
// created with whatever umask the guest shell inherited (typically 022 → 0644), and `mv -f`
// carries that mode onto the destination, so seeding an existing 0600 config would otherwise
// widen it to world-readable — and these files carry account and auth state (Claude Code's
// customApiKeyResponses, Gemini's selected auth type). 077 is deliberately one-way: krayt may
// tighten a seeded config's mode, never loosen it. mkdir -p inherits the same umask, which is
// the right default for a per-user config dir (~/.claude, ~/.gemini) and leaves an existing
// directory's mode alone.
func writeSeedFile(ctx context.Context, sb *sandbox.Client, name, user, guestPath string, content []byte) error {
	dir := path.Dir(guestPath)
	tmp := path.Join(dir, "."+path.Base(guestPath)+".krayt-seed-tmp")
	const script = `umask 077
dir=$1; tmp=$2; dst=$3
mkdir -p "$dir" && cat > "$tmp" && mv -f "$tmp" "$dst" || { echo "krayt-config-seed: write failed" >&2; exit 1; }`
	var stderr bytes.Buffer
	res, err := sb.Exec(ctx, sandbox.ExecSpec{
		Name: name, User: user,
		Command: []string{"sh", "-c", script, "sh", dir, tmp, guestPath},
		Stdin:   bytes.NewReader(content),
		Stderr:  &stderr,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return nil
}
