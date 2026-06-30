// Package secrets is the host side of per-task secret handling (§6.8). It loads a
// secrets file into a map that the orchestrator pushes over the control channel
// (SecretsBundle), and provides a Redactor that scrubs secret values out of log lines so
// they never reach the RunEvent stream, persisted logs, or artifacts.
//
// Everything here is OS-agnostic. The guest mounts the values on tmpfs at /run/secrets and
// applies the Redactor to container output before streaming it (§6.5, §6.8).
package secrets

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
)

// RedactionMarker replaces a secret value wherever it appears in logs.
const RedactionMarker = "[REDACTED]"

// Load reads a per-task secrets file in dotenv form — `KEY=VALUE` lines, with `#` comments,
// blank lines, an optional `export ` prefix, and optional surrounding single/double quotes
// on the value (§6.8). It returns the key→value map. The YAML form (secrets.yaml) noted in
// §6.8 is not parsed here yet.
func Load(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	values := map[string]string{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		raw = strings.TrimPrefix(raw, "export ")
		k, v, ok := strings.Cut(raw, "=")
		if !ok {
			return nil, fmt.Errorf("secrets: %s:%d: not KEY=VALUE: %q", path, line, sc.Text())
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("secrets: %s:%d: empty key", path, line)
		}
		values[k] = unquote(strings.TrimSpace(v))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("secrets: read %s: %w", path, err)
	}
	return values, nil
}

// unquote strips one layer of matching single or double quotes.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// Redactor replaces known secret values with RedactionMarker. It is built once per run from
// the secret values and applied to every container log line in the guest before the line is
// streamed to the host, so secret material never crosses the wire in logs (§6.5, §6.8).
type Redactor struct {
	values [][]byte // non-empty secret values, longest first
}

// NewRedactor builds a Redactor from secret values. Empty values are ignored (they would
// match everywhere), and values are sorted longest-first so a value that contains another
// is replaced before its substring.
func NewRedactor(values []string) *Redactor {
	uniq := map[string]struct{}{}
	var vs [][]byte
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, seen := uniq[v]; seen {
			continue
		}
		uniq[v] = struct{}{}
		vs = append(vs, []byte(v))
	}
	sort.Slice(vs, func(i, j int) bool { return len(vs[i]) > len(vs[j]) })
	return &Redactor{values: vs}
}

// Redact returns a copy of b with every secret value replaced by RedactionMarker. With no
// secrets it returns b unchanged. Redaction is substring-based per call, so a secret split
// across two log chunks would not be caught — acceptable for line-oriented container logs.
func (r *Redactor) Redact(b []byte) []byte {
	if r == nil || len(r.values) == 0 {
		return b
	}
	out := b
	for _, v := range r.values {
		if bytes.Contains(out, v) {
			out = bytes.ReplaceAll(out, v, []byte(RedactionMarker))
		}
	}
	return out
}

// Values returns the secret values, for building a Redactor on the guest from a received
// SecretsBundle.
func Values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
