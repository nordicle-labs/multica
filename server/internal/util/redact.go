package util

import (
	"encoding/json"
	"reflect"
	"strings"
	"unicode"
)

const maxSensitiveJSONDepth = 32

// RedactSensitiveJSON removes secret-bearing fields from JSON-compatible data.
// It accepts decoded string-keyed maps, slices and arrays, plus json.RawMessage.
// Object/array JSON encoded inside a string is sanitized and re-encoded; other
// strings, including valid JSON scalars, are left unchanged. Keys are matched
// without case or separators. Unsupported containers, malformed raw JSON and
// values beyond 32 nested containers become nil so the output fails closed.
func RedactSensitiveJSON(value any) any {
	return redactSensitiveJSON(value, 0)
}

func redactSensitiveJSON(value any, depth int) any {
	if depth >= maxSensitiveJSONDepth {
		return nil
	}

	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if sensitiveJSONKey(key) {
				delete(value, key)
				continue
			}
			value[key] = redactSensitiveJSON(child, depth+1)
		}
		return value
	case []any:
		for i := range value {
			value[i] = redactSensitiveJSON(value[i], depth+1)
		}
		return value
	case json.RawMessage:
		var decoded any
		if json.Unmarshal(value, &decoded) != nil {
			return nil
		}
		encoded, err := json.Marshal(redactSensitiveJSON(decoded, depth+1))
		if err != nil {
			return nil
		}
		return json.RawMessage(encoded)
	case string:
		trimmed := strings.TrimSpace(value)
		if len(trimmed) < 2 ||
			!((trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}') ||
				(trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']')) {
			return value
		}
		var decoded any
		if json.Unmarshal([]byte(trimmed), &decoded) != nil {
			return value
		}
		encoded, err := json.Marshal(redactSensitiveJSON(decoded, depth+1))
		if err != nil {
			return nil
		}
		return string(encoded)
	}

	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			if !sensitiveJSONKey(key) {
				out[key] = redactSensitiveJSON(iter.Value().Interface(), depth+1)
			}
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range rv.Len() {
			out[i] = redactSensitiveJSON(rv.Index(i).Interface(), depth+1)
		}
		return out
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		return redactSensitiveJSON(rv.Elem().Interface(), depth)
	case reflect.Struct, reflect.Chan, reflect.Func:
		return nil
	default:
		return value
	}
}

func sensitiveJSONKey(key string) bool {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
	switch normalized {
	case "accesskey", "accesstoken", "apikey", "apisecret", "auth",
		"authorization", "authtoken", "clientsecret", "cookie", "credential",
		"credentials", "customenv", "databaseurl", "dbpassword", "dburl", "env",
		"environment", "headers", "mcpconfig", "password", "privatekey", "redisurl",
		"refreshtoken", "secret", "secretkey", "token":
		return true
	default:
		return false
	}
}
