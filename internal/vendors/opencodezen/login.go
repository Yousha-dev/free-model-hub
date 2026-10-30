package opencodezen

import (
	"strings"
)

// AuthMode reports the credential format of this provider.
func (v *adapter) AuthMode() string { return "apikey" }

// KeyURL implements core.KeyInfoProvider.
func (v *adapter) KeyURL() string { return "https://opencode.ai/auth" }

// KeyNote implements core.KeyInfoProvider.
func (v *adapter) KeyNote() string {
	return "Not needed for free models: the keyless account already serves them. A Zen key only adds your own quota."
}

// MaskedKey returns the masked key: keeping the first four and last four characters, with *** in the middle.
//
// Used as a fallback for display names—**never** put the full key into the Label, because the Label goes into logs,
// management API responses, and page HTML. Credential materials should only exist in credential files.
func MaskedKey(key string) string {
	trimmed := strings.TrimSpace(key)
	// Keys that are too short are fully masked: exposing the first and last four characters is equivalent to exposing the whole thing.
	if len(trimmed) <= 8 {
		if trimmed == "" {
			return ""
		}
		return "***"
	}
	return trimmed[:4] + "***" + trimmed[len(trimmed)-4:]
}
