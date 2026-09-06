package task

import (
	"fmt"
	"net/netip"
	"strings"
)

// hopByHopHeaders are never valid injection targets: they are connection-scoped (RFC 7230
// §6.1), not credential/opt-in headers, and rewriting them at the proxy layer would be actively
// wrong rather than merely pointless (add-tls-mitm-credential-injection.md §1/§6).
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// InjectedSecretKeys returns the set of secrets-file key NAMES referenced by any
// inject[].set rule — these must be withheld from SecretsBundle (§2,
// add-tls-mitm-credential-injection.md's load-bearing change): an injected credential is
// attached host-side and must never ship to the guest at all.
func (np NetworkPolicy) InjectedSecretKeys() map[string]bool {
	if len(np.Inject) == 0 {
		return nil
	}
	keys := map[string]bool{}
	for _, r := range np.Inject {
		for _, k := range r.Set {
			keys[k] = true
		}
		for _, k := range r.Withheld {
			keys[k] = true
		}
	}
	return keys
}

// MergeInjectRules unions adapter-produced inject rules into the user's own network.inject
// (§4, inject-claude-oauth-token-at-proxy.md): "adapter-supplied rules travel the same path as
// user-supplied ones; there is no second channel." adapterRules is folded in per-host — a host
// the user never mentioned is added as a new rule; a host the user already has a rule for is
// merged header-by-header, and on a shared header (in Set or SetLiteral, matched
// case-insensitively) the USER'S value wins and the collision is skipped rather than silently
// overwritten. Strip lists are unioned (no "value" to conflict over). A user Refresh block always
// wins over an adapter one for the same host. Returns the merged rule set plus one human-readable
// line per overridden header, for the caller to log — so a user who wrote their own rule for an
// adapter-managed host can see exactly what was and wasn't taken from the adapter.
//
// The result still needs ValidateNetworkPolicy — this function only merges, it enforces nothing
// (an adapter rule naming an unallowlisted host, for instance, is caught there, not here).
func MergeInjectRules(user, adapterRules []InjectRule) (merged []InjectRule, overrides []string) {
	merged = make([]InjectRule, len(user))
	byHost := make(map[string]int, len(user)) // lower(host) -> index into merged
	for i, r := range user {
		merged[i] = cloneInjectRule(r)
		byHost[lower(r.Host)] = i
	}

	for _, ar := range adapterRules {
		idx, exists := byHost[lower(ar.Host)]
		if !exists {
			byHost[lower(ar.Host)] = len(merged)
			merged = append(merged, cloneInjectRule(ar))
			continue
		}
		dst := &merged[idx]
		dst.Strip = unionHeaders(dst.Strip, ar.Strip)

		claimed := map[string]bool{}
		for h := range dst.Set {
			claimed[lower(h)] = true
		}
		for h := range dst.SetLiteral {
			claimed[lower(h)] = true
		}
		for h, key := range ar.Set {
			if claimed[lower(h)] {
				overrides = append(overrides, fmt.Sprintf("user config overrides adapter-supplied header %q on host %q", h, ar.Host))
				// The adapter's header entry is dropped, but its credential must still never reach
				// SecretsBundle — that's the whole point of injecting it host-side. Withhold it
				// independently of Set so InjectedSecretKeys() still catches it.
				dst.Withheld = appendUnique(dst.Withheld, key)
				continue // and its SetPrefix below is skipped with it: prefixing the USER'S value would corrupt it
			}
			if dst.Set == nil {
				dst.Set = map[string]string{}
			}
			dst.Set[h] = key
			claimed[lower(h)] = true
			if prefix, ok := LookupHeader(ar.SetPrefix, h); ok {
				if dst.SetPrefix == nil {
					dst.SetPrefix = map[string]string{}
				}
				dst.SetPrefix[h] = prefix
			}
		}
		for h, v := range ar.SetLiteral {
			if claimed[lower(h)] {
				overrides = append(overrides, fmt.Sprintf("user config overrides adapter-supplied header %q on host %q", h, ar.Host))
				continue
			}
			if dst.SetLiteral == nil {
				dst.SetLiteral = map[string]string{}
			}
			dst.SetLiteral[h] = v
			claimed[lower(h)] = true
		}
		if dst.Refresh == nil {
			dst.Refresh = ar.Refresh
		} else if ar.Refresh != nil {
			overrides = append(overrides, fmt.Sprintf("user config overrides adapter-supplied refresh block on host %q", ar.Host))
		}
	}
	return merged, overrides
}

// cloneInjectRule deep-copies the maps/slice a rule owns, so MergeInjectRules never mutates a
// caller's InjectRule in place.
func cloneInjectRule(r InjectRule) InjectRule {
	out := InjectRule{Host: r.Host, Refresh: r.Refresh}
	if r.Strip != nil {
		out.Strip = append([]string(nil), r.Strip...)
	}
	if r.Set != nil {
		out.Set = make(map[string]string, len(r.Set))
		for k, v := range r.Set {
			out.Set[k] = v
		}
	}
	if r.SetPrefix != nil {
		out.SetPrefix = make(map[string]string, len(r.SetPrefix))
		for k, v := range r.SetPrefix {
			out.SetPrefix[k] = v
		}
	}
	if r.SetLiteral != nil {
		out.SetLiteral = make(map[string]string, len(r.SetLiteral))
		for k, v := range r.SetLiteral {
			out.SetLiteral[k] = v
		}
	}
	if r.Withheld != nil {
		out.Withheld = append([]string(nil), r.Withheld...)
	}
	return out
}

// appendUnique appends v to ss unless it's already present.
func appendUnique(ss []string, v string) []string {
	for _, s := range ss {
		if s == v {
			return ss
		}
	}
	return append(ss, v)
}

// LookupHeader returns m's value for header h under any casing. Exported because SetPrefix, a
// header-keyed map on an InjectRule, is consumed outside this package —
// internal/orchestrator resolves SetPrefix while resolving Set — and every consumer must agree
// that header names are case-insensitive (RFC 7230 §3.2) while Go map keys are not.
func LookupHeader(m map[string]string, h string) (string, bool) {
	for k, v := range m {
		if lower(k) == lower(h) {
			return v, true
		}
	}
	return "", false
}

// unionHeaders merges two header-name lists, deduplicating case-insensitively while preserving a's
// original casing for anything already in it.
func unionHeaders(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, h := range a {
		if !seen[lower(h)] {
			seen[lower(h)] = true
			out = append(out, h)
		}
	}
	for _, h := range b {
		if !seen[lower(h)] {
			seen[lower(h)] = true
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ValidateNetworkPolicy fail-fasts every network.mitm/passthrough/inject rule at `krayt run`
// pre-flight — before any VM or image work — per §1 of add-tls-mitm-credential-injection.md.
// secretKeys is the set of key NAMES present in the task's secrets file (never values); pass an
// empty/nil map when there is no secrets file.
//
// One §1 rule is deliberately NOT checked here: "injection targets HTTPS only". Whether a given
// request to a rule's host arrives over plain HTTP or HTTPS is a runtime property of what the
// agent does, not something a static host name can encode — so it is enforced at request time
// instead, in the pre-msb host proxy's plain-HTTP handler path (a request to an injection-configured
// host over plain HTTP is refused with 400, never forwarded uninjected or injected in cleartext).
func ValidateNetworkPolicy(np NetworkPolicy, secretKeys map[string]bool) error {
	if len(np.Inject) > 0 && !np.MITM {
		return fmt.Errorf("network: inject requires mitm: true")
	}
	// Host entries are checked for shape BEFORE any cross-referencing, so a name msb could never
	// match fails the run here rather than vanishing from the effective policy later (see
	// validateHostPattern/validateHostEntry).
	for i, h := range np.Allow {
		if err := validateHostPattern(h); err != nil {
			return fmt.Errorf("network: allow[%d]: %w", i, err)
		}
	}
	for i, h := range np.Passthrough {
		if err := validateHostPattern(h); err != nil {
			return fmt.Errorf("network: passthrough[%d]: %w", i, err)
		}
	}

	if np.Mode == NetworkAllowlist {
		// COVERAGE, not overlap, and directional: allow must subsume everything the passthrough
		// entry could ever match. `allow: ["*.example.com"] + passthrough: ["api.example.com"]` is
		// valid; the reverse is not. See coversPattern.
		for _, h := range np.Passthrough {
			if !anyCovers(np.Allow, h) {
				return passthroughNotCoveredError(h, np.Allow)
			}
		}
	}

	seenHost := map[string]bool{}
	for i, rule := range np.Inject {
		host := lower(rule.Host)
		if host == "" {
			return fmt.Errorf("network: inject[%d]: host is required", i)
		}
		if err := validateHostPattern(rule.Host); err != nil {
			return fmt.Errorf("network: inject[%d]: %w", i, err)
		}
		// Deliberately EXACT-string, not coverage: "*.example.com" and "api.example.com" are two
		// legitimately distinct rules, not a duplicate of each other.
		if seenHost[host] {
			return fmt.Errorf("network: inject: host %q has more than one rule", host)
		}
		seenHost[host] = true

		// OVERLAP, not coverage, and symmetric: a wildcard passthrough swallowing an exact inject
		// host is the silent case — the host appears nowhere in the passthrough list literally, so
		// the error has to name the entry that shadowed it.
		if entry, ok := anyOverlaps(np.Passthrough, rule.Host); ok {
			return fmt.Errorf("network: inject[%d]: host %q is covered by passthrough entry %q — a "+
				"passthrough host is tunneled un-MITM'd and can never receive injection", i, host, entry)
		}
		if np.Mode == NetworkAllowlist && !anyCovers(np.Allow, rule.Host) {
			return fmt.Errorf("network: inject[%d]: host %q must also be in allow (mode: allowlist)", i, host)
		}
		if len(rule.Set) == 0 && len(rule.SetLiteral) == 0 {
			return fmt.Errorf("network: inject[%d] (%s): set or set_literal must name at least one header", i, host)
		}

		strip := rule.Strip
		if len(strip) == 0 {
			// "strip defaults to the key set of set" (§1) — SetLiteral is deliberately excluded: a
			// literal (e.g. a static opt-in header) is not a credential a guest could meaningfully
			// forge to smuggle a second value the way an auth header could.
			for h := range rule.Set {
				strip = append(strip, h)
			}
		}
		for _, h := range strip {
			if err := validateHeaderName(h); err != nil {
				return fmt.Errorf("network: inject[%d] (%s) strip: %w", i, host, err)
			}
		}
		// Header names are case-insensitive (RFC 7230 §3.2), but Go map keys are not — "X-Api-Key"
		// in set and "x-api-key" in set_literal would pass a case-sensitive check and then race
		// each other for the canonical header at injection time (net/http.Header.Set canonicalizes
		// both to the same key, and map iteration order is unspecified). setNames tracks every
		// header seen so far, lower-cased, to catch that case-insensitively — across set and
		// set_literal, and between two same-cased spellings within either map.
		setNames := map[string]bool{}
		for h, key := range rule.Set {
			if err := validateHeaderName(h); err != nil {
				return fmt.Errorf("network: inject[%d] (%s) set: %w", i, host, err)
			}
			if !secretKeys[key] {
				return fmt.Errorf("network: inject[%d] (%s): set[%q] names secrets-file key %q, which does not exist",
					i, host, h, key)
			}
			if setNames[lower(h)] {
				return fmt.Errorf("network: inject[%d] (%s): set has more than one header named %q (case-insensitive)", i, host, h)
			}
			setNames[lower(h)] = true
		}
		for h := range rule.SetLiteral {
			if err := validateHeaderName(h); err != nil {
				return fmt.Errorf("network: inject[%d] (%s) set_literal: %w", i, host, err)
			}
			if setNames[lower(h)] {
				return fmt.Errorf("network: inject[%d] (%s): header %q collides case-insensitively with another header in set/set_literal", i, host, h)
			}
			setNames[lower(h)] = true
		}
		// set_prefix modifies a set header's value rather than naming a header of its own, so it
		// must reference one — a prefix with nothing to prefix is a config error, not a no-op that
		// silently sends the credential without its scheme.
		for h, prefix := range rule.SetPrefix {
			if err := validateHeaderName(h); err != nil {
				return fmt.Errorf("network: inject[%d] (%s) set_prefix: %w", i, host, err)
			}
			if !hasHeaderKey(rule.Set, h) {
				return fmt.Errorf("network: inject[%d] (%s): set_prefix[%q] has no matching set[%q] entry to prefix", i, host, h, h)
			}
			if err := validateHeaderValue(prefix); err != nil {
				return fmt.Errorf("network: inject[%d] (%s) set_prefix[%q]: %w", i, host, h, err)
			}
		}

		if rule.Refresh != nil {
			r := rule.Refresh
			if r.Host == "" || r.PathPrefix == "" || len(r.ResponseTokenFields) == 0 {
				return fmt.Errorf("network: inject[%d] (%s): refresh requires host, path_prefix, and at "+
					"least one response_token_fields entry", i, host)
			}
			// A refresh host is dialed by the proxy like any other and is matched against the
			// same policy, so a shape the proxy could never honor is as broken here as in allow.
			if err := validateHostEntry(r.Host); err != nil {
				return fmt.Errorf("network: inject[%d] (%s) refresh: %w", i, host, err)
			}
		}
	}
	return nil
}

// hasHeaderKey reports whether m contains h under any casing — header names are case-insensitive
// (RFC 7230 §3.2) but Go map keys are not, so `Authorization` in set and `authorization` in
// set_prefix must still count as the same header.
func hasHeaderKey(m map[string]string, h string) bool {
	for k := range m {
		if lower(k) == lower(h) {
			return true
		}
	}
	return false
}

// validateHeaderValue rejects a non-empty literal that could not be sent as a header value: CR/LF
// would let a rule split one header into several (response/request splitting), and an empty value
// means the config expressed an intent it cannot carry out.
func validateHeaderValue(v string) error {
	if v == "" {
		return fmt.Errorf("value is empty")
	}
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("value contains a CR or LF")
	}
	return nil
}

// validateHeaderName rejects anything that is not a valid HTTP token (RFC 7230 §3.2.6) or that
// names a hop-by-hop header (§1, §6).
func validateHeaderName(h string) error {
	if !isHTTPToken(h) {
		return fmt.Errorf("invalid header name %q", h)
	}
	if hopByHopHeaders[lower(h)] {
		return fmt.Errorf("hop-by-hop header %q is not a valid injection target", h)
	}
	return nil
}

// isHTTPToken reports whether s is a valid RFC 7230 token — the grammar HTTP header field names
// must satisfy.
func isHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isTokenChar(r) {
			return false
		}
	}
	return true
}

func isTokenChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// validateHostPattern is the entry point for every host list krayt hands msb — `network.allow`,
// `network.passthrough`, and a secret's scope (`network.inject[].host`). It accepts an exact host,
// or one leading `*.` wildcard naming a domain suffix, and delegates everything after that prefix
// to validateHostEntry so there is exactly one copy of the byte-class, label, IPv6 and port rules.
//
// The `*.` spelling and its semantics are msb's, on all three flags by msb's own design
// ("Mirrors the syntax already used by --tls-bypass and --secret so users see one wildcard
// convention across the CLI", crates/cli/lib/net_rule.rs): `*.example.com` covers example.com
// itself plus any label-aligned subdomain. See hostCovers for the Go mirror.
//
// It mirrors msb's STRICTEST acceptance set, on every surface — including where msb itself would
// not. `--net-rule` is guarded (bare `*` refused; a single-label suffix refused as SuffixTooBroad,
// crates/network/lib/policy/name.rs), but the secret surface has no validation at all:
// HostPattern::parse (packages/microsandbox-types/rust/lib/domain.rs) is three lines that turn `*`
// into HostPattern::Any — msb's own doc for that variant reads "Any host (dangerous — secret can be
// exfiltrated)" — and `*.*.example.com` into a Wildcard matching nothing, a credential that
// mysteriously never substitutes. krayt's pre-flight is the only guard there, so it runs the same
// strict check on all three fields and is never more permissive than msb's guarded surface.
//
// What it deliberately does NOT do is keep a public-suffix list: `*.co.uk`, `*.github.io` and
// `*.s3.amazonaws.com` are accepted (KRAYT_SPEC.md §6.6, §10). No label-count threshold can
// separate `*.blob.core.windows.net` — four labels, multi-tenant, the case this exists for — from
// `*.example.com`, and a curated denylist is stale by construction. The gap is documented and
// surfaced in the pre-boot policy print (internal/cli.printNetworkPolicy) instead of pretended
// away.
func validateHostPattern(h string) error {
	s := strings.TrimSpace(h)
	suffix, isWildcard := strings.CutPrefix(s, "*.")
	if !isWildcard {
		if s == "*" {
			return fmt.Errorf("host %q is not a valid wildcard: krayt has no \"any host\" spelling. "+
				"msb reads `--secret NAME@*` as HostPattern::Any — \"any host (dangerous — secret can "+
				"be exfiltrated)\" in msb's own words — and krayt must never emit it; name a domain "+
				"suffix instead, as \"*.example.com\"", h)
		}
		if strings.Contains(s, "*") {
			return fmt.Errorf("host %q has a '*' that is not a leading \"*.\": krayt accepts one "+
				"wildcard, only as the whole leftmost label, as \"*.example.com\" — msb matches a "+
				"suffix, never a partial label, so %q could only ever match nothing", h, h)
		}
		return validateHostEntry(h)
	}
	if suffix == "" {
		return fmt.Errorf("host %q names no suffix: a wildcard must say what it covers, as "+
			"\"*.example.com\"", h)
	}
	if strings.Contains(suffix, "*") {
		return fmt.Errorf("host %q has more than one '*': krayt accepts exactly one wildcard, and only "+
			"as the leftmost label. msb's secret surface would take this as a Wildcard pattern that "+
			"matches nothing at all (HostPattern::parse), so the credential would silently never "+
			"substitute", h)
	}
	if err := validateHostEntry(suffix); err != nil {
		return fmt.Errorf("wildcard host %q: %w", h, err)
	}
	if _, err := netip.ParseAddr(suffix); err == nil {
		return fmt.Errorf("host %q wildcards an IP literal: a wildcard names a DNS suffix and an "+
			"address has no subdomains — write the address itself, without the \"*.\"", h)
	}
	if !strings.Contains(suffix, ".") {
		return fmt.Errorf("host %q is too broad: %q is a single label, so this would match every "+
			"domain under that TLD. msb refuses the same shape on --net-rule (SuffixTooBroad) and "+
			"krayt refuses it on every field — name at least two labels, as \"*.example.com\"", h, suffix)
	}
	return nil
}

// validateHostEntry rejects an EXACT allow/passthrough/secret-scope host msb's matcher could never
// match, so the failure is a pre-flight config error naming the entry instead of a silent
// difference between the policy the user wrote and the one the run enforces. Callers that also
// accept a wildcard go through validateHostPattern, which strips one leading `*.` and delegates the
// rest here — so every rule below is enforced exactly once, on both spellings.
//
// What msb does with an accepted entry, and why the rules below are the right ones: `--net-rule
// allow@<host>` becomes a Destination::Domain matched against the DNS-cache binding for the
// connected IP (crates/network/lib/policy/types.rs, deferred_domain_match); `--tls-bypass <host>`
// and `--secret NAME@<host>` are matched by their own independent matchers
// (crates/network/lib/tls/state.rs; packages/microsandbox-types/rust/lib/domain.rs,
// HostPattern::matches). All three compare folded host strings. None of them parses a URL, strips a
// port, or translates an internationalized name — so a spelling that is not the bare host is not a
// near-miss, it is a rule nothing can ever match, in a config that reads as though egress were
// permitted.
//
// It is deliberately stricter than those matchers, which is only ever safe in this direction:
// pre-flight strictness can fail a run before it starts, never let through something msb would
// drop.
//   - Bracketed IPv6 ("[::1]") is refused rather than unwrapped, because this package's own
//     cross-checks (passthrough ⊆ allow, secret host ⊆ allow) compare lower()ed strings, which does
//     not unwrap — so "[::1]" in one list and "::1" in another would fail to cross-match. One
//     spelling, demanded up front.
//   - lower() alone is not enough: it case-folds ASCII bytes and passes everything else through, so
//     without the byte-class check an `allow: ["api.examplİ.com"]` (or a URL, or a host with a stray
//     '/') validates clean and then names a host no request carries.
//   - The label rules at the end refuse shapes that fold to a usable string but can never match a
//     real request host: ".example", "example." and "a..example", and likewise
//     "api.example.com:443" and "a:b" — msb matches the host alone, with no port.
//
// One consequence is load-bearing elsewhere: because a comma is refused here,
// internal/sandbox.SecretArgs can keep rendering `--secret NAME@HOST[,HOST...]` — a comma is msb's
// own separator there — without an `allow: ["a.example,evil.example"]` entry silently becoming two
// scoped hosts.
func validateHostEntry(h string) error {
	s := strings.TrimSpace(h)
	if s == "" {
		return fmt.Errorf("host is empty")
	}
	// A wildcard reaching here came from a caller that is exact-only (a refresh block's host), or
	// from validateHostPattern's own delegation of a pattern carrying a second '*'. Either way the
	// generic "not a bare hostname" byte-class error below would name the wrong problem.
	if strings.Contains(s, "*") {
		return fmt.Errorf("host %q is a wildcard, which this field does not accept: only "+
			"network.allow, network.passthrough and a secret's hosts take the \"*.suffix\" form; "+
			"this entry must name one exact host", h)
	}
	if inner, ok := strings.CutPrefix(s, "["); ok {
		if inner, ok := strings.CutSuffix(inner, "]"); ok {
			if addr, err := netip.ParseAddr(inner); err == nil && !addr.Is4() {
				return fmt.Errorf("host %q must be written without brackets, as %q — brackets are "+
					"URL authority syntax, not part of the address", h, inner)
			}
		}
		return fmt.Errorf("host %q is bracketed but is not an IPv6 literal", h)
	}
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == ':':
		case c >= 0x80:
			return fmt.Errorf("host %q is not an ASCII hostname: krayt matches host rules "+
				"byte-wise over ASCII and never translates, so an internationalized name must be "+
				"written as its punycode (\"xn--\") form", h)
		default:
			return fmt.Errorf("host %q is not a bare hostname: write the host alone — letters, "+
				"digits, '.', '-', or an IPv6 literal — with no scheme, path or userinfo", h)
		}
	}
	// A host carrying ':' is an IPv6 literal (brackets were refused above), whose grammar is
	// colon-separated groups rather than dot-separated labels — "::1" has no labels to check and
	// would fail every rule below for no reason. The colon has to earn that exemption, though:
	// "api.example.com:443", "a:b" and "example.com:" are not addresses, and skipping the label
	// rules for them merely on the strength of a ':' let them validate clean. They can never match
	// anything — msb matches the host alone, with no port, on all three of the matchers named above
	// — which is precisely the silently-ineffective allow/passthrough/secret-scope entry this
	// function exists to refuse.
	//
	// An Is4 check like the bracketed branch's would be dead here: a string containing ':' never
	// parses as an IPv4 address, so ParseAddr succeeding already means an IPv6 literal.
	if strings.Contains(s, ":") {
		if _, err := netip.ParseAddr(s); err != nil {
			return fmt.Errorf("host %q is neither a bare hostname nor an IPv6 literal: a host rule "+
				"names the host alone and msb matches request hosts with the port stripped, so "+
				"a ':' here can only be part of an IPv6 address — drop the port", h)
		}
		return nil
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" {
			return fmt.Errorf("host %q has an empty label: a leading or trailing '.', or two "+
				"in a row, can never match a real host", h)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("host %q has a label (%q) that starts or ends with '-': that can "+
				"never match a real host", h, label)
		}
	}
	return nil
}

// lower folds a host/header name byte-wise over ASCII only — deliberately NOT strings.ToLower,
// whose Unicode simple case folding maps U+0130 ('İ') onto ASCII 'i'. It only FOLDS, though; what
// is acceptable as a host at all is validateHostEntry's job, and every host rule goes through
// both. Together they were the pre-flight counterpart of the pre-msb host proxy's normalizeHost,
// which enforced the same ASCII-only rule at the running proxy's choke point.
func lower(s string) string {
	s = strings.TrimSpace(s)
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// hostCovers reports whether pattern — an exact host, or "*.suffix" — matches the concrete host.
// It is the Go mirror of msb's three independent implementations of the same rule, all of which
// agree and all of which krayt has to agree with, since a krayt host list can reach any of them:
//
//   - matches_suffix — crates/network/lib/policy/types.rs (net rules, `allow@*.example.com`)
//   - HostPattern::matches — packages/microsandbox-types/rust/lib/domain.rs (secrets, `--secret`)
//   - DomainPattern::matches_normalized — crates/network/lib/tls/state.rs (`--tls-bypass`)
//
// The rule is apex-inclusive and label-aligned: "*.example.com" matches "example.com",
// "api.example.com" and "a.b.example.com", and does NOT match "evilexample.com". That alignment is
// the whole security property, so the check is `host == suffix` or `host` ending in "." + suffix —
// never a bare strings.HasSuffix. Folding is ASCII-only lower(), not strings.ToLower, for the U+0130
// reason lower()'s own comment gives.
func hostCovers(pattern, host string) bool {
	p, h := lower(pattern), lower(host)
	if suffix, ok := strings.CutPrefix(p, "*."); ok {
		return h == suffix || strings.HasSuffix(h, "."+suffix)
	}
	return p == h
}

// coversPattern reports whether pattern a subsumes EVERY host pattern b could match. It is
// DIRECTIONAL, and the direction is the point: `allow: ["*.example.com"]` covers
// `passthrough: ["api.example.com"]`, but `allow: ["api.example.com"]` does not cover
// `passthrough: ["*.example.com"]` — the second exempts a whole subtree from TLS interception that
// the allowlist never granted. Use it for the ⊆ checks (passthrough ⊆ allow, secret host ⊆ allow),
// never for conflicts.
//
// An exact host can never cover a wildcard: a wildcard's match set is infinite and an exact host's
// is one member of it.
func coversPattern(a, b string) bool {
	if suffix, ok := strings.CutPrefix(lower(b), "*."); ok {
		if !strings.HasPrefix(lower(a), "*.") {
			return false
		}
		// b covers exactly {suffix} ∪ subdomains(suffix); a covers all of it iff a covers suffix.
		return hostCovers(a, suffix)
	}
	return hostCovers(a, b)
}

// patternsOverlap reports whether any single host could match both a and b. It is SYMMETRIC — use
// it for conflict checks, where either pattern shadowing the other is equally a problem. The case
// it exists for is silent otherwise: a wildcard `passthrough` entry swallowing an exact secret host
// means that secret can never be substituted (a passthrough host is tunneled un-MITM'd), and the
// secret host appears nowhere in the passthrough list literally.
func patternsOverlap(a, b string) bool {
	return coversPattern(a, b) || coversPattern(b, a)
}

// anyCovers reports whether any entry in patterns subsumes p (coversPattern, directional).
func anyCovers(patterns []string, p string) bool {
	for _, entry := range patterns {
		if coversPattern(entry, p) {
			return true
		}
	}
	return false
}

// anyOverlaps returns the first entry in patterns that overlaps p (patternsOverlap, symmetric), and
// whether there was one. The entry itself is returned because the error that reports it must name
// the offending line: with a wildcard, p may not appear in patterns at all.
func anyOverlaps(patterns []string, p string) (string, bool) {
	for _, entry := range patterns {
		if patternsOverlap(entry, p) {
			return entry, true
		}
	}
	return "", false
}

// passthroughNotCoveredError renders the `passthrough ⊄ allow` diagnostic. A wildcard passthrough
// gets its own wording because the generic "must also be in allow" reads as a spelling complaint,
// when the actual problem is breadth: --tls-bypass is emitted verbatim (netpolicy_msb.go) and msb's
// bypass matcher is independent of its net-rule matcher, so a bypass wider than the allowlist is a
// real mismatch between the policy krayt printed and the one msb enforces.
func passthroughNotCoveredError(h string, allow []string) error {
	if suffix, ok := strings.CutPrefix(strings.TrimSpace(h), "*."); ok {
		return fmt.Errorf("network: passthrough host %q is not covered by allow: allow names %s, but "+
			"this passthrough exempts every subdomain of %s from TLS interception. The allowlist must "+
			"be at least as wide as anything exempted from it — write %q in allow, or narrow "+
			"passthrough to a host allow already covers (mode: allowlist)",
			h, quotedList(allow), suffix, h)
	}
	return fmt.Errorf("network: passthrough host %q must also be in allow (mode: allowlist)", h)
}

// quotedList renders ss for an error message: `nothing at all` when empty, the single host quoted
// when there is one, a comma-separated list otherwise.
func quotedList(ss []string) string {
	switch len(ss) {
	case 0:
		return "no hosts at all"
	case 1:
		return fmt.Sprintf("the single host %q", ss[0])
	}
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}
