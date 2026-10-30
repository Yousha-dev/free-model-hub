package tests

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/tests/testhost"
)

// activitiesPreviewResponse is the successful response for /activities/preview.
type activitiesPreviewResponse struct {
	OK         bool            `json:"ok"`
	Message    string          `json:"message"`
	Activities []core.PlanInfo `json:"activities"`
	Warning    string          `json:"warning"`
}

// TestZCodeActivityPreviewIsReadOnlyAndDegradesWithoutUserID covers two boundary conditions for activity probing:
//   - Backend is read-only: probing does not touch billing/claim at all;
//   - Pure API Key credentials (without JWT) can probe; when user_id is missing, activity cannot be reported, but it does not fail overall,
//     instead returning a warning + the upstream current delivery.
func TestZCodeActivityPreviewIsReadOnlyAndDegradesWithoutUserID(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.AddAccount("1", "zcode-with-uid.json", `{"vendor":"zcode","api_key":"sk-test","user_id":"u-test","profile":{"platform":"win32","arch":"x64","device_mid":"dev-test"}}`)
	host.AddAccount("2", "zcode-without-uid.json", `{"vendor":"zcode","api_key":"sk-test-2","profile":{"platform":"win32","arch":"x64","device_mid":"dev-test-2"}}`)

	var claimCalls, activationCalls int
	host.SetUpstream(func(method, rawURL, _ string) testhost.UpstreamResponse {
		switch {
		case method == http.MethodPost && rawURL == "https://zcode.z.ai/api/v1/event/report":
			activationCalls++
			return testhost.UpstreamResponse{Status: 200, Body: `{"code":0}`}
		case method == http.MethodGet && rawURL == "https://zcode.z.ai/api/v1/marketing/touch?seq=0":
			return testhost.UpstreamResponse{Status: 200, Body: `{"code":0,"data":{"deliveries":[{"campaign_id":"c1","banner":{"background":{"args":{"zcode_plan":{"plan_id":"dev-day-1024","name":"ZCode 1024 Dev Day","priority":9,"starts_at":1792756800,"ends_at":1792976400,"entitlements":[{"show_name":"GLM-5.3-Flash","grant_units":300000000,"period":"one_time"}]}}}}}]}}`}
		case rawURL == "https://zcode.z.ai/api/v1/zcode-plan/billing/claim":
			claimCalls++
		}
		return testhost.UpstreamResponse{Status: 404, Body: `{"code":404}`}
	})

	// With user_id: report two activity events, probing result is complete with no warnings.
	withUID := previewActivities(t, "zcode-with-uid.json")
	if !withUID.OK || withUID.Warning != "" {
		t.Fatalf("with uid: %+v", withUID)
	}
	if len(withUID.Activities) != 1 || withUID.Activities[0].PlanID != "dev-day-1024" {
		t.Fatalf("activities = %+v", withUID.Activities)
	}
	if withUID.Activities[0].Grants[0].Model != "GLM-5.3-Flash" {
		t.Fatalf("grants = %+v", withUID.Activities[0].Grants)
	}
	if activationCalls != 2 {
		t.Fatalf("activation calls = %d, want 2", activationCalls)
	}

	// Without user_id: activity reporting is unavailable, but the endpoint still successfully returns deliveries with a warning explaining the reason.
	withoutUID := previewActivities(t, "zcode-without-uid.json")
	if !withoutUID.OK || len(withoutUID.Activities) != 1 {
		t.Fatalf("without uid: %+v", withoutUID)
	}
	if !strings.Contains(withoutUID.Warning, "user_id") {
		t.Fatalf("warning = %q", withoutUID.Warning)
	}

	// Parameter and capability boundaries: a missing account ID or an unsupported provider must give a readable reason.
	if missing := previewActivities(t, ""); missing.OK || missing.Message != "Missing account_id" {
		t.Fatalf("missing account: %+v", missing)
	}
	host.AddAccount("3", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode"}`)
	if unsupported := previewActivities(t, "opencodezen-oc_test.json"); unsupported.OK || !strings.Contains(unsupported.Message, "campaign detection") {
		t.Fatalf("unsupported vendor: %+v", unsupported)
	}

	if claimCalls != 0 {
		t.Fatalf("activity preview invoked claim %d times", claimCalls)
	}
}

// previewActivities calls the management endpoint once and decodes the response body.
func previewActivities(t *testing.T, accountID string) activitiesPreviewResponse {
	t.Helper()
	raw, err := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(pluginapi.ManagementRequest{
		Method: "POST", Path: "/v0/management/plugins/free-model-hub/activities/preview",
		Headers: http.Header{"X-Host-Callback-Id": []string{testCallbackID}}, Body: []byte(`{"account_id":"` + accountID + `"}`),
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	var env abiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v raw=%s", err, raw)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("response: %v raw=%s env=%+v", err, raw, env)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	var body activitiesPreviewResponse
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("body: %v body=%s", err, resp.Body)
	}
	return body
}
