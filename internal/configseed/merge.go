// Package configseed implements the fill-in-never-override JSON merge
// (seed-agent-first-run-config.md decision 3) used to seed an agent's first-run config file
// without ever clobbering an image author's or a user's own explicit setting. It is pure and
// does no I/O — internal/orchestrator owns reading/writing the guest file this operates on.
package configseed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// Merge implements decision 3's rule: defaults fill in whatever existing doesn't already have;
// nothing existing is ever overwritten, no matter its type — including an explicit false or
// null — and no matter whether a default disagrees with it in shape (a type conflict just skips
// that branch, leaving existing untouched). Objects merge recursively; arrays take the union,
// appending each default element not already present (deep equality) and preserving existing
// order. Both inputs are treated as read-only; the result is an independent copy, so a caller
// can't accidentally alias a shared Defaults literal into the file it just merged. changed
// reports whether the merge altered anything, so a caller can skip a write when nothing did.
func Merge(existing, defaults map[string]any) (result map[string]any, changed bool) {
	result = make(map[string]any, len(existing))
	for k, v := range existing {
		result[k] = v
	}
	for k, dv := range defaults {
		ev, present := result[k]
		if !present {
			result[k] = cloneValue(dv)
			changed = true
			continue
		}
		switch dvt := dv.(type) {
		case map[string]any:
			evt, ok := ev.(map[string]any)
			if !ok {
				continue // type conflict: existing is not an object — keep it, skip this branch
			}
			m, ch := Merge(evt, dvt)
			if ch {
				result[k] = m
				changed = true
			}
		case []any:
			evt, ok := ev.([]any)
			if !ok {
				continue // type conflict: existing is not an array — keep it, skip this branch
			}
			a, ch := mergeArray(evt, dvt)
			if ch {
				result[k] = a
				changed = true
			}
		default:
			// existing already holds SOME value for this key — scalar, object, array, null,
			// whatever it is — and it wins outright; there is nothing further to merge.
		}
	}
	return result, changed
}

// mergeArray appends each element of defaults not already present in existing (by deep
// equality), preserving existing's order and never reordering or removing anything in it.
func mergeArray(existing, defaults []any) (result []any, changed bool) {
	result = append([]any(nil), existing...)
	for _, d := range defaults {
		if containsDeepEqual(result, d) {
			continue
		}
		result = append(result, cloneValue(d))
		changed = true
	}
	return result, changed
}

func containsDeepEqual(s []any, v any) bool {
	for _, e := range s {
		if reflect.DeepEqual(e, v) {
			return true
		}
	}
	return false
}

// cloneValue deep-copies a JSON-shaped value (map[string]any / []any / scalar) so a value
// pulled from a caller-owned Defaults literal never aliases into the merge result.
func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = cloneValue(vv)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = cloneValue(vv)
		}
		return s
	default:
		return v
	}
}

// Apply parses existing as JSON, merges defaults into it (Merge, above), and re-marshals the
// result. Empty existing (a missing guest file, decision 4.2) is treated as {}. existing bytes
// that decode to anything other than a JSON object — an array, a string, a number, invalid JSON
// — are refused with an error: the caller must never overwrite a file krayt could not parse
// (decision 4.3), the same policy the gemini-cli entrypoint's own settings merge already uses.
//
// When nothing changed, Apply returns existing verbatim (not a re-marshaled copy) so a caller
// comparing bytes sees the file is byte-identical, and changed is false so the caller can skip
// the write entirely.
func Apply(existing []byte, defaults map[string]any) (merged []byte, changed bool, err error) {
	obj, err := decodeObject(existing)
	if err != nil {
		return nil, false, err
	}
	mergedObj, changed := Merge(obj, defaults)
	if !changed {
		return existing, false, nil
	}
	b, err := json.MarshalIndent(mergedObj, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("configseed: marshal merged config: %w", err)
	}
	return b, true, nil
}

func decodeObject(b []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("configseed: invalid JSON: %w", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("configseed: not a JSON object (got %T)", v)
	}
	return obj, nil
}
