package patch

import (
	"bufio"
	"bytes"
	"path"
	"strconv"
	"strings"
)

// Finding is one patch-safety concern: a changed path that can execute code outside the
// agent's workspace edit (a git hook, a CI job, a shell startup file, a newly-executable
// file). The lint is a review aid, not a gate — the human still reviews changes.patch — so
// it errs toward a small, curated, low-false-positive set (§14 Phase 5).
type Finding struct {
	Path   string // repo-relative path from the diff
	Reason string // why it warrants a closer look
}

// pathRule flags a changed path by a predicate on it. Ordered most-specific first; a path
// matches at most one rule (first win) so a file is never double-reported for its location.
type pathRule struct {
	match  func(p string) bool
	reason string
}

var pathRules = []pathRule{
	{gitHook, "modifies a git hook (runs automatically on git operations)"},
	{ciConfig, "modifies CI configuration (runs in your pipeline with its credentials)"},
	{shellStartup, "modifies a shell startup file (runs on your next shell/login)"},
	{direnv, "adds/edits a direnv .envrc (auto-executes when you cd into the repo)"},
}

// Lint scans a unified diff for changes that can execute outside the reviewed workspace
// edit. It reports each suspicious path once, in first-seen order. Best-effort: it reads the
// `diff --git`/mode lines, so a path with an embedded " b/" is parsed leniently rather than
// perfectly — a lint, not a security boundary.
func Lint(diff []byte) []Finding {
	var out []Finding
	seen := map[string]bool{}
	add := func(p, reason string) {
		if p == "" {
			return
		}
		key := p + "\x00" + reason
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Finding{Path: p, Reason: reason})
	}

	var cur string
	sc := bufio.NewScanner(bytes.NewReader(diff))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024) // tolerate long/binary lines
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			cur = diffGitPath(line)
			for _, r := range pathRules {
				if r.match(cur) {
					add(cur, r.reason)
					break
				}
			}
		case strings.HasPrefix(line, "new file mode "), strings.HasPrefix(line, "new mode "):
			// A file created or chmod'd executable (any of the x bits) is worth a look even
			// when its path is unremarkable.
			if execMode(line) {
				add(cur, "becomes executable (mode +x)")
			}
		}
	}
	return out
}

// diffGitPath extracts the post-image path from a `diff --git a/<p> b/<p>` line. It takes the
// text after the last " b/" (so a rename `a/old b/new` yields new), which is correct unless a
// path literally contains " b/"; acceptable for a heuristic lint.
func diffGitPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	i := strings.LastIndex(rest, " b/")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(rest[i+3:])
}

// execMode reports whether a "new file mode <octal>" / "new mode <octal>" line grants any
// execute bit.
func execMode(line string) bool {
	fields := strings.Fields(line)
	oct := fields[len(fields)-1]
	m, err := strconv.ParseInt(oct, 8, 32)
	if err != nil {
		return false
	}
	return m&0o111 != 0
}

func gitHook(p string) bool {
	return strings.Contains(p, ".git/hooks/") ||
		strings.HasPrefix(p, ".githooks/") || strings.Contains(p, "/.githooks/")
}

func ciConfig(p string) bool {
	if strings.HasPrefix(p, ".github/workflows/") || strings.Contains(p, "/.github/workflows/") {
		return true
	}
	if strings.HasPrefix(p, ".circleci/") || strings.Contains(p, "/.circleci/") {
		return true
	}
	switch path.Base(p) {
	case ".gitlab-ci.yml", "Jenkinsfile", "azure-pipelines.yml", "bitbucket-pipelines.yml", ".drone.yml":
		return true
	}
	return false
}

func shellStartup(p string) bool {
	switch path.Base(p) {
	case ".bashrc", ".bash_profile", ".bash_login", ".profile", ".zshrc", ".zprofile", ".zshenv":
		return true
	}
	return false
}

func direnv(p string) bool { return path.Base(p) == ".envrc" }
