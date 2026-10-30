package tests

// This file covers auth capabilities: login dispatch and cross-provider credential ownership determination.

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/app"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/tests/testhost"
)

// Regression tests for login session binding.
//
// The guard's problem: the host relies on the vendor field in AuthLoginStartResponse.Metadata to bind
// the login session back to the specific provider; without it, the host cannot get the auth link, and the page only shows "Failed to get auth link"
// . This constraint used to rely solely on each provider remembering to set it themselves, causing providers like codearts / trae
// to be unable to add accounts for a long time—even though the plugin side's LoginStart clearly returned
// the URL successfully, the log only had "started login", completely misleading the troubleshooting direction.
//
// Now the identifier is uniformly injected by the root layer (the dispatch point in auth_login.go), and this test case is the guard for that contract.

// TestLoginStartInjectsVendorMetadata verifies that the root layer supplies the login session binding for the provider.
//
// Use **real codearts** instead of a stub: its LoginStart is purely locally constructed (no network calls),
// and its in-package implementation does not set Metadata itself—which is the exact scenario the regression covers.
func TestLoginStartInjectsVendorMetadata(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	vendor, okVendor := testApp.Registry().ByID("codearts")
	if !okVendor {
		t.Fatal("codearts vendor not registered")
	}

	result := callMethod(t, pluginabi.MethodAuthLoginStart, map[string]any{
		"metadata": map[string]any{core.VendorKey: vendor.ID()},
	})

	var response pluginapi.AuthLoginStartResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode login response: %v (raw=%s)", errUnmarshal, result)
	}
	if response.URL == "" {
		t.Fatal("authorization link is empty")
	}
	// Core assertion: Even if the provider does not set Metadata, the root layer must still supply the vendor binding.
	if got := response.Metadata[core.VendorKey]; got != vendor.ID() {
		t.Fatalf("metadata.vendor = %v, want %q（the host relies on this to bind the login session）", got, vendor.ID())
	}
	if got := response.Metadata["region"]; got != vendor.Region() {
		t.Fatalf("metadata.region = %v, want %q", got, vendor.Region())
	}
}

// Regression tests for cross-provider ownership determination.
//
// This is the plugin's most critical guard: the host feeds all non-builtin credentials sequentially to each plugin,
// and overly broad ownership determination would cause the host to overwrite other accounts with this plugin's structure (silently).
// When adding new providers, these test cases ensure that "each provider's credentials are only claimed by itself".

func TestAuthParseVendorOwnershipDiscrimination(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// Criteria explanation: These test cases **deliberately omit the provider** (empty string), simulating judgment
	// when the host has not yet attributed the file to any plugin. Providing a provider would allow workbuddy's
	// `provider == "freehub"` fallback to accept everything, making the test lose its discriminatory power.
	cases := []struct {
		name       string
		fileName   string
		provider   string
		body       string
		wantHandle bool
	}{
		// OpenCodeZEN: opencodezen filename + api_key.
		{"opencodezen file name", "opencodezen-key1.json", "", `{"api_key":"sk-abc"}`, true},
		{"opencodezen zen_key field", "my-account.json", "", `{"zen_key":"sk-abc"}`, true},
		// Cline: cline filename / workos prefix / camelCase refreshToken.
		{"cline file name", "cline-me@x.com.json", "", `{"accessToken":"at","refreshToken":"rt"}`, true},
		{"cline workos prefix", "acc.json", "", `{"access_token":"workos:at"}`, true},
		// Existing providers: still claimed according to their respective conventions.
		{"workbuddy nested", "workbuddycn-u1.json", "", `{"account":{"uid":"u1"},"auth":{"accessToken":"at","realm":"cn"}}`, true},
		{"qoder pt token", "qodercn-u1.json", "", `{"token":"pt-abc"}`, true},
		// New providers: ZCode, Trae CN/Global, CodeArts, MiniMax Code
		{"zcode filename", "zcode-key1.json", "", `{"api_key":"abc.123"}`, true},
		{"traecn filename", "traecn-user1.json", "", `{"accessToken":"at_123"}`, true},
		{"traeglobal filename", "traeglobal-user1.json", "", `{"accessToken":"at_123","region":"global"}`, true},
		{"codearts filename", "codearts-user1.json", "", `{"access_key_id":"ak","secret_access_key":"sk"}`, true},
		{"minimaxcodecn filename", "minimaxcodecn-user1.json", "", `{"token":"at_123"}`, true},
		{"minimaxcodeglobal filename", "minimaxcodeglobal-user1.json", "", `{"token":"at_123","region":"global"}`, true},

		// New providers: AutoClaw CN/Global, Raccoon.
		{"autoclawcn filename", "autoclawcn-user1.json", "", `{"accessToken":"at","refreshToken":"rt","region":"cn"}`, true},
		{"autoclawglobal filename", "autoclawglobal-user1.json", "", `{"accessToken":"at","refreshToken":"rt","region":"global"}`, true},
		{"autoclaw vendor field", "acc.json", "", `{"vendor":"autoclawcn","accessToken":"at"}`, true},
		{"raccoon filename", "raccoon-7591340.json", "", `{"access_token":"a","refresh_token":"r","uid":"7591340"}`, true},
		{"raccoon pinyin filename", "xiaohuanxiong-u1.json", "", `{"access_token":"a","refresh_token":"r","uid":"1"}`, true},
		// Cross-provider false-swallowing protection: Generic structures must fall under the **correct** provider.
		{"qoder pt token is qoder", "acc.json", "", `{"access_token":"pt-abc"}`, true},
		{"cline camel is cline", "acc.json", "", `{"accessToken":"at","refreshToken":"rt"}`, true},
		// What truly nobody should claim: containing only generic fields, no exclusive features.
		{"flat device token is nothing", "acc.json", "", `{"device_token":"dt-abc","account":{"uid":"u1"}}`, false},
		{"generic snake pair is nothing", "acc.json", "", `{"access_token":"at","refresh_token":"rt"}`, false},
		{"generic token is nothing", "acc.json", "", `{"token":"sk-abc"}`, false},
		{"generic access_token is nothing", "acc.json", "", `{"access_token":"at"}`, false},
		{"workbuddy provider hint overrides", "other.json", app.ProviderKey, `{"accessToken":"at"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
				Provider: tc.provider,
				FileName: tc.fileName,
				RawJSON:  []byte(tc.body),
			})
			var response pluginapi.AuthParseResponse
			if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
				t.Fatalf("decode parse response: %v", errUnmarshal)
			}
			if response.Handled != tc.wantHandle {
				t.Fatalf("handled = %v, want %v", response.Handled, tc.wantHandle)
			}
		})
	}
}

// TestLegacyWorkBuddyAccountNotClaimedByCline verifies that CodeBuddy/WorkBuddy accounts added before installing FreeTier 2API
// are not mistakenly swallowed first by Cline, and that CN and Global editions are correctly attributed to workbuddycn and workbuddyglobal respectively.
func TestLegacyWorkBuddyAccountNotClaimedByCline(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	cases := []struct {
		name       string
		fileName   string
		body       string
		wantVendor string
	}{
		{
			name:       "workbuddy global flat legacy file",
			fileName:   "workbuddy-fb0e95d.json",
			body:       `{"accessToken":"at_123","refreshToken":"rt_456","domain":"www.workbuddy.ai","uid":"fb0e95d"}`,
			wantVendor: "workbuddyglobal",
		},
		{
			name:       "workbuddy global legacy with device token",
			fileName:   "workbuddy-user1.json",
			body:       `{"accessToken":"at_123","refreshToken":"rt_456","domain":"workbuddy.ai","device_token":"dt_789"}`,
			wantVendor: "workbuddyglobal",
		},
		{
			name:       "workbuddy cn flat legacy file",
			fileName:   "workbuddy-user2.json",
			body:       `{"accessToken":"at_123","refreshToken":"rt_456","domain":"www.codebuddy.cn","uid":"user2"}`,
			wantVendor: "workbuddycn",
		},
		{
			name:       "workbuddy cn default legacy file",
			fileName:   "workbuddy-user3.json",
			body:       `{"accessToken":"at_123","refreshToken":"rt_456","uid":"user3"}`,
			wantVendor: "workbuddycn",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
				FileName: tc.fileName,
				RawJSON:  []byte(tc.body),
			})
			var response pluginapi.AuthParseResponse
			if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
				t.Fatalf("decode parse response: %v", errUnmarshal)
			}
			if !response.Handled {
				t.Fatalf("expected account %s to be handled", tc.fileName)
			}
			gotVendor := response.Auth.Attributes[core.VendorKey]
			if gotVendor != tc.wantVendor {
				t.Fatalf("vendor = %q, want %q (must not be claimed by cline)", gotVendor, tc.wantVendor)
			}
		})
	}
}

// ---- localhost callback paste fallback ----
//
// The following three guards cover two types of lockups actually encountered by users:
//
//  1. Trae's session table is self-maintained in SessionStore, and the root layer cannot find it through the public registry,
//     causing polling to always be judged as "Login session does not exist or has expired" — what the user sees is precisely "Authorization failed: Login session does not exist…".
//  2. Both upstream servers only recognize localhost callbacks, which cannot be received when the plugin and browser are not on the same machine,
//     so users can complete the login by pasting the entire link from the address bar back.

// startVendorLogin initiates a login and returns a response with state.
func startVendorLogin(t *testing.T, vendorID string) pluginapi.AuthLoginStartResponse {
	t.Helper()
	raw := callMethod(t, pluginabi.MethodAuthLoginStart, map[string]any{
		"metadata": map[string]any{core.VendorKey: vendorID},
	})
	var response pluginapi.AuthLoginStartResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		t.Fatalf("decode login start: %v (raw=%s)", errUnmarshal, raw)
	}
	if response.State == "" {
		t.Fatalf("%s login did not return state: %s", vendorID, raw)
	}
	return response
}

// codeartsPastedCallback is a sample of the real callback link staying on 127.0.0.1 after browser authorization.
//
// Key evidence: ticket_id / secret are both **issued by Huawei Cloud**, not the pair sent by LoginStart
// into /authorize; the plugin previously polled with self-created values, so the console was forever stuck at "Waiting for authorization…".
const codeartsPastedCallback = "http://127.0.0.1/oauth/callback?secret=d55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280" +
	"&redirect=https%3A%2F%2Fcodearts.huaweicloud.com%2Fportal%2Flogin%3Ftheme%3Dlight%26locale%3Dzh-cn" +
	"%26ticket_id%3D657abbb247eab363d1a461285fc50145%26authorizationAvailable%3Dfalse" +
	"%26secret%3Dd55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280"

const codeartsUpstreamSecret = "0628ec6bf686e409a532e813dc1a0140e49d1dcef1f96e2252f5a67e01d715d9"

// TestTraeLoginPollReachesVendorSession locks down that Trae's login polling can retrieve its own session.
//
// This regresses the combination of "self-maintained session table + root layer only checking public registry": when both occur together, every time Trae's
// Polling will always be judged as "login session does not exist or has expired", and login cannot reach the browser step at all.
func TestTraeLoginPollReachesVendorSession(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	started := startVendorLogin(t, "traecn")
	raw := callMethod(t, pluginabi.MethodAuthLoginPoll, map[string]any{"state": started.State})
	var polled pluginapi.AuthLoginPollResponse
	if errUnmarshal := json.Unmarshal(raw, &polled); errUnmarshal != nil {
		t.Fatalf("decode poll: %v (raw=%s)", errUnmarshal, raw)
	}
	if polled.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending（message=%q）", polled.Status, polled.Message)
	}
}

// TestSubmitLoginCallbackCompletesTraeLogin end-to-end verifies Trae's paste fallback: from the login link
// extract trace/machine to construct the browser callback, and submitting it should yield credentials.
func TestSubmitLoginCallbackCompletesTraeLogin(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	started := startVendorLogin(t, "traecn")
	parsed, errParse := url.Parse(started.URL)
	if errParse != nil {
		t.Fatalf("parse login url: %v", errParse)
	}
	query := parsed.Query()
	trace, machine := query.Get("login_trace_id"), query.Get("machine_id")
	if trace == "" || machine == "" {
		t.Fatalf("login link lacks trace/machine: %s", started.URL)
	}

	// The callback intentionally omits refreshToken: if included, the plugin would fetch a token from upstream, requiring network access for the test.
	callback := "http://127.0.0.1:18080/authorize?loginTraceID=" + trace + "&machine_id=" + machine +
		"&userJwt=" + url.QueryEscape(`{"Token":"jwt-access","TokenExpireAt":1793607527194}`) +
		"&userInfo=" + url.QueryEscape(`{"UserID":"user-1","ScreenName":"tester","TenantID":"tenant-1"}`)

	result, errSubmit := testApp.SubmitLoginCallback(context.Background(), started.State, callback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success（message=%q）", result.Status, result.Message)
	}
	if result.Auth.Label != "Trae-tester" || result.Auth.FileName != "traecn-user-1.json" {
		t.Fatalf("credential label mismatch: label=%q file=%q", result.Auth.Label, result.Auth.FileName)
	}
}

// TestSubmitLoginCallbackCompletesCodeArtsLogin end-to-end verifies CodeArts' paste fallback.
//
// Key point: the ticket number must still be **this session's own** (taken from the authorization link), and only the secret
// should be replaced with the one issued by the portal. Querying the ticket interface with the portal intermediate ticket from the callback will only cause upstream to return "invalid ticketId".
func TestSubmitLoginCallbackCompletesCodeArtsLogin(t *testing.T) {
	var gotTicket string
	host := testhost.New()
	host.SetUpstream(func(method, rawURL, body string) testhost.UpstreamResponse {
		if !strings.Contains(rawURL, "/v1/login/ticket") {
			return testhost.UpstreamResponse{Status: 404, Body: `{"code":404,"msg":"not stubbed"}`}
		}
		// The input parameter url shadowed the net/url package, so here it is parsed back into query parameters as a string.
		query, errQuery := url.Parse(rawURL)
		if errQuery != nil {
			return testhost.UpstreamResponse{Status: 400, Body: `{"code":400,"msg":"bad url"}`}
		}
		gotTicket = query.Query().Get("ticket_id")
		if query.Query().Get("secret") != codeartsUpstreamSecret {
			t.Errorf("ticket query did not use the secret issued by the portal: %s", rawURL)
		}
		return testhost.UpstreamResponse{
			Status: 200,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body: `{"user_id":"user-1","user_name":"tester","refresh_token":"refresh-1",` +
				`"credentials":{"access_key_id":"AK-test","secret_access_key":"SK-test",` +
				`"security_token":"ST-test","expiration":"2026-10-19T18:00:00Z"}}`,
		}
	})
	hostbridge.Configure(host.Call)
	t.Cleanup(func() { hostbridge.Configure(nil) })
	setupTestPlugin(t)

	started := startVendorLogin(t, "codearts")
	parsed, errParse := url.Parse(started.URL)
	if errParse != nil {
		t.Fatalf("parse login url: %v", errParse)
	}
	sessionTicket := parsed.Query().Get("ticket_id")
	if sessionTicket == "" {
		t.Fatalf("authorization link lacks ticket_id: %s", started.URL)
	}

	// Construct a real-shaped callback using this session's ticket number: top-level secret + portal login page in redirect.
	portal := "https://codearts.huaweicloud.com/portal/login?theme=light&locale=zh-cn&ticket_id=" + sessionTicket
	callback := "http://127.0.0.1:7866/oauth/callback?secret=" + codeartsUpstreamSecret +
		"&redirect=" + url.QueryEscape(portal)

	result, errSubmit := testApp.SubmitLoginCallback(context.Background(), started.State, callback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success（message=%q）", result.Status, result.Message)
	}
	if gotTicket != sessionTicket {
		t.Fatalf("ticket query used %q, want this session's %q", gotTicket, sessionTicket)
	}
	if result.Auth.FileName != "codearts-user-1.json" {
		t.Fatalf("file name = %q, want codearts-user-1.json", result.Auth.FileName)
	}
	if !strings.Contains(string(result.Auth.StorageJSON), "AK-test") {
		t.Fatalf("credential not persisted: %s", result.Auth.StorageJSON)
	}
}

// TestSubmitLoginCallbackRejectsSessionlessVendor guards the boundary: device code type providers have no local
// callback, and the paste entry must be explicitly rejected rather than giving a confusing failure.
func TestSubmitLoginCallbackRejectsSessionlessVendor(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// Qoder CN is device code login: LoginStart is purely local, and there is no local callback to paste.
	started := startVendorLogin(t, "qodercn")
	if _, errSubmit := testApp.SubmitLoginCallback(
		context.Background(), started.State, "http://127.0.0.1/authorize?a=1"); errSubmit == nil {
		t.Fatal("providers without a local callback must reject pasting")
	}
}

// TestVendorAccountLimitEnforced verifies that a single provider allows a maximum of 10 accounts (core.MaxAccountsPerVendor).
func TestVendorAccountLimitEnforced(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	for i := 1; i <= core.MaxAccountsPerVendor; i++ {
		id := string(rune(96 + i))
		host.AddAccount(id, "codearts-user-"+id+".json", `{"vendor":"codearts","access_key_id":"ak","secret_access_key":"sk"}`)
	}

	// When the same provider already has 10 accounts, initiating the 11th login must be rejected.
	_, errStart := testApp.HandleMethod(pluginabi.MethodAuthLoginStart, mustMarshal(map[string]any{
		"metadata": map[string]any{core.VendorKey: "codearts"},
	}))
	if errStart == nil || !strings.Contains(errStart.Error(), "maximum of 10 accounts") {
		t.Fatalf("expected account limit error for codearts, got %v", errStart)
	}

	// Other providers with fewer than 10 accounts are not affected.
	if _, errOther := testApp.HandleMethod(pluginabi.MethodAuthLoginStart, mustMarshal(map[string]any{
		"metadata": map[string]any{core.VendorKey: "qodercn"},
	})); errOther != nil {
		t.Fatalf("other vendor should still be allowed, got %v", errOther)
	}
}
