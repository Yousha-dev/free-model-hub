package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"free-model-hub/cpasdk/pluginapi"
)

// This file provides common parsing and conversion helpers for providers.
//
// These functions were originally duplicated across multiple provider packages (with drifting signatures) and are now consolidated here:
// Only one implementation is allowed per semantic, with provider packages only handling differences in keys and characteristics.

// FirstNonEmpty returns the first non-empty string after trimming whitespace (returns the trimmed value).
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// GetString tries multiple keys in order and returns the first non-empty string (trimmed).
func GetString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, key := range keys {
		value, okValue := m[key]
		if !okValue || value == nil {
			continue
		}
		if text, okText := value.(string); okText {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// GetMap tries multiple keys in order and returns the first map value (returns nil if missing or type mismatch).
func GetMap(m map[string]any, keys ...string) map[string]any {
	if m == nil {
		return nil
	}
	for _, key := range keys {
		value, okValue := m[key]
		if !okValue || value == nil {
			continue
		}
		if sub, okSub := value.(map[string]any); okSub {
			return sub
		}
	}
	return nil
}

// GetInt64 tries multiple keys in order and returns the first convertible integer.
//
// Does not perform unit normalization: providers that need to convert milliseconds to seconds should use GetUnixSeconds.
func GetInt64(m map[string]any, keys ...string) int64 {
	if m == nil {
		return 0
	}
	for _, key := range keys {
		value, okValue := m[key]
		if !okValue || value == nil {
			continue
		}
		switch number := value.(type) {
		case float64:
			return int64(number)
		case int64:
			return number
		case int:
			return int64(number)
		case json.Number:
			if parsed, errParse := number.Int64(); errParse == nil {
				return parsed
			}
		}
	}
	return 0
}

// millisecondThreshold is the threshold to determine if a timestamp appears to be in milliseconds.
//
// 1e12 seconds is approximately the year 33658, which normal second-level timestamps cannot exceed; conversely, millisecond-level
// timestamps (13 digits) will definitely exceed it. Upstream sources mix both units, so they are normalized here.
const millisecondThreshold = 1e12

// GetUnixSeconds is the same as GetInt64, but normalizes timestamps that are obviously in milliseconds to seconds.
func GetUnixSeconds(m map[string]any, keys ...string) int64 {
	value := GetInt64(m, keys...)
	if value > millisecondThreshold {
		return value / 1000
	}
	return value
}

// FileNameMatchesConvention determines whether a file name matches a provider's naming convention.
//
// Convention: <name>.json / <name>-<id>.json / <name>_<id>.json / <name>.<id>.json,
// or containing a -<name> / _<name> segment in the name (e.g., my-cline.json).
func FileNameMatchesConvention(fileName string, names ...string) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base == "" {
		return false
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		name = strings.ToLower(name)
		if base == name {
			return true
		}
		for _, separator := range []string{"-", "_", "."} {
			if strings.HasPrefix(base, name+separator) ||
				strings.Contains(base, separator+name) {
				return true
			}
		}
	}
	return false
}

// VendorFieldVerdict determines attribution based on the vendor field in the credential.
//
// Returns (decided, claimed):
//   - decided=false means the vendor field provides no conclusion (missing, or just a family alias,
//     such as "minimax" for minimaxcode, "trae" for trae), and the caller continues to check other evidence;
//   - decided=true and claimed=true means it explicitly belongs to this provider;
//   - decided=true and claimed=false means it explicitly belongs to another provider, and **must be rejected**.
//
// This section is the first gate for "strict attribution": if another provider is explicitly declared, it is never claimed.
func VendorFieldVerdict(raw map[string]any, vendorID string, aliases ...string) (decided, claimed bool) {
	declared := strings.ToLower(GetString(raw, "vendor"))
	if declared == "" {
		return false, false
	}
	if declared == strings.ToLower(strings.TrimSpace(vendorID)) {
		return true, true
	}
	for _, alias := range aliases {
		if declared == strings.ToLower(strings.TrimSpace(alias)) {
			return false, false
		}
	}
	return true, false
}

// NativeExtractor retrieves the native provider object from the normalized credential.
type NativeExtractor[T any] func(*Credential) (*T, bool)

// ConvertModels batch converts the provider's native model list into host contract types.
//
// vendorID passed as empty string: models for this plugin are registered with bare names (see SplitModelID explanation).
// convert returning false means skipping this entry (usually due to a missing ID).
func ConvertModels[T any](models []T, convert func(T) (ModelDescriptor, bool)) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		descriptor, okModel := convert(model)
		if !okModel {
			continue
		}
		out = append(out, ModelInfoToPluginAPI("", descriptor))
	}
	return out
}

// DefaultMaxOutputTokens is the fallback value when upstream does not provide an output limit.
//
// Set to 16384: this is the common default value for mainstream models across upstream providers, and the value
// consistently adopted in the fallback lists of both qoder2api and workbuddy2api.
const DefaultMaxOutputTokens int64 = 16384

// ContentTypeJSON is the JSON content type returned by the plugin to the host.
const ContentTypeJSON = "application/json; charset=utf-8"

// DefaultContextWindowTokens is the fallback context window when model metadata is missing.
//
// Consistent with the workbuddy fallback table (1,000,000): setting it too small causes clients to mistakenly think it exceeds the context,
// while setting it too large only causes upstream to reject it based on its own limits—the latter is recoverable.
const DefaultContextWindowTokens int64 = 1000000

// DefaultContextWindow returns the fallback context window (ignoring the model name).
func DefaultContextWindow(_ string) int64 { return DefaultContextWindowTokens }

// This file provides provider-agnostic request parsing helpers.

// ExtractClientIP extracts the client IP from request headers (X-Forwarded-For takes precedence).
func ExtractClientIP(header http.Header) string {
	if header == nil {
		return ""
	}
	if forwarded := strings.TrimSpace(header.Get("X-Forwarded-For")); forwarded != "" {
		if idx := strings.IndexByte(forwarded, ','); idx >= 0 {
			forwarded = forwarded[:idx]
		}
		return strings.TrimSpace(forwarded)
	}
	return strings.TrimSpace(header.Get("X-Real-IP"))
}

// This file provides random string and PKCE helpers shared across providers.
//
// This logic originally existed separately in codearts/qoder/zcode/opencodezen/minimaxcode/workbuddy
// (with inconsistent signatures); consolidated here, falling back to deterministic fallbacks when the random source is unavailable.

var randomFallbackCounter int64

// RandomHex generates an n-byte random hexadecimal string (2n characters).
//
// Fall back to a time- and counter-based deterministic string when the random source is unavailable: these IDs are only used for session/request identification
// uniqueness, and degraded entropy is more acceptable than failing the entire request chain.
func RandomHex(n int) string {
	if n <= 0 {
		n = 16
	}
	buf := make([]byte, n)
	if _, errRand := rand.Read(buf); errRand != nil {
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&randomFallbackCounter, 1))
	}
	return hex.EncodeToString(buf)
}

// jwtPayload decodes the second payload section of a JWT (without signature verification); returns nil if decoding fails.
func jwtPayload(token string) []byte {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode == nil {
		return payload
	}
	seg := parts[1]
	if rem := len(seg) % 4; rem > 0 {
		seg += strings.Repeat("=", 4-rem)
	}
	if payload, errDecode = base64.URLEncoding.DecodeString(seg); errDecode == nil {
		return payload
	}
	return nil
}

// MaskToken returns a masked token for display purposes: keeps the first 4 and last 4 characters, and fills the middle with *.
// If it does not exceed 8 characters, it is fully masked; returns an empty string for empty values.
func MaskToken(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 8 {
		return strings.Repeat("*", len(trimmed))
	}
	return trimmed[:4] + strings.Repeat("*", len(trimmed)-8) + trimmed[len(trimmed)-4:]
}

// JWTExpiry decodes the exp (seconds) from the JWT payload, returning 0 if it cannot be decoded. It does not verify the signature, only reads exp.
func JWTExpiry(token string) int64 {
	payload := jwtPayload(token)
	if len(payload) == 0 {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp
}

// JWTUserID decodes the user ID from the JWT payload (trying common keys in sequence), returning empty if it cannot be decoded.
func JWTUserID(token string) string {
	payload := jwtPayload(token)
	if len(payload) == 0 {
		return ""
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return ""
	}
	return GetString(claims, "user_id", "userId", "uid", "userID")
}

// TokenFingerprint returns a stable short fingerprint of the token (first 12 hex characters of SHA-256).
//
// Used to give an account a stable and mutually distinct file name suffix when "upstream does not provide a uid" (see FileNameFor):
// The same credential always yields the same value, different credentials are practically guaranteed not to be identical, and it is irreversible and unpersisted.
// Returns an empty string for empty values.
func TokenFingerprint(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:6])
}

// NewPKCEPair generates a PKCE verifier and S256 challenge.
func NewPKCEPair() (verifier, challenge string, err error) {
	buffer := make([]byte, 32)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return "", "", errRead
	}
	verifier = base64.RawURLEncoding.EncodeToString(buffer)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// FirstNonZero returns the first non-zero integer.
func FirstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

// IntFromAny converts a JSON numeric value to an int (returns 0 if missing or invalid).
func IntFromAny(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int64:
		return int(number)
	case int:
		return number
	case json.Number:
		parsed, errParse := number.Int64()
		if errParse == nil {
			return int(parsed)
		}
	}
	return 0
}

// StringField reads a string field from a map (with whitespace trimmed).
func StringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	value, _ := m[key].(string)
	return strings.TrimSpace(value)
}

// MaxInt returns the larger integer.
func MaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ContainsAny reports whether the text contains any of the tokens.
func ContainsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// SleepContext is a cancellable sleep; returns false if the context has been cancelled.
func SleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// RawStringField reads a string field from a map (without trimming whitespace).
func RawStringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	value, _ := m[key].(string)
	return value
}
