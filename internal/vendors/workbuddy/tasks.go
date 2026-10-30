package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
)

// This file implements the growth task list, registration, and reward claiming APIs.
//
// The structural differences among the three paths are the biggest pitfalls here:
//   - List and registration are in the chat domain (chatBase) with business envelopes;
//   - **Reward claiming is in the Web domain** (webBase), with task_code in the **path**, no body, and x-client-platform: web.
//     The chat domain's /tasks/reward/claim path **does not exist** and always returns 400 "task not completed",
//     which is the real reason why early reward claiming attempts all failed.
//   - Mini-program task codes must use the same-name API with X-Client-Platform: miniprogram.

// This file implements helper methods that require real chat interactions.
//
// Some tasks (expert usage, skill tasting, night owl, desktop school season) have criteria requiring
// **server-generated requestId from real conversations** -- self-created UUIDs will not be scored.
// Therefore, these methods send a real conversation and extract the upstream-returned ID from the SSE stream.

// This file implements growth system event APIs: consecutive logins, travel, night owls, school season, gift packs, and compensation.
//
// These APIs are all attached to the chat or billing domains with varying structures (envelope nesting, idempotency codes, business error status representation),
// so they are encapsulated into methods one by one, allowing upper-level logic to focus solely on business semantics.

// Streak is the consecutive login status.
type Streak struct {
	// Days is the number of consecutive login days.
	Days int64
	// MakeupCards is the remaining makeup check-in card balance.
	MakeupCards int64
	// Tiers are the redemption tiers.
	Tiers []StreakTier
}

// StreakTier is a redemption tier.
type StreakTier struct {
	// Tier is the tier identifier (7d / 14d / 28d).
	Tier string
	// Status is the status: locked / claimed / empty (redeemable).
	Status string
}

// cstZone is the timezone used by the upstream calendar day (fixed UTC+8, independent of system tzdata).
var cstZone = time.FixedZone("CST", 8*3600)

// ReportDesktopEvents reports events using the desktop fingerprint family (chat domain /v2/report).
func (c *Client) ReportDesktopEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(cred.Realm())
	return c.postReport(ctx, cred, endpoints.ChatBase+reportPath, events, func(req *http.Request) {
		c.applyDesktopHeaders(req, cred)
	})
}

// postReport is the common reporting export shared by all fingerprint families.
func (c *Client) postReport(ctx context.Context, cred *Credential, url string, events []map[string]any, decorate func(*http.Request)) error {
	// body must be an array: upstream parses by array, and a single object will be silently discarded.
	encoded, errEncode := json.Marshal(events)
	if errEncode != nil {
		return fmt.Errorf("encode report events: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return fmt.Errorf("build report request: %w", errReq)
	}
	decorate(req)

	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return fmt.Errorf("report request: %w", errDo)
	}
	defer closeBody(resp.Body)

	body := make([]byte, 0, 1024)
	buffer := make([]byte, 1024)
	for {
		read, errRead := resp.Body.Read(buffer)
		if read > 0 {
			body = append(body, buffer[:read]...)
			if len(body) > apiBodyLimit {
				break
			}
		}
		if errRead != nil {
			break
		}
	}
	if resp.StatusCode >= 400 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	}
	// The reporting API also returns success for "already reported", so business codes are not checked.
	return nil
}

// Report default model (upstream does not strictly validate models for activity reporting, a generic value suffices).
const ()

// This file is WorkBuddy's task center: action table, scanning, execution, school season, and scheduled task families.
//
// These rules are entirely determined by CodeBuddy upstream's event system (task codes, event chains, scoring windows),
// so they remain within the provider package; the root layer only handles account traversal, concurrency queues, and HTTP responses.

const ()

// ---- Task Scanning ----

// ---- Task Execution ----

// ---- School Season Status ----

// ---- Scheduled Task Families ----

// ---- Action Implementation ----

// This file implements the "daily active reward" for Global accounts, i.e., Global check-in.
//
// Background (conclusions from actual testing of the codebuddy-intl-cpa project, 2026-09-28 -> 09-29,
// cross-validated across 7 accounts): Global does not have the CN version's "gas station" style daily check-in --
// the check-in event status API always returns active:false for Global, and the daily-checkin endpoint does not credit rewards.
// Global's daily earnings path is the **activity beacon**: the official client sends requests to every conversation with
// POST /v2/report sends a chat_request_send event. Today's growth heatmap
// (GET /activity/growth/heatmap) with score > 0 is considered "lit", and early the next morning
// (China timezone) automatically grants a 30-point Bonus Pack (platform reward points, valid for 30 days);
// continuous lighting can also unlock streak tiers (14d +50, 28d +150).
//
// Design highlights (consistent with the reference implementation):
//   - Idempotent: uses the last cell of the heatmap as the API's own "today", and if lit, does not report again,
//     so it does not rely on the local timezone for natural day determination;
//   - Beacon non-retry: events are idempotent by day, but blind resending doubles the daily count (score 2 -> 4);
//     single failures are left for the next scheduled point to handle;
//   - Multiple scheduled points per day: already lit points only cost one heatmap read, with almost zero cost.

// This file implements four closed loops: streak redemption and lottery, cat travel, night owl, and back-to-school season.
//
// Common feature: all attached "after check-in" or executed at independent top of the hour, and all are idempotent by day.

// Streak status types (Streak / StreakTier) are defined in activity.go -- they are upstream interface
// return structures belonging to the protocol layer; this file only consumes them. Merging the two previous definitions
// of the same name and shape was cross-package duplication and has been deduplicated.

// Back-to-school season expert (fixed expert built into the activity).
const ()

// Balance is the resource balance of the account.
//
// Quantities use float64: personal resource pack quotas are integers, but enterprise usage interface credits are decimals
// (measured 2.59 / 3.87 / 4.11). Using integers truncates decimals, and when consuming 0.9 points, the dashboard
// shows no change at all.
type Balance struct {
	// Remain is the remaining points.
	Remain float64
	// Total is the total quota for this period.
	Total float64
	// Enterprise indicates this balance comes from the enterprise usage interface (not a personal resource pack).
	Enterprise bool
	// Packages are plan details.
	Packages []BalancePackage
}

// BalancePackage is the balance of a single resource pack.
type BalancePackage struct {
	// Name is the resource pack name.
	Name string
	// Remain is the remaining amount.
	Remain float64
	// Total is the total amount.
	Total float64
	// ExpireAt is the expiration time (Unix seconds, 0 means no expiration).
	ExpireAt int64
	// ResetAt is the cycle reset time (Unix seconds, 0 means no cycle).
	//
	// Deliberately separated from ExpireAt: enterprise quotas arriving at the point **reset**, they do not expire.
	// Mixing them would cause the dashboard to display "next month's reset" as "expiring soon".
	ResetAt int64
	// Period is the cycle text (e.g., "2026-10-01 ~ 2026-10-31"), empty means not displayed.
	Period string
}

// billingResourceAccount is a single plan in the balance response.
//
// Fields are **upstream original PascalCase** (the upstream interface does not use camelCase).
type billingResourceAccount struct {
	PackageName         string `json:"PackageName"`
	CycleEndTime        string `json:"CycleEndTime"` // "2006-01-02 15:04:05", empty = no expiration
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

// billingResourceResponse is the response structure of the balance interface.
//
// Double-layer envelope: Response.Data.Accounts. The request body must also include pagination and product code,
// otherwise upstream returns an empty list (this point was missed in the early implementation, causing the balance to always be 0).
type billingResourceResponse struct {
	Response struct {
		Data struct {
			Accounts []billingResourceAccount `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// packageRemainUsed aggregates the remain/used/size of a single package.
//
// Data extraction rules (copied from upstream single source of truth, do not change):
//   - Cycle plans take precedence (CycleCapacitySize > 0): use the three Cycle fields,
//     clamp remain to [0, size] first; used takes the maximum of CycleUsed and size-remain,
//     and when used is larger, back-calculate remain = size - used;
//   - Otherwise fall back to the three Capacity fields, where used is 0 but if size > remain, supplement used by the difference.
func packageRemainUsed(account billingResourceAccount) (remain, used, size int64) {
	if account.CycleCapacitySize > 0 {
		remain = account.CycleCapacityRemain
		size = account.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if account.CycleCapacityUsed > used {
			used = account.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = account.CapacityRemain
	used = account.CapacityUsed
	size = account.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// FetchBalance queries the account balance.
//
// The personal resource interface only covers personal plans; enterprise account quotas are handled in resolveBalance.
func (c *Client) FetchBalance(cred *Credential) (*Balance, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	// The request body has the fixed shape required by upstream: missing ProductCode or pagination will yield an empty list.
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}

	data, errCall := c.billingMeterJSON(cred, billingMeterPaths(region, billingMeterPath, billingMeterPathV2), body)
	if errCall != nil {
		return nil, errCall
	}
	var payload billingResourceResponse
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode balance: %w", errUnmarshal)
	}
	return c.resolveBalance(cred, region, parseBalance(payload))
}

// billingMeterPaths returns candidate paths for a billing interface.
//
// Global tries paths without /v2 first, falling back to paths with /v2 on 404; CN only has paths with /v2.
// This difference stems from the history of two upstream deployments, not a typo.
func billingMeterPaths(region Region, plain, v2 string) []string {
	if region.IsGlobal() {
		return []string{plain, v2}
	}
	return []string{v2}
}

// billingMeterJSON tries candidate paths sequentially, returning the data unwrapped from the first successful response.
//
// Switch paths only on 404: other errors cannot be rescued by switching paths.
func (c *Client) billingMeterJSON(cred *Credential, paths []string, body any) (json.RawMessage, error) {
	endpoints := GetEndpoints(cred.Realm())
	lastErr := fmt.Errorf("billing endpoint is unavailable")
	for _, path := range paths {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, http.MethodPost, body)
		if errCall == nil {
			return data, nil
		}
		lastErr = errCall
		if upstreamErr, okErr := errCall.(*core.Failure); !okErr || upstreamErr.Kind != core.FailureKindNotFound {
			return nil, errCall
		}
	}
	return nil, lastErr
}

// parseBalance normalizes the balance response.
//
// remain/total are the **sums** across all plans (users care about total account capacity, not individual plans).
func parseBalance(payload billingResourceResponse) *Balance {
	balance := &Balance{}
	for _, account := range payload.Response.Data.Accounts {
		remain, _, size := packageRemainUsed(account)
		balance.Remain += float64(remain)
		balance.Total += float64(size)
		balance.Packages = append(balance.Packages, BalancePackage{
			Name:     strings.TrimSpace(account.PackageName),
			Remain:   float64(remain),
			Total:    float64(size),
			ExpireAt: parsePackageExpiry(account.CycleEndTime),
		})
	}
	return balance
}

// resolveBalance switches to the enterprise usage interface when an enterprise account receives empty personal quotas.
//
// An enterprise account requesting the personal interface receives HTTP 200 + empty Accounts (not an error),
// which displays directly as 0 / 0, leading the operator to mistakenly think the account is exhausted. Enterprise quotas can only
// be provided by the enterprise endpoint, so it falls back here after determining "personal has no quota + enterprise tenant id exists".
// Non-enterprise accounts maintain original behavior: empty Accounts genuinely means no quota.
func (c *Client) resolveBalance(cred *Credential, region Region, personal *Balance) (*Balance, error) {
	if personal.Total > 0 {
		return personal, nil
	}
	if strings.TrimSpace(cred.EnterpriseIDValue()) == "" {
		return personal, nil
	}
	return c.fetchEnterpriseBalance(cred, region)
}

// enterpriseUsageResponse is the response of the enterprise usage interface.
//
// credit is **consumed in this cycle** (fractional points), limitNum is the cycle quota, and remaining = difference between the two.
// cycleEndTime / cycleResetTime are merely cycle refresh moments, not resource pack expirations.
type enterpriseUsageResponse struct {
	Credit         *float64 `json:"credit"`
	LimitNum       *float64 `json:"limitNum"`
	CycleStartTime string   `json:"cycleStartTime"`
	CycleEndTime   string   `json:"cycleEndTime"`
	CycleResetTime string   `json:"cycleResetTime"`
}

// fetchEnterpriseBalance synthesizes a Balance using the enterprise usage interface.
//
// The request body uses an empty object: this endpoint locates by enterprise tenant in the token, requiring no pagination or product code;
// the mandatory fields of the personal resource package set do not apply here.
func (c *Client) fetchEnterpriseBalance(cred *Credential, region Region) (*Balance, error) {
	data, errCall := c.billingMeterJSON(cred,
		billingMeterPaths(region, enterpriseUsagePath, enterpriseUsagePathV2), map[string]any{})
	if errCall != nil {
		return nil, errCall
	}
	return parseEnterpriseBalance(data)
}

// parseEnterpriseBalance normalizes the enterprise usage response.
//
// Missing limitNum is considered a contract violation: treating it as 0 would repeat the already reported false impression of "0 / 0".
func parseEnterpriseBalance(data json.RawMessage) (*Balance, error) {
	var payload enterpriseUsageResponse
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode enterprise balance: %w", errUnmarshal)
	}
	if payload.LimitNum == nil {
		return nil, fmt.Errorf("decode enterprise balance: missing limitNum")
	}
	total := *payload.LimitNum
	used := 0.0
	if payload.Credit != nil {
		used = *payload.Credit
	}
	remain := total - used
	if remain < 0 {
		remain = 0
	}
	if remain > total {
		remain = total
	}
	balance := &Balance{Remain: remain, Total: total, Enterprise: true}
	period := ""
	start, end := strings.TrimSpace(payload.CycleStartTime), strings.TrimSpace(payload.CycleEndTime)
	if start != "" && end != "" {
		period = start + " ~ " + end
	}
	balance.Packages = append(balance.Packages, BalancePackage{
		Name:    "Enterprise Quota",
		Remain:  remain,
		Total:   total,
		ResetAt: parsePackageExpiry(core.FirstNonEmpty(payload.CycleResetTime, payload.CycleEndTime)),
		Period:  period,
	})
	return balance, nil
}

// parsePackageExpiry parses the plan expiration moment (UTC+8 wall clock); returns 0 on parse failure.
func parsePackageExpiry(raw string) int64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0
	}
	parsed, errParse := time.ParseInLocation(packageEndLayout, trimmed, cstZone)
	if errParse != nil {
		return 0
	}
	return parsed.Unix()
}

// billingJSON initiates a billing domain request and unwraps the envelope.
func (c *Client) billingJSON(cred *Credential, url, method string, body any) (json.RawMessage, error) {
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return nil, fmt.Errorf("encode request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(c.opts.Context, method, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return nil, fmt.Errorf("build request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	return c.doJSON(req)
}

// FetchQuota aggregates account quota information (for host management console display).
func (c *Client) FetchQuota(cred *Credential) (*Balance, error) {
	return c.FetchBalance(cred)
}

// BuildQuotaResponse converts account quotas into the host's normalized quota structure.
//
// Structure hierarchy: Summary holds key figures (read directly by the host's quota card),
// Groups/Buckets hold details (viewed upon expansion).
func BuildQuotaResponse(credential *Credential, balance *Balance) pluginapi.QuotaFetchResponse {
	region := credential.Realm()
	plan := string(region)
	planName := RealmDisplayName(region)
	if balance.Enterprise {
		// Enterprise quota and personal plans are two isolated quota lines and cannot be merged into the same plan name.
		plan = "enterprise"
		planName = "Enterprise Quota"
	}
	response := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     plan,
			TierName: planName,
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "Remaining Points", Value: balance.Remain, Format: "number"},
			{Key: "credit_total", Label: "Total Quota", Value: balance.Total, Format: "number"},
		},
	}
	if balance.Total > 0 {
		used := balance.Total - balance.Remain
		if used < 0 {
			used = 0
		}
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "credit_used", Label: "Points Used", Value: used, Format: "number"})
	}
	if len(balance.Packages) > 0 {
		buckets := make([]pluginapi.QuotaBucket, 0, len(balance.Packages))
		for _, pkg := range balance.Packages {
			fraction := 0.0
			if pkg.Total > 0 {
				fraction = pkg.Remain / pkg.Total
			}
			description := formatCreditAmount(pkg.Remain) + " / " + formatCreditAmount(pkg.Total)
			if pkg.Period != "" {
				description += " · Period " + pkg.Period
			}
			bucket := pluginapi.QuotaBucket{
				Window:            pkg.Name,
				RemainingFraction: fraction,
				Description:       description,
			}
			// Expiration (revocation) and replenishment (period reset) are fetched separately: enterprise quotas replenish at specific points,
			// and should not be displayed as "Expiring soon".
			if pkg.ExpireAt > 0 {
				bucket.ResetTime = FormatUnixTime(pkg.ExpireAt)
			} else if pkg.ResetAt > 0 {
				bucket.ResetTime = FormatUnixTime(pkg.ResetAt)
			}
			buckets = append(buckets, bucket)
		}
		response.Groups = []pluginapi.QuotaGroup{{
			DisplayName: "Resource Pack Details",
			Buckets:     buckets,
		}}
	}
	return response
}

// formatCreditAmount formats the credit amount into readable text.
//
// Enterprise quotas are decimals (observed 4.11 / 2.59), while personal quotas are integers: two decimal places are enough
// to express the former without rendering the latter as "2000.00", while hiding long tails from float operations.
func formatCreditAmount(value float64) string {
	return strconv.FormatFloat(math.Round(value*100)/100, 'f', -1, 64)
}

// FormatUnixTime formats Unix seconds to RFC3339 (returns empty string for invalid values).
func FormatUnixTime(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}
