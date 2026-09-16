package configseed

import (
	"reflect"
	"strings"
	"testing"
)

// TestMergeTable covers decision 3's rule directly against Merge's map[string]any shape (the
// "missing file"/"non-object JSON"/"invalid JSON" cases live in TestApplyTable instead, since
// Merge itself only ever sees already-decoded objects).
func TestMergeTable(t *testing.T) {
	cases := []struct {
		name        string
		existing    map[string]any
		defaults    map[string]any
		wantResult  map[string]any
		wantChanged bool
	}{
		{
			name:        "empty existing gets every default",
			existing:    map[string]any{},
			defaults:    map[string]any{"a": true, "b": "x"},
			wantResult:  map[string]any{"a": true, "b": "x"},
			wantChanged: true,
		},
		{
			name:        "existing bool false is not overwritten by default true",
			existing:    map[string]any{"hasCompletedOnboarding": false},
			defaults:    map[string]any{"hasCompletedOnboarding": true},
			wantResult:  map[string]any{"hasCompletedOnboarding": false},
			wantChanged: false,
		},
		{
			name:        "existing null is not overwritten",
			existing:    map[string]any{"k": nil},
			defaults:    map[string]any{"k": "default"},
			wantResult:  map[string]any{"k": nil},
			wantChanged: false,
		},
		{
			name:        "existing string is not overwritten",
			existing:    map[string]any{"k": "existing-value"},
			defaults:    map[string]any{"k": "default-value"},
			wantResult:  map[string]any{"k": "existing-value"},
			wantChanged: false,
		},
		{
			name:        "existing number is not overwritten",
			existing:    map[string]any{"k": float64(1)},
			defaults:    map[string]any{"k": float64(2)},
			wantResult:  map[string]any{"k": float64(1)},
			wantChanged: false,
		},
		{
			name:        "existing object is not overwritten by a scalar default",
			existing:    map[string]any{"k": map[string]any{"nested": true}},
			defaults:    map[string]any{"k": "scalar"},
			wantResult:  map[string]any{"k": map[string]any{"nested": true}},
			wantChanged: false,
		},
		{
			name: "nested object merge fills in only the missing subkey",
			existing: map[string]any{
				"security": map[string]any{"other": "keep-me"},
			},
			defaults: map[string]any{
				"security": map[string]any{"auth": map[string]any{"selectedType": "gemini-api-key"}},
			},
			wantResult: map[string]any{
				"security": map[string]any{
					"other": "keep-me",
					"auth":  map[string]any{"selectedType": "gemini-api-key"},
				},
			},
			wantChanged: true,
		},
		{
			name:        "array union appends the missing element, preserving order",
			existing:    map[string]any{"approved": []any{"existing-1"}},
			defaults:    map[string]any{"approved": []any{"new-1"}},
			wantResult:  map[string]any{"approved": []any{"existing-1", "new-1"}},
			wantChanged: true,
		},
		{
			name:        "array union is a no-op when the element is already present",
			existing:    map[string]any{"approved": []any{"same"}},
			defaults:    map[string]any{"approved": []any{"same"}},
			wantResult:  map[string]any{"approved": []any{"same"}},
			wantChanged: false,
		},
		{
			name:        "type conflict: existing scalar where default is an object",
			existing:    map[string]any{"k": "not-an-object"},
			defaults:    map[string]any{"k": map[string]any{"nested": true}},
			wantResult:  map[string]any{"k": "not-an-object"},
			wantChanged: false,
		},
		{
			name:        "type conflict: existing scalar where default is an array",
			existing:    map[string]any{"k": "not-an-array"},
			defaults:    map[string]any{"k": []any{"x"}},
			wantResult:  map[string]any{"k": "not-an-array"},
			wantChanged: false,
		},
		{
			name:        "unrelated existing keys survive untouched",
			existing:    map[string]any{"keepMe": "yes", "hasCompletedOnboarding": true},
			defaults:    map[string]any{"hasCompletedOnboarding": true},
			wantResult:  map[string]any{"keepMe": "yes", "hasCompletedOnboarding": true},
			wantChanged: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := Merge(tc.existing, tc.defaults)
			if !reflect.DeepEqual(got, tc.wantResult) {
				t.Errorf("Merge() result = %#v, want %#v", got, tc.wantResult)
			}
			if changed != tc.wantChanged {
				t.Errorf("Merge() changed = %v, want %v", changed, tc.wantChanged)
			}
		})
	}
}

// TestMergeDoesNotAliasDefaults proves a value newly added from defaults is an independent copy:
// mutating the result must never reach back into the caller's own Defaults literal.
func TestMergeDoesNotAliasDefaults(t *testing.T) {
	defaults := map[string]any{"approved": []any{"a"}}
	result, changed := Merge(map[string]any{}, defaults)
	if !changed {
		t.Fatal("want changed=true")
	}
	result["approved"].([]any)[0] = "mutated"
	if defaults["approved"].([]any)[0] != "a" {
		t.Fatal("Merge aliased the caller's Defaults slice — mutating the result mutated the input")
	}
}

// TestApplyTable covers the byte-level entry point, including the cases Merge itself can't see:
// a missing file, non-object JSON, and invalid JSON.
func TestApplyTable(t *testing.T) {
	t.Run("missing file (empty bytes) is treated as {}", func(t *testing.T) {
		merged, changed, err := Apply(nil, map[string]any{"hasCompletedOnboarding": true})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		if !strings.Contains(string(merged), `"hasCompletedOnboarding": true`) {
			t.Errorf("merged = %s, want hasCompletedOnboarding: true", merged)
		}
	})

	t.Run("empty object", func(t *testing.T) {
		merged, changed, err := Apply([]byte(`{}`), map[string]any{"a": true})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		if !strings.Contains(string(merged), `"a": true`) {
			t.Errorf("merged = %s", merged)
		}
	})

	t.Run("unchanged returns the original bytes verbatim", func(t *testing.T) {
		orig := []byte(`{"hasCompletedOnboarding":false}`)
		merged, changed, err := Apply(orig, map[string]any{"hasCompletedOnboarding": true})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if changed {
			t.Fatal("want changed=false")
		}
		if string(merged) != string(orig) {
			t.Errorf("merged = %s, want the original bytes verbatim", merged)
		}
	})

	t.Run("a JSON array is refused", func(t *testing.T) {
		_, _, err := Apply([]byte(`[]`), map[string]any{"a": true})
		if err == nil {
			t.Fatal("want an error for non-object JSON")
		}
	})

	t.Run("a JSON string is refused", func(t *testing.T) {
		_, _, err := Apply([]byte(`"x"`), map[string]any{"a": true})
		if err == nil {
			t.Fatal("want an error for non-object JSON")
		}
	})

	t.Run("invalid JSON is refused", func(t *testing.T) {
		_, _, err := Apply([]byte(`{not valid json`), map[string]any{"a": true})
		if err == nil {
			t.Fatal("want an error for invalid JSON")
		}
	})
}
