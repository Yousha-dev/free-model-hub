package trae

// This file implements daily check-in and quota query for the Trae family.
//
// Check-in (CN version only; Global version does not have this event):
//   - First POST checkin_credits/status to check today's status (enable / checked_in / credits);
//   - If not checked in, POST checkin_credits/claim to claim. claim also returns
//     code=0 for "already checked in today", so "checked in" can only be determined by status, making the status pre-check a necessary flow.
//   - Business code 9074 is device-level rate limiting (not already checked in), propagated as a rate-limit error.
//
// Quota (CN version only): POST pay/ide_user_ent_usage to fetch the benefit package list,
// only counting credit packages with product_type=2 (credits_limit=-1 means unlimited).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

const (
	// checkinDeviceBrand/Type matches the default check-in device of Trae2api-cn.
	checkinDeviceBrand = "ASUS TUF Gaming A15 FA507RM_FA507RM"
	checkinDeviceType  = "windows"
)

// checkinHeaders sets up the request headers shared by check-in and quota.
func checkinHeaders(cred *Credential) map[string]string {
	return map[string]string{
		"Authorization":  "Cloud-IDE-JWT " + cred.AccessToken,
		"Content-Type":   "application/json",
		"X-Device-Id":    CheckinDeviceID(cred),
		"X-Device-Brand": checkinDeviceBrand,
		"X-Device-Type":  checkinDeviceType,
	}
}

// CheckinDeviceID derives the 16-digit device ID used for check-in: sha256(account identity) mod 10^16.
//
// Identity prioritizes JWT payload.data.id (consistent with Trae2api-cn, stable after token refresh),
// falling back to credential uid / device_id.
func CheckinDeviceID(cred *Credential) string {
	identity := jwtIdentity(cred.AccessToken)
	if identity == "" {
		identity = cred.UID
	}
	if identity == "" {
		identity = cred.DeviceID
	}
	if identity == "" {
		identity = "default_trae_user"
	}
	sum := sha256.Sum256([]byte(identity))
	n := new(big.Int).SetBytes(sum[:])
	n.Mod(n, big.NewInt(10000000000000000))
	return fmt.Sprintf("%016d", n)
}

// jwtIdentity extracts the account id (payload.data.id) from Cloud-IDE-JWT.
func jwtIdentity(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return ""
	}
	var parsed struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errJSON := json.Unmarshal(payload, &parsed); errJSON != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Data.ID)
}

// ---- Quota ----

// FetchQuota queries account credits: remaining / total / used.
func FetchQuota(ctx context.Context, cred *Credential, cfg Config) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential missing")
	}
	if cfg.QuotaHost == "" {
		// Global version upstream has no quota endpoint (404 observed).
		return nil, core.NewPluginError("quota_unsupported", "Trae Global version does not support quota query", http.StatusNotImplemented)
	}

	payload, errBody := json.Marshal(map[string]any{"require_usage": true, "req_source": 1})
	if errBody != nil {
		return nil, errBody
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cfg.QuotaHost+EpEntUsage, bytes.NewReader(payload))
	if errReq != nil {
		return nil, errReq
	}
	for key, value := range checkinHeaders(cred) {
		req.Header.Set(key, value)
	}

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("trae quota request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	var result struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				ProductType int `json:"product_type"`
				Quota       struct {
					CreditsLimit any `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount any `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if errJSON := json.Unmarshal(body, &result); errJSON != nil {
		return nil, fmt.Errorf("decode quota response: %w", errJSON)
	}

	var total, used float64
	unlimited := false
	for _, pack := range result.UserEntitlementPackList {
		// product_type=0 is a feature toggle package, with no credit quota.
		if pack.EntitlementBaseInfo.ProductType != 2 {
			continue
		}
		limit, okLimit := flexFloat(pack.EntitlementBaseInfo.Quota.CreditsLimit)
		if !okLimit {
			continue
		}
		if limit < 0 {
			unlimited = true
			continue
		}
		total += limit
		if amount, okAmount := flexFloat(pack.Usage.CreditsAmount); okAmount && amount > 0 {
			used += amount
		}
	}

	// Key names must fall within auth.Totals vocabulary (credit_remain / credit_total /
	// credit_used), otherwise the console's "Remaining / Total" will silently display 0 / 0.
	// Provider packages cannot import auth (layering convention), so keys with the same name are manually written here by convention.
	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "Trae CN Benefits",
			TierName: "Standard",
			TierID:   cfg.VendorID,
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "Remaining Credits", Value: total - used, Unit: "credits"},
			{Key: "credit_total", Label: "Total Quota", Value: total, Unit: "credits"},
			{Key: "credit_used", Label: "Used Credits", Value: used, Unit: "credits"},
		},
	}
	if unlimited {
		response.Summary[0] = pluginapi.QuotaMetric{Key: "credit_remain", Label: "Remaining Credits", Value: -1, Unit: "unlimited"}
	}
	return response, nil
}

// flexFloat leniently parses upstream numeric fields: accepts JSON numbers and numeric strings,
// returns false if key is missing / empty string / null (callers treat as 0 or skip).
func flexFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, errParse := typed.Float64()
		return parsed, errParse == nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, errParse := strconv.ParseFloat(trimmed, 64)
		return parsed, errParse == nil
	default:
		return 0, false
	}
}
