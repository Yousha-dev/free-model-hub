package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// stubHostHTTPDo replaces the host HTTP bridge with a stub that "records the request + returns empty JSON".
func stubHostHTTPDo(t *testing.T) **http.Request {
	t.Helper()
	return stubHostWithBody(t, `{}`)
}

// stubHostWithBody is the same as stubHostHTTPDo, but returns the specified response body.
func stubHostWithBody(t *testing.T, responseBody string) **http.Request {
	t.Helper()
	var captured *http.Request
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
		}
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, nil)
		if errNew != nil {
			return nil, errNew
		}
		for key, values := range req.Headers {
			for _, value := range values {
				httpReq.Header.Add(key, value)
			}
		}
		captured = httpReq
		return json.Marshal(map[string]any{
			"StatusCode": 200,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       []byte(responseBody),
		})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
	return &captured
}

// TestChatRequestSendsBenefitChannelHeader locks the switch for the benefit channel.
//
// The upstream uses maas_type to distinguish between "daily free quota" and "subscription points": benefit is required to use the free quota.
// This header must also be set **before signing**, otherwise the signature won't cover it, rendering it ineffective.
func TestChatRequestSendsBenefitChannelHeader(t *testing.T) {
	captured := stubHostHTTPDo(t)

	_, errChat := Chat(context.Background(), "", ChatRequest{
		Credential: &Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"},
		Body:       []byte(`{"model":"glm-5.3-flash"}`),
		Benefit:    true,
	})
	if errChat != nil {
		t.Fatalf("chat: %v", errChat)
	}
	if *captured == nil {
		t.Fatal("no request captured")
	}
	if got := (*captured).Header.Get(HeaderMaasType); got != MaasBenefit {
		t.Fatalf("%s = %q, want %q", HeaderMaasType, got, MaasBenefit)
	}
	if signed := (*captured).Header.Get("Authorization"); !strings.Contains(signed, "maas_type") {
		t.Fatalf("Authorization not overridden for %s: %s", HeaderMaasType, signed)
	}
}

// TestChatRequestWithoutBenefitChannel guards the reverse: ensuring this header is not sent when explicitly using the points channel.
func TestChatRequestWithoutBenefitChannel(t *testing.T) {
	captured := stubHostHTTPDo(t)

	_, errChat := Chat(context.Background(), "", ChatRequest{
		Credential: &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"},
		Body:       []byte(`{"model":"glm-5.2"}`),
	})
	if errChat != nil {
		t.Fatalf("chat: %v", errChat)
	}
	if *captured != nil && (*captured).Header.Get(HeaderMaasType) != "" {
		t.Fatalf("credit channel should not carry %s, got %q", HeaderMaasType, (*captured).Header.Get(HeaderMaasType))
	}
}

// TestBuildAuthorizeURLCarriesCallbackPort locks the port parameter of the authorization URL.
//
// The portal builds the callback URL using the port; without it, it falls back to port 80, leaving the browser stuck on an unopenable
// 127.0.0.1 address, and the plugin will never wait for the callback.
func TestBuildAuthorizeURLCarriesCallbackPort(t *testing.T) {
	url := buildAuthorizeURL("ticket-1", "challenge-1")
	for _, want := range []string{
		"port=" + strconv.Itoa(LocalCallbackPort),
		"client_id=" + ClientID,
		"ticket_id=ticket-1",
		"code_challenge_method=SHA-256",
		"plugin-name=" + PluginName,
	} {
		if !strings.Contains(url, want) {
			t.Fatalf("authorization link missing %s: %s", want, url)
		}
	}
	// The ticket secret must not enter the authorization URL: it would appear in browser history and logs.
	if strings.Contains(url, "secret") {
		t.Fatalf("authorization link should not carry secret: %s", url)
	}
}

// TestRequestTokenUsesSTSHost locks the token endpoint host.
//
// /v1/oauth2/tokens is only on sts; snap-manager does not have this API (observed 404 APIG.0101).
func TestRequestTokenUsesSTSHost(t *testing.T) {
	if !strings.Contains(STSHost, "sts.cn-north-4.myhuaweicloud.com") {
		t.Fatalf("STSHost = %q", STSHost)
	}
	if strings.Contains(STSHost, "snap-manager") {
		t.Fatalf("token endpoint should not point to snap-manager: %q", STSHost)
	}
}

// TestFetchQuotaTreatsMissingBenefitAsZeroNotError locks the presentation of "account has no benefit privileges".
//
// Upstream returns 4004 benefit not found for such accounts (observed: same with signature and various identity headers present),
// yet the same account's benefit model list is normal and check-in returns success—so this is an account state, not a query failure.
// Treating it as an error causes the console to display "-- / --" across the row, making it impossible for the user to tell whether quota is absent or the query is broken.
func TestFetchQuotaTreatsMissingBenefitAsZeroNotError(t *testing.T) {
	stubHostWithBody(t, `{"error_code":"4004","error_msg":"benefit not found","result":null}`)

	quota, errQuota := FetchQuota(context.Background(), &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	if errQuota != nil {
		t.Fatalf("missing welfare benefits should not cause an error: %v", errQuota)
	}
	if quota == nil || quota.Subscription == nil {
		t.Fatal("quota result is empty")
	}
	if len(quota.Summary) == 0 || quota.Summary[0].Key != "total" {
		t.Fatalf("total key must be present even when unactivated, otherwise console cannot calculate the total: %+v", quota.Summary)
	}
	if !strings.Contains(quota.Summary[0].Label, "not activated") {
		t.Fatalf("label must explain why: %q", quota.Summary[0].Label)
	}
}

// TestFetchQuotaUsesStandardRemainingKeys locks the standard keys.
//
// The console's "Remaining / Total" only recognizes remaining / total / used (internal/auth.Totals),
// and using custom keys would cause these two columns to permanently show 0 / 0.
func TestFetchQuotaUsesStandardRemainingKeys(t *testing.T) {
	stubHostWithBody(t, `{"error_code":"0000","result":{"channel":"benefit",`+
		`"daily_token_limit":10000000,"daily_tokens_used":2500000,`+
		`"monthly_token_limit":100000000,"monthly_tokens_used":40000000}}`)

	quota, errQuota := FetchQuota(context.Background(), &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	if errQuota != nil {
		t.Fatalf("fetch quota: %v", errQuota)
	}
	values := map[string]float64{}
	for _, metric := range quota.Summary {
		values[metric.Key] = metric.Value
	}
	if values["total"] != 10000000 {
		t.Fatalf("total = %v, want 10000000", values["total"])
	}
	// Remaining must be calculated from "daily quota - used today", rather than echoing upstream fields that do not exist.
	if values["remaining"] != 7500000 {
		t.Fatalf("remaining = %v, want 7500000", values["remaining"])
	}
	if values["used"] != 2500000 {
		t.Fatalf("used = %v, want 2500000", values["used"])
	}
}

// TestParseBuiltinModelsReadsBuiltinEnvelope locks the true envelope of the builtin catalog.
//
// Upstream response is {"count":N,"builtinModels":[...]}, but early implementation only recognized top-level models[],
// resulting in parsing failure every round, silently falling back to the builtin list—the console persistently displayed 4 hardcoded old models,
// and the context 131072/8192 did not match real values, losing image capabilities and multipliers entirely.
func TestParseBuiltinModelsReadsBuiltinEnvelope(t *testing.T) {
	body := `{"count":3,"builtinModels":[
	  {"model_id":"GLM-5.2","model_name":"GLM-5.2","model_desc":"Latest Flagship: Long-horizon tasks & complex projects","enable":true,"supports_images":false,
	   "credit":[{"input_from":0,"status":"0","ratio_display":"0.7x"},{"input_from":32001,"status":"0","ratio_display":"0.7x"},{"input_from":-1,"status":"0","ratio_display":"0.7x"}]},
	  {"model_id":"kimi-k2.6-vl","model_name":"Kimi-K2.6-VL","model_desc":"Outstanding programming performance","supports_images":true,"credit":[]},
	  {"model_id":"retired-model","model_name":"Retired","enable":false,"supports_images":false}]}`

	models, errParse := parseBuiltinModels([]byte(body))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if len(models) != 2 {
		t.Fatalf("models with enable=false should be skipped, got %d items: %+v", len(models), models)
	}

	glm := models[0]
	if glm.ID != "GLM-5.2" || glm.Name != "GLM-5.2" {
		t.Fatalf("ID/Name parsing error: %+v", glm)
	}
	if glm.Credits != "0.7x" {
		t.Fatalf("multiplier should use base tier ratio_display, got %q", glm.Credits)
	}
	// The builtin catalog carries no upper limit, supplemented by the fallback list; output limit must be clamped to the upstream hard limit.
	if glm.ContextWindow != 202752 {
		t.Fatalf("ContextWindow = %d, want 202752", glm.ContextWindow)
	}
	if glm.MaxTokens != maxOutputTokenCap {
		t.Fatalf("MaxTokens = %d, want %d", glm.MaxTokens, maxOutputTokenCap)
	}
	if !glm.SupportsTools {
		t.Fatal("CodeArts models all support tool calling")
	}

	vl := models[1]
	if !vl.SupportsImages {
		t.Fatal("models with supports_images=true must report image capability")
	}
	if vl.Credits != "" {
		t.Fatalf("models without credit should have empty multiplier, got %q", vl.Credits)
	}
	// Models not in the fallback list do not fabricate context, leaving it to the core layer for fallback.
	if vl.ContextWindow != 0 || vl.MaxTokens != 0 {
		t.Fatalf("unknown limit should be 0, got %d/%d", vl.ContextWindow, vl.MaxTokens)
	}
}

// TestParseBuiltinModelsAcceptsLegacyModelsKey guards the compatibility branch.
func TestParseBuiltinModelsAcceptsLegacyModelsKey(t *testing.T) {
	models, errParse := parseBuiltinModels([]byte(`{"models":[{"model_name":"glm-5.2"}]}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if len(models) != 1 || models[0].ID != "glm-5.2" {
		t.Fatalf("old envelope not compatible: %+v", models)
	}
}

// TestBaseCreditDisplayPicksBaseTier locks the multiplier tier selection rule.
//
// Upstream provides multiple tiers based on input length (base tier input_from=0), but the console has only one slot, so pick the base tier;
// tiers with status explicitly not "0" are considered inactive and skipped. ratio and ratio_display are not of the same dimension,
// and must be passed through as-is without conversion.
func TestBaseCreditDisplayPicksBaseTier(t *testing.T) {
	zero, high := 0, 32001
	cases := []struct {
		name  string
		tiers []creditTier
		want  string
	}{
		{"取基础档", []creditTier{{InputFrom: &high, RatioDisplay: "1.0x"}, {InputFrom: &zero, RatioDisplay: "0.7x"}}, "0.7x"},
		{"跳过未生效档", []creditTier{{InputFrom: &zero, Status: "1", RatioDisplay: "9x"}, {InputFrom: &high, RatioDisplay: "0.7x"}}, "0.7x"},
		{"缺 input_from 当基础档", []creditTier{{RatioDisplay: "0.32x"}}, "0.32x"},
		{"空档位返回空", nil, ""},
	}
	for _, tc := range cases {
		if got := baseCreditDisplay(tc.tiers); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCapOutputTokensClampsToUpstreamHardLimit locks the hard limit for output tokens.
//
// Observed upstream pre-check: max_tokens=65536 passes, 65537 and above causes the entire request to return 400
// (InferHub.001001005.400). Catalog self-description 131072/393216, copying it directly would cause the client
// to hit requests with max_tokens exceeding the limit, causing total request failure.
func TestCapOutputTokensClampsToUpstreamHardLimit(t *testing.T) {
	if got := capOutputTokens(65537); got != maxOutputTokenCap {
		t.Fatalf("65537 should be clamped to %d, got %d", maxOutputTokenCap, got)
	}
	if got := capOutputTokens(393216); got != maxOutputTokenCap {
		t.Fatalf("393216 should be clamped to %d, got %d", maxOutputTokenCap, got)
	}
	if got := capOutputTokens(8192); got != 8192 {
		t.Fatalf("within-limit value should remain unchanged, got %d", got)
	}
	if got := capOutputTokens(0); got != 0 {
		t.Fatalf("unknown limit remains 0, got %d", got)
	}
}

// TestAdapterModelCredits locks the consistency between the multiplier table and the catalog.
//
// Multipliers and the catalog must come from the same probing round; models without multipliers do not enter the table, and the console displays "—",
// rather than rendering "missing from upstream" as "free".
func TestAdapterModelCredits(t *testing.T) {
	v := &adapter{}
	infos := v.modelsToPluginAPI([]Model{
		{ID: "GLM-5.2", Name: "GLM-5.2", ContextWindow: 202752, MaxTokens: 65536, SupportsTools: true, Credits: "0.7x"},
		{ID: "kimi-k2.6-vl", Name: "Kimi-K2.6-VL", SupportsTools: true, SupportsImages: true},
	})
	if len(infos) != 2 {
		t.Fatalf("incorrect conversion count: %d", len(infos))
	}
	if !core.ModelSupportsImages(infos[1]) {
		t.Fatal("image capability not propagated to host contract")
	}

	credits := v.ModelCredits()
	if got := credits["GLM-5.2"]; got != "0.70X" {
		t.Fatalf("multiplier should be normalized to 0.70X, got %q", got)
	}
	if _, exists := credits["kimi-k2.6-vl"]; exists {
		t.Fatal("models without multiplier should not enter the table")
	}
}

// TestBundledCatalogCarriesNoPricing locks that the fallback list stores only limits and capabilities.
//
// Descriptions and multipliers take the upstream catalog as the sole source: copying another copy in the fallback leaves no way when upstream adjusts prices/renames
// Tests can reveal that it is expired. Upon regression, the console would show a long-deprecated multiplier.
func TestBundledCatalogCarriesNoPricing(t *testing.T) {
	for _, model := range bundledModels() {
		if model.Credits != "" {
			t.Fatalf("fallback list for %s should not carry multiplier: %q", model.ID, model.Credits)
		}
		if model.Description != "" {
			t.Fatalf("fallback list for %s should not carry description: %q", model.ID, model.Description)
		}
	}
}

// TestFallbackLimitIndexCoversBundledIDs verifies that the upper limit index shares the same source as the fallback list.
func TestFallbackLimitIndexCoversBundledIDs(t *testing.T) {
	for _, model := range bundledModels() {
		hint, ok := fallbackLimitIndex[strings.ToLower(model.ID)]
		if !ok {
			t.Fatalf("%s not in fallback limit index", model.ID)
		}
		if hint[0] != model.ContextWindow || hint[1] != model.MaxTokens {
			t.Fatalf("%s limit index = %v, want [%d %d]", model.ID, hint, model.ContextWindow, model.MaxTokens)
		}
	}
}

// TestCanonicalizeRequestModelResolvesCase locks the normalization of legacy spellings (lowercase) to registered IDs.
//
// The host matches precisely by ID, while older documentation specifies glm-5.2; upstream is case-sensitive, so sending it as-is
// would be rejected. Normalization only occurs when hitting a registered entry, and unregistered names pass through as-is.
func TestCanonicalizeRequestModelResolvesCase(t *testing.T) {
	v := &adapter{}
	v.modelsToPluginAPI([]Model{{ID: "GLM-5.2"}, {ID: "Qwen3-VL-235B"}})

	cases := []struct {
		in       string
		wantName string
	}{
		{`{"model":"glm-5.2","max_tokens":1024}`, "GLM-5.2"},
		{`{"model":"qwen3-vl-235b"}`, "Qwen3-VL-235B"},
		{`{"model":"glm-5.3"}`, "glm-5.3"},
	}
	for _, tc := range cases {
		out := v.prepareRequestBody([]byte(tc.in))
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("%s: output is not valid JSON: %v", tc.in, err)
		}
		if payload["model"] != tc.wantName {
			t.Fatalf("%s: model = %v, want %s", tc.in, payload["model"], tc.wantName)
		}
	}

	for _, body := range []string{`not json`, `{"max_tokens":1}`, `{"model":42}`, `null`} {
		if got := string(v.prepareRequestBody([]byte(body))); got != body {
			t.Fatalf("rewritten when it should not be: %q", got)
		}
	}
}

// TestModelsFromCatalogFallsBack covers catalog chain degradation: network failure, non-200 (folded into
// error by fetchBuiltinCatalog), bad JSON, and empty lists all fall back to the fallback list, while promo
// channel models are still merged in.
func TestModelsFromCatalogFallsBack(t *testing.T) {
	benefit := []Model{{ID: "snap-chat", Name: "snap-chat", Benefit: true}}
	fallbacks := []struct {
		name string
		body []byte
		err  error
	}{
		{name: "网络失败", err: errors.New("boom")},
		{name: "坏 JSON", body: []byte(`{"builtinModels":`)},
		{name: "空清单", body: []byte(`{"count":0,"builtinModels":[]}`)},
		{name: "空响应体", body: nil},
	}
	for _, tc := range fallbacks {
		models := modelsFromCatalog(tc.body, tc.err, benefit)
		if len(models) != len(bundledCatalog)+1 {
			t.Fatalf("%s: models = %d, want %d", tc.name, len(models), len(bundledCatalog)+1)
		}
		if models[len(models)-1].ID != "snap-chat" {
			t.Fatalf("%s: welfare model not merged: %+v", tc.name, models)
		}
	}

	body := []byte(`{"count":1,"builtinModels":[{"model_id":"GLM-5.2","model_name":"GLM-5.2"}]}`)
	models := modelsFromCatalog(body, nil, benefit)
	if len(models) != 2 || models[0].ID != "GLM-5.2" || models[1].ID != "snap-chat" {
		t.Fatalf("models = %+v", models)
	}
	if models[0].ContextWindow != 202752 {
		t.Fatalf("context fallback = %d, want 202752", models[0].ContextWindow)
	}
}
