package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// billingHeaders constructs the billing family request headers.
//
// Platform, language, timezone, and device ID are all taken from the account profile: upstream performs risk control clustered by device,
// and global spoofed values would cluster all accounts into the same machine.
func billingHeaders(cred *Credential) map[string]string {
	headers := upstreamAuthHeaders(cred)
	headers["Content-Type"] = "application/json"
	return headers
}

// billingRequest initiates a billing family request and parses the business code.
//
// Returns the business code and response body (business code is -1 if parsing fails). Network and auth failures uniformly return
// classified upstream errors, so callers do not need to judge status codes individually.
func billingRequest(ctx context.Context, cred *Credential, method, path string, payload any) (int, map[string]any, error) {
	return billingRequestWithHeaders(ctx, cred, method, path, payload, nil)
}

// billingRequestWithHeaders is a variant of billingRequest with additional headers.
//
// Claiming a plan requires adding captcha challenge headers (CaptchaHeader/CaptchaRegionHeader) in addition to identity headers,
// while quota queries do not—using additional header parameters distinguishes the two, avoiding each caller from manually assembling headers.
func billingRequestWithHeaders(ctx context.Context, cred *Credential, method, path string, payload any, extra map[string]string) (int, map[string]any, error) {
	var bodyReader io.Reader
	if payload != nil {
		encoded, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return 0, nil, errMarshal
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, errNew := http.NewRequestWithContext(ctx, method, BillingBase+path, bodyReader)
	if errNew != nil {
		return 0, nil, errNew
	}
	for key, value := range billingHeaders(cred) {
		req.Header.Set(key, value)
	}
	for key, value := range extra {
		req.Header.Set(key, value)
	}

	resp, errDo := hostbridge.Client(ctx, billingHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, nil, transientErr("billing request failed: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, billingMaxBody))
	if errRead != nil {
		return 0, nil, transientErr("failed to read billing response: " + errRead.Error())
	}

	// Every non-2xx goes to Classify: it already handles "on 403 check the captcha challenge first, then an invalid credential"
	// (see errors.go), so that logic is not repeated here.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, nil, Classify(resp.StatusCode, string(raw))
	}

	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return -1, nil, nil
	}
	return businessCode(parsed), parsed, nil
}

// businessCode gets the upstream business code; returns -1 if missing or non-numeric (treated as failure).
func businessCode(body map[string]any) int {
	if body == nil {
		return -1
	}
	switch v := body["code"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if out, errParse := strconv.Atoi(strings.TrimSpace(v)); errParse == nil {
			return out
		}
	}
	return -1
}

// dataMap retrieves the data object from the response body.
func dataMap(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	data, _ := body["data"].(map[string]any)
	return data
}

// billingBlockReason reports why this credential cannot make billing family requests; returns an empty string when eligible.
//
// Billing family only serves Coding Plan (JWT). Pure API Key accounts have no plans and should not generate
// billing traffic—this neither retrieves data nor pointlessly increases an upstream risk control signal.
func billingBlockReason(cred *Credential) string {
	if cred == nil {
		return "missing credential"
	}
	if !cred.UsesPlanChannel() {
		return "this account has no Coding Plan credential (JWT), and does not participate in plan and quota queries"
	}
	return ""
}

// classifyBillingOutcome translates billing family business failure codes into readable text.
func classifyBillingOutcome(code int, body map[string]any) string {
	serverMsg := core.FirstNonEmpty(core.RawStringField(body, "msg"), core.RawStringField(body, "message"))
	base := map[int]string{
		codePlanNotFound:    "plan does not exist",
		codePlanClosed:      "event has ended or plan is temporarily unavailable",
		codePlanClaimed:     "this plan has already been claimed",
		codePlanNotEligible: "not eligible to claim",
		codeDailyQuotaUsed:  "daily claim quota has been exhausted",
		3001:                "incomplete request parameters (missing device identifier)",
		3007:                "captcha verification failed",
		401:                 "please log in before claiming",
	}[code]
	if base == "" {
		base = "upstream returned a business error"
	}
	if serverMsg != "" {
		return base + "(" + serverMsg + ")"
	}
	return base
}

// fetchQuota queries the plan and remaining quota for a ZCode account.
func (v *adapter) fetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, transientErr("zcode credential is nil")
	}

	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "ZCode Coding Plan",
			TierName: "Standard",
			TierID:   "zcode",
		},
	}

	if reason := billingBlockReason(cred); reason != "" {
		// Non-Coding Plan accounts: state truthfully, do not disguise as "query successful but no data".
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key: "status", Label: "Account Status", Value: 1, Unit: reason,
		})
		// Include the reason: the page uses this to display explanations on the button rather than showing "no available plans to claim".
		response.Claim = &pluginapi.ClaimStatus{Error: reason}
		return response, nil
	}

	// One snapshot feeds three things: subscription name, per-model remaining quota and plan claim status.
	// The extra preview request means the "button disabled" state needs no separate polling endpoint.
	snapshot := v.fetchPlanSnapshot(ctx, cred)
	applySubscription(snapshot, response)
	response.Claim = claimStatusOf(snapshot)
	response.Summary = append(response.Summary, snapshot.metrics...)
	response.Groups = append(response.Groups, snapshot.groups...)
	if snapshot.errBalance != nil {
		return nil, snapshot.errBalance
	}
	return response, nil
}

// applySubscription uses held plans to supplement the subscription name (retains default value on failure).
//
// Data comes from billing/balance already obtained in the plan snapshot, no extra requests are sent.
func applySubscription(snapshot planSnapshot, response *pluginapi.QuotaFetchResponse) {
	plan := subscriptionPlan(snapshot.plans, snapshot.balances, snapshot.serverTime)
	if plan == nil {
		return
	}
	name := core.RawStringField(plan, "name")
	if name == "" {
		return
	}
	response.Subscription.Plan = name
	response.Subscription.TierName = core.FirstNonEmpty(
		normalizePlanID(core.FirstNonEmpty(
			core.RawStringField(plan, "plan_id"),
			core.RawStringField(plan, "planId"),
		)),
		"zcode",
	)
}

// subscriptionPlan selects the active plan most closely associated with this account's quota bucket.
//
// Correlation evidence is limited to expiration time: actual testing shows billing/balance balances lack plan_id,
// and when the bucket's expires_at matches the plan's ends_at, they represent the same entitlement (used to distinguish
// between the 1024 gift package and Basic when they coexist, rather than relying on array order—the first active in the array isn't necessarily the one in use).
// If there are no quota buckets or expiration times don't match, fall back to the earliest active plan, consistent with existing display, without inventing names.
func subscriptionPlan(plans, balances []map[string]any, serverTime int64) map[string]any {
	active := activePlans(plans, serverTime)
	if len(active) == 0 {
		return nil
	}
	if len(balances) == 0 {
		return active[0]
	}
	bucketExpiries := make(map[int64]int, len(balances))
	for _, bucket := range balances {
		if expires := core.GetUnixSeconds(bucket, "expires_at"); expires > 0 {
			bucketExpiries[expires]++
		}
	}
	best, bestHits := active[0], 0
	for _, plan := range active {
		if hits := bucketExpiries[core.GetUnixSeconds(plan, "ends_at")]; hits > bestHits {
			best, bestHits = plan, hits
		}
	}
	return best
}

// balanceQuota folds the billing/balance quota buckets into (metrics, window groups) in a single pass.
//
// A single pass produces both simultaneously: per-bucket metrics go into the summary string, and window groups drive the host card's progress bar;
// parsing the same response in two separate passes previously caused metric drift (P1 review comment).
// Deduplicate buckets with the same name into the summary in order of appearance (if the upstream has multiple for the same model, only count the first to avoid double counting).
func balanceQuota(balances []map[string]any) ([]pluginapi.QuotaMetric, []pluginapi.QuotaGroup) {
	if len(balances) == 0 {
		return nil, nil
	}

	var totalRemaining, totalTotal, totalUsed float64
	seen := map[string]bool{}
	parsed := 0
	metrics := make([]pluginapi.QuotaMetric, 0, len(balances))
	group := pluginapi.QuotaGroup{DisplayName: "Model Quota"}
	for _, entry := range balances {
		name := core.FirstNonEmpty(core.RawStringField(entry, "show_name"), core.RawStringField(entry, "model"))
		if name == "" {
			continue
		}
		parsed++
		remaining := floatField(entry, "remaining_units")
		used := floatField(entry, "used_units")
		total := floatField(entry, "total_units")
		if !seen[name] {
			seen[name] = true
			totalRemaining += remaining
			totalTotal += total
			totalUsed += used
		}
		metrics = append(metrics, pluginapi.QuotaMetric{
			Key:   "remaining_" + sanitizeMetricKey(name),
			Label: name + " Remaining",
			Value: remaining,
			Unit:  "tokens",
		})
		if used > 0 {
			metrics = append(metrics, pluginapi.QuotaMetric{
				Key:   "used_" + sanitizeMetricKey(name),
				Label: name + " Used",
				Value: used,
				Unit:  "tokens",
			})
		}

		// Proportional logic matches qoder: if total exists, use proportion; if total is missing
		// but balance remains, treat as full (if upstream only gives balance, don't draw 0%, which creates the illusion of "exhausted").
		fraction := 0.0
		switch {
		case total > 0:
			fraction = core.ClampFraction(remaining / total)
		case remaining > 0:
			fraction = 1
		}
		bucket := pluginapi.QuotaBucket{
			Window:            name,
			RemainingFraction: fraction,
			Description:       fmt.Sprintf("Used %s / Total %s tokens", formatTokens(used), formatTokens(total)),
		}
		if expires := core.GetUnixSeconds(entry, "expires_at"); expires > 0 {
			// ResetTime's established semantic is "reset time", while one-time gift packages do not reset
			// and expire upon reaching their end time—here we put expires_at at the bucket granularity as a reference for "expiration",
			// Plan-level expiry display is in planExpiryGroup (uses Description).
			bucket.ResetTime = time.Unix(expires, 0).UTC().Format(time.RFC3339)
		}
		group.Buckets = append(group.Buckets, bucket)
	}
	if parsed == 0 {
		// All items failed to parse: do not output a fake summary of "0 remaining", return empty metrics.
		return nil, nil
	}
	// default group summary is placed first: the account table's "Remaining / Total" relies on these two keys to calculate proportions.
	summary := []pluginapi.QuotaMetric{
		{Key: "remaining", Label: "Remaining", Value: totalRemaining, Unit: "tokens"},
	}
	if totalTotal > 0 {
		summary = append(summary, pluginapi.QuotaMetric{Key: "total", Label: "Total", Value: totalTotal, Unit: "tokens"})
	}
	if totalUsed > 0 {
		summary = append(summary, pluginapi.QuotaMetric{Key: "used", Label: "Used", Value: totalUsed, Unit: "tokens"})
	}
	return append(summary, metrics...), []pluginapi.QuotaGroup{group}
}

// planExpiryGroup retrieves the **nearest expiration time** among still-active plans as a plan validity notice.
//
// Logic is consistent with heldPlanIDs (shared via activePlans): empty status is treated as active,
// while active status past ends_at is treated as expired. When multiple plans coexist, take the nearest expiration—users
// care about "when the next one expires", and taking the latest would hide upcoming expirations.
// Does not occupy ResetTime (that is the "reset time" semantic; gift packages do not reset), using Description instead.
func planExpiryGroup(plans []map[string]any, serverTime int64) *pluginapi.QuotaGroup {
	nearest := int64(0)
	for _, plan := range activePlans(plans, serverTime) {
		if ends := core.GetUnixSeconds(plan, "ends_at"); ends > 0 && (nearest == 0 || ends < nearest) {
			nearest = ends
		}
	}
	if nearest == 0 {
		return nil
	}
	return core.QuotaExpiryGroup(time.Unix(nearest, 0))
}

// activePlans returns still-active plan entries (referenced as-is, without copying).
//
// Determination logic is unique: missing status is treated as active (upstream historical format), non-active status
// is excluded; active status past ends_at is also excluded (consistent with official client normalizeStartPlanExpiry
// normalization). heldPlanIDs and planExpiryGroup share this function to ensure that the "held"
// and "validity" views are always consistent.
func activePlans(plans []map[string]any, serverTime int64) []map[string]any {
	out := make([]map[string]any, 0, len(plans))
	for _, entry := range plans {
		status := strings.ToLower(strings.TrimSpace(core.RawStringField(entry, "status")))
		if status != "" && status != "active" {
			continue
		}
		if endsAt := core.GetUnixSeconds(entry, "ends_at"); endsAt > 0 && serverTime > 0 && endsAt <= serverTime {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// formatTokens formats the token count into an integer string (decimals rounded).
func formatTokens(value float64) string {
	return strconv.FormatFloat(value, 'f', 0, 64)
}

// sanitizeMetricKey converts a model name into a stable metric key.
func sanitizeMetricKey(name string) string {
	replacer := strings.NewReplacer(" ", "_", ".", "_", "/", "_", "-", "_")
	return strings.ToLower(replacer.Replace(strings.TrimSpace(name)))
}

// floatField leniently retrieves a float field (JSON numbers are always float64).
func floatField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// Coding Plan package detection and manual claiming.
//
// The workflow has only two steps, identical to the reference implementation zcode2api/app/claim.py:
//
//  1. GET billing/preview?app_version=&platform= → data.plans[] (upstream offerings)
//  2. POST billing/claim body {"plan_id":...} (must include CAPTCHA token)
//
// **Claiming must be performed by a human**: upstream strictly validates billing/claim for Alibaba Cloud frictionless verification tokens,
// No token means 3007. The reference project's "invisible verification (browserless)" solver for this has 4500 lines of
// Node, which this plugin won't reinvent; instead, we pass the SDK initialization parameters to the console page, rendering
// the slider in the user's browser, and the plugin only forwards after user success (see core.PlanClaimer contract).
//
// Upstream business codes:
//
//	0 Success / 1001 Plan not found / 1002 Event ended / 1003 Already claimed
//	1004 Not eligible / 1005 Daily quota exhausted / 3007 Verification failed / 3012 Risk control interception

// Claim-related business codes (common codes already existing in errors.go are not repeated here).
const (
	codePlanNotFound    = 1001 // Plan not found (event offline)
	codePlanClosed      = 1002 // Event has ended or is temporarily unavailable for claim
	codePlanClaimed     = 1003 // Already claimed
	codePlanNotEligible = 1004 // Does not meet claim requirements
	codeDailyQuotaUsed  = 1005 // Today's claim quota is exhausted
)

// Claim throttling: rapid requests from the same exit (= same CPA host) will trigger WAF 1005/3012,
// so a minimum interval is left between two claims. Reference implementations do the same per proxy port; here there is no
// proxy concept, equivalent to throttling within an instance.
//
// **State belongs to the instance**: multiple Apps in the same process each have their own throttle window, independent of each other
// (consistent with the README instance isolation convention, and matching the multi-App coexistence scenario in tests).
const defaultClaimMinInterval = 15 * time.Second

// claimGate is a claim throttler for a vendor instance.
type claimGate struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

// newClaimGate constructs a throttler with a default interval of defaultClaimMinInterval.
func newClaimGate() *claimGate {
	return &claimGate{interval: defaultClaimMinInterval}
}

// setInterval overrides the minimum interval (0 means no throttling).
func (g *claimGate) setInterval(interval time.Duration) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.interval = interval
	g.mu.Unlock()
}

// wait blocks until the next claim is allowed to be initiated.
//
// Returns false if ctx has been cancelled: the caller should abort this claim instead of forcing it through.
func (g *claimGate) wait(ctx context.Context) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	interval := g.interval
	wait := interval - time.Since(g.last)
	g.mu.Unlock()
	if interval <= 0 || wait <= 0 {
		return true
	}
	return core.SleepContext(ctx, wait)
}

// markIssued records that a claim has been dispatched (the starting point for throttle timing).
func (g *claimGate) markIssued() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.last = time.Now()
	g.mu.Unlock()
}

// ── Captcha Configuration ──────────────────────────────────────────────────────────────

// FetchCaptchaConfig fetches the SDK initialization parameters for Alibaba Cloud invisible verification.
//
// The source is the **auth-free public endpoint** client/configs data.configs.captcha — the plugin reads the model
// manifest and is already hitting this endpoint, so this just parses an extra layer of fields without adding extra requests.
func FetchCaptchaConfig(ctx context.Context) (core.CaptchaConfig, error) {
	fallback := core.CaptchaConfig{
		Enabled: true,
		SceneID: captchaFallbackScene,
		Region:  captchaFallbackRegion,
		Prefix:  captchaFallbackPrefix,
	}

	endpoint := ZCodeOrigin + PathClientConfigs + "?app_version=" + ClientAppVersion
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errNew != nil {
		return fallback, nil
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, errDo := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if errDo != nil {
		return fallback, nil
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return fallback, nil
	}

	config := parseCaptchaConfig(raw)
	if config.SceneID == "" {
		// When upstream does not provide sceneId, retain the fallback prefix/region, only filling in the fields that can be obtained.
		config.Prefix = core.FirstNonEmpty(config.Prefix, fallback.Prefix)
		config.Region = core.FirstNonEmpty(config.Region, fallback.Region)
		return fallback, nil
	}
	config.Enabled = true
	return config, nil
}

// parseCaptchaConfig extracts data.configs.captcha from the client/configs response.
func parseCaptchaConfig(body []byte) core.CaptchaConfig {
	var payload struct {
		Data struct {
			Configs struct {
				Captcha struct {
					Enabled bool   `json:"enabled"`
					SceneID string `json:"sceneId"`
					Region  string `json:"region"`
					Prefix  string `json:"prefix"`
				} `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return core.CaptchaConfig{}
	}
	captcha := payload.Data.Configs.Captcha
	return core.CaptchaConfig{
		Enabled: captcha.Enabled,
		SceneID: strings.TrimSpace(captcha.SceneID),
		Region:  strings.TrimSpace(captcha.Region),
		Prefix:  strings.TrimSpace(captcha.Prefix),
	}
}

// ── Plan Discovery ────────────────────────────────────────────────────────────────

// planSnapshot is an "account plan view": what upstream offers and what the account currently holds.
//
// All are taken from the billing family: offering status is in billing/preview, holdings and balances are in
// billing/balance (billing/current is deprecated; balance's data.plans is authoritative).
// A snapshot consists of two requests, reused for quota queries, button states, and pre-claim validation.
type planSnapshot struct {
	// offered is the plans currently offered upstream (in descending order of priority).
	offered []core.PlanInfo
	// held is the set of plan IDs already held by the account and still active.
	held map[string]bool
	// plans are the raw plan entries returned by balance (for subscription name and status display).
	plans []map[string]any
	// balances are the raw quota bucket entries returned by balance (subscription names associated with plans by expiration time).
	balances []map[string]any
	// serverTime is the server time in the balance response (seconds, milliseconds normalized); 0 if missing.
	serverTime int64
	// metrics are the remaining quotas for each model calculated from balance.
	metrics []pluginapi.QuotaMetric
	// groups are window groups converted from balance (host card renders progress bar and expiration time).
	groups []pluginapi.QuotaGroup
	// errPreview is the failure reason for the preview request (held is empty when balance fails).
	errPreview error
	// errBalance is the failure reason for the balance request; when non-empty, the quota result is incomplete.
	errBalance error
	// errActivation is the failure reason for active reporting, used to explain "why no campaigns can be seen".
	errActivation string
}

// claimable returns unclaimed offered plans.
func (s planSnapshot) claimable() []core.PlanInfo {
	out := make([]core.PlanInfo, 0, len(s.offered))
	for _, plan := range s.offered {
		if !s.held[plan.PlanID] {
			out = append(out, plan)
		}
	}
	return out
}

// fetchPlanSnapshot fetches the plan view.
//
// The order has semantics: active reporting must be **before** preview—the upstream decides whether to offer campaigns based on "daily active",
// doing it the other way around will cause a new day to never get campaigns (see activation.go).
//
// balance failure is not considered an overall failure: at this point, the held collection is treated as empty (the result is conservative—claimed plans
// will be listed as claimable again, and when the user clicks and gets upstream 1003, the semantics are still correct), but the quota and subscription name
// will be missing, and the caller decides whether to report a failure based on errBalance. preview failure is recorded in errPreview.
func (v *adapter) fetchPlanSnapshot(ctx context.Context, cred *Credential) planSnapshot {
	snapshot := planSnapshot{held: map[string]bool{}}
	snapshot.errActivation = v.ensureActivation(ctx, cred)

	code, body, errBalance := billingRequest(ctx, cred, http.MethodGet,
		PathBillingBalance+"?app_version="+url.QueryEscape(ClientAppVersion), nil)
	switch {
	case errBalance != nil:
		snapshot.errBalance = errBalance
	case code != codeSuccess:
		snapshot.errBalance = Classify(0, classifyBillingOutcome(code, body))
	case !billingEnvelopeComplete(body):
		// Business code success but missing balances array: this is an upstream revision or truncated response, not
		// "this account has no quota". Treating it as success would cause the page to take default subscription names and empty quotas as true,
		// and the user wouldn't notice the anomaly; report as transient so the next query has a chance to recover.
		snapshot.errBalance = transientErr("ZCode billing/balance response missing balances array")
	default:
		snapshot.serverTime = serverSeconds(body)
		snapshot.plans = planRows(body)
		snapshot.balances = objectRows(dataMap(body)["balances"])
		snapshot.held = heldPlanIDs(snapshot.plans, snapshot.serverTime)
		snapshot.metrics, snapshot.groups = balanceQuota(snapshot.balances)
		if expiry := planExpiryGroup(snapshot.plans, snapshot.serverTime); expiry != nil {
			snapshot.groups = append(snapshot.groups, *expiry)
		}
	}

	offered, code, errOffered := fetchOfferedPlans(ctx, cred)
	if errOffered != nil {
		snapshot.errPreview = errOffered
		return snapshot
	}
	if code != codeSuccess {
		snapshot.errPreview = claimFailure(code, nil)
		return snapshot
	}
	snapshot.offered = offered
	return snapshot
}

// planRows gets data.plans from the balance response.
func planRows(body map[string]any) []map[string]any {
	return objectRows(dataMap(body)["plans"])
}

// objectRows picks objects out of a JSON array, ignoring entries of other forms.
func objectRows(value any) []map[string]any {
	items, _ := value.([]any)
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if entry, okEntry := item.(map[string]any); okEntry {
			rows = append(rows, entry)
		}
	}
	return rows
}

// serverSeconds gets the server time in seconds from the response body; returns 0 when missing.
//
// Use server time instead of the local clock: expiration judgment must be consistent with the upstream standard, and local clock drift would cause
// just-ended campaigns to continue showing as "held". Upstream occasionally gives 13-digit millisecond timestamps,
// core.GetUnixSeconds is responsible for standardizing both formats into seconds.
func serverSeconds(body map[string]any) int64 {
	return core.GetUnixSeconds(dataMap(body), "server_time")
}

// billingEnvelopeComplete reports whether the balance response is complete with parseable quota buckets.
//
// Only requires data.balances to be an array: an empty array is a valid answer (the account indeed has no buckets), while missing
// fields indicate upstream revision or truncated response. plans is not a hard requirement—it only affects subscription name display,
// and judging it as a failure would turn "upstream omitted a decorative field" into failing to query the quota.
func billingEnvelopeComplete(body map[string]any) bool {
	_, okBalances := dataMap(body)["balances"].([]any)
	return okBalances
}

// heldPlanIDs gets the plan IDs held and currently active by the account from balance's data.plans.
//
// For active criteria see activePlans (missing status is considered active, past ends_at is considered expired):
// status other than active is not considered held, otherwise buttons will remain grayed out after campaigns go offline.
func heldPlanIDs(plans []map[string]any, serverTime int64) map[string]bool {
	held := map[string]bool{}
	for _, entry := range activePlans(plans, serverTime) {
		planID := normalizePlanID(core.FirstNonEmpty(
			core.RawStringField(entry, "plan_id"),
			core.RawStringField(entry, "planId"),
		))
		if planID != "" {
			held[planID] = true
		}
	}
	return held
}

// fetchOfferedPlans fetches upstream offered plans and sorts them by priority.
func fetchOfferedPlans(ctx context.Context, cred *Credential) ([]core.PlanInfo, int, error) {
	path := PathBillingPreview +
		"?app_version=" + url.QueryEscape(ClientAppVersion) +
		"&platform=" + url.QueryEscape(cred.Profile.PlatformFull())
	code, body, errRequest := billingRequest(ctx, cred, http.MethodGet, path, nil)
	if errRequest != nil {
		return nil, 0, errRequest
	}
	if code != codeSuccess {
		return nil, code, nil
	}

	return normalizePlans(planRows(body)), codeSuccess, nil
}

// normalizePlans normalizes raw plan entries into a list of PlanInfos.
//
// Discards entries without plan_id, deduplicates by plan_id, and sorts by descending priority / ascending plan_id:
// Same priority must also have a stable order, otherwise page display will jitter with upstream return order. billing/preview and
// marketing/touch pipelines share this convention.
func normalizePlans(rows []map[string]any) []core.PlanInfo {
	plans := make([]core.PlanInfo, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		plan := parsePlan(row)
		if plan == nil || seen[plan.PlanID] {
			continue
		}
		seen[plan.PlanID] = true
		plans = append(plans, *plan)
	}
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].Priority != plans[j].Priority {
			return plans[i].Priority > plans[j].Priority
		}
		return plans[i].PlanID < plans[j].PlanID
	})
	return plans
}

// ProbeActivity only probes the account's currently visible campaign offerings without executing claims.
//
// Probing allows API Key accounts to participate (such as 1024 Dev Day which doesn't restrict Coding Plan),
// so it bypasses billingBlockReason (that channel only serves JWT). Active reporting failure is only downgraded to
// Warning: API Key credentials lacking user_id cannot report active status, but marketing channels may still have offerings,
// failing directly would make the endpoint completely unavailable to them.
func (v *adapter) ProbeActivity(ctx context.Context, cred *core.Credential) (core.ActivityProbeResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.ActivityProbeResult{}, errNative
	}
	if native == nil {
		return core.ActivityProbeResult{}, core.NewPluginError("zcode_credential_missing", "zcode credential is nil", http.StatusBadRequest)
	}
	warning := v.ensureActivation(ctx, native)
	activities, errQuery := queryMarketingTouch(ctx, native)
	if errQuery != nil {
		return core.ActivityProbeResult{}, errQuery
	}
	return core.ActivityProbeResult{Activities: activities, Warning: warning}, nil
}

// PreviewPlans returns plans currently claimable by the account (excluding already held ones).
//
// An empty slice means there are nothing claimable, and the page accordingly grays out the button to "Claimed".
func (v *adapter) PreviewPlans(ctx context.Context, cred *core.Credential) ([]core.PlanInfo, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return v.previewPlans(ctx, native)
}

func (v *adapter) previewPlans(ctx context.Context, cred *Credential) ([]core.PlanInfo, error) {
	if reason := billingBlockReason(cred); reason != "" {
		return nil, core.NewPluginError("zcode_no_plan_channel", reason, http.StatusBadRequest)
	}
	snapshot := v.fetchPlanSnapshot(ctx, cred)
	if snapshot.errPreview != nil {
		return nil, snapshot.errPreview
	}
	return snapshot.claimable(), nil
}

// claimStatusOf folds the plan view into the claim status of the quota response.
//
// **"Empty list" and "not checked" must differ**: only a successful preview with nothing offered means "claimed";
// reporting "claimed" when the activity report or preview failed misleads the page, and the user would miss an offer.
func claimStatusOf(snapshot planSnapshot) *pluginapi.ClaimStatus {
	status := &pluginapi.ClaimStatus{
		Available: len(snapshot.offered),
		Claimable: len(snapshot.claimable()),
	}
	switch {
	case snapshot.errPreview != nil:
		status.Error = snapshot.errPreview.Error()
	case len(snapshot.offered) == 0 && snapshot.errActivation != "":
		// Offers require same-day activity: without a reported activity, an empty list does not mean "all claimed".
		status.Error = snapshot.errActivation
	}
	return status
}

// parsePlan normalizes one plan; returns nil when plan_id is missing.
func parsePlan(raw map[string]any) *core.PlanInfo {
	planID := normalizePlanID(core.FirstNonEmpty(
		core.RawStringField(raw, "plan_id"),
		core.RawStringField(raw, "planId"),
	))
	if planID == "" {
		return nil
	}
	return &core.PlanInfo{
		PlanID:      planID,
		Name:        core.FirstNonEmpty(core.RawStringField(raw, "name"), planID),
		Description: core.RawStringField(raw, "description"),
		Priority:    int(floatField(raw, "priority")),
		Grants:      parsePlanGrants(raw),
		StartsAt:    unixMillis(raw, "starts_at"),
		EndsAt:      unixMillis(raw, "ends_at"),
	}
}

// parsePlanGrants extracts the token quota a plan grants.
//
// The upstream may send grants[] (parsed form) or entitlements[] (raw form).
// Both are accepted; when both are empty it returns nil rather than an empty slice, since nil means "none" in JSON and
// lets the front end tell "no quota info" apart from "quota is 0".
func parsePlanGrants(raw map[string]any) []core.PlanGrant {
	var grants []core.PlanGrant
	appendGrant := func(name string, units float64, period string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		grants = append(grants, core.PlanGrant{Model: name, Units: units, Period: period})
	}

	if entitlements, okList := raw["entitlements"].([]any); okList {
		for _, item := range entitlements {
			entry, okEntry := item.(map[string]any)
			if !okEntry {
				continue
			}
			// Only model-usage token grants count; other units mean nothing on the page.
			if meter := core.RawStringField(entry, "meter"); meter != "" && meter != "model_usage" {
				continue
			}
			if unit := core.RawStringField(entry, "unit_type"); unit != "" && unit != "token" {
				continue
			}
			appendGrant(
				core.FirstNonEmpty(core.RawStringField(entry, "show_name"), core.RawStringField(entry, "showName")),
				firstPositiveFloat(entry, "grant_units", "grantUnits"),
				core.RawStringField(entry, "period"),
			)
		}
	}
	if len(grants) == 0 {
		if list, okList := raw["grants"].([]any); okList {
			for _, item := range list {
				entry, okEntry := item.(map[string]any)
				if !okEntry {
					continue
				}
				appendGrant(
					core.RawStringField(entry, "name"),
					firstPositiveFloat(entry, "units"),
					core.RawStringField(entry, "period"),
				)
			}
		}
	}
	return grants
}

// firstPositiveFloat returns the first present, positive float field.
func firstPositiveFloat(raw map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if value := floatField(raw, key); value > 0 {
			return value
		}
	}
	return 0
}

// normalizePlanID normalizes a plan ID (the upstream mixes case; compare after trimming).
func normalizePlanID(planID string) string {
	return strings.ToLower(strings.TrimSpace(planID))
}

// unixMillis converts upstream timestamps to milliseconds; missing or non-positive values return 0.
//
// Both seconds and milliseconds are first normalized to seconds by core.GetUnixSeconds, so a 13-digit
// millisecond timestamp is not multiplied by 1000 again and pushed decades ahead.
func unixMillis(m map[string]any, keys ...string) int64 {
	seconds := core.GetUnixSeconds(m, keys...)
	if seconds <= 0 {
		return 0
	}
	return seconds * 1000
}

// ── Claiming ────────────────────────────────────────────────────────────────

// ClaimPlan claims the given plan (with an empty PlanID it picks the highest-priority claimable plan).
//
// Every call first runs a live preview: claiming is a manual user action, so it uses
// the real claimable set at the moment of the click, never a cache.
func (v *adapter) claimPlan(ctx context.Context, cred *Credential, req core.ClaimPlanRequest) (core.ClaimPlanResponse, error) {
	if reason := billingBlockReason(cred); reason != "" {
		return core.ClaimPlanResponse{}, core.NewPluginError("zcode_no_plan_channel", reason, http.StatusBadRequest)
	}
	if strings.TrimSpace(req.VerifyParam) == "" {
		// Without a token, do not call the upstream: it always returns 3007 and wastes a risk-control attempt.
		return core.ClaimPlanResponse{}, core.NewPluginError(
			"zcode_captcha_required", "complete the captcha on the page first", http.StatusBadRequest)
	}

	plan, errPick := v.pickClaimPlan(ctx, cred, req.PlanID)
	if errPick != nil {
		return core.ClaimPlanResponse{}, errPick
	}

	if !v.claims.wait(ctx) {
		return core.ClaimPlanResponse{}, core.NewPluginError(
			"zcode_claim_canceled", "claim canceled (interrupted while waiting for the next allowed submit time)", http.StatusRequestTimeout)
	}

	headers := map[string]string{CaptchaHeader: strings.TrimSpace(req.VerifyParam)}
	if region := strings.TrimSpace(req.VerifyRegion); region != "" {
		headers[CaptchaRegionHeader] = region
	}
	v.claims.markIssued()

	code, body, errRequest := billingRequestWithHeaders(
		ctx, cred, http.MethodPost, PathBillingClaim,
		map[string]string{"plan_id": plan.PlanID}, headers,
	)
	if errRequest != nil {
		return core.ClaimPlanResponse{}, errRequest
	}
	if code != codeSuccess {
		return core.ClaimPlanResponse{}, claimFailure(code, body)
	}

	// A successful response carries the server clock and plan start/end, passed through to the page as is.
	result := core.ClaimPlanResponse{PlanID: plan.PlanID, PlanName: plan.Name}
	if claimed, okClaimed := dataMap(body)["plan"].(map[string]any); okClaimed {
		result.PlanName = core.FirstNonEmpty(core.RawStringField(claimed, "name"), plan.Name)
		result.StartsAt = unixMillis(claimed, "starts_at")
		result.EndsAt = unixMillis(claimed, "ends_at")
	}
	return result, nil
}

// pickClaimPlan decides which plan to claim.
//
// With a plan_id, verify it is claimable now (in the preview and not held); otherwise take
// the highest-priority claimable one. With no candidate, fail rather than send an invalid plan_id upstream.
func (v *adapter) pickClaimPlan(ctx context.Context, cred *Credential, planID string) (*core.PlanInfo, error) {
	available, errPreview := v.previewPlans(ctx, cred)
	if errPreview != nil {
		return nil, errPreview
	}
	if len(available) == 0 {
		return nil, core.NewPluginError(
			"zcode_nothing_to_claim", "this account already claimed every available plan", http.StatusBadRequest)
	}

	wanted := normalizePlanID(planID)
	if wanted == "" {
		best := available[0]
		return &best, nil
	}
	for index := range available {
		if available[index].PlanID == wanted {
			return &available[index], nil
		}
	}
	return nil, core.NewPluginError(
		"zcode_plan_not_claimable", "this plan is already claimed or not claimable now; refresh and try again", http.StatusBadRequest)
}

// claimFailure turns a claim business code into a readable error.
//
// Same source as classifyBillingOutcome on the quota path, but claiming adds 1003 (already claimed, reported as success
// so the page disables the button) and 1005 (with the slot recovery time).
func claimFailure(code int, body map[string]any) error {
	serverMsg := core.FirstNonEmpty(core.RawStringField(body, "msg"), core.RawStringField(body, "message"))
	base := classifyBillingOutcome(code, body)

	// 1005 carries the slot recovery time in data.plan.ends_at; the page uses it to say when to retry.
	if code == codeDailyQuotaUsed && serverMsg == "" {
		if nextAt := claimQuotaRecovery(body); nextAt > 0 {
			return core.NewPluginError(
				"zcode_claim_quota_used",
				base+" (slots recover at "+formatClaimRecovery(nextAt)+")",
				http.StatusTooManyRequests,
			)
		}
	}
	return core.NewPluginError("zcode_claim_failed", base, http.StatusBadRequest)
}

// claimQuotaRecovery reads the slot recovery time (milliseconds) from a 1005 response.
func claimQuotaRecovery(body map[string]any) int64 {
	plan, okPlan := dataMap(body)["plan"].(map[string]any)
	if !okPlan {
		return 0
	}
	return unixMillis(plan, "ends_at")
}

// formatClaimRecovery formats the recovery time as "today/tomorrow HH:MM:SS".
func formatClaimRecovery(at int64) string {
	if at <= 0 {
		return "-"
	}
	return time.UnixMilli(at).In(claimTimezone).Format("01-02 15:04:05")
}

// claimTimezone is the display timezone for the quota recovery time (Beijing Time),
// consistent with the upstream "reset daily at 00:00" policy.
var claimTimezone = time.FixedZone("Asia/Shanghai", 8*3600)

// CaptchaConfig implements core.CaptchaProvider.
func (v *adapter) CaptchaConfig(ctx context.Context) (core.CaptchaConfig, error) {
	return FetchCaptchaConfig(ctx)
}

// ClaimPlan implements core.PlanClaimProvider.
func (v *adapter) ClaimPlan(ctx context.Context, cred *core.Credential, req core.ClaimPlanRequest) (core.ClaimPlanResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.ClaimPlanResponse{}, errNative
	}
	return v.claimPlan(ctx, native, req)
}

// ---- Activity Reporting ----
//
// Campaign delivery uses "daily active" as the eligibility signal: the official desktop client reports this
// on every startup and every calendar day. Accounts missing these two events always have an empty preview. We only report here, not claim.

// Activation event reporting: makes the upstream treat this account as a "today active official client".
//
// Why it is needed: the billing/preview response is **not** a pure account entitlement snapshot; promotional plan delivery
// uses "daily active" as the eligibility signal. The official desktop client sends app_launch on every startup, and
// app_daily_active once per calendar day (from packages/services/src/telemetry/telemetryCore.ts,
// reportAppLaunch / reportAppDailyActive). Accounts that do not send these two events have an empty preview—
// so the plugin calculates 0 available claims, the console permanently grays out the button as "Claimed", while the account actually hasn't
// claimed any plans. This is unrelated to the claim API itself: manual claiming when a campaign is active does succeed.
//
// The event body strictly aligns with the official 16 fields of sendReport; the endpoint is not under zcode-plan, nor does it carry
// Authorization (the official endpoint also reports anonymously).

// activationElements are the two element names reported by the official client on startup/daily active.
var activationElements = []string{"app_launch", "app_daily_active"}

// activationTimeout is the upper limit for a single telemetry point: telemetry failure must not block quota queries.
const activationTimeout = 10 * time.Second

// activationState is an in-process deduplication table for "report once per account per calendar day".
//
// The state belongs to the instance rather than disk persistence: independent when multiple apps coexist (same convention as claimGate).
// Reporting an extra round after process restart has no side effects—upstream itself deduplicates by device_mid + date.
type activationState struct {
	mu   sync.Mutex
	last map[string]string // uid → local date (account profile timezone)
}

func newActivationState() *activationState {
	return &activationState{last: map[string]string{}}
}

// shouldReport reports whether to report this time, and preempts if needed (to prevent concurrent duplicate reports).
func (s *activationState) shouldReport(uid string, today string) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last[uid] == today {
		return false
	}
	s.last[uid] = today
	return true
}

// forget revokes the preemption: allows the next retry when reporting fails.
func (s *activationState) forget(uid string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.last, uid)
	s.mu.Unlock()
}

// localDate returns the calendar date (YYYY-MM-DD) in the account profile timezone.
//
// Falls back to UTC if timezone cannot be retrieved: coarser deduplication granularity has no side effects, just might report an extra time.
func localDate(profile DeviceProfile, now time.Time) string {
	zone := strings.TrimSpace(profile.Timezone)
	if zone != "" {
		if location, errLoad := time.LoadLocation(zone); errLoad == nil {
			return now.In(location).Format("2006-01-02")
		}
	}
	return now.UTC().Format("2006-01-02")
}

// activationEventBody constructs the official sendReport 16-field event body.
func activationEventBody(element string, cred *Credential, userID string) map[string]any {
	profile := cred.Profile
	profile.ensure()
	return map[string]any{
		"event_id":           NewUUID(),
		"client_timezone":    profile.Timezone,
		"client_language":    profile.Language,
		"element_name":       element,
		"event_region":       "app",
		"event_type":         "view",
		"event_text":         "",
		"event_extra_detail": map[string]any{},
		"user_id":            userID,
		"screen_resolution":  profile.Screen,
		"app_version":        ClientAppVersion,
		"device_os_category": profile.OSCategory(),
		"device_os_version":  profile.OSVersion,
		"device_mid":         profile.DeviceMID,
		"mac_id":             "",
		"marketing_params":   "{}",
	}
}

// postActivationEvent reports a single activation event.
//
// Uses hostbridge.Client sharing the same egress with other upstream requests; carries no Authorization, only the official
// source header (fields related to this report in identity headers are already in the event body; the header itself is used to pass the gateway).
func postActivationEvent(ctx context.Context, cred *Credential, body map[string]any) error {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return errMarshal
	}

	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost,
		ZCodeOrigin+PathEventReport, bytes.NewReader(encoded))
	if errNew != nil {
		return errNew
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("HTTP-Referer", ZCodeOrigin)
	req.Header.Set("X-Title", ClientTitle)

	resp, errDo := hostbridge.Client(ctx, activationTimeout).Do(req)
	if errDo != nil {
		return fmt.Errorf("activation report failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("activation report HTTP %d: %s", resp.StatusCode, truncateText(string(raw), 120))
	}
	// Business code non-zero is treated as failure; non-JSON responses are treated as success (gateway may return empty body).
	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal == nil {
		if code := businessCode(parsed); code != -1 && code != 0 {
			return fmt.Errorf("activation report business code %d: %s", code, truncateText(string(raw), 120))
		}
	}
	return nil
}

// ensureActivation ensures this account has reported activity to the platform today, returning the failure reason (empty string means no need to report again
// or already successful).
//
// Failure **does not block** subsequent previews: the official client also doesn't stop business due to telemetry failure; here we pass the reason to
// the caller to decide whether to display or ignore.
func (v *adapter) ensureActivation(ctx context.Context, cred *Credential) string {
	if cred == nil {
		return ""
	}
	userID := strings.TrimSpace(cred.UserID)
	if userID == "" {
		userID = UserIDFromJWT(cred.JWTToken)
	}
	if userID == "" {
		// Cannot report without user identity (official event body attributes to user_id); state explicitly rather than silently skipping,
		// otherwise the user will continue to see an permanently empty campaign list.
		return "Credential lacks user_id, unable to report active event (campaign delivery may be incomplete; please re-login or provide user_id)"
	}

	today := localDate(cred.Profile, time.Now())
	if !v.activations.shouldReport(userID, today) {
		return ""
	}

	for _, element := range activationElements {
		if err := postActivationEvent(ctx, cred, activationEventBody(element, cred, userID)); err != nil {
			v.activations.forget(userID)
			logger.Info("zcode activation report failed (%s): %v", element, err)
			return err.Error()
		}
	}
	return ""
}

// truncateText truncates excessively long upstream response snippets to avoid stuffing entire HTML pages into error messages.
func truncateText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// ---- Marketing Campaigns ----
//
// marketing/touch is ZCode's delivery channel, independent of the billing family: read-only, does not participate
// Claimable, also open to API Key accounts (billing only serves JWT Coding Plans).

// Marketing touch (marketing/touch): ZCode delivery channel.
//
// Independent pipeline from the billing family (billing/*): marketing channel is read-only, does not participate in claims, and is
// also open to API Key accounts (billing only serves JWT Coding Plans). Delivery uses "daily active" as qualification
// signal, event reporting is completed by the caller before probing (see activation.go).

const pathMarketingTouch = "/api/v1/marketing/touch"

// marketingHeaders reuses upstream identity and auth headers; this interface is read-only GET, only declaring Accept additionally.
func marketingHeaders(cred *Credential) map[string]string {
	headers := upstreamAuthHeaders(cred)
	headers["Accept"] = "application/json"
	return headers
}

// queryMarketingTouch fetches the account's currently visible campaign deliveries.
func queryMarketingTouch(ctx context.Context, cred *Credential) ([]core.PlanInfo, error) {
	u, errParse := url.Parse(ZCodeOrigin + pathMarketingTouch)
	if errParse != nil {
		return nil, errParse
	}
	// seq=0 fetches the first screen. Upstream has no public cursor semantics, so no speculative pagination here: extra pagination would
	// pull the same batch of deliveries again, amplifying request volume instead.
	u.RawQuery = "seq=0"

	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if errNew != nil {
		return nil, errNew
	}
	for key, value := range marketingHeaders(cred) {
		req.Header.Set(key, value)
	}

	resp, errDo := hostbridge.Client(ctx, billingHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, billingMaxBody))
	if errRead != nil {
		return nil, errRead
	}
	return parseMarketingTouch(resp.StatusCode, raw)
}

// parseMarketingTouch parses the marketing touch response.
//
// Only recognize data.deliveries[].banner.background.args.zcode_plan: campaign plans are tucked in the banner's
// rendering arguments, other deliveries (pure text/images) have no plan_id and will be dropped by normalizePlans.
//
// Do not filter secondarily by starts_at/ends_at: upstream already filters deliveries by current time, local clock drift would only
// cause false positives. What is returned here is "upstream's current deliveries", not "claimable by this account at this moment".
func parseMarketingTouch(status int, raw []byte) ([]core.PlanInfo, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, core.NewPluginError("zcode_activity_response",
			"Marketing campaign response is not valid JSON: "+truncateText(string(raw), 120), http.StatusBadGateway)
	}
	if status < 200 || status >= 300 {
		return nil, core.NewPluginError("zcode_activity_http",
			fmt.Sprintf("Marketing campaign query HTTP %d: %s", status, truncateText(string(raw), 120)), http.StatusBadGateway)
	}
	// Treat as success on 2xx with no business code: gateway may strip the envelope, same convention as activation reporting.
	if code := businessCode(payload); code != 0 && code != -1 {
		message := core.FirstNonEmpty(core.RawStringField(payload, "msg"), "upstream did not provide a reason")
		return nil, core.NewPluginError("zcode_activity_business",
			fmt.Sprintf("Marketing campaign query business code %d: %s", code, message), http.StatusBadGateway)
	}

	deliveries := objectRows(dataMap(payload)["deliveries"])
	plans := make([]map[string]any, 0, len(deliveries))
	for _, delivery := range deliveries {
		if plan := nestedMap(delivery, "banner", "background", "args", "zcode_plan"); plan != nil {
			plans = append(plans, plan)
		}
	}
	return normalizePlans(plans), nil
}

// nestedMap traverses objects layer by layer along a field path, returning nil if any layer is missing or not an object.
func nestedMap(raw map[string]any, path ...string) map[string]any {
	for _, key := range path {
		if raw, _ = raw[key].(map[string]any); raw == nil {
			return nil
		}
	}
	return raw
}
