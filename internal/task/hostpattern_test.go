package task

import (
	"strings"
	"testing"
)

// TestHostCovers pins the matching rule krayt has to share with msb, whose three independent
// implementations all agree on it (matches_suffix, HostPattern::matches,
// DomainPattern::matches_normalized — cited in hostCovers). Apex-inclusive and label-aligned: the
// alignment is the security property, so "evilexample.com" must fail against "*.example.com" even
// though a bare strings.HasSuffix would pass it.
func TestHostCovers(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		host    string
		want    bool
	}{
		{"apex itself", "*.example.com", "example.com", true},
		{"subdomain", "*.example.com", "api.example.com", true},
		{"deep subdomain", "*.example.com", "a.b.example.com", true},
		{"non-aligned neighbour", "*.example.com", "evilexample.com", false},
		{"suffix in the middle", "*.example.com", "example.com.evil.com", false},
		{"mixed case pattern", "*.EXAMPLE.com", "api.example.com", true},
		{"mixed case host", "*.example.com", "API.Example.COM", true},
		{"mixed case both", "*.Example.COM", "Api.EXAMPLE.com", true},
		{"exact equals exact", "api.example.com", "api.example.com", true},
		{"exact differs", "api.example.com", "other.example.com", false},
		{"exact does not cover its own subdomain", "example.com", "api.example.com", false},
		{"suffix longer than host", "*.a.b.example.com", "example.com", false},
		{"IPv6 literal exact match", "2606:4700:4700::1111", "2606:4700:4700::1111", true},
		{"IPv6 literal mismatch", "2606:4700:4700::1111", "::1", false},
		{"whitespace is folded away", "  *.example.com  ", " api.example.com ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostCovers(tc.pattern, tc.host); got != tc.want {
				t.Errorf("hostCovers(%q, %q) = %v, want %v", tc.pattern, tc.host, got, tc.want)
			}
		})
	}
}

// TestCoversPatternIsDirectional: coverage is a ⊆ question and must never be answered
// symmetrically. A wildcard allow entry covers an exact passthrough/secret host; an exact allow
// entry does not cover a wildcard passthrough, which would exempt a whole subtree the allowlist
// never granted.
func TestCoversPatternIsDirectional(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"*.example.com", "api.example.com", true},
		{"api.example.com", "*.example.com", false},
		{"*.example.com", "*.api.example.com", true},
		{"*.api.example.com", "*.example.com", false},
		{"*.example.com", "*.example.com", true},
		{"*.example.com", "example.com", true}, // apex-inclusive, same as hostCovers
		{"example.com", "example.com", true},
		{"*.example.com", "*.evilexample.com", false},
		{"*.example.com", "evilexample.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.a+" covers "+tc.b, func(t *testing.T) {
			if got := coversPattern(tc.a, tc.b); got != tc.want {
				t.Errorf("coversPattern(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestPatternsOverlapIsSymmetric: conflict checks ask "could one host match both?", which has no
// direction — and both argument orders must agree, or the check would depend on which list the
// caller happened to iterate first.
func TestPatternsOverlapIsSymmetric(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"*.a.com", "*.x.a.com", true},
		{"*.a.com", "*.b.com", false},
		{"*.a.com", "api.a.com", true},
		{"*.a.com", "a.com", true},
		{"*.a.com", "evila.com", false},
		{"api.a.com", "api.a.com", true},
		{"api.a.com", "other.a.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			forward := patternsOverlap(tc.a, tc.b)
			reverse := patternsOverlap(tc.b, tc.a)
			if forward != tc.want {
				t.Errorf("patternsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, forward, tc.want)
			}
			if forward != reverse {
				t.Errorf("patternsOverlap is not symmetric: (%q,%q)=%v but (%q,%q)=%v",
					tc.a, tc.b, forward, tc.b, tc.a, reverse)
			}
		})
	}
}

func TestAnyCoversAndAnyOverlaps(t *testing.T) {
	allow := []string{"api.github.com", "*.example.com"}
	if !anyCovers(allow, "a.b.example.com") {
		t.Error("anyCovers did not find the wildcard entry covering a.b.example.com")
	}
	if anyCovers(allow, "other.example.org") {
		t.Error("anyCovers matched a host no entry covers")
	}
	entry, ok := anyOverlaps(allow, "deep.example.com")
	if !ok || entry != "*.example.com" {
		t.Errorf("anyOverlaps = (%q, %v), want (\"*.example.com\", true) — the error must be able to "+
			"name the entry that shadowed the host", entry, ok)
	}
	if _, ok := anyOverlaps(allow, "nowhere.example.org"); ok {
		t.Error("anyOverlaps reported an overlap where there is none")
	}
}

// TestValidateHostPatternAccepts is the accepted set, spelled out so each acceptance is a pinned
// decision rather than a side effect of the byte class.
func TestValidateHostPatternAccepts(t *testing.T) {
	good := []string{
		"*.example.com",
		"*.blob.core.windows.net", // the motivating case: <account>.blob.core.windows.net
		"  *.EXAMPLE.com  ",       // trimmed and ASCII-folded like any other entry
		"*.xn--80ak6aa92e.com",    // punycode is ordinary ASCII LDH
		"*.host-1.sub.example",
		// DELIBERATE, not an oversight — support-wildcard-network-hosts.md decision 3. msb has no
		// public-suffix list and says so, and krayt keeps none either: no label-count threshold can
		// separate *.blob.core.windows.net (four labels, multi-tenant, the case this feature exists
		// for) from *.example.com (two labels, single-owner). So *.co.uk and *.github.io are
		// accepted, and `*.github.io` really does allow every GitHub Pages tenant. The gap is
		// documented (§6.6, §10) and surfaced in the pre-boot policy print, not validated away. Do
		// not "fix" this by adding a denylist.
		"*.co.uk",
		"*.github.io",
		// Exact hosts still validate through the same entry point, unchanged.
		"api.anthropic.com", "1.2.3.4", "::1", "2606:4700:4700::1111",
	}
	for _, h := range good {
		t.Run(h, func(t *testing.T) {
			if err := validateHostPattern(h); err != nil {
				t.Errorf("validateHostPattern(%q) = %v, want nil", h, err)
			}
		})
	}
}

// TestValidateHostPatternRejectionsAreDiagnostic pins that each rejected wildcard spelling fails
// for a reason the operator can act on, not through the generic "not a bare hostname" byte-class
// branch — the error that started this task by never mentioning wildcards at all.
func TestValidateHostPatternRejectionsAreDiagnostic(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string // a substring the message must carry
	}{
		{"bare star", "*", "any host"},
		{"star dot nothing", "*.", "no suffix"},
		{"single-label suffix", "*.com", "single label"},
		{"single-label suffix, local", "*.local", "single label"},
		{"two wildcards", "*.*.example.com", "exactly one wildcard"},
		{"double star", "**.example.com", "leftmost label"},
		{"trailing star in label", "api*.example.com", "leftmost label"},
		{"leading star in label", "*api.example.com", "leftmost label"},
		{"interior wildcard label", "api.*.example.com", "leftmost label"},
		{"IPv4 suffix", "*.1.2.3.4", "no subdomains"},
		{"IPv6 suffix", "*.::1", "no subdomains"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHostPattern(tc.host)
			if err == nil {
				t.Fatalf("validateHostPattern(%q) = nil, want an error", tc.host)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validateHostPattern(%q) = %q, which does not explain the problem (want a "+
					"message containing %q)", tc.host, err, tc.want)
			}
		})
	}
}

// TestValidateHostEntryRejectsWildcards is the other half: the fields that are still exact-only —
// a refresh block's host — must say so, rather than emitting the byte-class error.
func TestValidateHostEntryRejectsWildcards(t *testing.T) {
	for _, h := range []string{"*", "*.example.com", "*.*.example.com", "api*.example.com"} {
		t.Run(h, func(t *testing.T) {
			err := validateHostEntry(h)
			if err == nil {
				t.Fatalf("validateHostEntry(%q) = nil, want an error", h)
			}
			if !strings.Contains(err.Error(), "wildcard") {
				t.Errorf("validateHostEntry(%q) = %q, which never mentions wildcards", h, err)
			}
		})
	}
}
