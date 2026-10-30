package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// This file implements optional account operations for Qoder: quota query and check-in.

// This file implements Qoder quota query.
//
// Unlike WorkBuddy credit balance, Qoder quota is a "plan usage bucket": the upstream returns
// two objects, userQuota / addOnQuota, each with used / total / remaining.
// Normalized into the host's QuotaFetchResponse (Summary holds key numbers, Groups holds details).

// quotaHTTPTimeout is the limit for quota-related requests.
const quotaHTTPTimeout = 30 * time.Second

// QuotaBucket represents a quota bucket.
type QuotaBucket struct {
	Used      float64
	Total     float64
	Remaining float64
	ResetTime string
}

// Quota represents a single quota query result.
type Quota struct {
	Plan            string
	UserQuota       *QuotaBucket
	AddonQuota      *QuotaBucket
	IsQuotaExceeded bool
	ExpiresAt       int64
}

// FetchQuota queries the account quota.
func FetchQuota(ctx context.Context, cred *Credential) (*Quota, error) {
	bearer, errToken := quotaBearerToken(ctx, cred)
	if errToken != nil {
		return nil, errToken
	}
	endpoints := GetEndpoints(cred.Region)

	raw, errQuota := httpGetBearerJSON(ctx, endpoints.QuotaEndpoint, bearer)
	if errQuota != nil {
		return nil, errQuota
	}
	return &Quota{
		Plan:            fetchPlanTierName(ctx, endpoints, bearer),
		IsQuotaExceeded: raw["isQuotaExceeded"] == true,
		ExpiresAt:       int64(toFloat(raw, "expiresAt")),
		UserQuota:       extractQuotaBucket(raw, "userQuota"),
		AddonQuota:      extractQuotaBucket(raw, "addOnQuota"),
	}, nil
}

// quotaBearerToken exchanges a credential for a bearer token recognized by the quota API.
//
// Device tokens (dt-...) can be used directly; PATs (pt-...) must first be exchanged via jobToken
// to get securityOauthToken—the upstream quota API does not recognize PATs directly.
func quotaBearerToken(ctx context.Context, cred *Credential) (string, error) {
	deviceToken, _ := ParseOAuthSecret(cred.Token)
	if strings.HasPrefix(deviceToken, "dt-") {
		return deviceToken, nil
	}
	seed := FingerprintSeed("", cred.Token)
	endpoints := GetEndpoints(cred.Region)
	jobToken, errExchange := ExchangeJobToken(ctx, cred.Token,
		DeriveMachineID(seed), DeriveMachineToken(seed), DeriveMachineType(seed),
		endpoints.JobTokenURL)
	if errExchange != nil {
		return "", classifyCredentialError(fmt.Errorf("exchange token: %w", errExchange))
	}
	oauthToken := StrVal(jobToken, "securityOauthToken")
	if oauthToken == "" {
		return "", NewError("qoder_credential_invalid", "jobToken response missing securityOauthToken", http.StatusUnauthorized)
	}
	return oauthToken, nil
}

// httpGetBearerJSON sends a GET request with Bearer and parses it into a JSON object.
func httpGetBearerJSON(ctx context.Context, endpoint, token string) (map[string]any, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, NewError("invalid_request", errRequest.Error(), http.StatusBadRequest)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, errDo := hostbridge.Client(ctx, quotaHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, NewError("qoder_upstream_transient", errDo.Error(), http.StatusBadGateway)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, NewError("qoder_upstream_error", errRead.Error(), http.StatusBadGateway)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			status = http.StatusUnauthorized
		}
		return nil, NewError("qoder_upstream_error",
			fmt.Sprintf("Quota API returned HTTP %d: %s", resp.StatusCode, core.Truncate(string(body), 300)), status)
	}
	var result map[string]any
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		return nil, NewError("qoder_upstream_error", "Quota API did not return JSON: "+errUnmarshal.Error(), http.StatusBadGateway)
	}
	return result, nil
}

// fetchPlanTierName queries the plan name; failure does not affect quota display, leaving it empty.
func fetchPlanTierName(ctx context.Context, endpoints Endpoints, token string) string {
	result, errPlan := httpGetBearerJSON(ctx, endpoints.PlanEndpoint, token)
	if errPlan != nil {
		return ""
	}
	if name, okName := result["plan_tier_name"].(string); okName {
		return name
	}
	return ""
}

// extractQuotaBucket extracts the quota bucket.
//
// The criterion for a "bucket does not exist" is an **empty object** (upstream returns `{}` for missing buckets), not "all values are zero":
// The userQuota for a Free plan is `{used:0, total:0, remaining:0}`, and swallowing it as all-zero would cause
// the page to show "upstream did not return quota", while the real semantics are "quota info exists, but the balance is 0".
func extractQuotaBucket(data map[string]any, key string) *QuotaBucket {
	obj, okObj := data[key].(map[string]any)
	if !okObj || len(obj) == 0 {
		return nil
	}
	bucket := &QuotaBucket{
		Used:      toFloat(obj, "used"),
		Total:     toFloat(obj, "total"),
		Remaining: toFloat(obj, "remaining"),
	}
	if reset, okReset := obj["resetTime"].(string); okReset {
		bucket.ResetTime = reset
	} else if reset, okReset := obj["reset_time"].(string); okReset {
		bucket.ResetTime = reset
	}
	return bucket
}

// BuildQuotaResponse translates upstream quota into the host's normalized structure.
func BuildQuotaResponse(quota *Quota) pluginapi.QuotaFetchResponse {
	response := pluginapi.QuotaFetchResponse{}
	if quota.Plan != "" {
		response.Subscription = &pluginapi.QuotaSubscription{
			Plan: quota.Plan, TierName: quota.Plan, TierID: quota.Plan,
		}
	}
	if group := quotaGroup("Plan Quota", quota.UserQuota); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	if group := quotaGroup("Personal Add-on", quota.AddonQuota); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	if quota.UserQuota != nil {
		// total must be output together: the page's "remaining/total + usage rate" needs it to calculate the ratio.
		// The upstream may give total=0 for Free plans (quota measured by remaining amount), in which case 0 is still output—
		// the page will show "—", which is more honest than making up a number.
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "user_quota_remaining", Label: "Plan Remaining Quota", Value: quota.UserQuota.Remaining, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "user_quota_total", Label: "Plan Total Quota", Value: quota.UserQuota.Total, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "user_quota_used", Label: "Plan Used Quota", Value: quota.UserQuota.Used, Unit: "credits"})
	}
	if quota.AddonQuota != nil {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "addon_quota_remaining", Label: "Add-on Remaining Quota", Value: quota.AddonQuota.Remaining, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "addon_quota_total", Label: "Add-on Total Quota", Value: quota.AddonQuota.Total, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "addon_quota_used", Label: "Add-on Used Quota", Value: quota.AddonQuota.Used, Unit: "credits"})
	}
	if quota.IsQuotaExceeded {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "quota_exceeded", Label: "Quota Status", Value: 1, Unit: "Exhausted"})
	}
	if quota.ExpiresAt > 0 {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "expires_at", Label: "Quota Expiration Time", Value: float64(quota.ExpiresAt), Format: "number", Unit: "unix"})
	}
	return response
}

// quotaGroup converts a quota bucket into a "remaining ratio" group.
func quotaGroup(displayName string, bucket *QuotaBucket) *pluginapi.QuotaGroup {
	if bucket == nil {
		return nil
	}
	fraction := 0.0
	switch {
	case bucket.Total > 0:
		fraction = core.ClampFraction(bucket.Remaining / bucket.Total)
	case bucket.Remaining > 0:
		fraction = 1
	}
	description := fmt.Sprintf("Used %.0f / Total %.0f, Remaining %.0f", bucket.Used, bucket.Total, bucket.Remaining)
	return &pluginapi.QuotaGroup{
		DisplayName: displayName,
		Buckets: []pluginapi.QuotaBucket{{
			Window:            "billing-cycle",
			RemainingFraction: fraction,
			ResetTime:         bucket.ResetTime,
			Description:       description,
		}},
	}
}

// toFloat reads a numeric field in JSON as float64 (compatible with multiple number formats).
func toFloat(m map[string]any, key string) float64 {
	switch value := m[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		parsed, _ := value.Float64()
		return parsed
	}
	return 0
}

// Check-in status values.
const ()

// checkinHeaders are the request headers required for desktop check-in (confirmed via packet capture).
//
// cosy-clienttype: 10 is the desktop identifier; upstream allows check-in APIs based on it,
// without requiring a cosy signature (this is the biggest difference between check-in and chat APIs).
//
// Machine headers **are only sent when the user provides a real identity issued by official components** (see identity.go):
// Upstream performs strict validation on machinetoken/machinetype; any derived, random, or concatenated value will
// cause available events to be entirely filtered out—on the CN version, they are visible, but sending a fake header makes them invisible. When no real identity
// is available, none are sent, which is the only choice that avoids self-sabotage.
func checkinHeaders(bearer string, identity MachineIdentity) map[string]string {
	headers := map[string]string{
		"authorization":   "Bearer " + bearer,
		"accept":          "application/json",
		"accept-language": "zh-CN",
		"user-agent":      "Qoder",
		"cosy-clienttype": "10",
		"cosy-version":    identity.Version,
	}
	if !identity.Native() {
		return headers
	}
	headers["cosy-machinetoken"] = identity.Token
	headers["cosy-machinetype"] = identity.Type
	if identity.Code != "" {
		headers["cosy-machinecode"] = identity.Code
	}
	return headers
}
