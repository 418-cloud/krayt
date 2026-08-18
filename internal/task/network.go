package task

import (
	"fmt"
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
	return out
}

// LookupHeader returns m's value for header h under any casing. Exported because header-keyed
// maps on an InjectRule (SetPrefix, AppendCSV) are consumed outside this package —
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
// instead, in internal/proxy's plain-HTTP handler path (a request to an injection-configured
// host over plain HTTP is refused with 400, never forwarded uninjected or injected in cleartext).
func ValidateNetworkPolicy(np NetworkPolicy, secretKeys map[string]bool) error {
	if len(np.Inject) > 0 && !np.MITM {
		return fmt.Errorf("network: inject requires mitm: true")
	}
	allow := lowerSet(np.Allow)
	passthrough := lowerSet(np.Passthrough)

	if np.Mode == NetworkAllowlist {
		for _, h := range np.Passthrough {
			if !allow[lower(h)] {
				return fmt.Errorf("network: passthrough host %q must also be in allow (mode: allowlist)", h)
			}
		}
	}

	seenHost := map[string]bool{}
	for i, rule := range np.Inject {
		host := lower(rule.Host)
		if host == "" {
			return fmt.Errorf("network: inject[%d]: host is required", i)
		}
		if seenHost[host] {
			return fmt.Errorf("network: inject: host %q has more than one rule", host)
		}
		seenHost[host] = true

		if passthrough[host] {
			return fmt.Errorf("network: inject[%d]: host %q is also in passthrough — a passthrough "+
				"host is tunneled un-MITM'd and can never receive injection", i, host)
		}
		if np.Mode == NetworkAllowlist && !allow[host] {
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

func lowerSet(ss []string) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[lower(s)] = true
	}
	return out
}
