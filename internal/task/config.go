package task

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config mirrors the optional krayt.yaml (§8.1). Every field is optional; the CLI overlays
// command-line flags on top, and flags win (defaults → file → flags, §8.3). Pointer/empty
// fields mean "unset" so the CLI can tell whether the file provided a value.
type Config struct {
	Image        string            `yaml:"image"`
	Task         string            `yaml:"task"`
	Repo         string            `yaml:"repo"`
	Secrets      string            `yaml:"secrets"`
	IncludeDirty *bool             `yaml:"include_dirty"`
	BundleDepth  *int              `yaml:"bundle_depth"`
	Env          map[string]string `yaml:"env"`
	Network      struct {
		Mode  string   `yaml:"mode"`
		Allow []string `yaml:"allow"`
	} `yaml:"network"`
	Resources struct {
		CPUs    *int   `yaml:"cpus"`
		Memory  string `yaml:"memory"`
		Disk    string `yaml:"disk"`
		Timeout string `yaml:"timeout"`
	} `yaml:"resources"`
	Questions struct {
		Mode      string `yaml:"mode"`
		Timeout   string `yaml:"timeout"`
		OnTimeout string `yaml:"on_timeout"`
	} `yaml:"questions"`
	Agent struct {
		Adapter string `yaml:"adapter"`
	} `yaml:"agent"`
	Container struct {
		Capabilities   []string `yaml:"capabilities"`    // opt-in caps re-granted to the drop-all default (§8.1)
		Seccomp        string   `yaml:"seccomp"`         // "" (default profile) | "unconfined"
		ReadonlyRootfs *bool    `yaml:"readonly_rootfs"` // opt-in; pointer distinguishes unset from explicit false
	} `yaml:"container"`
}

// LoadConfig reads and parses a krayt.yaml. Unknown keys are rejected so typos surface
// rather than being silently ignored.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// ParseMiB parses a memory size ("4GiB", "512MiB", or a bare number = MiB) into MiB.
// Fractional units are fine as long as they land on a whole MiB (e.g. "1.5GiB" = 1536); a
// value that would truncate (e.g. "1.5MiB") is rejected rather than silently rounded.
func ParseMiB(s string) (uint64, error) {
	n, unit, err := splitSize(s)
	if err != nil {
		return 0, err
	}
	var mib float64
	switch unit {
	case "", "m", "mb", "mib":
		mib = n
	case "g", "gb", "gib":
		mib = n * 1024
	default:
		return 0, fmt.Errorf("unknown memory unit %q (use MiB or GiB)", unit)
	}
	return wholeUnits(mib, s, "MiB")
}

// ParseGiB parses a disk size ("20GiB", "20480MiB", or a bare number = GiB) into GiB. As with
// ParseMiB, the result must be a whole number of the unit ("2048MiB" = 2 is fine; "512MiB"
// would be 0.5 GiB and is rejected rather than truncated to a 0-GiB disk).
func ParseGiB(s string) (uint64, error) {
	n, unit, err := splitSize(s)
	if err != nil {
		return 0, err
	}
	var gib float64
	switch unit {
	case "", "g", "gb", "gib":
		gib = n
	case "m", "mb", "mib":
		gib = n / 1024
	default:
		return 0, fmt.Errorf("unknown disk unit %q (use MiB or GiB)", unit)
	}
	return wholeUnits(gib, s, "GiB")
}

// wholeUnits converts a size that must be an exact whole number of `unit`-sized units to
// uint64, rejecting a fractional value rather than truncating it. size is non-negative
// (splitSize rejects signs), so the uint64 round-trip is a safe integrality check.
func wholeUnits(size float64, raw, unit string) (uint64, error) {
	u := uint64(size)
	if float64(u) != size {
		return 0, fmt.Errorf("invalid size %q: not a whole number of %s", raw, unit)
	}
	return u, nil
}

// splitSize splits "<number><unit>" into the number and a lowercased unit.
func splitSize(s string) (float64, string, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, "", fmt.Errorf("invalid size %q", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, "", fmt.Errorf("invalid size %q: %w", s, err)
	}
	return n, strings.ToLower(strings.TrimSpace(s[i:])), nil
}
