package zcode

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
)

// Pure function tests for plan detection and claiming (offline).
//
// Covers three types of issues prone to silent failures:
//   - Normalization: plan_id casing/whitespace, expired plans with non-active status should not count as "held";
//   - Set operations: offered ∩ held = basis for graying out buttons;
//   - Business code translation: UI text comes directly from here.

func TestBillingHeadersFallBackToAPIKey(t *testing.T) {
	cred := &Credential{APIKey: "sk-test", Profile: DeviceProfile{Platform: "win32", Arch: "x64"}}
	headers := billingHeaders(cred)
	if headers["x-api-key"] != "sk-test" || headers["Authorization"] != "" {
		t.Fatalf("API key headers = %#v", headers)
	}
}

func TestNormalizePlanIDIsCaseAndSpaceInsensitive(t *testing.T) {
	// Upstream plan_ids have inconsistent casing ("ZCode-V3-Start-Plan" and "zcode-v3-start-plan"
	// refer to the same plan). Without normalization, claimed plans would be listed as claimable again.
	if got := normalizePlanID("  ZCode-V3-Start-Plan \n"); got != "zcode-v3-start-plan" {
		t.Fatalf("normalizePlanID = %q, want zcode-v3-start-plan", got)
	}
	if got := normalizePlanID("   "); got != "" {
		t.Fatalf("normalizePlanID(blank) = %q, want empty string", got)
	}
}

func TestParsePlanRequiresPlanID(t *testing.T) {
	// Entries missing a plan_id cannot be claimed and must be dropped instead of generating plans with empty IDs.
	if got := parsePlan(map[string]any{"name": "无 ID 套餐"}); got != nil {
		t.Fatalf("parsePlan(no plan_id) = %+v, want nil", got)
	}
	// planId is the camelCase alias; upstream uses both field names.
	if got := parsePlan(map[string]any{"planId": "p1", "name": "套餐一"}); got == nil || got.PlanID != "p1" {
		t.Fatalf("parsePlan(planId) = %+v, want plan_id=p1", got)
	}
}

func TestParsePlanGrantsFromEntitlements(t *testing.T) {
	plan := parsePlan(map[string]any{
		"plan_id":   "p1",
		"name":      "GLM 活动",
		"priority":  float64(7),
		"starts_at": float64(1700000000),
		"ends_at":   float64(1800000000),
		"entitlements": []any{
			map[string]any{
				"meter": "model_usage", "unit_type": "token",
				"show_name": "GLM-5.3", "grant_units": float64(250000), "period": "daily",
			},
			map[string]any{
				// Non-token metering is meaningless to the page and should be filtered out.
				"meter": "storage", "unit_type": "byte",
				"show_name": "网盘", "grant_units": float64(1),
			},
			map[string]any{
				"meter": "model_usage", "unit_type": "token",
				"show_name": "GLM-5.3-Flash", "grant_units": float64(0),
			},
		},
	})
	if plan == nil {
		t.Fatal("parsePlan returned nil")
	}
	if plan.Priority != 7 || plan.StartsAt != 1700000000000 || plan.EndsAt != 1800000000000 {
		t.Fatalf("plan metadata parsing error: %+v", plan)
	}
	if len(plan.Grants) != 2 {
		t.Fatalf("grant count = %d, want 2 (storage item filtered out, zero-quota items kept for display): %+v", len(plan.Grants), plan.Grants)
	}
	if plan.Grants[0].Model != "GLM-5.3" || plan.Grants[0].Units != 250000 || plan.Grants[0].Period != "daily" {
		t.Fatalf("first grant item = %+v", plan.Grants[0])
	}
}

func TestParsePlanGrantsFallsBackToGrantsField(t *testing.T) {
	// Upstream may also directly provide pre-parsed grants[].
	plan := parsePlan(map[string]any{
		"plan_id": "p1",
		"grants":  []any{map[string]any{"name": "GLM-5.3", "units": float64(1000), "period": "one_time"}},
	})
	if plan == nil || len(plan.Grants) != 1 || plan.Grants[0].Model != "GLM-5.3" {
		t.Fatalf("grants fallback parsing failed: %+v", plan)
	}
}

func TestHeldPlanIDsExcludesInactivePlans(t *testing.T) {
	plans := planRows(map[string]any{"data": map[string]any{"plans": []any{
		map[string]any{"plan_id": "active-plan", "status": "active"},
		map[string]any{"plan_id": "expired-plan", "status": "expired"},
		map[string]any{"plan_id": "implicit-active"},
		map[string]any{"status": "active"}, // 无 ID：跳过
	}}})
	held := heldPlanIDs(plans, 0)
	if !held["active-plan"] || !held["implicit-active"] {
		t.Fatalf("active plan should be counted as held: %+v", held)
	}
	// Expired plans must be excluded: otherwise, buttons will remain grayed out permanently after the event ends.
	if held["expired-plan"] {
		t.Fatalf("expired plan should not be counted as held: %+v", held)
	}
}

func TestHeldPlanIDsTreatsPastEndsAtAsExpired(t *testing.T) {
	// Upstream might still mark ended events as active (official client normalizes them to expired based on server time).
	// Without this normalization, buttons would stay grayed out as "Claimed" after events end.
	plans := planRows(map[string]any{"data": map[string]any{"plans": []any{
		map[string]any{"plan_id": "ended", "status": "active", "ends_at": float64(1000)},
		map[string]any{"plan_id": "running", "status": "active", "ends_at": float64(3000)},
		map[string]any{"plan_id": "no-end", "status": "active"},
	}}})
	held := heldPlanIDs(plans, 2000)
	if held["ended"] {
		t.Fatalf("active plan with past ends_at should not be counted as held: %+v", held)
	}
	if !held["running"] || !held["no-end"] {
		t.Fatalf("unexpired / no-expiration-time plan should be counted as held: %+v", held)
	}
	// Do not infer expiration when server time is missing (0), remaining conservative.
	if !heldPlanIDs(plans, 0)["ended"] {
		t.Fatal("should not infer expiration using local clock when server time is missing")
	}
}

func TestPlanSnapshotClaimableSubtractsHeld(t *testing.T) {
	snapshot := planSnapshot{
		offered: []core.PlanInfo{{PlanID: "a"}, {PlanID: "b"}, {PlanID: "c"}},
		held:    map[string]bool{"b": true},
	}
	claimable := snapshot.claimable()
	if len(claimable) != 2 {
		t.Fatalf("claimable count = %d, want 2: %+v", len(claimable), claimable)
	}
	for _, plan := range claimable {
		if plan.PlanID == "b" {
			t.Fatal("already held plans should not appear in the claimable list")
		}
	}
}

func TestClaimStatusCountsAndErrors(t *testing.T) {
	clean := claimStatusOf(planSnapshot{
		offered: []core.PlanInfo{{PlanID: "a"}, {PlanID: "b"}},
		held:    map[string]bool{"b": true},
	})
	if clean.Available != 2 || clean.Claimable != 1 || clean.Error != "" {
		t.Fatalf("claim status error: %+v", clean)
	}
	// All claimed -> Claimable=0, causing the page to gray out the button as "Claimed".
	all := claimStatusOf(planSnapshot{
		offered: []core.PlanInfo{{PlanID: "a"}},
		held:    map[string]bool{"a": true},
	})
	if all.Claimable != 0 {
		t.Fatalf("claimable after full claim = %d, want 0", all.Claimable)
	}
	// preview failures must include an error: the page uses this to keep the button clickable rather than incorrectly showing "Claimed".
	failed := claimStatusOf(planSnapshot{
		held:       map[string]bool{},
		errPreview: core.NewPluginError("x", "preview 失败", 502),
	})
	if failed.Error == "" || failed.Claimable != 0 {
		t.Fatalf("preview failure should carry error: %+v", failed)
	}

	// Active reporting fails and activity list is empty: cannot be called "Claimed" -- an empty list does not mean fully claimed.
	activationFailed := claimStatusOf(planSnapshot{
		held:          map[string]bool{},
		errActivation: "激活上报 HTTP 500",
	})
	if activationFailed.Error == "" {
		t.Fatalf("empty list must carry error when active report fails: %+v", activationFailed)
	}
	// Conversely: when offers exist, active reporting failures do not affect button availability and should not disturb the user.
	withPlans := claimStatusOf(planSnapshot{
		offered:       []core.PlanInfo{{PlanID: "a"}},
		held:          map[string]bool{},
		errActivation: "激活上报 HTTP 500",
	})
	if withPlans.Error != "" || withPlans.Claimable != 1 {
		t.Fatalf("should not carry error when claimable plans exist: %+v", withPlans)
	}
}

func TestParseCaptchaConfig(t *testing.T) {
	body := "{\"data\":{\"configs\":{\"captcha\":{\"enabled\":true,\"sceneId\":\"sid\",\"region\":\"cn\",\"prefix\":\"pfx\"}}}}"
	config := parseCaptchaConfig([]byte(body))
	if config.SceneID != "sid" || config.Region != "cn" || config.Prefix != "pfx" || !config.Enabled {
		t.Fatalf("verification code config parsing error: %+v", config)
	}
	if got := parseCaptchaConfig([]byte("{\"data\":{}}")); got != (core.CaptchaConfig{}) {
		t.Fatalf("missing fields should return zero values, got %+v", got)
	}
	// Fallback constants must be complete (used to render sliders when configs are unavailable).
	if captchaFallbackScene == "" || captchaFallbackRegion == "" || captchaFallbackPrefix == "" {
		t.Fatal("verification code fallback constant should not be empty")
	}
}

func TestClaimFailureMessages(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{codePlanClaimed, "already been claimed"},
		{codePlanNotFound, "plan does not exist"},
		{codeDailyQuotaUsed, "daily claim quota has been exhausted"},
		{codeCaptchaFailed, "captcha verification failed"},
	}
	for _, item := range cases {
		message := claimFailure(item.code, map[string]any{}).Error()
		if !strings.Contains(message, item.want) {
			t.Fatalf("code=%d message = %q, want it to contain %q", item.code, message, item.want)
		}
	}
}

func TestClaimFailureQuotaRecoveryCarriesTime(t *testing.T) {
	// 1005 includes quota recovery time, which the page uses to prompt when to retry.
	err := claimFailure(codeDailyQuotaUsed, map[string]any{
		"data": map[string]any{"plan": map[string]any{"ends_at": float64(1800000000)}},
	})
	if !strings.Contains(err.Error(), "slots recover at") {
		t.Fatalf("1005 must include the slot recovery time, got %q", err.Error())
	}
}

func TestClaimEndpointsUseBillingBase(t *testing.T) {
	// Both claiming and preview hang under the billing family: incorrect paths result in 404, and 404
	// only manifests as "query failed" on the page, making it hard to troubleshoot.
	if got := BillingBase + PathBillingPreview; got != "https://zcode.z.ai/api/v1/zcode-plan/billing/preview" {
		t.Fatalf("preview endpoint = %q", got)
	}
	if got := BillingBase + PathBillingClaim; got != "https://zcode.z.ai/api/v1/zcode-plan/billing/claim" {
		t.Fatalf("claim endpoint = %q", got)
	}
}

func TestClaimThrottleBlocksAndHonorsCancellation(t *testing.T) {
	// The semantics of throttling are "block until allowed", not "reject": the caller waits
	// briefly during the same claim attempt, and waiting should not be treated as a failure.
	gate := newClaimGate()
	gate.markIssued()
	gate.setInterval(0)
	if !gate.wait(context.Background()) {
		t.Fatal("should not throttle when interval is 0")
	}

	// Give up when the interval significantly exceeds the remaining context time, rather than hanging the request.
	gate.setInterval(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if gate.wait(ctx) {
		t.Fatal("should abort current claim after context cancellation")
	}

	// Short intervals should wait until the target time and then proceed.
	gate.setInterval(60 * time.Millisecond)
	gate.markIssued()
	start := time.Now()
	if !gate.wait(context.Background()) {
		t.Fatal("short interval should wait before proceeding")
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("actual wait %s, shorter than throttle interval", waited)
	}
}

// TestClaimGateIsPerInstance locks down "throttle windows belong to provider instances".
//
// This regresses a pre-refactor flaw: intervals and timers were package-level variables, causing one App's claim
// to block another App's claim, conflicting with the instance isolation declared in the README.
func TestClaimGateIsPerInstance(t *testing.T) {
	busy := newClaimGate()
	busy.setInterval(time.Hour)
	busy.markIssued()

	idle := newClaimGate()
	idle.setInterval(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if !idle.wait(ctx) {
		t.Fatal("another instance should not be blocked by this instance's claim throttle")
	}
	if busy.wait(ctx) {
		t.Fatal("this instance just sent a claim, should throttle")
	}
}

// Pure function tests for active reporting (offline).
//
// Covers three issues that fail silently and manifest as "activity list always empty":
//   - Event body fields must match official sendReport (failing fields may cause upstream to ignore silently);
//   - Report once per account per day (duplicate reports yield no benefit and only spam upstream);
//   - Dates are calculated based on the account profile timezone (synonymous with official reportAppDailyActive's toLocalDateKey).

func TestActivationEventBodyMatchesOfficialFields(t *testing.T) {
	cred := &Credential{
		JWTToken: "jwt",
		Profile: DeviceProfile{
			Platform: "win32", Arch: "x64", OSVersion: "10.0.22631",
			Language: "zh-CN", Timezone: "Asia/Shanghai",
			Screen: "1920x1080", DeviceMID: "mid-1",
		},
	}
	body := activationEventBody("app_launch", cred, "user-1")

	// All 16 keys of official sendReport are mandatory (extra ones won't be rejected, missing ones may be ignored).
	want := []string{
		"event_id", "client_timezone", "client_language", "element_name",
		"event_region", "event_type", "event_text", "event_extra_detail",
		"user_id", "screen_resolution", "app_version", "device_os_category",
		"device_os_version", "device_mid", "mac_id", "marketing_params",
	}
	for _, key := range want {
		if _, ok := body[key]; !ok {
			t.Fatalf("event body missing field %q: %+v", key, body)
		}
	}
	if len(body) != len(want) {
		t.Fatalf("event body field count = %d, want %d: %+v", len(body), len(want), body)
	}

	if body["element_name"] != "app_launch" || body["event_region"] != "app" || body["event_type"] != "view" {
		t.Fatalf("event semantic field error: %+v", body)
	}
	// Device fields must be retrieved from the account profile, otherwise multiple accounts will merge into the same machine.
	if body["device_mid"] != "mid-1" || body["device_os_category"] != "windows" ||
		body["screen_resolution"] != "1920x1080" || body["client_timezone"] != "Asia/Shanghai" {
		t.Fatalf("device field not taken from account profile: %+v", body)
	}
	if body["marketing_params"] != "{}" || body["mac_id"] != "" {
		t.Fatalf("fixed field error: %+v", body)
	}
	if body["event_id"] == "" || body["event_id"] == activationEventBody("app_launch", cred, "user-1")["event_id"] {
		t.Fatal("event_id must be newly generated each time")
	}
}

func TestActivationElementsAreOfficialPair(t *testing.T) {
	// Both are indispensable: app_launch is for startup, app_daily_active is for daily active.
	want := []string{"app_launch", "app_daily_active"}
	if len(activationElements) != len(want) {
		t.Fatalf("activationElements = %v", activationElements)
	}
	for index, element := range want {
		if activationElements[index] != element {
			t.Fatalf("activationElements[%d] = %q, want %q", index, activationElements[index], element)
		}
	}
}

func TestActivationStateDedupesPerUserPerDay(t *testing.T) {
	state := newActivationState()
	if !state.shouldReport("u1", "2026-10-23") {
		t.Fatal("first report should proceed")
	}
	if state.shouldReport("u1", "2026-10-23") {
		t.Fatal("same account should not report repeatedly on the same day")
	}
	// Accounts do not affect each other.
	if !state.shouldReport("u2", "2026-10-23") {
		t.Fatal("different accounts should report independently")
	}
	// Re-allow access across days.
	if !state.shouldReport("u1", "2026-10-24") {
		t.Fatal("should report again across days")
	}
}

func TestActivationStateForgetsOnFailure(t *testing.T) {
	state := newActivationState()
	state.shouldReport("u1", "2026-10-23")
	// Reporting failure must revoke the placeholder, otherwise the campaign is locked out for the entire calendar day.
	state.forget("u1")
	if !state.shouldReport("u1", "2026-10-23") {
		t.Fatal("retry should be allowed after failure")
	}
}

func TestLocalDateFollowsProfileTimezone(t *testing.T) {
	// 2026-10-23T20:00:00Z: Shanghai is already Oct 24, New York is still Oct 23.
	at := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	if got := localDate(DeviceProfile{Timezone: "Asia/Shanghai"}, at); got != "2026-10-24" {
		t.Fatalf("Shanghai date = %q, want 2026-10-24", got)
	}
	if got := localDate(DeviceProfile{Timezone: "America/New_York"}, at); got != "2026-10-23" {
		t.Fatalf("New York date = %q, want 2026-10-23", got)
	}
	// Fall back to UTC when the timezone is missing or invalid, instead of panicking or returning an empty string.
	if got := localDate(DeviceProfile{}, at); got != "2026-10-23" {
		t.Fatalf("timezone-free date = %q, want 2026-10-23", got)
	}
	if got := localDate(DeviceProfile{Timezone: "Not/AZone"}, at); got != "2026-10-23" {
		t.Fatalf("invalid timezone date = %q, want 2026-10-23", got)
	}
}

func TestEnsureActivationExplainsMissingUserID(t *testing.T) {
	// Do not report when user_id is missing, but a reason must be given: otherwise users will only see an empty campaign list forever.
	v := Instances(core.Env{})[0].(*adapter)
	reason := v.ensureActivation(context.Background(), &Credential{JWTToken: "not-a-jwt"})
	if reason == "" {
		t.Fatal("must return a reason when user_id is missing")
	}
	if reason := v.ensureActivation(context.Background(), nil); reason != "" {
		t.Fatalf("nil credential should be silent, got %q", reason)
	}
}

func TestMarketingHeadersCarryCredential(t *testing.T) {
	// JWT priority: when a credential has both JWT and API Key, only the JWT Bearer can be sent.
	h := marketingHeaders(&Credential{
		JWTToken: "jwt-token",
		APIKey:   "sk-test",
		Profile:  DeviceProfile{Platform: "win32", Arch: "x64", Language: "en-US"},
	})
	if h["Authorization"] != "Bearer jwt-token" || h["x-api-key"] != "" {
		t.Fatalf("JWT headers = %#v", h)
	}
	if h["Accept"] != "application/json" || h["x-request-id"] == "" {
		t.Fatalf("missing request headers: %#v", h)
	}
	// Language must come from the account profile, rather than being overwritten by hardcoded zh-CN.
	if got := h["X-Client-Language"]; got != "en-US" {
		t.Fatalf("X-Client-Language = %q, want en-US", got)
	}

	// Pure API Key credentials (added via the console) must carry x-api-key, otherwise the upstream treats them as anonymous requests.
	h = marketingHeaders(&Credential{APIKey: "sk-test", Profile: DeviceProfile{Platform: "win32", Arch: "x64"}})
	if h["x-api-key"] != "sk-test" || h["Authorization"] != "" {
		t.Fatalf("API key headers = %#v", h)
	}
}

func TestParseMarketingTouchNormalizesPlans(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"deliveries":[
		{"banner":{"background":{"args":{"zcode_plan":{"plan_id":"low","name":"Low","priority":1,"entitlements":[{"show_name":"GLM-5.3-Flash","grant_units":100,"period":"one_time"}]}}}}},
		{"banner":{"background":{"args":{"zcode_plan":{"plan_id":"high","name":"High","priority":9}}}}},
		{"banner":{"background":{"args":{"zcode_plan":{"plan_id":"HIGH","name":"High 重复","priority":9}}}}},
		{"banner":{"background":{"args":{"button":{"text":"learn more"}}}}},
		{"banner":{"background":{"args":{"zcode_plan":{"name":"没有 plan_id"}}}}}
	]}}`)

	plans, err := parseMarketingTouch(http.StatusOK, raw)
	if err != nil {
		t.Fatalf("parseMarketingTouch: %v", err)
	}
	// Deliveries with missing plan_id and duplicate plan_ids (different cases) must both be discarded.
	if len(plans) != 2 {
		t.Fatalf("plans = %+v, want 2", plans)
	}
	// Descending priority order, with ascending plan_id for equal priorities.
	if plans[0].PlanID != "high" || plans[1].PlanID != "low" {
		t.Fatalf("order = %q, %q", plans[0].PlanID, plans[1].PlanID)
	}
	if len(plans[1].Grants) != 1 || plans[1].Grants[0].Model != "GLM-5.3-Flash" {
		t.Fatalf("grants = %+v", plans[1].Grants)
	}
}

func TestParseMarketingTouchErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantDetail string
	}{
		{
			name:       "业务码非零",
			status:     http.StatusOK,
			body:       `{"code":4001,"msg":"today not active"}`,
			wantCode:   "zcode_activity_business",
			wantDetail: "today not active",
		},
		{
			name:       "HTTP 非 2xx",
			status:     http.StatusUnauthorized,
			body:       `{"code":401,"msg":"unauthorized"}`,
			wantCode:   "zcode_activity_http",
			wantDetail: "HTTP 401",
		},
		{
			name:       "响应不是 JSON",
			status:     http.StatusOK,
			body:       `<html>login</html>`,
			wantCode:   "zcode_activity_response",
			wantDetail: "login",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseMarketingTouch(test.status, []byte(test.body))
			var pluginErr *core.PluginError
			if !errors.As(err, &pluginErr) {
				t.Fatalf("err = %v, want *core.PluginError", err)
			}
			if pluginErr.Code != test.wantCode || pluginErr.HTTPStatus != http.StatusBadGateway {
				t.Fatalf("err = %+v", pluginErr)
			}
			if !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("err = %q, want detail %q", err, test.wantDetail)
			}
		})
	}
}

// ---- billing/balance quota conversion ----

// balanceFixture returns the measured billing/balance clipped response from 2026-10-25 (field structure matches).
// Returns a new copy each time to avoid sharing mutable package-level state between test cases.
func balanceFixture() map[string]any {
	return map[string]any{
		"data": map[string]any{
			"server_time": float64(1792900000),
			"plans": []any{
				map[string]any{
					"plan_id": "zcode-v3-start-plan-1024",
					"name":    "ZCode 1024 Dev Day",
					"status":  "active",
					"ends_at": float64(1792976400),
				},
				map[string]any{
					"plan_id": "zcode-basic",
					"name":    "Basic",
					"status":  "",
					"ends_at": float64(1795219200),
				},
				map[string]any{
					"plan_id": "zcode-expired",
					"name":    "Expired",
					"status":  "active",
					"ends_at": float64(1792800000),
				},
			},
			"balances": []any{
				map[string]any{
					"show_name":       "GLM-5.3-Flash",
					"total_units":     float64(300000000),
					"used_units":      float64(169395514),
					"remaining_units": float64(130604486),
					"expires_at":      float64(1792976400),
				},
				map[string]any{
					"show_name":       "GLM-5.3",
					"total_units":     float64(200000000),
					"used_units":      float64(15638817),
					"remaining_units": float64(184361183),
					"expires_at":      float64(1792976400),
				},
			},
		},
	}
}

// balanceRowsFixture gets the quota bucket entries from the measured fixture (balanceQuota only takes entries).
func balanceRowsFixture() []map[string]any {
	return objectRows(dataMap(balanceFixture())["balances"])
}

// metricValue gets the metric value for the specified key in the summary.
func metricValue(metrics []pluginapi.QuotaMetric, key string) (float64, bool) {
	for _, metric := range metrics {
		if metric.Key == key {
			return metric.Value, true
		}
	}
	return 0, false
}

// TestBalanceQuotaExposeTotalsFamily verifies the default family summary: the account table's
// "remaining / total" column relies on them to calculate ratios, while per-model metrics keep independent keys.
func TestBalanceQuotaExposeTotalsFamily(t *testing.T) {
	metrics, groups := balanceQuota(balanceRowsFixture())
	for key, want := range map[string]float64{
		"remaining": 130604486 + 184361183,
		"total":     500000000,
		"used":      169395514 + 15638817,
	} {
		if got, ok := metricValue(metrics, key); !ok || got != want {
			t.Fatalf("metric %q = %v (found %v), want %v", key, got, ok, want)
		}
	}
	// Model quota group: one per bucket, complete with scores and expiration times.
	if len(groups) != 1 || len(groups[0].Buckets) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	first := groups[0].Buckets[0]
	want := 130604486.0 / 300000000.0
	if first.RemainingFraction < want-0.0001 || first.RemainingFraction > want+0.0001 {
		t.Fatalf("fraction = %v, want ~%v", first.RemainingFraction, want)
	}
	if first.ResetTime != "2026-10-26T01:00:00Z" {
		t.Fatalf("ResetTime = %q, want 2026-10-26T01:00:00Z (UTC)", first.ResetTime)
	}
}

// TestBalanceQuotaEdgeCases covers edge cases: empty balances, total=0, out-of-bounds score clamping,
// and ensuring no fake summary is given when all entries fail to parse.
func TestBalanceQuotaEdgeCases(t *testing.T) {
	if m, g := balanceQuota(nil); m != nil || g != nil {
		t.Fatalf("empty balances must yield nil/nil, got %+v %+v", m, g)
	}

	// total=0: scores are displayed as "has balance = full", and remaining is still counted in the summary.
	metrics, groups := balanceQuota([]map[string]any{
		{"show_name": "X", "total_units": float64(0), "remaining_units": float64(5)},
	})
	if got, ok := metricValue(metrics, "remaining"); !ok || got != 5 {
		t.Fatalf("total=0 metrics = %+v", metrics)
	}
	if got := groups[0].Buckets[0].RemainingFraction; got != 1 {
		t.Fatalf("total=0 fraction = %v, want 1 (unknown total but positive remaining)", got)
	}

	// remaining exceeds total: clamped to 1.
	_, groups = balanceQuota([]map[string]any{
		{"show_name": "X", "total_units": float64(10), "remaining_units": float64(50)},
	})
	if groups[0].Buckets[0].RemainingFraction != 1 {
		t.Fatalf("clamp fraction = %v", groups[0].Buckets[0].RemainingFraction)
	}

	// All entries missing show_name: do not give a fake summary of "remaining 0".
	metrics, _ = balanceQuota([]map[string]any{
		{"total_units": float64(10)},
	})
	if metrics != nil {
		t.Fatalf("unparsable entries must yield nil metrics, got %+v", metrics)
	}
}

// TestPlanExpiryGroupNearestActive verifies validity rules: missing status is treated as active,
// active but expired ends_at is excluded, multiple plans pick the nearest unexpired; expiration uses Description.
func TestPlanExpiryGroupNearestActive(t *testing.T) {
	body := balanceFixture()
	plans := planRows(body)
	serverTime := serverSeconds(body)
	group := planExpiryGroup(plans, serverTime)
	if group == nil {
		t.Fatal("expiry group missing")
	}
	bucket := group.Buckets[0]
	// The nearest unexpired one is 1792976400 (1024 gift pack), not the later basic (missing status also counts as active).
	if bucket.ResetTime != "" {
		t.Fatalf("ResetTime must stay empty for one-time plans, got %q", bucket.ResetTime)
	}
	if bucket.RemainingFraction != 0 {
		t.Fatalf("expiry bucket must not render a progress bar, got %v", bucket.RemainingFraction)
	}
	want := "Plan expires 2026-10-26T01:00:00Z"
	if bucket.Description != want {
		t.Fatalf("Description = %q, want %q", bucket.Description, want)
	}
}

// TestActivePlansSharedSemantics verifies that the shared semantics of activePlans are
// consumed consistently by heldPlanIDs and planExpiryGroup.
func TestActivePlansSharedSemantics(t *testing.T) {
	plans := planRows(balanceFixture())
	active := activePlans(plans, serverSeconds(balanceFixture()))
	if len(active) != 2 {
		t.Fatalf("active plans = %d, want 2 (1024 gift package + empty status basic; expired items excluded)", len(active))
	}
	held := heldPlanIDs(plans, serverSeconds(balanceFixture()))
	if len(held) != 2 || held["zcode-expired"] {
		t.Fatalf("held = %+v", held)
	}
}

// TestSubscriptionPlanSelection verifies subscription name selection: prefer plans expiring concurrently with the quota bucket,
// fall back to the earliest active plan when no correlation evidence exists, and keep the default name when no active plans exist.
//
// Array order is untrustworthy: when "promotional gift pack + basic plan" coexist, the first active is not necessarily
// the one the account is currently consuming, and displaying the wrong plan name would cause users to use models with incorrect quota expectations.
func TestSubscriptionPlanSelection(t *testing.T) {
	const serverTime = 1792900000
	matched := []map[string]any{
		{"plan_id": "promo", "name": "Promo", "status": "active", "ends_at": float64(1800000000)},
		{"plan_id": "in-use", "name": "In Use", "status": "active", "ends_at": float64(1792976400)},
		{"plan_id": "pending", "name": "Pending", "status": "pending", "ends_at": float64(1792976400)},
	}
	ended := []map[string]any{
		{"plan_id": "ended", "name": "Ended", "status": "active", "ends_at": float64(1792800000)},
		{"plan_id": "running", "name": "Running", "status": "", "ends_at": float64(1800000000)},
	}
	tests := []struct {
		name     string
		plans    []map[string]any
		balances []map[string]any
		wantPlan string
		wantTier string
	}{
		{
			name:  "按桶到期时刻命中",
			plans: matched,
			// Both buckets belong to the 1024 gift pack; the promo expires later but has no buckets pointing to it.
			balances: []map[string]any{
				{"show_name": "GLM-5.3", "expires_at": float64(1792976400)},
				{"show_name": "GLM-5.3-Flash", "expires_at": float64(1792976400)},
			},
			wantPlan: "In Use", wantTier: "in-use",
		},
		{
			name:     "桶到期对不上时退回第一个生效套餐",
			plans:    ended,
			balances: []map[string]any{{"show_name": "X", "expires_at": float64(9999)}},
			wantPlan: "Running", wantTier: "running",
		},
		{
			name:     "没有额度桶时退回第一个生效套餐",
			plans:    ended,
			wantPlan: "Running", wantTier: "running",
		},
		{
			name: "没有生效套餐时保留默认订阅名",
			plans: []map[string]any{
				{"plan_id": "gone", "name": "Gone", "status": "expired", "ends_at": float64(1000)},
			},
			balances: []map[string]any{{"show_name": "X"}},
			wantPlan: "ZCode Coding Plan", wantTier: "Standard",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{
				Plan: "ZCode Coding Plan", TierName: "Standard", TierID: "zcode",
			}}
			applySubscription(planSnapshot{
				plans: test.plans, balances: test.balances, serverTime: serverTime,
			}, response)
			if response.Subscription.Plan != test.wantPlan || response.Subscription.TierName != test.wantTier {
				t.Fatalf("subscription = %q / %q, want %q / %q",
					response.Subscription.Plan, response.Subscription.TierName, test.wantPlan, test.wantTier)
			}
		})
	}
}

// TestBillingEnvelopeComplete distinguishes between "upstream didn't provide the balances array (revision/truncation)" and
// "the account genuinely has no quota buckets (empty array)". The former must error out, while the latter is a valid answer.
func TestBillingEnvelopeComplete(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"真空账号：数组齐全但为空", map[string]any{"data": map[string]any{
			"plans": []any{}, "balances": []any{},
		}}, true},
		{"缺 balances", map[string]any{"data": map[string]any{"plans": []any{}}}, false},
		{"balances 不是数组", map[string]any{"data": map[string]any{"balances": "oops"}}, false},
		{"缺 data", map[string]any{"code": codeSuccess}, false},
		{"空响应", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := billingEnvelopeComplete(test.body); got != test.want {
				t.Fatalf("billingEnvelopeComplete = %v, want %v", got, test.want)
			}
		})
	}
}

// TestActivePlansNormalizesMillisecondTimestamps verifies that 13-bit millisecond timestamps do not cause
// an entire batch of active plans to be misjudged as expired: when upstream mixes seconds and milliseconds, normalization must occur before comparison.
func TestActivePlansNormalizesMillisecondTimestamps(t *testing.T) {
	plans := []map[string]any{
		{"plan_id": "ms", "status": "active", "ends_at": float64(1792976400000)},
		{"plan_id": "s", "status": "active", "ends_at": float64(1800000000)},
	}
	if active := activePlans(plans, 1792900000); len(active) != 2 {
		t.Fatalf("active plans = %+v, want both entries", active)
	}
	body := map[string]any{"data": map[string]any{"server_time": float64(1792900000000)}}
	if got := serverSeconds(body); got != 1792900000 {
		t.Fatalf("serverSeconds = %d, want 1792900000", got)
	}
}
