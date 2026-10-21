package util

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
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

func TestRedactSensitiveJSONCommonSecretAliases(t *testing.T) {
	const fakeSecret = "HERM1086_FAKE_ALIAS_SECRET"
	input := map[string]any{
		"safe":                  "kept",
		"api_key":               fakeSecret,
		"api_secret":            fakeSecret,
		"secret_key":            fakeSecret,
		"secret":                fakeSecret,
		"access_token":          fakeSecret,
		"auth_token":            fakeSecret,
		"private_key":           fakeSecret,
		"database_url":          fakeSecret,
		"db_password":           fakeSecret,
		"db_url":                fakeSecret,
		"redis_url":             fakeSecret,
		"password":              fakeSecret,
		"token":                 fakeSecret,
		"aws_secret_access_key": fakeSecret,
		"secret_access_key":     fakeSecret,
		"auth":                  fakeSecret,
	}

	out, err := json.Marshal(RedactSensitiveJSON(input))
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if bytes.Contains(out, []byte(fakeSecret)) {
		t.Fatalf("common secret alias survived redaction: %s", out)
	}
	if !bytes.Contains(out, []byte(`"safe":"kept"`)) {
		t.Fatalf("safe value removed: %s", out)
	}
}

func TestRedactSensitiveJSONAcceptedContainers(t *testing.T) {
	const fakeSecret = "HERM1086_FAKE_TYPED_SECRET"
	tests := []struct {
		name  string
		value any
	}{
		{"raw message", json.RawMessage(`{"safe":"kept","headers":{"Authorization":"` + fakeSecret + `"}}`)},
		{"string map", map[string]string{"safe": "kept", "token": fakeSecret}},
		{"typed slice", []map[string]any{{"safe": "kept", "env": map[string]string{"TOKEN": fakeSecret}}}},
		{"embedded object string", `{"safe":"kept","nested":{"headers":{"Authorization":"` + fakeSecret + `"}}}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := json.Marshal(RedactSensitiveJSON(tc.value))
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			if bytes.Contains(out, []byte(fakeSecret)) {
				t.Fatalf("secret remained in %s: %s", tc.name, out)
			}
			if !bytes.Contains(out, []byte("kept")) {
				t.Fatalf("safe value removed from %s: %s", tc.name, out)
			}
		})
	}
}

func TestRedactSensitiveJSONPreservesOrdinaryStrings(t *testing.T) {
	for _, value := range []string{"true", "42", `"business value"`, "not json"} {
		if got := RedactSensitiveJSON(value); got != value {
			t.Fatalf("RedactSensitiveJSON(%q) = %#v, want unchanged", value, got)
		}
	}
}

func TestRedactSensitiveJSONBoundsDepthFailClosed(t *testing.T) {
	const fakeSecret = "HERM1086_FAKE_DEEP_SECRET"
	root := map[string]any{}
	cursor := root
	for range 64 {
		next := map[string]any{}
		cursor["child"] = next
		cursor = next
	}
	cursor["safe"] = fakeSecret

	out, err := json.Marshal(RedactSensitiveJSON(root))
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(out), fakeSecret) {
		t.Fatalf("depth limit leaked deep value: %s", out)
	}
}
