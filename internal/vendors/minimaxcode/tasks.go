package minimaxcode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// FetchQuota queries the MiniMax account quota balance.
func FetchQuota(ctx context.Context, cred *Credential, baseURLOverride string) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", http.StatusUnauthorized)
	}

	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildSigninTarget(cred, base, EpMembership, http.MethodPost, "{}")

	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return nil, core.ToPluginError(errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, Classify(resp.StatusCode, string(body))
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return nil, fmt.Errorf("decode quota response: %w", errDec)
	}

	coreMap := raw
	if data, ok := raw["data"].(map[string]any); ok {
		coreMap = data
	}

	// Token Plan (subscription) uses an independent window system: 5-hour window + 7-day window,
	// provided by coding_plan/remains (pure Bearer, no account signature required).
	head := extractTokenPlanHead(coreMap)
	var groups []pluginapi.QuotaGroup
	if head != nil {
		groups = head.groups(ctx, cred)
	}
	return quotaResponse(coreMap, head, groups), nil
}

// quotaResponse folds membership data into a quota response.
//
// When Token Plan is present, subscription name and tier take precedence. Op point metrics with zero
// remaining give way to token metrics. Window groups are passed by the caller—failure only results in a few fewer progress bars,
// while subscription and remaining balance remain valid. TierID is solely determined by "whether it's a Token Plan" and does not drift with window detection success or failure.
func quotaResponse(coreMap map[string]any, head *tokenPlanHead, groups []pluginapi.QuotaGroup) *pluginapi.QuotaFetchResponse {
	planName := core.GetString(coreMap, "plan_name")
	if planName == "" {
		planName = "MiniMax Code"
	}
	tierID := "minimax"
	summary := opSummaryMetrics(coreMap)
	if head != nil {
		planName = head.planName
		tierID = "minimax-token-plan"
		// When Token Plan is present, zero op point metrics are omitted from the summary (preventing "Total remaining quota 0"
		// from misleading users ahead of the token balance). Negative values are preserved: upstream uses -1 to mean "unlimited".
		kept := summary[:0]
		for _, metric := range summary {
			if metric.Value != 0 {
				kept = append(kept, metric)
			}
		}
		summary = append(kept, head.metrics...)
	}
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: planName, TierName: planName, TierID: tierID},
		Summary:      summary,
		Groups:       groups,
	}
}

// opSummaryMetrics extracts the op point summary (billing system outside of Token Plan).
func opSummaryMetrics(coreMap map[string]any) []pluginapi.QuotaMetric {
	var totalRemaining, freeRemaining, purchasedRemaining float64
	if summary, ok := coreMap["op_credit_summary"].(map[string]any); ok {
		totalRemaining = parseFloat(summary["total_remaining_amount"])
		freeRemaining = parseFloat(summary["free_remaining_amount"])
		purchasedRemaining = parseFloat(summary["purchased_remaining_amount"])
	} else {
		totalRemaining = parseFloat(coreMap["opcredit_balance"])
		if totalRemaining == 0 {
			totalRemaining = parseFloat(coreMap["total_remains_credit"])
		}
	}

	metrics := []pluginapi.QuotaMetric{{
		Key:   "remaining",
		Label: "Total Remaining Quota",
		Value: totalRemaining,
		Unit:  "points",
	}}
	if freeRemaining > 0 {
		metrics = append(metrics, pluginapi.QuotaMetric{
			Key: "free", Label: "Free Quota", Value: freeRemaining, Unit: "points",
		})
	}
	if purchasedRemaining > 0 {
		metrics = append(metrics, pluginapi.QuotaMetric{
			Key: "purchased", Label: "Purchased Quota", Value: purchasedRemaining, Unit: "points",
		})
	}
	return metrics
}

// tokenPlanHead is the Token Plan summary in the membership response.
type tokenPlanHead struct {
	planName    string
	groupID     string
	expiresAtMS int64
	metrics     []pluginapi.QuotaMetric
}

// extractTokenPlanHead extracts the Token Plan summary from the membership response.
//
// Returns nil if unsubscribed (has_token_plan=false) or if remaining is 0 with no expiration time,
// causing the caller to fall back to op point display.
func extractTokenPlanHead(coreMap map[string]any) *tokenPlanHead {
	if !parseBool(coreMap["has_token_plan"]) {
		return nil
	}
	tier := strings.TrimSpace(core.GetString(coreMap, "token_plan_tier"))
	remains := parseFloat(coreMap["tp_remains"])
	expiresAtMS := core.GetInt64(coreMap, "token_plan_expires_at")
	if remains == 0 && expiresAtMS == 0 {
		return nil
	}
	info := &tokenPlanHead{
		planName:    "MiniMax Token Plan",
		groupID:     core.GetString(coreMap, "op_group_id"),
		expiresAtMS: expiresAtMS,
	}
	if tier != "" {
		// Tier names may start with non-ASCII characters (like "Advanced"): take the first letter by rune,
		// do not slice by bytes, otherwise invalid UTF-8 will be produced.
		runes := []rune(tier)
		info.planName += " " + strings.ToUpper(string(runes[0])) + string(runes[1:])
	}
	if remains > 0 {
		// Use an independent token_plan family (token_remaining) for the key: when coexisting with the op point
		// default family remaining, they will not overwrite each other (Totals fetches by family).
		info.metrics = append(info.metrics, pluginapi.QuotaMetric{
			Key:   "token_remaining",
			Label: "Token Plan Remaining",
			Value: remains,
			Unit:  "tokens",
		})
	}
	return info
}

// groups fetches the M Plan 5-hour / 7-day windows, along with the plan expiration time.
//
// Failure of coding_plan/remains does not break the entire quota response: missing windows just mean two fewer progress bars on the card,
// and the remaining balance metrics remain valid.
func (t *tokenPlanHead) groups(ctx context.Context, cred *Credential) []pluginapi.QuotaGroup {
	groups := fetchTokenPlanRemains(ctx, cred, t.groupID)
	if t.expiresAtMS > 0 {
		groups = append(groups, *core.QuotaExpiryGroup(time.UnixMilli(t.expiresAtMS)))
	}
	return groups
}

// fetchTokenPlanRemains queries coding_plan/remains and folds it into window groups.
//
// The endpoint is pure Bearer (platform domain, not signed account domain), returning 5-hour and
// 7-day windows per model line; status=2 means "exhausted and percentage no longer published", displayed as 0%;
// status=3 windows are not displayed—consistent with the official client's presentation.
func fetchTokenPlanRemains(ctx context.Context, cred *Credential, groupID string) []pluginapi.QuotaGroup {
	platform := PlatformHostFor(cred.Region)
	// Timeout tightened to 10s: this probe is attached to the quota query path, and when the account list
	// fetches quotas per account, a 30s timeout would be magnified into a long tail for the entire refresh cycle.
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, platform+EpCodingPlanRemains, nil)
	if errNew != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	if groupID != "" {
		req.Header.Set("X-Group-Id", groupID)
	}
	resp, errDo := hostbridge.Client(ctx, 10*time.Second).Do(req)
	if errDo != nil {
		logger.Debug("minimax token plan windows: request failed: %v", errDo)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Debug("minimax token plan windows: endpoint returned %s", resp.Status)
		return nil
	}
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		logger.Debug("minimax token plan windows: read failed: %v", errRead)
		return nil
	}
	return parseTokenPlanRemains(raw)
}

// parseTokenPlanRemains folds the coding_plan/remains response into 5-hour / 7-day window groups.
func parseTokenPlanRemains(raw []byte) []pluginapi.QuotaGroup {
	var body map[string]any
	if errDecode := json.Unmarshal(raw, &body); errDecode != nil {
		logger.Debug("minimax token plan windows: decode failed: %v", errDecode)
		return nil
	}
	rows, _ := body["model_remains"].([]any)
	return tokenPlanGroups(rows)
}

// tokenPlanGroups assembles window groups by model line.
func tokenPlanGroups(rows []any) []pluginapi.QuotaGroup {
	interval := pluginapi.QuotaGroup{DisplayName: "5-Hour Window"}
	weekly := pluginapi.QuotaGroup{DisplayName: "7-Day Window"}
	for _, rawRow := range rows {
		row, okRow := rawRow.(map[string]any)
		if !okRow {
			continue
		}
		name := strings.TrimSpace(core.GetString(row, "model_name"))
		if name == "" {
			continue
		}
		display := name
		if strings.EqualFold(name, "general") {
			display = "General Quota"
		}
		if bucket, ok := planWindowBucket(display, "5-Hour",
			row["current_interval_remaining_percent"], row["current_interval_status"],
			row["end_time"],
			row["current_interval_usage_count"], row["current_interval_total_count"]); ok {
			interval.Buckets = append(interval.Buckets, bucket)
		}
		if bucket, ok := planWindowBucket(display, "7-Day",
			row["current_weekly_remaining_percent"], row["current_weekly_status"],
			row["weekly_end_time"],
			row["current_weekly_usage_count"], row["current_weekly_total_count"]); ok {
			weekly.Buckets = append(weekly.Buckets, bucket)
		}
	}
	groups := make([]pluginapi.QuotaGroup, 0, 2)
	if len(interval.Buckets) > 0 {
		groups = append(groups, interval)
	}
	if len(weekly.Buckets) > 0 {
		groups = append(groups, weekly)
	}
	return groups
}

// planWindowBucket folds a window row into a display bucket.
//
// Return false to indicate that the window is not worth displaying: status=3 (not displayed upstream), or missing fields/
// Renaming caused the percentage to be missing without being explicitly marked as exhausted—the latter rendering as 0% creates a fake
// "exhausted" progress bar, which is more misleading than not displaying it.
func planWindowBucket(display, window string, remaining, status, end, usage, total any) (pluginapi.QuotaBucket, bool) {
	statusCode := parseFloat(status)
	if statusCode == 3 {
		return pluginapi.QuotaBucket{}, false
	}
	// status=2 means "exhausted and no longer publishing percentage": display as 0% (the account truly has no quota).
	// In all other cases, missing percentages (nil/empty string) are not displayed.
	var percent float64
	hasPercent := false
	switch typed := remaining.(type) {
	case float64:
		percent, hasPercent = typed, true
	case string:
		if strings.TrimSpace(typed) != "" {
			percent, hasPercent = parseFloat(typed), true
		}
	}
	if !hasPercent {
		if statusCode == 2 {
			percent, hasPercent = 0, true
		} else {
			return pluginapi.QuotaBucket{}, false
		}
	}
	// The field name is *_remaining_percent, measured scale is 0..100: fixed /100.
	// Do not use heuristics like "<=1 as decimal"—1% remaining would be misread as 100%.
	fraction := 0.0
	if statusCode != 2 {
		fraction = core.ClampFraction(percent / 100)
	}
	bucket := pluginapi.QuotaBucket{
		Window:            display + " · " + window,
		RemainingFraction: fraction,
	}
	if endMS := int64(parseFloat(end)); endMS > 0 {
		bucket.ResetTime = time.UnixMilli(endMS).UTC().Format(time.RFC3339)
	}
	if used, total := parseFloat(usage), parseFloat(total); used > 0 || total > 0 {
		bucket.Description = fmt.Sprintf("Window usage count %s / %s", strconv.FormatFloat(used, 'f', 0, 64), strconv.FormatFloat(total, 'f', 0, 64))
	}
	return bucket, true
}

// parseBool loosely parses boolean fields.
func parseBool(val any) bool {
	switch v := val.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

func parseFloat(val any) float64 {
	if val == nil {
		return 0
	}
	switch v := val.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return 0
}
