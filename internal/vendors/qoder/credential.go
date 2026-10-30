package qoder

// This file implements the parsing and attribution determination for Qoder credentials.
//
// Qoder's credential structure is much looser than WorkBuddy's (due to historical reasons):
// the token field has seven candidate names, or it might be a qoder2api export format
// where the entire pair of credentials is packed into the secret field's JSON string. Therefore, parsing should be as lenient as possible, while **attribution determination must be as strict as possible**—lenient in, strict out.

import (
	"encoding/json"
	"fmt"
	"strings"

	"free-model-hub/internal/core"
)

// Vendor instance identifier. Also acts as the credential filename prefix.
const (
	// VendorIDCN is the CN version (qoder.com.cn).
	VendorIDCN = "qodercn"
	// VendorIDGlobal is the Global version (qoder.com).
	VendorIDGlobal = "qoderglobal"

	// protocolName is the common identifier segment of this protocol in filenames.
	protocolName = "qoder"
)

// Region / RegionCN / RegionGlobal / NormalizeRegion are defined in endpoints.go
// —they are also server endpoint tables, belonging to the protocol-layer infrastructure.

// VendorIDFor returns the vendor identifier corresponding to the given region.
func VendorIDFor(region Region) string {
	if region == RegionCN {
		return VendorIDCN
	}
	return VendorIDGlobal
}

// VendorNameFor returns the display name for the given region.
func VendorNameFor(region Region) string {
	if region == RegionCN {
		return "Qoder CN"
	}
	return "Qoder Global"
}

// credentialTokenKeys are the field names in credentials that may carry a token.
//
// The seven candidates come from historical accumulation: different versions of qoder2api exports,
// desktop packet captures, and manual user entries all have their own formats. Trying all of them is much friendlier than asking the user to modify the file.
var credentialTokenKeys = []string{
	"token", "device_token", "access_token", "api_key",
	"personal_token", "pat", "qoder_token",
}

// Credential is the normalized Qoder credential.
type Credential struct {
	// Token is the outbound credential: PAT (pt-...) or device token (dt-...).
	Token string
	// RefreshToken is used for refreshing (drt-... prefix).
	RefreshToken string
	// Region is the region the account belongs to.
	Region Region
	// Label is the display name.
	Label string
	// Email is the account email (populated during the login flow).
	Email string

	// MachineToken / MachineType / MachineCode are the device identities issued by official components.
	//
	// The upstream performs **strict validation** on the active list based on device identity (see identity.go), and the plugin cannot
	// forge it; it can only be obtained by the user from the machine where they logged into the official desktop and filled into the credential file.
	// Empty means not provided, and the machine header will not be sent during check-in.
	MachineToken string
	MachineType  string
	MachineCode  string
}

// ParseCredential parses the Qoder credential JSON.
func ParseCredential(raw []byte, fallbackRegion Region) (*Credential, error) {
	cred := &Credential{Region: fallbackRegion}
	if len(raw) == 0 {
		return cred, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return cred, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred.Token = firstTokenIn(payload)
	if refresh, okRefresh := payload["refresh_token"].(string); okRefresh {
		cred.RefreshToken = strings.TrimSpace(refresh)
	}

	// The export format of qoder2api puts the entire pair of credentials into the JSON string of the secret field.
	// When directly importing such files (or copying export entries as-is), dt- and drt- must be successfully decoded.
	if nested, okNested := payload["secret"].(string); okNested && strings.TrimSpace(nested) != "" {
		trimmed := strings.TrimSpace(nested)
		inner := map[string]any{}
		if errInner := json.Unmarshal([]byte(trimmed), &inner); errInner == nil {
			if cred.Token == "" {
				cred.Token = firstTokenIn(inner)
			}
			if cred.RefreshToken == "" {
				if refresh, okRefresh := inner["refresh_token"].(string); okRefresh {
					cred.RefreshToken = strings.TrimSpace(refresh)
				}
			}
		} else if cred.Token == "" {
			// Non-JSON secrets are treated directly as credentials (consistent with upstream ParseOAuthSecret).
			cred.Token = trimmed
		}
	}
	if cred.Token == "" {
		return cred, fmt.Errorf("credential has no token field (tried: %s)", strings.Join(credentialTokenKeys, ", "))
	}
	if region, okRegion := payload["region"].(string); okRegion && strings.TrimSpace(region) != "" {
		cred.Region = NormalizeRegion(region)
	}
	if label, okLabel := payload["label"].(string); okLabel {
		cred.Label = strings.TrimSpace(label)
	}
	if email, okEmail := payload["email"].(string); okEmail {
		cred.Email = strings.TrimSpace(email)
	}
	cred.MachineToken = firstStringIn(payload, "machine_token", "machineToken")
	cred.MachineType = firstStringIn(payload, "machine_type", "machineType")
	cred.MachineCode = firstStringIn(payload, "machine_code", "machineCode")
	return cred, nil
}

// firstStringIn returns the first non-empty string value among the given keys.
func firstStringIn(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// firstTokenIn returns the first non-empty token field in a single layer of JSON.
func firstTokenIn(payload map[string]any) string {
	for _, key := range credentialTokenKeys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// LooksLikeCredential determines whether a JSON document belongs to Qoder (any region).
//
// **Determination must lean strict**: the host feeds all non-builtin credentials sequentially to each plugin.
// Mistakenly swallowing another plugin's credentials will cause the host to overwrite the other account with this plugin's structure (silently).
//
// Only accept Qoder-specific evidence:
//  1. Token values carry Qoder-specific prefixes (pt- / drt-);
//  2. Filenames contain the qoder segment;
//  3. The secret field is present (a characteristic of the qoder2api export format).
//
// **Intentionally avoiding the dt- prefix**: WorkBuddy's credentials also have a device_token field and use
// the dt- prefix (actual tests show both vendors use the same device token prefix), so using it as a criterion would inevitably cause cross-vendor misidentification.
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if hasQoderTokenPrefix(raw) {
		return true
	}
	if _, okSecret := raw["secret"].(string); okSecret {
		return true
	}
	if core.FileNameMatchesConvention(fileName, protocolName, VendorIDCN, VendorIDGlobal) {
		return true
	}
	_ = provider
	return false
}

// hasQoderTokenPrefix determines whether the token in the credential carries the Qoder-unique prefix.
//
// pt- Personal Access Token / drt- Refresh Token — these two prefixes are unique to Qoder.
// The device token's dt- prefix **is not included here**: WorkBuddy uses the same prefix as well.
func hasQoderTokenPrefix(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	for _, key := range append(append([]string{}, credentialTokenKeys...), "refresh_token") {
		value, okValue := raw[key].(string)
		if !okValue {
			continue
		}
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "pt-") || strings.HasPrefix(trimmed, "drt-") {
			return true
		}
	}
	return false
}

// RegionForCredential infers the region from the credential, returning empty if it cannot be inferred.
//
// There is only one region-related field, region (Qoder's endpoint table is not like WorkBuddy's which
// can infer from domain — it doesn't even have a domain field).
func RegionForCredential(raw map[string]any) Region {
	if raw == nil {
		return ""
	}
	if region, okRegion := raw["region"].(string); okRegion && strings.TrimSpace(region) != "" {
		return NormalizeRegion(region)
	}
	return ""
}

// ---- Active Device Identity ----
//
// The upstream performs strict validation on cosy-machinetoken + cosy-machinetype, accepting only raw pairing
// issued by official components. Only the real identity provided by the user is read here (credential field -> global config), never artificially generated.

// MachineIdentity is the device identity to be carried in a check-in request.
type MachineIdentity struct {
	// Token / Type are paired machine identities issued by official components and must be used as-is.
	Token string
	Type  string
	// Code is cosy-machinecode, optional (actual tests show it does not affect campaign visibility).
	Code string
	// Version is cosy-version (desktop client version).
	Version string
}

// Native reports whether the identity is available (token and type must be paired, both are required).
func (m MachineIdentity) Native() bool { return m.Token != "" && m.Type != "" }
