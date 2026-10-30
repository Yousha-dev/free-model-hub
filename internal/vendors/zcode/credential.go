package zcode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"free-model-hub/internal/core"
)

// ZCode credential parsing, feature recognition, and desensitization.
//
// A ZCode account may hold two types of credentials simultaneously:
//
//   - JWTToken (zcode.z.ai, Bearer) -- Start Plan chat channel, quota queries, and plan redemption;
//   - APIKey (x-api-key) -- Chat channel for pure API Key accounts.
//
// OAuth login obtains both at once (JWT is issued directly, API Key is exchanged via access_token).

// Credential is the normalized ZCode account credential.
type Credential struct {
	Vendor   string        `json:"vendor,omitempty"`
	APIKey   string        `json:"api_key,omitempty"`
	JWTToken string        `json:"jwt_token,omitempty"`
	Email    string        `json:"email,omitempty"`
	UserID   string        `json:"user_id,omitempty"`
	Profile  DeviceProfile `json:"profile,omitempty"`
	Label    string        `json:"label,omitempty"`
	AuthMode string        `json:"auth_mode,omitempty"` // oauth / apikey
}

// UsesPlanChannel reports whether this credential supports the Start Plan (JWT) channel.
func (c *Credential) UsesPlanChannel() bool {
	return c != nil && strings.TrimSpace(c.JWTToken) != ""
}

// OutboundToken returns the outbound chat credential (preferring API Key, falling back to JWT).
func (c *Credential) OutboundToken() string {
	if c == nil {
		return ""
	}
	if strings.TrimSpace(c.APIKey) != "" {
		return c.APIKey
	}
	return strings.TrimSpace(c.JWTToken)
}

// AccountKey returns a stable identifier used for account serialization: user_id is preferred, falling back to email,
// and then to the outbound credential fingerprint (manually imported credentials may lack both); abnormal
// objects with completely missing credential fields return an empty string, to be handled by callers with an empty key fallback.
func (c *Credential) AccountKey() string {
	if c == nil {
		return ""
	}
	if key := strings.TrimSpace(c.UserID); key != "" {
		return "uid:" + key
	}
	if key := strings.TrimSpace(c.Email); key != "" {
		return "email:" + key
	}
	token := c.OutboundToken()
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return "token:" + hex.EncodeToString(sum[:8])
}

// ParseCredential parses the credential JSON.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("zcode credential is empty")
	}

	var m map[string]any
	if errUnmarshal := json.Unmarshal(raw, &m); errUnmarshal != nil {
		return nil, fmt.Errorf("decode zcode credential json: %w", errUnmarshal)
	}

	c := &Credential{
		Vendor:   core.FirstNonEmpty(core.GetString(m, "vendor"), VendorID),
		APIKey:   core.FirstNonEmpty(core.GetString(m, "api_key"), core.GetString(m, "apiKey")),
		JWTToken: core.FirstNonEmpty(core.GetString(m, "jwt_token"), core.GetString(m, "jwtToken")),
		Email:    strings.ToLower(core.GetString(m, "email")),
		UserID:   core.GetString(m, "user_id"),
		Label:    core.GetString(m, "label"),
		AuthMode: core.GetString(m, "auth_mode"),
	}

	c.Profile = parseProfile(m)
	c.Profile.ensure()

	if c.APIKey == "" && c.JWTToken == "" {
		return nil, fmt.Errorf("zcode credential missing api_key or jwt_token")
	}
	if c.AuthMode == "" {
		c.AuthMode = "apikey"
	}
	return c, nil
}

// parseProfile reads the device profile (supporting fingerprint objects embedded at the top level or under profile/fingerprint keys).
func parseProfile(m map[string]any) DeviceProfile {
	nested := m
	for _, key := range []string{"profile", "fingerprint", "device"} {
		if sub, ok := m[key].(map[string]any); ok && sub != nil {
			nested = sub
			break
		}
	}
	return DeviceProfile{
		Platform:  core.GetString(nested, "platform"),
		Arch:      core.GetString(nested, "arch"),
		OSVersion: core.FirstNonEmpty(core.GetString(nested, "os_version"), core.GetString(nested, "osVersion")),
		Language:  core.GetString(nested, "language"),
		Timezone:  core.GetString(nested, "timezone"),
		Screen:    core.GetString(nested, "screen"),
		DeviceMID: core.FirstNonEmpty(
			core.GetString(nested, "device_mid"),
			core.GetString(nested, "deviceMid"),
			core.GetString(m, "device_mid"),
		),
	}
}

// StorageJSON exports the standardized persistence JSON.
func (c *Credential) StorageJSON() ([]byte, error) {
	m := map[string]any{
		"type":      "freehub",
		"vendor":    VendorID,
		"auth_mode": core.FirstNonEmpty(c.AuthMode, "apikey"),
	}
	if c.APIKey != "" {
		m["api_key"] = c.APIKey
	}
	if c.JWTToken != "" {
		m["jwt_token"] = c.JWTToken
	}
	if c.Email != "" {
		m["email"] = c.Email
	}
	if c.UserID != "" {
		m["user_id"] = c.UserID
	}
	if c.Label != "" {
		m["label"] = c.Label
	}
	// Profile is always persisted: if device_mid changes, the upstream considers it a new device and it must be persisted.
	m["profile"] = c.Profile
	return json.MarshalIndent(m, "", "  ")
}

// MergeStorageJSON merges the refreshed credentials back into the original file.
//
// Only updates changing fields, preserving other keys manually maintained by the user in the auth file.
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if updated == nil {
		return original, nil
	}
	// type / vendor are written unconditionally: ownership determination relies on these keys, so even if an old
	// credential copied from elsewhere lacks them, the merge result must supply them.
	setFields := func(m map[string]any) {
		m["type"] = "freehub"
		m["vendor"] = VendorID
		if updated.APIKey != "" {
			m["api_key"] = updated.APIKey
		}
		if updated.JWTToken != "" {
			m["jwt_token"] = updated.JWTToken
		}
		if updated.Email != "" {
			m["email"] = updated.Email
		}
		if updated.UserID != "" {
			m["user_id"] = updated.UserID
		}
		if updated.AuthMode != "" {
			m["auth_mode"] = updated.AuthMode
		}
		if updated.Label != "" {
			m["label"] = updated.Label
		}
		if updated.Profile.DeviceMID != "" {
			m["profile"] = updated.Profile
		}
	}
	// fresh and apply share setFields: when the original file is missing or corrupted, the produced object
	// is completely identical to the one on "merge success", and type/vendor will definitely be present.
	return core.MergeCredentialJSON(original, func() ([]byte, error) {
		fresh := map[string]any{}
		setFields(fresh)
		return json.MarshalIndent(fresh, "", "  ")
	}, setFields)
}

// LooksLikeCredential determines whether a file or data belongs to ZCode.
//
// **Must be strict**: The host feeds all non-builtin format credentials sequentially to plugins, and mistakenly swallowing
// another plugin's credentials will cause the host to overwrite that account with this plugin's structure. Therefore, claim only upon clear evidence of ownership.
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	// Explicit vendor field takes precedence: if another vendor is declared, never claim it.
	if decided, claimed := core.VendorFieldVerdict(raw, VendorID); decided {
		return claimed
	}

	// Filename prefix matching (conventionally managed centrally by core).
	baseName := strings.ToLower(strings.TrimSpace(fileName))
	if core.FileNameMatchesConvention(fileName, VendorID) {
		return true
	}

	// 3) Carries ZCode-exclusive traces: profile keys or auth_mode markers.
	//    Neither of these keys will appear in other providers' credentials, serving as strong features.
	if _, ok := raw["profile"]; ok {
		if core.GetString(raw, "device_mid") != "" || core.GetString(raw, "jwt_token") != "" {
			return true
		}
	}

	// 4) Carries both jwt_token and api_key (the dual-credential form of this ZCode plugin).
	if core.GetString(raw, "jwt_token") != "" && core.GetString(raw, "api_key") != "" {
		return true
	}

	// 5) Zhipu/Z.AI API Key format (xxx.xxx) + filename containing zcode/zai
	key := core.FirstNonEmpty(core.GetString(raw, "api_key"), core.GetString(raw, "apiKey"))
	if key != "" && strings.Count(key, ".") == 1 &&
		(strings.Contains(baseName, "zcode") || strings.Contains(baseName, "zai")) {
		return true
	}

	return false
}

// fileNameFor generates the ZCode credential filename (following the plugin's <vendor>-<identifier>.json convention).
func fileNameFor(identifier string) string {
	return core.FileNameFor(VendorID, identifier)
}

// UserIDFromJWT extracts user_id from the JWT payload (user_id preferred, sub fallback).
//
// user_id is the user identifier reported during official client activation, parsed in real-time from the token rather than cached:
// user_id will automatically follow when the token is refreshed.
func UserIDFromJWT(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	claims := decodeJWTPayload(parts[1])
	for _, key := range []string{"user_id", "sub"} {
		if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// EmailFromJWT extracts email from the JWT payload (fallback when OAuth user profile is missing).
func EmailFromJWT(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	claims := decodeJWTPayload(parts[1])
	for _, key := range []string{"email", "user_email", "mail"} {
		if v, ok := claims[key].(string); ok && strings.Contains(v, "@") {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

// decodeJWTPayload decodes a base64url-encoded payload; returns an empty map on failure.
func decodeJWTPayload(segment string) map[string]any {
	decoded, errDecode := base64URLDecode(segment)
	if errDecode != nil {
		return nil
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(decoded, &claims); errUnmarshal != nil {
		return nil
	}
	return claims
}

// Common request layer for the billing family (billing/*, usage).
//
// These endpoints are all under zcode.z.ai's zcode-plan, share the same identity headers, and **must**
// include X-Device-Mid -- the upstream returns code=3001 when missing. Authentication uses Coding Plan JWT
// (API Key channels have no concept of plans and do not participate in the billing family).
//
// event/report is not included: it is not under zcode-plan and does not require authentication, see activation.go.
//
// Refer to _auth_headers in zcode2api/app/quota.py and _billing_request in app/claim.py.

const (
	// billingHTTPTimeout is the single request limit for the billing family.
	billingHTTPTimeout = 20 * time.Second
	// billingMaxBody is the read limit for billing responses.
	billingMaxBody = 4 << 20
)

// Quota query: billing/balance (held plan + remaining quota).
//
// Only meaningful for Coding Plan (JWT) accounts—API Keys are user-provided pay-as-you-go keys
// and have no plan window. Refer to zcode2api/app/quota.py.

// Client device profile per account (one device per account) and upstream identity header construction.
//
// Why it is needed: the billing family (balance/preview/claim) **strictly requires** X-Device-Mid,
// and when missing the upstream returns code=3001; meanwhile, upstream clusters by device for risk control,
// so multiple accounts sharing the same platform/kernel/resolution aggregate into a correlation signal of "N accounts opened on one machine".
//
// Therefore, profiles are sampled from **complete SKU sets** (platform × arch × os_version × screen bindings),
// prohibiting field Cartesian products—combinations like "darwin-arm64 + 1366x768" are not real computers.
// Linux is excluded from the pool: the official desktop form factor is mainly Mac / Windows, whereas server kernel characteristics stand out.
//
// Refer to zcode2api/app/fingerprint.py and app/identity.py.

// DeviceProfile is a single account's device profile, with fields directly mapping upstream identity headers and event fields.
type DeviceProfile struct {
	Platform  string `json:"platform"`   // darwin / win32
	Arch      string `json:"arch"`       // arm64 / x64
	OSVersion string `json:"os_version"` // X-Os-Version (os.release() semantics)
	Language  string `json:"language"`   // X-Client-Language
	Timezone  string `json:"timezone"`   // X-Client-Timezone (IANA)
	Screen    string `json:"screen"`     // Activation event screen_resolution
	DeviceMID string `json:"device_mid"` // X-Device-Mid, one per account
}

// PlatformFull returns the X-Platform value format (darwin-arm64).
func (p DeviceProfile) PlatformFull() string {
	if p.Platform == "" {
		return DefaultPlatform
	}
	return p.Platform + "-" + p.Arch
}

// OSCategory returns X-Os-Category (darwin→macos / win32→windows).
func (p DeviceProfile) OSCategory() string {
	switch p.Platform {
	case "darwin", "macos":
		return "macos"
	case "win32", "windows":
		return "windows"
	default:
		return "linux"
	}
}

// sku is a complete, usable desktop device combination.
type sku struct {
	weight    int
	platform  string
	arch      string
	osVersion string
	screen    string
}

// desktopSKUs are complete combinations generalized from common official desktop form factors.
//
// Weights represent sampling probabilities: Apple silicon and Win11 mainstream models account for the majority, with older models having a small share.
var desktopSKUs = []sku{
	// Apple silicon MacBook Air/Pro 13–14" (darwin 24 = Sequoia, 25 = Tahoe)
	{10, "darwin", "arm64", "24.5.0", "1512x982"},
	{10, "darwin", "arm64", "24.6.0", "1512x982"},
	{8, "darwin", "arm64", "24.5.0", "1728x1117"},
	{8, "darwin", "arm64", "24.6.0", "1728x1117"},
	{8, "darwin", "arm64", "25.5.0", "1512x982"},
	{6, "darwin", "arm64", "25.5.0", "1728x1117"},
	{5, "darwin", "arm64", "23.6.0", "1512x982"},
	{4, "darwin", "arm64", "24.5.0", "2560x1440"},
	{3, "darwin", "arm64", "24.6.0", "2560x1600"},
	// Intel Mac legacy stock (darwin 24+ no longer equips x64)
	{2, "darwin", "x64", "23.6.0", "1920x1080"},
	{2, "darwin", "x64", "22.6.0", "1440x900"},
	// Windows 11 mainstream + a small amount of Win10
	{8, "win32", "x64", "10.0.22631", "1920x1080"},
	{7, "win32", "x64", "10.0.26100", "1920x1080"},
	{5, "win32", "x64", "10.0.22631", "2560x1440"},
	{4, "win32", "x64", "10.0.26200", "1920x1080"},
	{3, "win32", "x64", "10.0.26100", "2560x1440"},
	{3, "win32", "x64", "10.0.22621", "1920x1080"},
	{2, "win32", "x64", "10.0.22631", "3840x2160"},
	{2, "win32", "x64", "10.0.19045", "1920x1080"},
	{1, "win32", "x64", "10.0.22000", "1920x1080"},
}

// locales are realistic regional pairs (language ↔ timezone homologous, shared by activation events).
var locales = []struct{ language, timezone string }{
	{"zh-CN", "Asia/Shanghai"},
	{"en-US", "America/New_York"},
	{"en-US", "America/Los_Angeles"},
	{"en-GB", "Europe/London"},
	{"de-DE", "Europe/Berlin"},
	{"ja-JP", "Asia/Tokyo"},
	{"ko-KR", "Asia/Seoul"},
	{"en-SG", "Asia/Singapore"},
}

// NewDeviceProfile samples and generates a complete desktop profile (called once when logging into the pool and persisted).
func NewDeviceProfile() DeviceProfile {
	total := 0
	for _, item := range desktopSKUs {
		total += item.weight
	}
	picked := desktopSKUs[0]
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(total))); err == nil {
		remaining := n.Int64()
		for _, item := range desktopSKUs {
			remaining -= int64(item.weight)
			if remaining < 0 {
				picked = item
				break
			}
		}
	}
	locale := locales[0]
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(len(locales)))); err == nil {
		locale = locales[n.Int64()]
	}
	return DeviceProfile{
		Platform:  picked.platform,
		Arch:      picked.arch,
		OSVersion: picked.osVersion,
		Language:  locale.language,
		Timezone:  locale.timezone,
		Screen:    picked.screen,
		DeviceMID: NewUUID(),
	}
}

// ensureProfile fills in missing fields in the profile in place (used for upgrading old credentials).
//
// If the whole device is missing, fill a complete SKU set; otherwise only fill empty fields **within the same platform**—
// mixing platforms produces self-contradictory profiles (such as win32 + macOS kernel version), which is worse than missing fields.
func (p *DeviceProfile) ensure() {
	if p.Platform == "" && p.Arch == "" && p.OSVersion == "" && p.Screen == "" {
		fresh := NewDeviceProfile()
		fresh.DeviceMID = core.FirstNonEmpty(p.DeviceMID, fresh.DeviceMID)
		*p = fresh
		return
	}
	if p.Platform == "" {
		p.Platform = "darwin"
	}
	if p.Arch == "" {
		if p.Platform == "win32" {
			p.Arch = "x64"
		} else {
			p.Arch = "arm64"
		}
	}
	if p.OSVersion == "" || !osVersionMatchesPlatform(p.Platform, p.OSVersion) {
		p.OSVersion = defaultOSVersionFor(p.Platform)
	}
	if p.Screen == "" {
		p.Screen = "1920x1080"
	}
	if p.Language == "" {
		p.Language = DefaultLanguage
	}
	if p.Timezone == "" {
		p.Timezone = DefaultTimezone
	}
	if p.DeviceMID == "" {
		p.DeviceMID = NewUUID()
	}
}

// osVersionMatchesPlatform reports whether the kernel version string matches the platform.
//
// Upstream parses this field's format by platform; mixing them (win32 + 25.5.0) is an obvious spoofing signal.
func osVersionMatchesPlatform(platform, osVersion string) bool {
	switch platform {
	case "win32", "windows":
		return strings.HasPrefix(osVersion, "10.")
	case "darwin", "macos":
		// darwin kernel version has two major version numbers (22–25).
		return !strings.HasPrefix(osVersion, "10.")
	default:
		return true
	}
}

// defaultOSVersionFor returns the default kernel version for a given platform.
func defaultOSVersionFor(platform string) string {
	switch platform {
	case "win32", "windows":
		return "10.0.22631"
	default:
		return DefaultOSVersion
	}
}

// IdentityHeaders is the set of X- identity headers required by upstream (excluding auth headers).
type IdentityHeaders struct {
	values map[string]string
}

// Set writes an identity header.
func (h *IdentityHeaders) Set(key, value string) {
	if h.values == nil {
		h.values = map[string]string{}
	}
	h.values[key] = value
}

// Apply writes identity headers to the target map (keeping key names in the exact casing required by upstream).
func (h *IdentityHeaders) Apply(target map[string]string) {
	for key, value := range h.values {
		target[key] = value
	}
}

// upstreamAuthHeaders are generic upstream identity and authentication headers.
//
// JWT takes priority (Coding Plan channel), falling back to API Key when absent: gateways on both
// paths recognize both credentials, though the billing family is only meaningful for JWT. Request-level trace headers are regenerated per request.
func upstreamAuthHeaders(cred *Credential) map[string]string {
	headers := map[string]string{}
	BuildIdentityHeaders(cred.Profile).Apply(headers)
	if strings.TrimSpace(cred.JWTToken) != "" {
		headers["Authorization"] = "Bearer " + cred.JWTToken
	} else if strings.TrimSpace(cred.APIKey) != "" {
		headers["x-api-key"] = cred.APIKey
	}
	headers["x-request-id"] = NewUUID()
	return headers
}

// BuildIdentityHeaders constructs the complete identity headers (aligning with the official desktop client companion header set).
//
// When an account is present, use its device profile; fall back to global camouflage constants if profile is zero-value.
func BuildIdentityHeaders(profile DeviceProfile) *IdentityHeaders {
	profile.ensure()
	headers := &IdentityHeaders{}
	headers.Set("HTTP-Referer", HTTPReferer)
	headers.Set("User-Agent", UserAgent)
	headers.Set("X-ZCode-App-Version", ClientAppVersion)
	headers.Set("X-Title", ClientTitle)
	headers.Set("X-ZCode-Agent", ClientAgent)
	headers.Set("X-Platform", profile.PlatformFull())
	headers.Set("X-Release-Channel", ReleaseChannel)
	headers.Set("X-Client-Language", profile.Language)
	headers.Set("X-Client-Timezone", profile.Timezone)
	headers.Set("X-Os-Category", profile.OSCategory())
	headers.Set("X-Os-Version", profile.OSVersion)
	headers.Set("X-Device-Mid", profile.DeviceMID)
	return headers
}

// BuildTraceHeaders constructs trace headers.
//
// JWT/Plan channel (= upstream start-plan) **only allows** these three headers: the official client does not send
// x-query-id / x-session-id, and sending them incorrectly triggers upstream 3012 "unusual activity" risk control.
// Regenerated per request.
func BuildTraceHeaders() map[string]string {
	return map[string]string{
		"x-request-id":         NewUUID(),
		"x-zcode-session-type": "main",
		"x-zcode-trace-id":     NewUUID(),
	}
}

// NewUUID generates an RFC 4122 v4 UUID.
func NewUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// NewHexID generates a random hex string of n bytes.
func NewHexID(n int) string { return core.RandomHex(n) }
