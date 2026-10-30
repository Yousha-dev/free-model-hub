package auth

import (
	"testing"

	"free-model-hub/cpasdk/pluginapi"
)

// TestTotalsRecognizesEveryVendorKeyFamily locks the key noun table for "remaining / total".
//
// Totals only recognizes the whitelist. If a provider uses other key names, it will **silently** get 0/0, and the console will keep showing
// 0 / 0. This pins down all naming families currently used by various providers in the repository. If a new provider uses another
// set of names, it should fail here first rather than appearing as an empty progress bar after deployment.
func TestTotalsRecognizesEveryVendorKeyFamily(t *testing.T) {
	cases := []struct {
		name    string
		summary []pluginapi.QuotaMetric
		want    QuotaTotals
	}{
		{
			name: "标准键（CodeArts / MiniMax / Trae SOLO 早期）",
			summary: []pluginapi.QuotaMetric{
				{Key: "remaining", Value: 80},
				{Key: "total", Value: 100},
				{Key: "used", Value: 20},
			},
			want: QuotaTotals{Remaining: 80, Total: 100},
		},
		{
			name: "credit_* 族（WorkBuddy / Trae）",
			summary: []pluginapi.QuotaMetric{
				{Key: "credit_remain", Value: 4794.81},
				{Key: "credit_total", Value: 4800},
				{Key: "credit_used", Value: 5.19},
			},
			want: QuotaTotals{Remaining: 4794, Total: 4800},
		},
		{
			name: "quota_* 族（Qoder）",
			summary: []pluginapi.QuotaMetric{
				{Key: "user_quota_remaining", Value: 900},
				{Key: "user_quota_total", Value: 1000},
				{Key: "addon_quota_remaining", Value: 50},
				{Key: "addon_quota_total", Value: 100},
			},
			want: QuotaTotals{Remaining: 900, Total: 1000},
		},
		{
			name: "只有已用、没有总额时反推",
			summary: []pluginapi.QuotaMetric{
				{Key: "credit_remain", Value: 80},
				{Key: "credit_used", Value: 20},
			},
			want: QuotaTotals{Remaining: 80, Total: 100},
		},
		{
			name: "合法零额度不被后续额度族覆盖",
			summary: []pluginapi.QuotaMetric{
				{Key: "user_quota_remaining", Value: 0},
				{Key: "addon_quota_remaining", Value: 50},
				{Key: "user_quota_total", Value: 100},
				{Key: "addon_quota_total", Value: 200},
			},
			want: QuotaTotals{Remaining: 0, Total: 100},
		},
		{
			// Qoder Free plan: The plan quota is a legitimate {used:0,total:0,remaining:0},
			// and the ones with actual numbers are extension packs. Taking them in order of appearance would permanently lock the account at 0/0.
			name: "首个族为 0/0 时让位给有容量的族（Qoder Free + 拓展包）",
			summary: []pluginapi.QuotaMetric{
				{Key: "user_quota_remaining", Value: 0},
				{Key: "user_quota_total", Value: 0},
				{Key: "user_quota_used", Value: 0},
				{Key: "addon_quota_remaining", Value: 50},
				{Key: "addon_quota_total", Value: 200},
				{Key: "addon_quota_used", Value: 150},
			},
			want: QuotaTotals{Remaining: 50, Total: 200},
		},
		{
			name: "所有族都是 0/0 时保留第一个族（额度已用尽 / 未开通）",
			summary: []pluginapi.QuotaMetric{
				{Key: "user_quota_remaining", Value: 0},
				{Key: "user_quota_total", Value: 0},
				{Key: "addon_quota_remaining", Value: 0},
				{Key: "addon_quota_total", Value: 0},
			},
			want: QuotaTotals{Remaining: 0, Total: 0},
		},
		{
			name: "只给了 total=0 时不从 used 反推（presence 决定，非数值）",
			summary: []pluginapi.QuotaMetric{
				{Key: "credit_remain", Value: 0},
				{Key: "credit_total", Value: 0},
				{Key: "credit_used", Value: 20},
			},
			want: QuotaTotals{Remaining: 0, Total: 0},
		},
		{
			name: "无限额度（Trae 用 -1 / unlimited 表达）",
			summary: []pluginapi.QuotaMetric{
				{Key: "credit_remain", Value: -1, Unit: "unlimited"},
				{Key: "credit_total", Value: 0},
			},
			want: QuotaTotals{Unlimited: true},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Totals(&pluginapi.QuotaFetchResponse{Summary: testCase.summary})
			if got != testCase.want {
				t.Fatalf("Totals = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

// TestTotalsIgnoresUnknownKeys illustrates the boundary of the whitelist: unknown keys will not participate in conversion.
//
// This is not "expected behavior worth keeping", but rather a way to fix silent failures into observable assertions—
// when someone adds a new family to Totals, this reminds them to update the whitelist accordingly.
func TestTotalsIgnoresUnknownKeys(t *testing.T) {
	got := Totals(&pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{
		{Key: "remaining_credit", Value: 4794},
		{Key: "total_credit", Value: 4800},
		{Key: "used_credit", Value: 5},
	}})
	if got != (QuotaTotals{}) {
		t.Fatalf("Unregistered key families should not be converted, got %+v", got)
	}
}

// TestTokenPlanFamilyIndependentOfDefault locks the retrieval behavior when the token_plan family and default family
// coexist: points (default) take priority when they have capacity, and token balance does not overwrite points;
// it falls back to token_plan when points are exhausted. Accounts where MiniMax Token Plan and op points coexist
// rely on this behavior: the same key would cause a family collision, and the token balance would be silently swallowed by points.
func TestTokenPlanFamilyIndependentOfDefault(t *testing.T) {
	both := &pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{
		{Key: "remaining", Value: 500, Unit: "点"},
		{Key: "token_remaining", Value: 14291681135, Unit: "tokens"},
	}}
	if got := Totals(both); got.Remaining != 500 {
		t.Fatalf("default family must win when it has capacity, got %d", got.Remaining)
	}
	zeroPoints := &pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{
		{Key: "remaining", Value: 0, Unit: "点"},
		{Key: "token_remaining", Value: 14291681135, Unit: "tokens"},
	}}
	if got := Totals(zeroPoints); got.Remaining != 14291681135 {
		t.Fatalf("token_plan family must take over when default is empty, got %d", got.Remaining)
	}
}
