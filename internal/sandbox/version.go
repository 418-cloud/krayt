package sandbox

import (
	"fmt"
	"regexp"
	"strconv"
)

// MinVersion is the msb version the ADR (docs/adr-microsandbox-sandbox-layer.md) was verified
// against. msb is beta and has shipped a breaking wire change in a patch release, so krayt
// refuses to trust anything older: krayt doctor reports a hard failure and the run pre-flight
// refuses (add-msb-sandbox-driver.md decision 7).
var MinVersion = Version{Major: 0, Minor: 6, Patch: 16}

// Version is a parsed `msb --version`. Raw keeps the exact trimmed output for diagnostics —
// doctor and error messages show it verbatim rather than krayt's re-derived numbers, so a
// parsing mismatch is visible instead of silently papered over.
type Version struct {
	Major, Minor, Patch int
	Raw                 string
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Less reports whether v is older than o, comparing major.minor.patch lexicographically.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// semverPattern matches the first dotted-triple in `msb --version` output. The exact surrounding
// text (a leading binary name, a trailing build hash — the same shape firecracker's `--version`
// has, see internal/cli/doctor_linux.go's firecrackerCheck) is not load-bearing; only the version
// triple is.
var semverPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// parseVersion extracts a Version from raw `msb --version` output. It is deliberately tolerant
// of surrounding text rather than anchored to an exact format, since that format is not pinned
// by anything in the ADR or msb's own docs — only the presence of a semver triple is.
func parseVersion(raw string) (Version, error) {
	m := semverPattern.FindStringSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("sandbox: no version found in msb --version output %q", raw)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return Version{Major: major, Minor: minor, Patch: patch, Raw: raw}, nil
}
