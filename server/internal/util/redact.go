package util

import (
	"strings"
	"unicode"
)

// RedactSensitiveJSON removes secret-bearing fields from decoded JSON while
// preserving the surrounding non-sensitive shape. Keys are matched without
// case or separators so nested variants such as custom-env, CustomEnv and ENV
// cannot bypass the boundary.
func RedactSensitiveJSON(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if sensitiveJSONKey(key) {
				delete(value, key)
				continue
			}
			value[key] = RedactSensitiveJSON(child)
		}
	case []any:
		for i := range value {
			value[i] = RedactSensitiveJSON(value[i])
		}
	}
	return value
}

func sensitiveJSONKey(key string) bool {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
	switch normalized {
	case "accesstoken", "apikey", "authorization", "clientsecret", "cookie",
		"credential", "credentials", "customenv", "env", "environment",
		"headers", "mcpconfig", "password", "privatekey", "refreshtoken",
		"secret", "token":
		return true
	default:
		return false
	}
}
