package workbuddy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"free-model-hub/internal/core"
)

// This file implements credential parsing and writing back.
//
// Credentials have two formats (for historical reasons, both must be recognized):
//   - Nested: {"auth": {...}, "account": {...}, "device_token": ...} (OAuth login output)
//   - Flat: direct top-level fields for accessToken / refreshToken / uid / ... (handwritten or legacy)
//
// **Concurrency Convention**: All outbound request header construction must use getter methods (such as AccessTokenValue) to take a locked snapshot.
// Do not read fields directly -- the task scheduler and request forwarder concurrently refresh/read the same credential.

// protocolName is the common identifier segment for this protocol in filenames.
//
// The credential filenames for CN and Global are workbuddycn-<uid>.json and
// workbuddyglobal-<uid>.json respectively. Both contain the "workbuddy" segment, so it is used for protocol-level
// filename identification; the specific region is determined by the full prefix match in core.ResolveVendor.
const protocolName = "workbuddy"

// Credential represents the normalized account credential.
type Credential struct {
	mu sync.Mutex

	// AccessToken is the Bearer value for outbound Authorization.
	AccessToken string
	// RefreshToken is used to refresh the access token.
	RefreshToken string
	// ExpiresAt is the access token expiration time (Unix seconds).
	ExpiresAt int64
	// Domain is the domain to which the account belongs (used to infer realm and outbound X-Domain header).
	Domain string
	// realm is unexported: explicit declaration takes precedence, falling back to inference from Domain when empty.
	realm Region
	// UID is the unique account identifier (also the derivation seed for the device fingerprint).
	UID string
	// EnterpriseID is the enterprise tenant identifier (X-Enterprise-Id for CN).
	EnterpriseID string
	// Nickname is the display nickname.
	Nickname string
	// DeviceToken is X-Device-Token; empty means not injected.
	DeviceToken string
	// FilePath is the source file path of the credential (atomically written back here after refresh); empty means non-file source.
	FilePath string
}

// Top-level JSON form of Credential.
type credentialJSON struct {
	Auth *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
	} `json:"auth"`
	Account *struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
	DeviceToken string `json:"device_token"`

	// Fields for the flat format.
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// ParseCredential parses the credential JSON.
//
// fallbackRegion is the fallback domain when the account realm is not declared (from plugin configuration).
func ParseCredential(raw []byte, fallbackRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var parsed credentialJSON
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}

	cred := &Credential{}
	if parsed.Auth != nil {
		cred.AccessToken = strings.TrimSpace(parsed.Auth.AccessToken)
		cred.RefreshToken = strings.TrimSpace(parsed.Auth.RefreshToken)
		cred.ExpiresAt = parsed.Auth.ExpiresAt
		cred.Domain = strings.TrimSpace(parsed.Auth.Domain)
		cred.realm = normalizeRealmStrict(parsed.Auth.Realm)
	} else {
		cred.AccessToken = strings.TrimSpace(parsed.AccessToken)
		cred.RefreshToken = strings.TrimSpace(parsed.RefreshToken)
		cred.ExpiresAt = parsed.ExpiresAt
		cred.Domain = strings.TrimSpace(parsed.Domain)
		cred.realm = normalizeRealmStrict(parsed.Realm)
	}
	if parsed.Account != nil {
		cred.UID = strings.TrimSpace(parsed.Account.UID)
		cred.EnterpriseID = strings.TrimSpace(parsed.Account.EnterpriseID)
		cred.Nickname = strings.TrimSpace(parsed.Account.Nickname)
	} else {
		cred.UID = strings.TrimSpace(parsed.UID)
		cred.EnterpriseID = strings.TrimSpace(parsed.EnterpriseID)
		cred.Nickname = strings.TrimSpace(parsed.Nickname)
	}
	cred.DeviceToken = strings.TrimSpace(parsed.DeviceToken)

	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, fmt.Errorf("credential has neither accessToken nor refreshToken")
	}
	if cred.realm == "" {
		if inferred := realmFromDomain(cred.Domain); inferred != "" {
			cred.realm = inferred
		} else {
			cred.realm = fallbackRegion
		}
	}
	return cred, nil
}

// normalizeRealmStrict accepts only explicit cn or global values, returning an empty string otherwise.
func normalizeRealmStrict(raw string) Region {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(RegionCN):
		return RegionCN
	case string(RegionGlobal):
		return RegionGlobal
	}
	return ""
}

// realmFromDomain infers the domain from the hostname.
//
// Uses domain suffix rather than exact match: the global version has multiple subdomains (www.workbuddy.ai, api..., etc.),
// while all cn domains fall under codebuddy.cn / tencent.com.
func realmFromDomain(domain string) Region {
	trimmed := strings.ToLower(strings.TrimSpace(domain))
	trimmed = strings.TrimPrefix(trimmed, "https://")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if trimmed == "workbuddy.ai" || strings.HasSuffix(trimmed, ".workbuddy.ai") {
		return RegionGlobal
	}
	return RegionCN
}

// Realm returns the domain to which the credential belongs.
func (c *Credential) Realm() Region {
	if c == nil {
		return RegionCN
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.realm
}

// SetRealm overrides the credential domain (account-level override takes precedence over plugin config fallback).
func (c *Credential) SetRealm(region Region) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.realm = region
}

// AccessTokenValue safely reads the access token with a lock.
func (c *Credential) AccessTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken
}

// RefreshTokenValue safely reads the refresh token with a lock.
func (c *Credential) RefreshTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RefreshToken
}

// DomainValue safely reads the domain with a lock.
func (c *Credential) DomainValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Domain
}

// UIDValue safely reads the UID with a lock.
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// NicknameValue safely reads the nickname with a lock.
func (c *Credential) NicknameValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Nickname
}

// EnterpriseIDValue safely reads the enterprise identifier with a lock.
func (c *Credential) EnterpriseIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.EnterpriseID
}

// DeviceTokenValue safely reads the device token with a lock.
func (c *Credential) DeviceTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.DeviceToken
}

// ApplyTokenRefresh writes back the refresh result.
//
// Uses a "two-stage snapshot validation": the caller takes a snapshot before initiating the network request, and upon receiving the response,
// Compare whether the current value is still the snapshot — if it has been concurrently refreshed, abort the write-back to avoid overwriting the new value with an old response.
func (c *Credential) ApplyTokenRefresh(accessToken, refreshToken, domain string, expiresAt int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if trimmed := strings.TrimSpace(accessToken); trimmed != "" {
		c.AccessToken = trimmed
	}
	if trimmed := strings.TrimSpace(refreshToken); trimmed != "" {
		c.RefreshToken = trimmed
	}
	if trimmed := strings.TrimSpace(domain); trimmed != "" {
		c.Domain = trimmed
	}
	if expiresAt > 0 {
		c.ExpiresAt = expiresAt
	}
}

// Snapshot returns the current value of the access/refresh token for two-phase refresh validation.
func (c *Credential) Snapshot() (accessToken, refreshToken string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken, c.RefreshToken
}

// UnchangedSince reports whether the credential still matches the snapshot.
func (c *Credential) UnchangedSince(accessToken, refreshToken string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken == accessToken && c.RefreshToken == refreshToken
}

// StorageJSON serializes the credential into a nested format for writing back to the CPA credential file.
//
// Writing in a nested format is deliberate: CPA's auth file may also contain user-maintained fields
// (disabled / prefix / proxy_url / note / weight), and callers should perform a **merged update**
// rather than replacing the whole thing with this method's output (see MergeStorageJSON).
func (c *Credential) StorageJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	auth := map[string]any{
		"accessToken":  c.AccessToken,
		"refreshToken": c.RefreshToken,
		"expiresAt":    c.ExpiresAt,
	}
	if c.Domain != "" {
		auth["domain"] = c.Domain
	}
	if c.realm != "" {
		auth["realm"] = string(c.realm)
	}
	account := map[string]any{}
	if c.UID != "" {
		account["uid"] = c.UID
	}
	if c.EnterpriseID != "" {
		account["enterpriseId"] = c.EnterpriseID
	}
	if c.Nickname != "" {
		account["nickname"] = c.Nickname
	}
	out := map[string]any{
		// The type must be written: the host uses it to **directly determine ownership**; if present, it finds
		// this plugin by provider without iterating through plugins. Without it, the host falls back to filling
		// the provider with the "currently queried plugin's identifier", and preceding plugins with overly broad
		// ownership determination will steal this plugin's credentials, causing accounts to be misattributed to other providers on the host side (manifesting as accounts disappearing into thin air in the UI).
		"type": ProviderKey,
		"auth": auth,
	}
	if len(account) > 0 {
		out["account"] = account
	}
	if c.DeviceToken != "" {
		out["device_token"] = c.DeviceToken
	}
	return json.MarshalIndent(out, "", "  ")
}

// MergeStorageJSON performs a merged update on the original credential JSON and returns the new bytes.
//
// Merging based on the original file rather than constructing from scratch: CPA's auth file may contain user-maintained
// fields such as disabled / prefix / proxy_url / note / weight, and rebuilding it would silently drop them.
func MergeStorageJSON(original []byte, cred *Credential) ([]byte, error) {
	updated, errCred := cred.StorageJSON()
	if errCred != nil {
		return original, errCred
	}
	if len(original) == 0 {
		return updated, nil
	}
	var existing map[string]any
	if errUnmarshal := json.Unmarshal(original, &existing); errUnmarshal != nil {
		// The original file is not an object (or is corrupted): fall back to full replacement.
		return updated, nil
	}
	var fresh map[string]any
	if errUnmarshal := json.Unmarshal(updated, &fresh); errUnmarshal != nil {
		return original, errUnmarshal
	}
	mergeMaps(existing, fresh)
	// Fallback: regardless of whether the original file has a type, the merge result must include it.
	// Old credentials copied by users from elsewhere often lack this field; adding it settles ownership.
	if _, okType := existing["type"]; !okType {
		existing["type"] = ProviderKey
	}
	return json.MarshalIndent(existing, "", "  ")
}

// mergeMaps recursively merges the key-values of src into dst (src takes precedence).
func mergeMaps(dst, src map[string]any) {
	for key, value := range src {
		if srcMap, okSrc := value.(map[string]any); okSrc {
			if dstMap, okDst := dst[key].(map[string]any); okDst {
				mergeMaps(dstMap, srcMap)
				continue
			}
		}
		dst[key] = value
	}
}

// SaveCredentialFile atomically writes back the credential file.
func SaveCredentialFile(path string, cred *Credential) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil
	}
	original, errRead := os.ReadFile(trimmed)
	if errRead != nil {
		original = nil
	}
	merged, errMerge := MergeStorageJSON(original, cred)
	if errMerge != nil {
		return fmt.Errorf("merge credential file %s: %w", trimmed, errMerge)
	}
	tmp := trimmed + ".tmp"
	if errWrite := os.WriteFile(tmp, merged, 0o600); errWrite != nil {
		return fmt.Errorf("write credential file %s: %w", tmp, errWrite)
	}
	if errRename := os.Rename(tmp, trimmed); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace credential file %s: %w", trimmed, errRename)
	}
	return nil
}

// LooksLikeCredential determines whether a JSON belongs to this plugin (any region).
//
// **Ownership determination must be strict**: the host feeds all non-built-in credential files to each plugin in turn.
// Once another plugin's credentials are mistakenly swallowed, the host will use them to write out this plugin's credential structure, effectively polluting the other party's account
// (and this error is silent). Conversely, missing an identification is just a loading failure, which the user will notice immediately.
//
// Therefore, only **evidence specific to this plugin** is accepted, from strongest to weakest:
//  1. The type field in the file or the provider hint passed by the host is freetier;
//  2. The filename matches this plugin's naming convention (workbuddycn / workbuddyglobal prefix/infix);
//  3. It possesses this plugin's unique nested structure (auth.accessToken + auth.refreshToken).
//
// Deliberately **not** accepting top-level accessToken / refreshToken / device_token:
// These are common fields also used by other providers (e.g., some plugins also write device_token),
// and using them as criteria will inevitably lead to cross-plugin false positives.
//
// Note: This function only determines "belongs to this protocol" and **does not determine the region** — the region is determined by core.ResolveVendor
// 's file name prefix and vendor field, or re-confirmed by the specific vendor instance's Match.
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	// 1) Explicit declaration (most reliable and recommended writing: supports current freetier and historical workbuddy / codebuddy identifiers).
	for _, expected := range []string{ProviderKey, "workbuddy", "codebuddy"} {
		if strings.EqualFold(strings.TrimSpace(provider), expected) {
			return true
		}
		if typeValue, okType := raw["type"].(string); okType && strings.EqualFold(strings.TrimSpace(typeValue), expected) {
			return true
		}
	}

	// 2) Filename convention.
	if core.FileNameMatchesConvention(fileName, VendorIDCN, VendorIDGlobal, protocolName) {
		return true
	}

	// 3) This plugin's unique nested structure: auth contains both accessToken and refreshToken.
	// Having accessToken alone is not enough to determine (other providers may also nest this way).
	if nested, okNested := raw["auth"].(map[string]any); okNested {
		_, hasAccess := nested["accessToken"]
		_, hasRefresh := nested["refreshToken"]
		if hasAccess && hasRefresh {
			return true
		}
	}

	// 4) Domain features: workbuddy.ai or codebuddy.cn domain.
	domain := core.FirstNonEmpty(core.GetString(raw, "domain"), core.GetString(core.GetMap(raw, "auth"), "domain"))
	if d := strings.ToLower(domain); strings.Contains(d, "workbuddy.ai") || strings.Contains(d, "codebuddy.cn") {
		return true
	}
	return false
}

// ProviderKey is the provider key occupied by this plugin in CPA.
//
// **Shared by all providers**: the host's auth.identifier only returns a single string, called once at registration
// and cached permanently, and only claims the file if it strictly equals the auth file's type field. Therefore, provider distinction
// It applies to the credential file name prefix (workbuddycn-<uid>.json) and the vendor field inside the file,
// rather than the provider key.
const ProviderKey = core.ProviderKey

// RegionForCredential infers the region from the credential JSON, returning empty if it cannot be inferred.
//
// Determination order: nested auth.realm -> auth.domain suffix -> flat realm -> domain suffix.
// Exported for root-level vendor adapters: credential ownership determination requires it, and the determination logic belongs to the protocol layer.
func RegionForCredential(raw map[string]any) Region {
	if raw == nil {
		return ""
	}
	if nested, okNested := raw["auth"].(map[string]any); okNested {
		if realm := realmField(nested); realm != "" {
			return realm
		}
	}
	return realmField(raw)
}

// realmField reads the region from a layer of JSON: checking the explicit realm field first, then the domain suffix.
func realmField(level map[string]any) Region {
	if realm, okRealm := level["realm"].(string); okRealm && strings.TrimSpace(realm) != "" {
		if strict := normalizeRealmStrict(realm); strict != "" {
			return strict
		}
	}
	if domain, okDomain := level["domain"].(string); okDomain {
		if inferred := realmFromDomain(domain); inferred != "" {
			return inferred
		}
	}
	return ""
}

// This file implements daily check-in for WorkBuddy (CN edition).
//
// The daily-checkin endpoint only credits the CN edition; the Global edition's daily earning path is daily active rewards
// (active beacons, see tasks.go), routed by realm via Checkin/RunScheduled,
// and Global credentials should not be sent to this endpoint.

// This file implements credential refresh for WorkBuddy.
//
// Refresh chain: POST <ChatBase>/v2/plugin/auth/token, using the X-Refresh-Token header
// with the old refresh token to exchange for a new set of tokens.
//
// Security red line: The X-Refresh-Token header **is only allowed on this refresh endpoint**, and chat requests
// must never carry it—sending a refresh token to a chat upstream exposes long-term credentials to a service
// that only needs an access token.

// This file implements quota query for WorkBuddy.
//
// The quota metric is **credit balance**: the upstream billing interface returns remain/used/size by resource package,
// which is aggregated here into an account-level "remaining / total" while retaining plan details.

// packageEndLayout is the wall-clock format for plan expiration time (UTC+8).
const packageEndLayout = "2006-01-02 15:04:05"

// This file implements CodeBuddy's growth task event chain and client fingerprint derivation.
//
// This package is a pure logic layer, relying on the cb package for outbound calls (via the host HTTP bridge).
//
// The essence of the entire system is **four sets of client fingerprints**: the same /v2/report endpoint,
// where the server assigns requests to different task families based on the fingerprint:
//
//	CLI/billing fingerprint  Billing domain /v2/report    Chat activity reporting (chat_5, consecutive logins, adoption prerequisites)
//	Desktop fingerprint        Chat domain /v2/report       Desktop behavior event chain (RichMeow, templates, experts…)
//	Web fingerprint            Web domain /v2/report        Library reading and other web-side behaviors
//	Mini-program fingerprint   Billing domain /v2/report    Back-to-school season and mini-program exclusive tasks
//
// Three universal rules (all summarized from real failures):
//   - The body of /v2/report is always an **array**, not a single object;
//   - Reporting 200 ≠ scoring, must poll and read back (normal 4×3s, mp 2×3s, back-to-school season 3×2.5s);
//   - Events must include userId; if missing, the server returns 200 but silently discards them.

// Throttling parameters. Copied entirely from the original project's measured values—these numbers are risk control boundaries,
// speeding them up triggers upstream rate limiting, while slowing them down just wastes time.
const ()

// Polling parameters for asynchronous scoring.
var ()

// nowUnixMs returns the current millisecond timestamp.
func nowUnixMs() int64 { return time.Now().UnixMilli() }

// desktopFingerprint is the common fields injected into every event by the desktop fingerprint family.
//
// These fields collectively identify the request as "coming from a fixed Windows desktop client".
// Business fields can override them (business takes precedence during map merging).
func desktopFingerprint(credential *Credential, rt *runtime) map[string]any {
	now := nowUnixMs()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       credential.UIDValue(),
		"username":     credential.NicknameValue(),
		"userNickname": credential.NicknameValue(),
		"product":      "SaaS",
		// releaseDate and commit are official desktop version build info, fixed values are sufficient.
		"releaseDate": int64(1789036585355),
		"commit":      "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":     "WorkBuddy",
		"ideType":     "WorkBuddy",
		"ideVersion":  DesktopClientVersion,
		"machineId":   rt.fingerprint(credential, "machine"),
		"sessionId":   rt.fingerprint(credential, "session"),
		"extName":     "workbuddy-desktop",
		"extVersion":  DesktopClientVersion,
		"os":          "win32",
		"arch":        "x64",
		"osVersion":   "10.0.26220",
		"cpuCores":    20,
		"memorySize":  24,
		"timestamp":   now,
		"presentAt":   now,
	}
}

// mergeEvent merges business fields into the fingerprint base (business fields take priority).
func mergeEvent(base map[string]any, event map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(event))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range event {
		merged[key] = value
	}
	return merged
}

// DefaultBuddyID / DefaultBuddyName are for the Tencent Teacher Assistant app (required by Buddy_App_QQ).
const (
	DefaultBuddyID   = "cb_y5Dy46tPQGGWtueMxXbe"
	DefaultBuddyName = "企鹅教师助手"
)

// Desktop fingerprint family request context.
type desktopContext struct {
	credential *Credential
	events     []map[string]any
}

// run attaches the fingerprint to the event and sends it out.
func (c *desktopContext) run(ctx context.Context, client *Client) error {
	base := desktopFingerprint(c.credential, client.rt)
	merged := make([]map[string]any, 0, len(c.events))
	for _, event := range c.events {
		merged = append(merged, mergeEvent(base, event))
	}
	return client.ReportDesktopEvents(ctx, c.credential, merged)
}

// This file generates request IDs and extracts session keys from request bodies.

// NewHexID generates a 32-bit hexadecimal random ID.
func NewHexID() string {
	return core.RandomHex(16)
}

func newHexID() string {
	return NewHexID()
}

// NewUUIDv7Hex generates a hyphen-free UUIDv7 (32-bit hex).
//
// Official client conversationRequestId / messageId are both hyphen-free UUIDv7
// (client assertion regex ^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$),
// pure random hex is not a valid v7 format.
func NewUUIDv7Hex() string {
	var buf [16]byte
	if _, errRead := rand.Read(buf[:]); errRead != nil {
		// Random source failed: fallback to pure random legacy format, ID only needs to be unique.
		return NewHexID()
	}
	// 48bit millisecond timestamp + version 7 + random; variant set to 10 per RFC 9562.
	ms := time.Now().UnixMilli()
	buf[0] = byte(ms >> 40)
	buf[1] = byte(ms >> 32)
	buf[2] = byte(ms >> 24)
	buf[3] = byte(ms >> 16)
	buf[4] = byte(ms >> 8)
	buf[5] = byte(ms)
	buf[6] = (buf[6] & 0x0F) | 0x70
	buf[8] = (buf[8] & 0x3F) | 0x80
	return hex.EncodeToString(buf[:])
}

// ConversationKey extracts the session key from the request body to generate a stable session header family.
//
// Recognition order copies the original implementation (from fine to coarse grain):
//  1. metadata.conversation_id / conversationId
//  2. Top-level conversation_id / conversationId
//  3. prompt_cache_key
//  4. Derived key: system text + hash of the first user message
//
// Deliberately **not** using user_id as a session key: all sessions of the same user would stick to the same key,
// which instead ruins session isolation.
func ConversationKey(body []byte) string {
	payload := decodeBodyMap(body)
	if payload == nil {
		return ""
	}
	if metadata, okMeta := payload["metadata"].(map[string]any); okMeta {
		for _, key := range []string{"conversation_id", "conversationId"} {
			if value, okValue := metadata[key].(string); okValue && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	for _, key := range []string{"conversation_id", "conversationId"} {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if value, okValue := payload["prompt_cache_key"].(string); okValue && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return deriveConversationKey(payload)
}

// deriveConversationKey derives a session key using the system and the first user message.
//
// Generic OpenAI clients do not pass any session identifiers, derived keys allow them to also benefit from session aggregation.
// Prefix "d-" indicates this is a derived value (to distinguish it from values explicitly passed by the client).
func deriveConversationKey(payload map[string]any) string {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		return ""
	}
	systemText := ""
	firstUser := ""
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			if systemText == "" {
				systemText = contentSignature(message["content"])
			}
		case "user":
			if firstUser == "" {
				firstUser = contentSignature(message["content"])
			}
		}
	}
	if firstUser == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUser))
	return "d-" + hex.EncodeToString(sum[:])[:16]
}

// contentSignature compresses message content into a stable short signature.
//
// Array format (multimodal) only takes the text part and preserves the types of non-text parts, so
// image messages will not collide with pure text messages for the same signature.
func contentSignature(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			if text, okText := part["text"].(string); okText {
				parts = append(parts, text)
				continue
			}
			partType, _ := part["type"].(string)
			sum := sha256.Sum256([]byte(partType))
			parts = append(parts, "["+partType+":"+hex.EncodeToString(sum[:4])+"]")
		}
		return strings.Join(parts, "\x1f")
	}
	return ""
}

// RealmDisplayName returns the display name of the realm.
func RealmDisplayName(region Region) string {
	if region.IsGlobal() {
		return "Global (WorkBuddy AI)"
	}
	return "CN (CodeBuddy)"
}
