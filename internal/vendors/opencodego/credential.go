package opencodego

import (
	"encoding/json"
	"fmt"
	"strings"
)

// VendorID is the vendor ID and credential file prefix.
const VendorID = "opencodego"

// VendorName is the console display name.
const VendorName = "OpenCode Go (subscription key)"

// Credential is one OpenCode Go API key.
type Credential struct {
	APIKey string
	Label  string
}

func parseCredential(raw []byte) (*Credential, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{}
	for _, key := range []string{"api_key", "apikey", "apiKey", "key"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			cred.APIKey = strings.TrimSpace(value)
			break
		}
	}
	if cred.APIKey == "" {
		return nil, fmt.Errorf("credential has no api_key")
	}
	if label, ok := payload["label"].(string); ok {
		cred.Label = strings.TrimSpace(label)
	}
	return cred, nil
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "***" + key[len(key)-4:]
}
