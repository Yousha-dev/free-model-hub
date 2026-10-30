package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// This file locks down key criteria for daily active rewards (Global check-in).
//
// Both beacons and heatmap parsing are pipelines where "errors don't fail, they just silently drop records":
// A wrong event field name causes upstream to return 200 but drop the event, while wrong heatmap
// shape parsing turns unlit items into lit ones. Therefore, unit tests are used to lock down field names and both response shapes.

func testGlobalCredential(uid string) *Credential {
	credential := &Credential{
		AccessToken:  "at",
		RefreshToken: "rt",
		UID:          uid,
		Nickname:     "global tester",
		Domain:       "www.workbuddy.ai",
	}
	credential.SetRealm(RegionGlobal)
	return credential
}

// testTaskCredential constructs a test credential.
func testTaskCredential(uid string) *Credential {
	credential := &Credential{
		AccessToken:  "at",
		RefreshToken: "rt",
		UID:          uid,
		Nickname:     "测试账号",
		Domain:       "www.codebuddy.cn",
	}
	credential.SetRealm(RegionCN)
	return credential
}

// TestParseBalanceMatchesUpstreamShape verifies that balance parsing aligns with upstream's actual contract.
//
// This upstream API uses PascalCase fields, double envelopes (Response.Data.Accounts),
// and Cycle plans take priority. Early implementations parsed with camelCase, resulting in a constant 0.
func TestParseBalanceMatchesUpstreamShape(t *testing.T) {
	raw := `{"Response":{"Data":{"Accounts":[
	  {"PackageName":"基础套餐","CycleCapacitySize":1000,"CycleCapacityRemain":400,"CycleCapacityUsed":600},
	  {"PackageName":"加油包","CapacitySize":500,"CapacityRemain":500}
	]}}}`
	var payload billingResourceResponse
	if errUnmarshal := json.Unmarshal([]byte(raw), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	balance := parseBalance(payload)

	// Cycle plan: 1000 total, 400 remaining; Add-on pack: 500/500. Total 1500 / 900.
	if balance.Total != 1500 {
		t.Fatalf("total = %v, want 1500", balance.Total)
	}
	if balance.Remain != 900 {
		t.Fatalf("remain = %v, want 900", balance.Remain)
	}
	if len(balance.Packages) != 2 {
		t.Fatalf("packages = %d, want 2", len(balance.Packages))
	}
	if balance.Packages[0].Name != "基础套餐" || balance.Packages[0].Remain != 400 {
		t.Fatalf("first package = %+v", balance.Packages[0])
	}
}

// TestPackageRemainUsedCycleWins verifies the data retrieval rule for Cycle plans.
func TestPackageRemainUsedCycleWins(t *testing.T) {
	// When CycleUsed is greater than size-remain, remain should be derived based on CycleUsed.
	remain, used, size := packageRemainUsed(billingResourceAccount{
		CycleCapacitySize: 1000, CycleCapacityRemain: 900, CycleCapacityUsed: 300,
	})
	if size != 1000 || used != 300 || remain != 700 {
		t.Fatalf("cycle rule: remain=%d used=%d size=%d, want 700/300/1000", remain, used, size)
	}
	// Fall back to the three Capacity fields when there is no Cycle period.
	remain, used, size = packageRemainUsed(billingResourceAccount{
		CapacitySize: 500, CapacityRemain: 200,
	})
	if size != 500 || used != 300 || remain != 200 {
		t.Fatalf("capacity fallback: remain=%d used=%d size=%d, want 200/300/500", remain, used, size)
	}
	// Clamp remain to size if it exceeds size (occasional upstream dirty data).
	remain, _, size = packageRemainUsed(billingResourceAccount{
		CycleCapacitySize: 100, CycleCapacityRemain: 999,
	})
	if remain != 100 || size != 100 {
		t.Fatalf("clamp: remain=%d size=%d, want 100/100", remain, size)
	}
}

// claimHost is a mock host that returns only one type of upstream response: task list + claiming rewards.
type claimHost struct {
	taskBody  string
	claimBody string
}

// installHostHTTP uses a responder to answer all host.http.do calls and resets when the test case ends.
func installHostHTTP(t *testing.T, responder func(method, rawURL string) (status int, body string)) {
	t.Helper()
	hostbridge.Configure(func(_ string, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, fmt.Errorf("test host: unsupported method %s", method)
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method string `json:"method"`
			URL    string `json:"url"`
		}
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		status, body := responder(req.Method, req.URL)
		return json.Marshal(map[string]any{
			"StatusCode": status,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       []byte(body),
		})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

func installClaimHost(t *testing.T, host claimHost) {
	t.Helper()
	installHostHTTP(t, func(_, rawURL string) (int, string) {
		if strings.Contains(rawURL, "/claim") {
			return http.StatusOK, host.claimBody
		}
		return http.StatusOK, host.taskBody
	})
}

// balanceHost returns different quota responses based on the URL and records requested addresses.
type balanceHost struct {
	personalBody   string
	enterpriseBody string
	paths          []string
}

func installBalanceHost(t *testing.T, host *balanceHost) {
	t.Helper()
	installHostHTTP(t, func(_, rawURL string) (int, string) {
		host.paths = append(host.paths, rawURL)
		if strings.Contains(rawURL, "enterprise-user-usage") {
			return http.StatusOK, host.enterpriseBody
		}
		return http.StatusOK, host.personalBody
	})
}

func (h *balanceHost) called(substr string) bool {
	for _, path := range h.paths {
		if strings.Contains(path, substr) {
			return true
		}
	}
	return false
}

// TestFetchBalanceFallsBackToEnterpriseUsage verifies enterprise accounts getting
// 200 + empty Accounts on the personal API must fall back to the enterprise usage API, otherwise the panel is always 0 / 0.
func TestFetchBalanceFallsBackToEnterpriseUsage(t *testing.T) {
	host := &balanceHost{
		personalBody:   `{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalCount":0,"TotalDosage":0,"Accounts":null}}}}`,
		enterpriseBody: `{"code":0,"msg":"OK","data":{"credit":4.11,"limitNum":2000,"cycleStartTime":"2026-10-01 00:00:00","cycleEndTime":"2026-10-31 23:59:59","cycleResetTime":"2026-11-01 00:00:00"}}`,
	}
	installBalanceHost(t, host)
	client := NewClient(newRuntime(core.Env{}), Options{Context: context.Background()})
	cred := testTaskCredential("uid-enterprise-balance")
	cred.EnterpriseID = "fysxmvfpz6dc"

	balance, errFetch := client.FetchBalance(cred)
	if errFetch != nil {
		t.Fatalf("fetch balance: %v", errFetch)
	}
	if !balance.Enterprise {
		t.Fatal("企业账号的余额必须标记为 enterprise")
	}
	if balance.Total != 2000 || math.Abs(balance.Remain-1995.89) > 0.0001 {
		t.Fatalf("balance = %v/%v, want 1995.89/2000", balance.Remain, balance.Total)
	}
	if !host.called("/v2/billing/meter/get-user-resource") {
		t.Fatal("个人接口必须先被尝试（企业接口只是回退）")
	}
	if !host.called("get-enterprise-user-usage") {
		t.Fatal("企业账号必须回退到企业用量接口")
	}
}

// TestFetchBalanceKeepsEmptyForNonEnterpriseAccount verifies empty quotas for non-enterprise accounts are genuine
// empty quotas and must not trigger the enterprise API.
func TestFetchBalanceKeepsEmptyForNonEnterpriseAccount(t *testing.T) {
	host := &balanceHost{
		personalBody:   `{"code":0,"data":{"Response":{"Data":{"Accounts":null}}}}`,
		enterpriseBody: `{"code":0,"data":{"credit":1,"limitNum":100}}`,
	}
	installBalanceHost(t, host)
	client := NewClient(newRuntime(core.Env{}), Options{Context: context.Background()})
	cred := testTaskCredential("uid-personal-balance")

	balance, errFetch := client.FetchBalance(cred)
	if errFetch != nil {
		t.Fatalf("fetch balance: %v", errFetch)
	}
	if balance.Total != 0 || balance.Enterprise {
		t.Fatalf("balance = %+v, want empty personal quota", balance)
	}
	if host.called("enterprise-user-usage") {
		t.Fatal("非企业账号不应请求企业用量接口")
	}
}

// TestFetchBalancePrefersPersonalPackages verifies that if an enterprise account actually has quota on the personal API,
// personal resource packs take precedence without layering enterprise quota.
func TestFetchBalancePrefersPersonalPackages(t *testing.T) {
	host := &balanceHost{
		personalBody:   `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"加油包","CapacitySize":500,"CapacityRemain":500}]}}}}`,
		enterpriseBody: `{"code":0,"data":{"credit":1,"limitNum":100}}`,
	}
	installBalanceHost(t, host)
	client := NewClient(newRuntime(core.Env{}), Options{Context: context.Background()})
	cred := testTaskCredential("uid-enterprise-personal")
	cred.EnterpriseID = "ent-1"

	balance, errFetch := client.FetchBalance(cred)
	if errFetch != nil {
		t.Fatalf("fetch balance: %v", errFetch)
	}
	if balance.Enterprise || balance.Total != 500 || balance.Remain != 500 {
		t.Fatalf("balance = %+v, want personal 500/500", balance)
	}
	if host.called("enterprise-user-usage") {
		t.Fatal("个人额度非空时不应请求企业接口")
	}
}

// TestParseEnterpriseBalanceKeepsFractionalCredit verifies enterprise used quota can be fractional;
// truncating it to an integer would make "0.9 credits consumed" completely invisible on the panel.
func TestParseEnterpriseBalanceKeepsFractionalCredit(t *testing.T) {
	balance, errParse := parseEnterpriseBalance(json.RawMessage(`{
	  "credit":4.11,"limitNum":2000,
	  "cycleStartTime":"2026-10-01 00:00:00",
	  "cycleEndTime":"2026-10-31 23:59:59",
	  "cycleResetTime":"2026-11-01 00:00:00"}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if !balance.Enterprise || balance.Total != 2000 {
		t.Fatalf("balance = %+v", balance)
	}
	if math.Abs(balance.Remain-1995.89) > 0.0001 {
		t.Fatalf("remain = %v, want 1995.89", balance.Remain)
	}
	if len(balance.Packages) != 1 {
		t.Fatalf("packages = %d, want 1", len(balance.Packages))
	}
	pkg := balance.Packages[0]
	if pkg.Name != "Enterprise Quota" {
		t.Fatalf("package name = %q", pkg.Name)
	}
	if pkg.ExpireAt != 0 {
		t.Fatal("企业额度到点回满，不能设 ExpireAt（会被显示成即将过期）")
	}
	if pkg.ResetAt == 0 {
		t.Fatal("企业额度必须带周期回满时刻")
	}
	if !strings.Contains(pkg.Period, "2026-10-01") || !strings.Contains(pkg.Period, "2026-10-31") {
		t.Fatalf("period = %q", pkg.Period)
	}
}

// TestParseEnterpriseBalanceRejectsMissingLimit verifies responses missing limitNum cannot be displayed
// as 0 / 0: exactly the illusion this issue aims to eliminate.
func TestParseEnterpriseBalanceRejectsMissingLimit(t *testing.T) {
	for _, raw := range []string{`{}`, `{"credit":1}`, `null`, `<html>error</html>`} {
		if _, errParse := parseEnterpriseBalance(json.RawMessage(raw)); errParse == nil {
			t.Fatalf("parseEnterpriseBalance(%s) must return an error", raw)
		}
	}
}

// TestBuildQuotaResponseEnterprisePlan ensures enterprise quota is not displayed as a personal CN plan.
func TestBuildQuotaResponseEnterprisePlan(t *testing.T) {
	cred := testTaskCredential("uid-enterprise-quota")
	cred.EnterpriseID = "ent-1"
	balance, errParse := parseEnterpriseBalance(json.RawMessage(`{
	  "credit":4.11,"limitNum":2000,
	  "cycleStartTime":"2026-10-01 00:00:00",
	  "cycleEndTime":"2026-10-31 23:59:59",
	  "cycleResetTime":"2026-11-01 00:00:00"}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	response := BuildQuotaResponse(cred, balance)
	if response.Subscription == nil || response.Subscription.Plan != "enterprise" {
		t.Fatalf("subscription = %+v, want plan=enterprise", response.Subscription)
	}
	if response.Subscription.TierName != "Enterprise Quota" {
		t.Fatalf("tier = %q", response.Subscription.TierName)
	}
	metrics := map[string]float64{}
	for _, metric := range response.Summary {
		metrics[metric.Key] = metric.Value
	}
	if math.Abs(metrics["credit_used"]-4.11) > 0.0001 {
		t.Fatalf("credit_used = %v, want 4.11", metrics["credit_used"])
	}
	if math.Abs(metrics["credit_remain"]-1995.89) > 0.0001 {
		t.Fatalf("credit_remain = %v, want 1995.89", metrics["credit_remain"])
	}
	if len(response.Groups) != 1 || len(response.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v, want 1 bucket", response.Groups)
	}
	bucket := response.Groups[0].Buckets[0]
	if bucket.Window != "Enterprise Quota" {
		t.Fatalf("window = %q", bucket.Window)
	}
	if !strings.Contains(bucket.Description, "1995.89 / 2000") || !strings.Contains(bucket.Description, "2026-10-01") {
		t.Fatalf("description = %q", bucket.Description)
	}
	if bucket.ResetTime == "" {
		t.Fatal("Enterprise quota should display the periodic reset time")
	}
}

// TestBuildQuotaResponsePersonalDescription ensures personal quota is an integer and not displayed
// in formats like 400.00 / 1000.00.
func TestBuildQuotaResponsePersonalDescription(t *testing.T) {
	var payload billingResourceResponse
	raw := `{"Response":{"Data":{"Accounts":[{"PackageName":"基础套餐","CycleCapacitySize":1000,"CycleCapacityRemain":400}]}}}`
	if errUnmarshal := json.Unmarshal([]byte(raw), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	response := BuildQuotaResponse(testTaskCredential("uid-plain-quota"), parseBalance(payload))
	if len(response.Groups) != 1 || len(response.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v", response.Groups)
	}
	if got := response.Groups[0].Buckets[0].Description; got != "400 / 1000" {
		t.Fatalf("description = %q, want 400 / 1000", got)
	}
}
