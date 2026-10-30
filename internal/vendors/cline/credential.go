package cline

// This file implements parsing and attribution for Cline credentials.
//
// Cline credentials come from WorkOS device code login: the true long-term credential is refreshToken,
// while accessToken has an expiration and is rotated via the refresh chain.
//
// **Outbound tokens must have the workos: prefix**: the upstream identifies the credential source by this prefix,
// and missing it results in an invalid token. The prefix is centrally added in credential.go so callers don't need to remember it.

import (
	"encoding/json"
	"fmt"
	"strings"

	"free-model-hub/internal/core"
)

// VendorID is the provider instance identifier and also the credential filename prefix.
//
// Cline has only one deployment (api.cline.bot), with no CN/Global distinction.
const VendorID = "cline"

// VendorName is the display name in the management console.
const VendorName = "Cline"

// workosTokenPrefix is the required prefix for outbound Bearer tokens.
//
// This is an upstream hard requirement (not decorative): Cline uses it to distinguish WorkOS-issued tokens from
// tokens from other sources. It must be added across login, refresh, and import paths.
const workosTokenPrefix = "workos:"

// Credential is the normalized Cline credential.
type Credential struct {
	// AccessToken is the outbound token (**excluding** the workos: prefix, which is added by BearerToken upon sending).
	AccessToken string
	// RefreshToken is the long-term credential (login artifact, used to obtain a new access token via refresh).
	RefreshToken string
	// ExpiresAt is the AccessToken expiration time (Unix seconds); 0 means unknown.
	ExpiresAt int64
	// Email is the account email (populated during the login flow).
	Email string
	// Label is the display name.
	Label string
	// AccountID is the unique account identifier (login artifact, falls back to email if missing).
	AccountID string
}

// BearerToken returns the token ready for the Authorization header.
//
// Adding the workos: prefix is **idempotent**: credentials imported from files may already have the prefix
// (cline2api export format includes it), and adding it repeatedly would result in workos:workos:...
func (c *Credential) BearerToken() string {
	if c == nil {
		return ""
	}
	return WithWorkOSPrefix(c.AccessToken)
}

// WithWorkOSPrefix adds the workos: prefix to the token (returns it as-is if already present).
func WithWorkOSPrefix(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, workosTokenPrefix) {
		return trimmed
	}
	return workosTokenPrefix + trimmed
}

// StripWorkOSPrefix removes the workos: prefix (used during import).
func StripWorkOSPrefix(token string) string {
	return strings.TrimPrefix(strings.TrimSpace(token), workosTokenPrefix)
}

// ParseCredential parses the Cline credential JSON.
//
// Field name candidates are intentional because Cline credentials vary across sources:
//   - Plugin login persistence: accessToken / refreshToken (camelCase, matching upstream);
//   - cline2api export: access_token / refresh_token (snake_case);
//   - Manual entry: may only provide a refreshToken.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{
		AccessToken:  core.GetString(payload, "accessToken", "access_token"),
		RefreshToken: core.GetString(payload, "refreshToken", "refresh_token"),
		Email:        core.GetString(payload, "email", "account", "userEmail"),
		Label:        core.GetString(payload, "label", "name"),
		AccountID:    core.GetString(payload, "accountId", "account_id", "uid"),
	}
	cred.ExpiresAt = parseExpiry(payload["expiresAt"])
	if cred.ExpiresAt == 0 {
		cred.ExpiresAt = parseExpiry(payload["expires_at"])
	}
	// The workos: prefix in the token is stripped during normalization and uniformly added by BearerToken when sent.
	// This ensures raw tokens are stored in files, preventing the accumulation of workos:workos:.
	cred.AccessToken = StripWorkOSPrefix(cred.AccessToken)

	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, fmt.Errorf("credential has neither accessToken nor refreshToken")
	}
	return cred, nil
}

// LooksLikeCredential determines whether a JSON payload belongs to Cline.
//
// **The criteria must be relatively strict**: the host feeds all non-builtin credential formats to each plugin sequentially,
// and falsely claiming another's credential will cause the host to overwrite that account with this plugin's structure (silently).
//
// Only accept Cline-specific evidence:
//  1. If another vendor is explicitly declared, never claim it;
//  2. Tokens with the workos: prefix (exclusive to Cline);
//  3. Filenames containing the cline segment;
//  4. Never claim filenames that clearly belong to other providers (such as workbuddy-*.json, etc.);
//  5. Never claim credentials containing features of other providers (domain, device_token, auth/account objects, etc.);
//  6. Only allow unambiguous refreshToken fallback after excluding all other provider features.
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	// Explicit vendor field takes priority: if another provider is declared, never claim it.
	if decided, claimed := core.VendorFieldVerdict(raw, VendorID); decided {
		return claimed
	}
	if core.FileNameMatchesConvention(fileName, VendorID) {
		return true
	}
	if raw == nil {
		return false
	}
	if hasWorkOSPrefix(raw) {
		return true
	}
	// Cross-provider swallow prevention: if the filename or fields contain characteristics of other providers, never claim it.
	if isForeignFileName(fileName) || hasForeignMarker(raw) {
		return false
	}
	// Camel case refreshToken serves as a general fallback after excluding all other provider characteristics.
	if _, okRefresh := raw["refreshToken"]; okRefresh {
		return true
	}
	_ = provider
	return false
}

// isForeignFileName reports whether the filename matches the naming conventions of other known providers.
func isForeignFileName(fileName string) bool {
	return core.FileNameMatchesConvention(fileName,
		"workbuddy", "workbuddycn", "workbuddyglobal", "codebuddy",
		"qoder", "qodercn", "qoderglobal",
		"trae", "traecn", "traeglobal", "traesolo",
		"minimax", "minimaxcode", "minimaxcodecn", "minimaxcodeglobal",
		"opencode", "opencodezen", "zen",
		"zcode", "codearts",
	)
}

// hasForeignMarker reports whether the credential contains exclusive or characteristic fields of other providers.
func hasForeignMarker(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	for _, key := range []string{
		"device_token", "enterpriseId", "enterprise_id",
		"domain", "realm", "region", "secret", "zen_key",
		"agent_id", "agentId", "realUserID", "real_user_id",
		"access_key_id", "secret_access_key", "dpop_private_key",
	} {
		if val, exists := raw[key]; exists && val != nil && val != "" {
			return true
		}
	}
	// WorkBuddy nested format: auth or account is an object.
	if authMap, ok := raw["auth"].(map[string]any); ok && len(authMap) > 0 {
		return true
	}
	if accountMap, ok := raw["account"].(map[string]any); ok && len(accountMap) > 0 {
		return true
	}
	// Type declarations of other providers.
	if typeVal, ok := raw["type"].(string); ok {
		trimmed := strings.ToLower(strings.TrimSpace(typeVal))
		if trimmed != "" && trimmed != "freehub" && trimmed != "cline" {
			return true
		}
	}
	return false
}

// hasWorkOSPrefix determines whether the credential contains a token with the workos: prefix.
func hasWorkOSPrefix(raw map[string]any) bool {
	for _, key := range []string{"accessToken", "access_token", "token", "refreshToken", "refresh_token"} {
		if value, okValue := raw[key].(string); okValue {
			if strings.HasPrefix(strings.TrimSpace(value), workosTokenPrefix) {
				return true
			}
		}
	}
	return false
}
