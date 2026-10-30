package opencodezen

// This file implements the parsing and attribution of OpenCode ZEN credentials.
//
// The biggest difference from other providers: ZEN has **no login flow**, and the credential is just a static API key
// (applied for by the user on the OpenCode website and filled in). There is no distinction between access/refresh tokens,
// and no refresh mechanism either—the key is either valid or invalid.
//
// Therefore, this package does not have login.go / refresh.go: it's not missing, the upstream simply lacks these capabilities.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"free-model-hub/internal/core"
)

// VendorID is the provider instance identifier and also the credential file name prefix.
//
// ZEN has only one deployment (opencode.ai) with no CN/Global distinction, unlike WorkBuddy
// and Qoder which have two instances (cn and global).
const VendorID = "opencodezen"

// VendorName is the display name in the management console.
const VendorName = "OpenCode ZEN"

// ErrMissingAPIKey indicates that there is no available API key in the credential.
var ErrMissingAPIKey = errors.New("credential has no api key")

// Field names that may carry the API key in the credential.
//
// Multiple candidates are intentional: when users obtain keys from different channels and write credentials manually, formats vary.
// Trying all of them is more user-friendly than requiring them to modify the file.
var credentialKeyKeys = []string{
	"api_key", "apikey", "apiKey", "key", "zen_key", "token", "access_token",
}

// Credential is the normalized ZEN credential.
type Credential struct {
	// APIKey is the outbound credential (Bearer).
	APIKey string
	// Label is the display name. When not provided, the caller falls back to using the masked key.
	Label string
	// Anonymous marks a keyless credential that only reaches the free tier
	// through the official OpenCode CLI's public lane.
	Anonymous bool
}

// AnonymousCredentialJSON is the auth file written for keyless free-tier access.
func AnonymousCredentialJSON(providerKey string) []byte {
	return []byte(`{"type":"` + providerKey + `","vendor":"` + VendorID + `","anonymous":true,"label":"Free tier (no account)"}`)
}

// ParseCredential parses the ZEN credential JSON.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{APIKey: firstKeyIn(payload)}
	if anonymous, _ := payload["anonymous"].(bool); anonymous && cred.APIKey == "" {
		cred.Anonymous = true
	}
	if cred.APIKey == "" && !cred.Anonymous {
		return nil, fmt.Errorf("credential has no api key field (tried: %s)", strings.Join(credentialKeyKeys, ", "))
	}
	if label, okLabel := payload["label"].(string); okLabel {
		cred.Label = strings.TrimSpace(label)
	}
	if cred.Label == "" {
		if email, okEmail := payload["email"].(string); okEmail {
			cred.Label = strings.TrimSpace(email)
		}
	}
	return cred, nil
}

// firstKeyIn returns the first non-empty key field in a JSON layer.
func firstKeyIn(payload map[string]any) string {
	for _, key := range credentialKeyKeys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// LooksLikeCredential determines whether a JSON belongs to OpenCode ZEN.
//
// **The judgment must be strict**: the host feeds all non-builtin format credentials sequentially to each plugin,
// and mistakenly swallowing another's would cause the host to overwrite the other account with this plugin's structure (silently).
//
// Only accept ZEN-specific evidence:
//  1. The file name contains opencode / zen segments (most reliable—this is the name when users add it);
//  2. The credential contains ZEN-exclusive field names (zen_key / api_key), and **does not** have feature fields
//     from other providers.
//
// Deliberately avoid generic field names like "key" / "token" as criteria: Qoder's credentials also have token
// fields, and using them as criteria would inevitably lead to cross-provider false swallows.
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if core.FileNameMatchesConvention(fileName, VendorID, "opencode", "zen") {
		return true
	}
	if raw == nil {
		return false
	}
	// Only recognize if ZEN-exclusive field names are present. api_key appearing alone also counts—but other providers' features must be excluded.
	if hasZenSpecificField(raw) && !hasForeignMarker(raw) {
		return true
	}
	_ = provider
	return false
}

// hasZenSpecificField reports whether the credential contains ZEN-exclusive field names.
func hasZenSpecificField(raw map[string]any) bool {
	for _, key := range []string{"zen_key", "api_key", "apikey"} {
		if value, okValue := raw[key].(string); okValue && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// hasForeignMarker reports whether the credential contains feature fields of other providers.
//
// This is a fallback for "api_key field being too generic": WorkBuddy has device_token/account,
// Qoder has device_token/refresh_token/secret, and Cline has refreshToken.
// Hitting any of these indicates it is not a ZEN credential.
func hasForeignMarker(raw map[string]any) bool {
	for _, key := range []string{
		"device_token", "refresh_token", "refreshToken", "secret",
		"account", "auth", "region", "workos",
	} {
		if _, okKey := raw[key]; okKey {
			return true
		}
	}
	return false
}
