package raccoon

// This file implements the parsing, ownership determination, desensitization, and write-back merging of Raccoon credentials.
//
// Credential format (aligned with Agent2API / Agent2API account files):
//
//	{
//	  "vendor": "raccoon",
//	  "access_token": "eyJ…",
//	  "refresh_token": "eyJ…",
//	  "expires_at": 1791529021000,
//	  "uid": "7591340",
//	  "nickname": "Raccoon-1"
//	}
//
// Both access and refresh are JWTs issued directly by the upstream; the refresh_token **may rotate** upon each renewal,
// and the new token must be written back. expires_at is a millisecond timestamp (0 = unknown, triggering renewal via 401).

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"free-model-hub/internal/core"
)

// VendorID is the provider instance identifier and also the credential filename prefix (raccoon-<uid>.json).
const VendorID = "raccoon"

// VendorName is the display name in the management console.
const VendorName = "Raccoon"

// protocolName is the family alias (used for filename infix matching).
const protocolName = "raccoon"

// ErrMissingToken indicates that there is no available token in the credential.
var ErrMissingToken = errors.New("credential has neither accessToken nor refreshToken")

// Credential is the normalized Raccoon credential.
//
// Concurrency conventions are consistent with core.Credential: outbound values are locked and snapshotted via getters,
// and the refresh goroutine will concurrently modify the token fields.
type Credential struct {
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	// ExpiresAt is the expiration time of the access token (Unix **seconds**, normalized from milliseconds during parsing);
	// 0 indicates unknown.
	ExpiresAt int64
	UID       string
	Nickname  string
}

// ParseCredential parses the Raccoon credential JSON.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{}
	getStr := func(keys ...string) string {
		for _, key := range keys {
			if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	cred.AccessToken = getStr("access_token", "accessToken", "token")
	cred.RefreshToken = getStr("refresh_token", "refreshToken")
	cred.Nickname = getStr("nickname", "name", "label")
	cred.UID = getStr("uid", "user_id", "userId")
	if cred.ExpiresAt = core.GetUnixSeconds(payload, "expires_at", "expiresAt"); cred.ExpiresAt == 0 {
		// The response may only contain a JWT: read exp from claims as a fallback.
		cred.ExpiresAt = core.JWTExpiry(cred.AccessToken)
	}
	if strings.TrimSpace(cred.AccessToken) == "" && strings.TrimSpace(cred.RefreshToken) == "" {
		return nil, ErrMissingToken
	}
	return cred, nil
}

// LooksLikeCredential determines whether a JSON belongs to Raccoon.
//
// **Ownership determination must be strictly enforced** (following the same discipline across providers): the host will feed
// all non-builtin credentials to each plugin sequentially. Misidentifying another plugin's credential will cause the host to overwrite
// the other account with this plugin's structure (silently). Failing to recognize it only results in a loading failure that users will notice immediately, making the cost much lower.
//
// Only accept evidence specific to Raccoon:
//  1. The filename contains a raccoon / xiaohuanxiong segment;
//  2. The credential explicitly declares vendor: raccoon;
//  3. The credential carries Raccoon account file feature combinations (uid + snake_case token pairs, without other families' feature fields).
//
// Deliberately do not accept accessToken / refreshToken camelCase pairs as criteria: workbuddy / trae, etc.,
// also use them; the snake_case access_token/refresh_token + uid combination is the fixed format
// of Raccoon account files (Agent2API series storage format).
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if core.FileNameMatchesConvention(fileName, VendorID, protocolName, "xiaohuanxiong") {
		return true
	}
	if raw == nil {
		return false
	}
	if vendor, okVendor := raw[core.VendorKey].(string); okVendor {
		if strings.EqualFold(strings.TrimSpace(vendor), VendorID) {
			return true
		}
	}
	// Feature combination: snake_case pairs stored by Agent2API + uid.
	_, hasAccess := raw["access_token"]
	_, hasRefresh := raw["refresh_token"]
	_, hasUID := raw["uid"]
	if hasAccess && hasRefresh && hasUID {
		return true
	}
	_ = provider
	return false
}

// ---- Locked Getters (for concurrent outbound use) ----

// AccessTokenValue returns a locked snapshot of the access token.
func (c *Credential) AccessTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken
}

// RefreshTokenValue returns a locked snapshot of the refresh token.
func (c *Credential) RefreshTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RefreshToken
}

// ExpiresAtValue returns a locked snapshot of the expiration time (Unix seconds).
func (c *Credential) ExpiresAtValue() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ExpiresAt
}

// UIDValue returns a locked snapshot of the user ID.
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// NicknameValue returns a locked snapshot of the display name.
func (c *Credential) NicknameValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Nickname
}

// Snapshot returns a locked snapshot of the token pair.
func (c *Credential) Snapshot() (access, refresh string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken, c.RefreshToken
}

// UnchangedSince reports whether both tokens remain unchanged before and after the refresh.
func (c *Credential) UnchangedSince(accessBefore, refreshBefore string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken == accessBefore && c.RefreshToken == refreshBefore
}

// ApplyTokens writes the renewal results (an empty refresh indicates no upstream rotation, retaining the old value).
func (c *Credential) ApplyTokens(access, refresh string, expiresAt int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(access) != "" {
		c.AccessToken = strings.TrimSpace(access)
	}
	if strings.TrimSpace(refresh) != "" {
		c.RefreshToken = strings.TrimSpace(refresh)
	}
	if expiresAt > 0 {
		c.ExpiresAt = expiresAt
	}
}

// ApplyIdentity fills in the uid / nickname (obtained from the renewal or login response).
func (c *Credential) ApplyIdentity(uid, nickname string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(uid) != "" && c.UID == "" {
		c.UID = strings.TrimSpace(uid)
	}
	if strings.TrimSpace(nickname) != "" && c.Nickname == "" {
		c.Nickname = strings.TrimSpace(nickname)
	}
}

// UIDOrTokenID returns the account identifier: UID takes precedence, falling back to the user ID in the JWT claims.
func (c *Credential) UIDOrTokenID() string {
	if c == nil {
		return ""
	}
	if uid := c.UIDValue(); uid != "" {
		return uid
	}
	return core.JWTUserID(c.AccessTokenValue())
}

// StorageJSON serializes credentials into disk JSON (the vendor field redundantly declares ownership).
func (c *Credential) StorageJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	payload := map[string]any{
		core.VendorKey:  VendorID,
		"access_token":  c.AccessToken,
		"refresh_token": c.RefreshToken,
		"expires_at":    c.ExpiresAt * 1000,
	}
	if c.UID != "" {
		payload["uid"] = c.UID
	}
	if c.Nickname != "" {
		payload["nickname"] = c.Nickname
	}
	return json.MarshalIndent(payload, "", "  ")
}

// ---------------------------------------------------------------------------
// Credential write-back merge
// ---------------------------------------------------------------------------

// Reuse the core.MergeCredentialJSON skeleton: only overwrite token-related fields,
// while user-maintained fields like note / disabled are kept as is.

// MergeCredentialJSON merges the refreshed token back into the original credential JSON.
func MergeCredentialJSON(original []byte, cred *Credential) ([]byte, error) {
	return core.MergeCredentialJSON(original, cred.StorageJSON, func(target map[string]any) {
		target["access_token"] = cred.AccessTokenValue()
		target["refresh_token"] = cred.RefreshTokenValue()
		target["expires_at"] = cred.ExpiresAtValue() * 1000
	})
}
