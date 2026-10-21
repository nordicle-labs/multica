package util

import (
	"reflect"
	"testing"
)

func TestRedactSensitiveJSONNestedAndCaseInsensitive(t *testing.T) {
	input := map[string]any{
		"safe": "kept",
		"nested": map[string]any{
			"HeAdErS":    map[string]any{"Authorization": "HERM1086_FAKE_HEADER"},
			"custom-env": map[string]any{"TOKEN": "HERM1086_FAKE_ENV"},
			"children": []any{
				map[string]any{"Api_Key": "HERM1086_FAKE_API_KEY", "name": "kept"},
			},
		},
	}

	got := RedactSensitiveJSON(input)
	want := map[string]any{
		"safe": "kept",
		"nested": map[string]any{
			"children": []any{
				map[string]any{"name": "kept"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RedactSensitiveJSON() = %#v, want %#v", got, want)
	}
}
