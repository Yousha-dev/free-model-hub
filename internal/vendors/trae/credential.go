package trae

import (
	"encoding/json"
	"fmt"
	"strings"

	"free-model-hub/internal/core"
)

// This file implements the parsing, feature recognition, and storage of Trae family credentials.

type Credential struct {
	Vendor       string `json:"vendor,omitempty"`
	Region       Region `json:"region,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Label        string `json:"label,omitempty"`
}

// ParseCredential parses JSON credentials (reserved for protocol layer testing).
func ParseCredential(raw []byte, defaultRegion Region) (*Credential, error) {
	return parseCredential(raw, VendorIDFor(defaultRegion), defaultRegion)
}

// ParseCredentialFor parses credentials according to the instance profile.
func ParseCredentialFor(raw []byte, cfg Config) (*Credential, error) {
	return parseCredential(raw, cfg.VendorID, cfg.Region)
}

func parseCredential(raw []byte, defaultVendor string, defaultRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("trae credential is empty")
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode trae credential json: %w", err)
	}

	authObj := core.GetMap(m, "auth")
	if authObj == nil {
		authObj = m
	}
	accountObj := core.GetMap(m, "account")

	vendor := strings.ToLower(core.FirstNonEmpty(core.GetString(m, "vendor"), defaultVendor))
	r := defaultRegion
	regionStr := strings.ToLower(core.FirstNonEmpty(
		core.GetString(m, "region"),
		core.GetString(m, "realm"),
		core.GetString(m, "_edition"),
		string(defaultRegion),
	))
	switch {
	case regionStr == "global" || regionStr == "sg" || regionStr == "traeglobal":
		r = RegionGlobal
	default:
		r = RegionCN
	}
	c := &Credential{
		Vendor:       core.FirstNonEmpty(vendor, defaultVendor),
		Region:       r,
		AccessToken:  core.FirstNonEmpty(core.GetString(authObj, "access_token"), core.GetString(authObj, "accessToken"), core.GetString(authObj, "token")),
		RefreshToken: core.FirstNonEmpty(core.GetString(authObj, "refresh_token"), core.GetString(authObj, "refreshToken")),
		ExpiresAt:    core.GetUnixSeconds(authObj, "expires_at", "expiresAt", "TokenExpireAt"),
		UID:          core.FirstNonEmpty(core.GetString(authObj, "uid"), core.GetString(accountObj, "uid"), core.GetString(m, "uid")),
		Nickname:     core.FirstNonEmpty(core.GetString(authObj, "nickname"), core.GetString(accountObj, "nickname"), core.GetString(m, "nickname")),
		EnterpriseID: core.FirstNonEmpty(core.GetString(authObj, "enterprise_id"), core.GetString(accountObj, "enterprise_id"), core.GetString(m, "enterprise_id")),
		MachineID:    core.FirstNonEmpty(core.GetString(authObj, "machine_id"), core.GetString(authObj, "machineId"), core.GetString(m, "machine_id")),
		DeviceID:     core.FirstNonEmpty(core.GetString(authObj, "device_id"), core.GetString(authObj, "deviceId"), core.GetString(m, "device_id")),
		Label:        core.GetString(m, "label"),
	}

	if c.AccessToken == "" && c.RefreshToken == "" {
		return nil, fmt.Errorf("trae credential missing access_token and refresh_token")
	}
	return c, nil
}

// VendorIDFor returns the VendorID corresponding to a region.
func VendorIDFor(r Region) string {
	if r == RegionGlobal {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// LooksLikeCredential determines whether a file or data belongs to this Trae regional instance.
func LooksLikeCredential(raw map[string]any, fileName, provider string, targetRegion Region) bool {
	return LooksLikeCredentialFor(raw, fileName, provider, ConfigFor(targetRegion))
}

// LooksLikeCredentialFor determines whether a file or data belongs to a specific Trae profile.
func LooksLikeCredentialFor(raw map[string]any, fileName, provider string, cfg Config) bool {
	if decided, claimed := core.VendorFieldVerdict(raw, cfg.VendorID, "trae"); decided {
		return claimed
	}
	if core.FileNameMatchesConvention(fileName, cfg.VendorID) {
		return true
	}
	// Old family prefix trae-*.json: determined by the region inside the file.
	if core.FileNameMatchesConvention(fileName, "trae") {
		rStr := strings.ToLower(core.FirstNonEmpty(core.GetString(raw, "region"), core.GetString(raw, "realm"), core.GetString(raw, "_edition")))
		if cfg.Region == RegionGlobal {
			return rStr == "global" || rStr == "sg"
		}
		return rStr == "cn" || rStr == ""
	}
	return false
}

func (c *Credential) StorageJSON() ([]byte, error) {
	vendor := c.Vendor
	if vendor == "" {
		vendor = VendorIDFor(c.Region)
	}
	m := map[string]any{
		"type":          "freehub",
		"vendor":        vendor,
		"region":        string(c.Region),
		"access_token":  c.AccessToken,
		"refresh_token": c.RefreshToken,
		"expires_at":    c.ExpiresAt,
		"uid":           c.UID,
		"nickname":      c.Nickname,
		"enterprise_id": c.EnterpriseID,
		"machine_id":    c.MachineID,
		"device_id":     c.DeviceID,
	}
	if c.Label != "" {
		m["label"] = c.Label
	}
	return json.MarshalIndent(m, "", "  ")
}

// Check-in and quota: supported by the CN version; the upstream Global version has no corresponding endpoint.
