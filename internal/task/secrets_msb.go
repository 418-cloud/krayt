package task

import "fmt"

// SecretSpec is one network-scoped secret under msb (hand-secrets-to-msb.md): the secrets-file
// key NAME and the hosts msb may substitute its value into. It never carries a value —
// internal/sandbox.SecretArgs renders it as one `--secret NAME@HOST[,HOST...]` flag (argv, no
// value); the value itself travels only in the msb child's env (internal/sandbox.SecretEnv), on
// whichever invocation actually starts the sandbox (the Timing rule, KRAYT_SPEC.md §6.8).
type SecretSpec struct {
	Key   string
	Hosts []string
}

// SecretSpecsFromConfig converts network.inject[] entries into the msb-era secret-scoping shape
// (hand-secrets-to-msb.md decision 4). It hard-errors, naming itself, on any field from the
// pre-msb host/strip/set/set_prefix/set_literal/refresh shape rather than silently ignoring it:
// msb substitutes a placeholder STRING wherever it appears rather than replacing a named header,
// so none of those concepts has an equivalent to translate to. Silently dropping `strip`
// specifically would weaken the posture (an un-stripped pre-existing auth header reaches an
// allowed host untouched, KRAYT_SPEC.md §10) without telling anyone — hence a hard error, not a
// quiet no-op, for all of them.
func SecretSpecsFromConfig(crs []ConfigInjectRule) ([]SecretSpec, error) {
	specs := make([]SecretSpec, 0, len(crs))
	seen := map[string]int{} // key -> index of the entry that first claimed it
	for i, cr := range crs {
		if len(cr.Strip) > 0 {
			return nil, fmt.Errorf("network.inject[%d]: strip is not valid under msb — msb substitutes "+
				"a placeholder string wherever it appears, so there is no named header to strip; remove it", i)
		}
		if len(cr.Set) > 0 {
			return nil, fmt.Errorf("network.inject[%d]: set is not valid under msb — msb matches a "+
				"placeholder string, not a header name; name the secrets-file key directly with `key` instead", i)
		}
		if len(cr.SetPrefix) > 0 {
			return nil, fmt.Errorf("network.inject[%d]: set_prefix is not valid under msb — there is no "+
				"header to prefix a value onto; the agent CLI emits its own auth scheme and msb "+
				"substitutes the placeholder in place", i)
		}
		if len(cr.SetLiteral) > 0 {
			return nil, fmt.Errorf("network.inject[%d]: set_literal is not valid under msb — msb only "+
				"substitutes a declared secret's own placeholder; it has no channel for a fixed literal "+
				"header value", i)
		}
		if cr.Refresh != nil {
			return nil, fmt.Errorf("network.inject[%d]: refresh is not valid under msb — msb owns "+
				"credential substitution at the host proxy layer and has no refresh hook for krayt to drive", i)
		}
		if cr.Key == "" {
			return nil, fmt.Errorf("network.inject[%d]: key is required — the secrets-file key name "+
				"this rule scopes, never a value", i)
		}
		if first, dup := seen[cr.Key]; dup {
			return nil, fmt.Errorf("network.inject[%d]: key %q is already scoped by inject[%d] — one "+
				"entry per secret", i, cr.Key, first)
		}
		seen[cr.Key] = i
		hosts, err := hostsFromConfigInjectRule(cr)
		if err != nil {
			return nil, fmt.Errorf("network.inject[%d] (%s): %w", i, cr.Key, err)
		}
		specs = append(specs, SecretSpec{Key: cr.Key, Hosts: hosts})
	}
	return specs, nil
}

// hostsFromConfigInjectRule resolves the msb shape's host/hosts pair: exactly one of the two
// singular/plural forms must be set (decision 4's "host xor hosts").
func hostsFromConfigInjectRule(cr ConfigInjectRule) ([]string, error) {
	switch {
	case cr.Host != "" && len(cr.Hosts) > 0:
		return nil, fmt.Errorf("host and hosts are mutually exclusive — use exactly one")
	case cr.Host != "":
		return []string{cr.Host}, nil
	case len(cr.Hosts) > 0:
		return append([]string(nil), cr.Hosts...), nil
	default:
		return nil, fmt.Errorf("host or hosts is required")
	}
}
