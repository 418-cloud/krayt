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
		for h, key := range rule.Set {
			if err := validateHeaderName(h); err != nil {
				return fmt.Errorf("network: inject[%d] (%s) set: %w", i, host, err)
			}
			if !secretKeys[key] {
				return fmt.Errorf("network: inject[%d] (%s): set[%q] names secrets-file key %q, which does not exist",
					i, host, h, key)
			}
		}
		for h := range rule.SetLiteral {
			if err := validateHeaderName(h); err != nil {
				return fmt.Errorf("network: inject[%d] (%s) set_literal: %w", i, host, err)
			}
			if _, dup := rule.Set[h]; dup {
				return fmt.Errorf("network: inject[%d] (%s): header %q is in both set and set_literal", i, host, h)
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
