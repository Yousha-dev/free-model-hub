package openaicompat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Credential is the parsed credential of one provider account.
type Credential struct {
	APIKey string
	Label  string
	// Anonymous marks the keyless account of a keyless provider.
	Anonymous bool
}

func parseCredential(raw []byte, keyless bool) (*Credential, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{}
	for _, key := range []string{"api_key", "apikey", "apiKey", "key", "token"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			cred.APIKey = strings.TrimSpace(value)
			break
		}
	}
	if label, ok := payload["label"].(string); ok {
		cred.Label = strings.TrimSpace(label)
	}
	if cred.APIKey == "" {
		anonymous, _ := payload["anonymous"].(bool)
		if !keyless || !anonymous {
			return nil, fmt.Errorf("credential has no api_key")
		}
		cred.Anonymous = true
	}
	return cred, nil
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "***" + key[len(key)-4:]
}
