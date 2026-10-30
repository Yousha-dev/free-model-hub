package tests

// This file locks down the two resilience constraints for the management page's "refresh-fetches" behavior.
//
// Real-world incident: Trae CN's "remaining / total quota" and model list appeared intermittently. Root cause was not upstream
// rate-limiting (direct connection to upstream 200/0.5s), but two lifecycle bindings pointing to the wrong objects:
//
//  1. Batch quota queries were tied to the management RPC's context. A page refresh causes the host to cancel this RPC,
//     aborting the entire query round midway, leaving zero accounts written to the cache, so the account table stayed stuck at "Querying...".
//  2. Model list cache was written only during static probes and manual refreshes. If probes hadn't finished after a plugin reload,
//     the console would lack models for the entire provider; whereas the frontend only auto-refreshed when the "total was 0".
//
// Corresponding fix: Moved quota queries to the background scope (singleflight per round), and added model list fallback probing
// per provider when the cache is empty (singleflight + cooldown).

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
)

// traeQuotaFixture is the minimal response for the Trae quota API (product_type=2 for credit packages).
const traeQuotaFixture = `{"user_entitlement_pack_list":[
 {"entitlement_base_info":{"product_type":2,"quota":{"credits_limit":4000}},"usage":{"credits_amount":1.5}}
]}`

// addTraeAccount registers a Trae CN account, with quota APIs mocked by the test host.
func addTraeAccount(t *testing.T, host *fakeHost) {
	t.Helper()
	host.addAccount("1", "traecn-test.json",
		`{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   traeQuotaFixture,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})
}

// TestQuotaRunSurvivesManagementCancel verifies that quota queries do not abort when the management RPC is cancelled.
//
// After the management context is cancelled, the background round must continue to run and write to the cache, so the next /status can
// retrieve the quota result instead of falling back to "Querying...".
func TestQuotaRunSurvivesManagementCancel(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	addTraeAccount(t, host)

	// Cancelled RPC: When the host cancels, it directly shuts down the request context.
	resp := managementCallWithCancel(t, "POST", "/quotas")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/quotas = %d: %s", resp.StatusCode, resp.Body)
	}

	// Background round still running: Once the cache appears, /status must carry the quota result.
	if !waitFor(t, 5*time.Second, func() bool {
		accounts := statusAccounts(t)
		return len(accounts) == 1 && accounts[0].Quota != nil
	}) {
		t.Fatal("quota batch must finish in the background even when the management RPC is cancelled")
	}
	accounts := statusAccounts(t)
	if !accounts[0].Quota.OK {
		t.Fatalf("trae quota should succeed, got %+v", accounts[0].Quota)
	}
}

// TestConcurrentQuotaRequestsShareOneRun verifies singleflight: continuous page refreshes (the host receiving multiple /quotas)
// while the current round is still ongoing will not repeatedly hammer upstream with the same batch of accounts.
func TestConcurrentQuotaRequestsShareOneRun(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	host.addAccount("1", "traecn-test.json",
		`{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return fakeUpstreamResponse{Status: http.StatusOK, Body: traeQuotaFixture}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	done := make(chan struct{}, 4)
	go func() { managementCall(t, "POST", "/quotas"); done <- struct{}{} }()
	<-started // 第一轮已进入上游，后续请求应等待同一轮
	for i := 0; i < 3; i++ {
		go func() { managementCall(t, "POST", "/quotas"); done <- struct{}{} }()
	}
	time.Sleep(50 * time.Millisecond) // 让后三个请求拿到同一个进行中的轮次
	close(release)
	for i := 0; i < 4; i++ {
		<-done
	}
	if calls := host.UpstreamCalls(); calls != 1 {
		t.Fatalf("quota upstream calls = %d, want 1 (singleflight)", calls)
	}
}

// TestConsoleModelsProbeWhenCacheEmpty verifies that the console probes for models when the cache is empty.
//
// Scenario: Plugin just reloaded, host hasn't run static probes yet, but credentials are already loaded (active). At this point,
// /models must proactively probe once to list the models, rather than leaving the entire provider lacking models.
func TestConsoleModelsProbeWhenCacheEmpty(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	resp := managementCall(t, "GET", "/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/models = %d: %s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Models []struct {
			ID       string `json:"id"`
			VendorID string `json:"vendor_id"`
		} `json:"models"`
		PendingProbes int `json:"pending_probes"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode models: %v (%s)", errUnmarshal, resp.Body)
	}
	var found bool
	for _, model := range payload.Models {
		if model.ID == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("console models must include the probed vendor models, got %+v (pending=%d)",
			payload.Models, payload.PendingProbes)
	}
}

// TestModelsForAuthFillsConsoleCache verifies that model.for_auth conveniently writes to the console cache.
//
// The host loads credentials through this RPC; once written, the console can read the list even without fallback probing.
func TestModelsForAuthFillsConsoleCache(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"for-auth-only","name":"For Auth"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})

	models := testApp.RegisteredModels("workbuddycn")
	var found bool
	for _, model := range models {
		if model.ID == "for-auth-only" {
			found = true
		}
	}
	if !found {
		t.Fatalf("model.for_auth must fill the console cache, got %+v", models)
	}
}

// managementCallWithCancel initiates a management call and simulates the host cancelling immediately after the request is sent.
func managementCallWithCancel(t *testing.T, method, path string) pluginapi.ManagementResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, errHandle := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(pluginapi.ManagementRequest{
		Method: method,
		Path:   "/v0/management/plugins/free-model-hub" + path,
		Headers: http.Header{
			"X-Host-Callback-Id": []string{testCallbackID},
		},
		Context: ctx,
	}))
	if errHandle != nil {
		t.Fatalf("%s %s: %v", method, path, errHandle)
	}
	var env abiEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	var resp pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode management response: %v (raw=%s)", errUnmarshal, env.Result)
	}
	return resp
}

// TestForAuthFailureDoesNotWipeModelCache reproduces the incident where OpenCode models intermittently disappeared.
//
// Providers return empty lists upon probe failure according to contract (allowing the host to evict bad credentials). If this path writes an empty
// list into the console cache, a transient 401 will cause the entire provider's models to disappear until the next round of
// probing succeeds. Expected: The host still receives an empty list, but the console cache remains intact.
//
// Reproduce using OpenCode ZEN instead of WorkBuddy: the former actually hits upstream on every model.for_auth
// (WorkBuddy has a 1-hour catalog cache, so the second call won't fail).
func TestForAuthFailureDoesNotWipeModelCache(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode","label":"zen-test"}`)

	failing := false
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if !strings.Contains(url, "/inference/v1/models") {
			return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
		}
		if failing {
			return fakeUpstreamResponse{Status: http.StatusUnauthorized, Body: `{"error":{"message":"Invalid credential"}}`}
		}
		return fakeUpstreamResponse{
			Status: http.StatusOK,
			Body:   `{"data":[{"id":"kept-model-free"},{"id":"other-model"}]}`,
		}
	})

	// Succeed once first, writing to the console cache.
	callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})
	if len(testApp.RegisteredModels("opencodezen")) == 0 {
		t.Fatal("first for_auth call must populate the console cache")
	}

	// Then make upstream fail: host response remains empty (contract unchanged), but the cache must not be cleared.
	failing = true
	raw := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})
	var resp pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		t.Fatalf("decode model response: %v", errUnmarshal)
	}
	if len(resp.Models) != 0 {
		t.Fatalf("host must still receive an empty list for a failing credential, got %d", len(resp.Models))
	}
	models := testApp.RegisteredModels("opencodezen")
	if len(models) == 0 || models[0].ID != "kept-model-free" {
		t.Fatalf("console cache must survive a failed probe, got %+v", models)
	}
}

// TestRefreshZeroDoesNotWipeCache verifies that existing cache is not cleared when upstream returns an empty list.
func TestRefreshZeroDoesNotWipeCache(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))

	returnEmpty := false
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if !strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
		}
		if returnEmpty {
			return fakeUpstreamResponse{Status: http.StatusOK, Body: `{"code":0,"data":{"models":[]}}`}
		}
		return fakeUpstreamResponse{
			Status: http.StatusOK,
			Body:   `{"code":0,"data":{"models":[{"id":"kept-model","name":"Kept"}]}}`,
		}
	})

	managementCall(t, "POST", "/models/refresh")
	if len(testApp.RegisteredModels("workbuddycn")) == 0 {
		t.Fatal("refresh must populate the cache")
	}

	returnEmpty = true
	managementCall(t, "POST", "/models/refresh")
	if models := testApp.RegisteredModels("workbuddycn"); len(models) == 0 || models[0].ID != "kept-model" {
		t.Fatalf("an empty upstream list must not wipe the cache, got %+v", models)
	}
}

// TestModelCachePersistsToState verifies that lists are persisted to disk after successful probes, allowing recovery upon plugin reload.
func TestModelCachePersistsToState(t *testing.T) {
	host := installFakeHost(t)
	cfg := setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"persisted-model","name":"Persisted"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})

	statePath := filepath.Join(cfg.StateDir, "state.json")
	persisted := waitFor(t, 5*time.Second, func() bool {
		raw, errRead := os.ReadFile(statePath)
		return errRead == nil && strings.Contains(string(raw), "persisted-model")
	})
	if !persisted {
		t.Fatalf("model list must be persisted to %s after a successful probe", statePath)
	}
}

// TestUnavailableCredentialStillServesConsole reproduces "models intermittently disappearing + quota permanently querying".
//
// Incident chain: After upstream rate-limiting (WorkBuddy code:6004) or timeout, the host routing layer marks credentials as
// unavailable (temporary cooldown, automatically recovered later). If the plugin treats this as "no credentials":
//   - Model lists disappear entirely (ActiveVendorSet no longer contains that provider);
//   - Batch quota query skips that account → no results → page remains stuck at "Querying...".
//
// Verify the model side using OpenCode ZEN instead of Trae: it lacks ConsoleModelProvider,
// The console list comes directly from the Catalog cache, which cleanly reflects whether "this provider was skipped".
func TestUnavailableCredentialStillServesConsole(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode","label":"zen-test"}`)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/inference/v1/models") {
			return fakeUpstreamResponse{Status: http.StatusOK, Body: `{"data":[{"id":"kept-model-free"}]}`}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})
	// The host applies a temporary cooldown due to upstream rate limiting.
	host.MarkUnavailable("opencodezen-oc_test.json", true)

	models := managementCall(t, "GET", "/models")
	var payload struct {
		Models []struct {
			VendorID string `json:"vendor_id"`
		} `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(models.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode models: %v (%s)", errUnmarshal, models.Body)
	}
	var listed bool
	for _, model := range payload.Models {
		if model.VendorID == "opencodezen" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("a temporarily unavailable credential must not drop its vendor from the model list: %s", models.Body)
	}
}

// TestUnavailableCredentialStillQueried locks the quota column in the account overview.
//
// Same root cause as the model list: batch quota query reuses the credential filtering of pool.Load, treating the host's
// temporary cooldown as "no credentials" and skipping the entire batch. As a result, the account row gets no results, and the page forever
// stays stuck at "Querying...". The credential is only rate-limited; the quota API itself can be queried.
func TestUnavailableCredentialStillQueried(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	addTraeAccount(t, host)
	host.MarkUnavailable("traecn-test.json", true)

	if resp := managementCall(t, "POST", "/quotas"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/quotas = %d: %s", resp.StatusCode, resp.Body)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		for _, account := range statusAccounts(t) {
			if account.Quota != nil && account.Quota.OK {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("a temporarily unavailable credential must still yield quota, got %+v", statusAccounts(t))
	}
}

// TestCooldownWithFailingUpstreamKeepsConsole covers the true form of cooldown.
//
// The reason a credential is marked unavailable is usually that upstream is currently failing (rate limit, timeout). At this point:
//   - The quota query will fail, but **must be cached as ok=false**, displaying "-- / --" on the page instead of
//     staying stuck at "Querying..." forever — "queried but failed" and "not yet queried" are two different states;
//   - Model probing also fails, but **the cached list must be preserved**, preventing the entire provider from disappearing.
//
// The two assertions use two different providers: OpenCode ZEN has a model API but no quota API, used to
// verify cache retention; Trae has a quota API, used to verify failure results are cached (testing quota
// with OpenCode would trigger the "unsupported" branch, failing to test the cooldown).
func TestCooldownWithFailingUpstreamKeepsConsole(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode","label":"zen-test"}`)
	host.addAccount("2", "traecn-test.json",
		`{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`)

	healthy := true
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if !healthy {
			return fakeUpstreamResponse{Status: http.StatusTooManyRequests, Body: `{"error":{"message":"rate limited"}}`}
		}
		if strings.Contains(url, "/inference/v1/models") {
			return fakeUpstreamResponse{Status: http.StatusOK, Body: `{"data":[{"id":"kept-model-free"}]}`}
		}
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   traeQuotaFixture,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	// Succeed once first to write the model list into the cache.
	managementCall(t, "GET", "/models")

	// Upstream starts failing, and the host simultaneously applies a temporary cooldown.
	healthy = false
	host.MarkUnavailable("opencodezen-oc_test.json", true)
	host.MarkUnavailable("traecn-test.json", true)

	// Models: The cached list must be preserved.
	models := managementCall(t, "GET", "/models")
	if !strings.Contains(string(models.Body), "kept-model-free") {
		t.Fatalf("cached models must survive a failing probe during cooldown: %s", models.Body)
	}

	// Quota: Failures must also leave a "definite" result instead of remaining "Querying...".
	managementCall(t, "POST", "/quotas")
	var traeQuota *struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if !waitFor(t, 5*time.Second, func() bool {
		for _, account := range statusAccounts(t) {
			if account.VendorID == "traecn" && account.Quota != nil {
				traeQuota = account.Quota
				return true
			}
		}
		return false
	}) {
		t.Fatal("a failed quota query must still be cached; otherwise the console shows「查询中…」forever")
	}
	if traeQuota.OK {
		t.Fatalf("quota must report failure during cooldown, got %+v", traeQuota)
	}
	if traeQuota.Message == "" {
		t.Fatal("a failed quota must carry the upstream reason so the console can explain it")
	}
}

// TestDisabledCredentialSkipsQuota locks the other half of the boundary: user explicit disablement remains effective.
//
// /status lists disabled accounts (the page needs to display them and allow re-enabling), so the assertion focuses on
// "quota was not queried" rather than "account is not in the list".
func TestDisabledCredentialSkipsQuota(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	addTraeAccount(t, host)
	host.MarkDisabled("traecn-test.json", true)

	managementCall(t, "POST", "/quotas")
	time.Sleep(200 * time.Millisecond) // 给后台轮次一个窗口，确认它确实没有查询
	for _, account := range statusAccounts(t) {
		if account.Quota != nil {
			t.Fatalf("a disabled credential must not be queried for quota, got %+v", account.Quota)
		}
	}
}
