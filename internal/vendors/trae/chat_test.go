package trae

// Protocol layer unit test: use httptest + hostbridge direct connection stubs to solidify the TraeCode protocol contract.
// Fixtures are from real upstream responses (captured 2026-10), no external network access.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// stubHost sets up the direct-connect host bridge (both http.do and SSE streaming paths).
//
// Streaming path delivered per host contract: data first (done=false), then an empty read with done=true.
func stubHost(t *testing.T) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies = map[string][]byte{}
		nextID int
	)
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method   string              `json:"method"`
			URL      string              `json:"url"`
			Headers  map[string][]string `json:"headers"`
			Body     []byte              `json:"body"`
			StreamID string              `json:"stream_id"`
		}
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		switch method {
		case pluginabi.MethodHostHTTPDo:
			status, header, body, errDo := stubUpstream(req.Method, req.URL, req.Headers, req.Body)
			if errDo != nil {
				return nil, errDo
			}
			return json.Marshal(map[string]any{"StatusCode": status, "Headers": header, "Body": body})
		case pluginabi.MethodHostHTTPDoStream:
			status, header, body, errDo := stubUpstream(req.Method, req.URL, req.Headers, req.Body)
			if errDo != nil {
				return nil, errDo
			}
			mu.Lock()
			nextID++
			streamID := fmt.Sprintf("stub-%d", nextID)
			bodies[streamID] = body
			mu.Unlock()
			return json.Marshal(map[string]any{"status_code": status, "headers": header, "stream_id": streamID})
		case pluginabi.MethodHostHTTPStreamRead:
			mu.Lock()
			body, okBody := bodies[req.StreamID]
			if okBody {
				delete(bodies, req.StreamID)
			}
			mu.Unlock()
			if okBody {
				return json.Marshal(map[string]any{"payload": body, "done": false})
			}
			return json.Marshal(map[string]any{"done": true})
		case pluginabi.MethodHostHTTPStreamClose:
			return json.Marshal(map[string]any{})
		default:
			return nil, fmt.Errorf("stub host: unsupported method %s", method)
		}
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

func stubUpstream(method, url string, headers map[string][]string, body []byte) (int, http.Header, []byte, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	httpReq, errNew := http.NewRequest(method, url, reader)
	if errNew != nil {
		return 0, nil, nil, errNew
	}
	for key, values := range headers {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	resp, errDo := (&http.Client{}).Do(httpReq)
	if errDo != nil {
		return 0, nil, nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return 0, nil, nil, errRead
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

// testConfig returns a CN profile pointing to the stub server.
func testConfig(t *testing.T, serverURL string) Config {
	t.Helper()
	cfg := ConfigFor(RegionCN)
	cfg.ChatHost = serverURL
	cfg.AuthHost = serverURL
	cfg.RefreshHost = serverURL
	cfg.CheckinHost = serverURL
	cfg.QuotaHost = serverURL
	return cfg
}

func testCredential() *Credential {
	return &Credential{
		Vendor:       VendorIDCN,
		Region:       RegionCN,
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		UID:          "2487573687317868",
	}
}

// ---- Fixtures: Real upstream responses ----

const chatSSEFixture = `id: 1
event: metadata
data: {"model":"","session_id":"s-1"}

id: 2
event: timing_cost
data: {"name":"llm_raw_chat_v2"}

id: 3
event: output
data: {"response":"","reasoning_content":"思考","tool_calls":null}

id: 4
event: output
data: {"response":"你好","reasoning_content":null,"tool_calls":null}

id: 5
event: token_usage
data: {"prompt_tokens":16,"completion_tokens":479,"total_tokens":495,"reasoning_tokens":460}

id: 6
event: done
data: {"finish_reason":"stop"}

`

const catalogFixture = `{
  "allow_tenant_user_add_model": true,
  "config_info_list": [
    {
      "config_name": "glm-5.2",
      "config_switch": true,
      "usage": "chat_completion",
      "context_window_tokens": {"dev": 200000},
      "display_config": {"display_name": "GLM-5.2", "model_capability": "reasoning_model"},
      "extra_config": "{\"native_function_call\":true}",
      "model_detail_list": [{"model_name": "glm-5.2__dev", "max_tokens": 32000}]
    },
    {
      "config_name": "DeepSeek-V4-Pro",
      "config_switch": true,
      "usage": "chat_completion",
      "context_window_tokens": {"dev": 200000},
      "display_config": {"display_name": "DeepSeek-V4-Pro", "model_capability": "reasoning_model"},
      "extra_config": "{}",
      "model_detail_list": [{"model_name": "DeepSeek-V4-Pro__dev", "max_tokens": 32000}]
    },
    {
      "config_name": "deepseek-v4.1-flash",
      "config_switch": true,
      "usage": "chat_completion",
      "context_window_tokens": {"dev": 200000},
      "display_config": {"display_name": "DeepSeek-V4.1-Flash", "model_capability": "reasoning_model"},
      "extra_config": "{}",
      "model_detail_list": [{"model_name": "deepseek-v4.1-flash__dev", "max_tokens": 32000}]
    },
    {
      "config_name": "custom_model_gpt-5",
      "config_switch": true,
      "usage": "custom_model",
      "context_window_tokens": {"dev": 200000},
      "display_config": {"display_name": "GPT-5", "model_capability": "chat_model"},
      "model_detail_list": [{"model_name": "custom_model_gpt-5__max", "max_tokens": 32000}]
    },
    {
      "config_name": "glm-5-off",
      "config_switch": false,
      "usage": "chat_completion",
      "model_detail_list": [{"model_name": "glm-5-off__dev", "max_tokens": 32000}]
    }
  ]
}`

const quotaFixture = `{
  "is_credits_billing": true,
  "usage_summary": {"consumed_amount": 0.49, "total_amount": 4600},
  "user_entitlement_pack_list": [
    {
      "entitlement_base_info": {"product_type": 2, "quota": {"credits_limit": 4000}},
      "usage": {"credits_amount": ""}
    },
    {
      "entitlement_base_info": {"product_type": 0, "quota": {}},
      "usage": {"credits_amount": ""}
    },
    {
      "entitlement_base_info": {"product_type": 2, "quota": {"credits_limit": 500}},
      "usage": {"credits_amount": 0.4944}
    },
    {
      "entitlement_base_info": {"product_type": 2, "quota": {"credits_limit": 100}},
      "usage": {}
    }
  ]
}`

// ---- Request Body / Headers ----

func TestPrepareBodyBuildsTraeCodeEnvelope(t *testing.T) {
	cfg := ConfigFor(RegionCN)
	resolved := ResolvedModel{ConfigName: "glm-5.2", WireName: "glm-5.2__dev"}
	src := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false,"temperature":0.5}`)

	body, model, errPrep := PrepareBody(src, cfg, resolved)
	if errPrep != nil {
		t.Fatalf("PrepareBody: %v", errPrep)
	}
	if model != "glm-5.2" {
		t.Fatalf("model = %q, want glm-5.2", model)
	}

	var got map[string]any
	if errJSON := json.Unmarshal(body, &got); errJSON != nil {
		t.Fatalf("decode body: %v", errJSON)
	}
	if got["config_name"] != "glm-5.2" {
		t.Errorf("config_name = %v", got["config_name"])
	}
	if got["model_name"] != "glm-5.2__dev" {
		t.Errorf("model_name = %v, want glm-5.2__dev", got["model_name"])
	}
	if got["raw_chat_function"] != "chat_v3" {
		t.Errorf("raw_chat_function = %v", got["raw_chat_function"])
	}
	if got["config_source"] != float64(1) {
		t.Errorf("config_source = %v", got["config_source"])
	}
	if got["stream"] != true {
		t.Errorf("stream = %v, want true (upstream only speaks SSE)", got["stream"])
	}
	if got["conversation_id"] == "" || got["conversation_id"] != got["session_id"] {
		t.Errorf("conversation_id/session_id mismatch: %v / %v", got["conversation_id"], got["session_id"])
	}
	if _, ok := got["model"]; ok {
		t.Error("top-level model field should be deleted (to avoid confusion with model_name)")
	}
	// Pass-through fields retained.
	if got["temperature"] != 0.5 {
		t.Errorf("temperature missing: %v", got["temperature"])
	}
	// Wrap content into a block array.
	msgs, _ := got["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	blocks, okBlocks := first["content"].([]any)
	if !okBlocks || len(blocks) != 1 {
		t.Fatalf("content not converted to block array: %v", first["content"])
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hi" {
		t.Errorf("content block incorrect: %v", block)
	}
}

func TestApplyTraeHeadersSetsIDEIdentity(t *testing.T) {
	cfg := ConfigFor(RegionCN)
	cred := testCredential()
	req, _ := http.NewRequest(http.MethodPost, "https://example.com", nil)
	ApplyTraeHeaders(req, cred, cfg, true)

	want := map[string]string{
		"Authorization":        "Cloud-IDE-JWT test-access-token",
		"X-Cloudide-Token":     "test-access-token",
		"X-Ide-Token":          "test-access-token",
		"X-App-Id":             AppID,
		"X-App-Version":        "default",
		"X-Ide-Version":        IDEVersionCN,
		"X-Ide-Version-Code":   IDEVersionCodeCN,
		"X-Ide-Version-Type":   "stable",
		"X-App-Function":       "chat_v3",
		"X-Ide-Function":       "chat_v3",
		"Accept":               "text/event-stream",
		"Request-Traffic-Type": "prod",
	}
	for key, value := range want {
		if got := req.Header.Get(key); got != value {
			t.Errorf("header %s = %q, want %q", key, got, value)
		}
	}
	if req.Header.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id not set")
	}
}

// ---- Chat (Stub Server) ----

func TestChatAggregatesSSEIntoOpenAIResponse(t *testing.T) {
	stubHost(t)
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpModels:
			_, _ = w.Write([]byte(catalogFixture))
		case EpChat:
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(chatSSEFixture))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)

	body, errChat := Chat(context.Background(), "", ChatRequest{
		Credential: testCredential(),
		Profile:    cfg,
		Body:       []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	})
	if errChat != nil {
		t.Fatalf("Chat: %v", errChat)
	}

	// Request body follows the TraeCode protocol.
	if gotBody["model_name"] != "glm-5.2__dev" {
		t.Errorf("model_name received by upstream = %v", gotBody["model_name"])
	}
	if gotBody["raw_chat_function"] != "chat_v3" {
		t.Errorf("raw_chat_function received by upstream = %v", gotBody["raw_chat_function"])
	}

	var got map[string]any
	if errJSON := json.Unmarshal(body, &got); errJSON != nil {
		t.Fatalf("decode response: %v", errJSON)
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v", got["object"])
	}
	choices, _ := got["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "你好" {
		t.Errorf("content = %v, want hello", message["content"])
	}
	if message["reasoning_content"] != "思考" {
		t.Errorf("reasoning_content = %v, want thinking", message["reasoning_content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	usage, _ := got["usage"].(map[string]any)
	if usage["total_tokens"] != float64(495) {
		t.Errorf("usage.total_tokens = %v", usage["usage"])
	}
}

func TestChatStreamEmitsOpenAIChunks(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpModels:
			_, _ = w.Write([]byte(catalogFixture))
		case EpChat:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(chatSSEFixture))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)

	var chunks []string
	errStream := ChatStream(context.Background(), "", ChatRequest{
		Credential: testCredential(),
		Profile:    cfg,
		Body:       []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	}, func(chunk []byte) error {
		chunks = append(chunks, string(chunk))
		return nil
	})
	if errStream != nil {
		t.Fatalf("ChatStream: %v", errStream)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, `"chat.completion.chunk"`) {
		t.Error("chunk is not in OpenAI streaming format")
	}
	if !strings.Contains(joined, "思考") || !strings.Contains(joined, "你好") {
		t.Error("chunk missing reasoning chain or content delta")
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Error("missing termination chunk")
	}
}

func TestChatSurfacesUpstreamErrorEvent(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpModels:
			_, _ = w.Write([]byte(catalogFixture))
		case EpChat:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: error\ndata: {\"code\":4023,\"message\":\"the model is unknown\"}\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)

	_, errChat := Chat(context.Background(), "", ChatRequest{
		Credential: testCredential(),
		Profile:    cfg,
		Body:       []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	})
	if errChat == nil {
		t.Fatal("upstream error event should report error")
	}
	if !strings.Contains(errChat.Error(), "4023") {
		t.Errorf("error message missing upstream code: %v", errChat)
	}
}

func TestChatRejectsModelOutsideCatalog(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(catalogFixture))
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)

	_, errChat := Chat(context.Background(), "", ChatRequest{
		Credential: testCredential(),
		Profile:    cfg,
		Body:       []byte(`{"model":"not-a-model","messages":[{"role":"user","content":"hi"}]}`),
	})
	if errChat == nil {
		t.Fatal("models outside the catalog should report an error")
	}
	if !strings.Contains(errChat.Error(), "not-a-model") {
		t.Errorf("error message should specify model: %v", errChat)
	}
}

// ---- Model Catalog ----

func TestFetchCatalogFiltersAndResolvesWireIDs(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("get_detail_param should be POST, got %s", r.Method)
		}
		_, _ = w.Write([]byte(catalogFixture))
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)
	cred := testCredential()

	// Clear any existing cache to ensure this test case is independent.
	models, errModels := FetchModels(context.Background(), cred, cfg)
	if errModels != nil {
		t.Fatalf("FetchModels: %v", errModels)
	}
	ids := map[string]bool{}
	for _, model := range models {
		ids[model.ID] = true
	}
	if !ids["glm-5.2"] || !ids["DeepSeek-V4-Pro"] || !ids["deepseek-v4.1-flash"] {
		t.Errorf("optional chat models missing: %v", ids)
	}
	if ids["custom_model_gpt-5"] {
		t.Error("route config with usage=custom_model should not appear in manifest")
	}
	if ids["glm-5-off"] {
		t.Error("models with config_switch=false should not appear in manifest")
	}

	resolved, errResolve := (&catalogCache{}).resolve(context.Background(), "DeepSeek-V4-Pro", cfg, cred)
	if errResolve != nil {
		t.Fatalf("catalog.resolve: %v", errResolve)
	}
	if resolved.ConfigName != "DeepSeek-V4-Pro" || resolved.WireName != "DeepSeek-V4-Pro__dev" {
		t.Errorf("parsing result incorrect: %+v", resolved)
	}

	// Case-insensitive match, returning the exact spelling from the catalog.
	resolvedLower, errLower := (&catalogCache{}).resolve(context.Background(), "deepseek-v4-pro", cfg, cred)
	if errLower != nil {
		t.Fatalf("catalog.resolve(lower): %v", errLower)
	}
	if resolvedLower.ConfigName != "DeepSeek-V4-Pro" {
		t.Errorf("lowercase request should normalize to catalog exact spelling: %+v", resolvedLower)
	}
}

// TestFetchCatalogUsesChatFunction locks down that catalog request functions must match the chat side.
//
// Upstream maintains two wire ID namespaces by function: if the catalog fetches another (e.g.,
// solo_work_lite's deepseek_v4_pro__dev) and then sends a chat_v3 conversation, it will be judged as
// 4023 model is unknown.
func TestFetchCatalogUsesChatFunction(t *testing.T) {
	stubHost(t)
	var gotBody map[string]any
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(catalogFixture))
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)
	cfg.Function = "chat_v3"

	// nil cache: this call hits the upstream directly, unaffected by entries left by other test cases.
	if _, errModels := FetchModels(context.Background(), testCredential(), cfg); errModels != nil {
		t.Fatalf("FetchModels: %v", errModels)
	}
	if gotBody["function"] != "chat_v3" {
		t.Errorf("catalog request body function = %v, should follow cfg.Function", gotBody["function"])
	}
	for _, key := range []string{"X-App-Function", "X-Ide-Function"} {
		if got := gotHeaders.Get(key); got != "chat_v3" {
			t.Errorf("%s = %q, should follow cfg.Function", key, got)
		}
	}
}

// TestConfigureOverridesIDEVersion locks down that the version number can be overridden by configuration and isolated by instance ID.
func TestConfigureOverridesIDEVersion(t *testing.T) {
	cfg := ConfigFor(RegionCN)
	inst := &adapter{config: cfg}

	inst.Configure(core.Settings{}, map[string]string{
		"traecn_ide_version":      "9.9.9",
		"traecn_ide_version_code": "20990101",
		// Neither family-level keys nor keys from other instances should take effect.
		"trae_ide_version":       "1.1.1",
		"traeglobal_ide_version": "2.2.2",
	})
	if got := inst.profile(); got.IDEVersion != "9.9.9" || got.IDEVersionCode != "20990101" {
		t.Fatalf("override did not take effect: %q / %q", got.IDEVersion, got.IDEVersionCode)
	}
	if inst.profile().Function != "chat_v3" || inst.profile().ChatHost != cfg.ChatHost {
		t.Error("override version should not alter other profile fields")
	}

	inst.Configure(core.Settings{}, map[string]string{})
	if got := inst.profile(); got.IDEVersion != IDEVersionCN || got.IDEVersionCode != IDEVersionCodeCN {
		t.Errorf("should fallback to constant after removing config: %q / %q", got.IDEVersion, got.IDEVersionCode)
	}
}

// ---- Check-in ----

func checkinServer(t *testing.T, statusBody, claimBody string, claimStatus int) (*httptest.Server, *Config) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCheckinStatus:
			_, _ = w.Write([]byte(statusBody))
		case EpCheckinClaim:
			w.WriteHeader(claimStatus)
			_, _ = w.Write([]byte(claimBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	cfg := testConfig(t, server.URL)
	return server, &cfg
}

func TestCheckinDeviceIDIsStable(t *testing.T) {
	cred := testCredential()
	first := CheckinDeviceID(cred)
	if len(first) != 16 {
		t.Fatalf("device id length = %d, want 16", len(first))
	}
	if second := CheckinDeviceID(cred); second != first {
		t.Errorf("device id unstable: %s vs %s", first, second)
	}
}

// ---- Quota ----

func TestFetchQuotaSumsCreditPacks(t *testing.T) {
	stubHost(t)
	var gotPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpEntUsage {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("ide_user_ent_usage should be POST, got %s", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotPayload)
		_, _ = w.Write([]byte(quotaFixture))
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)

	quota, errQuota := FetchQuota(context.Background(), testCredential(), cfg)
	if errQuota != nil {
		t.Fatalf("FetchQuota: %v", errQuota)
	}
	if gotPayload["require_usage"] != true {
		t.Errorf("require_usage = %v", gotPayload["require_usage"])
	}
	if gotPayload["req_source"] != float64(1) {
		t.Errorf("req_source = %v", gotPayload["req_source"])
	}
	values := map[string]float64{}
	for _, metric := range quota.Summary {
		values[metric.Key] = metric.Value
	}
	if values["credit_total"] != 4600 {
		t.Errorf("total quota = %v, want 4600 (packages with product_type=0 are excluded)", values["credit_total"])
	}
	if values["credit_used"] != 0.4944 {
		t.Errorf("used = %v, want 0.4944", values["credit_used"])
	}
	if values["credit_remain"] != 4599.5056 {
		t.Errorf("remaining = %v, want 4599.5056", values["credit_remain"])
	}
	if quota.Subscription == nil || quota.Subscription.Plan != "Trae CN Benefits" {
		t.Errorf("unexpected subscription: %+v", quota.Subscription)
	}
}

func TestFetchQuotaHandlesUnlimitedPack(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"product_type":2,"quota":{"credits_limit":-1}},"usage":{"credits_amount":12.5}}
		]}`))
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)

	quota, errQuota := FetchQuota(context.Background(), testCredential(), cfg)
	if errQuota != nil {
		t.Fatalf("FetchQuota: %v", errQuota)
	}
	for _, metric := range quota.Summary {
		if metric.Key == "credit_remain" && (metric.Value != -1 || metric.Unit != "unlimited") {
			t.Errorf("unlimited quota remaining should be marked as -1/unlimited, got %v/%s", metric.Value, metric.Unit)
		}
	}
}

func TestFetchQuotaGlobalUnsupported(t *testing.T) {
	stubHost(t)
	cfg := ConfigFor(RegionGlobal)
	_, errQuota := FetchQuota(context.Background(), testCredential(), cfg)
	if errQuota == nil {
		t.Fatal("Global version should report quota not supported")
	}
}

// ---- Refresh ----

func TestRefreshCredentialParsesResultEnvelope(t *testing.T) {
	stubHost(t)
	var gotPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpExchange {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotPayload)
		_, _ = w.Write([]byte(`{"Result":{"Token":"new-access","RefreshToken":"new-refresh","TokenExpireAt":1793613527094,"TokenExpireDuration":7200}}`))
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)

	updated, changed, errRefresh := RefreshCredential(context.Background(), testCredential(), cfg)
	if errRefresh != nil {
		t.Fatalf("RefreshCredential: %v", errRefresh)
	}
	if !changed {
		t.Error("should report changed")
	}
	if updated.AccessToken != "new-access" {
		t.Errorf("access token = %q", updated.AccessToken)
	}
	if updated.RefreshToken != "new-refresh" {
		t.Errorf("refresh token = %q", updated.RefreshToken)
	}
	// Normalize millisecond timestamps to seconds.
	if updated.ExpiresAt != 1793613527 {
		t.Errorf("expires_at = %d, want 1793613527", updated.ExpiresAt)
	}
	// PascalCase payload.
	if gotPayload["ClientID"] != ClientIDCN {
		t.Errorf("ClientID = %v, want %s", gotPayload["ClientID"], ClientIDCN)
	}
	if gotPayload["RefreshToken"] != "test-refresh-token" {
		t.Errorf("RefreshToken = %v", gotPayload["RefreshToken"])
	}
	if gotPayload["ClientSecret"] != "-" {
		t.Errorf("ClientSecret = %v", gotPayload["ClientSecret"])
	}
}

func TestRefreshCredentialFallsBackToDuration(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Result":{"Token":"new-access","TokenExpireAt":0,"TokenExpireDuration":7200}}`))
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)

	updated, changed, errRefresh := RefreshCredential(context.Background(), testCredential(), cfg)
	if errRefresh != nil || !changed {
		t.Fatalf("RefreshCredential: changed=%v err=%v", changed, errRefresh)
	}
	if updated.ExpiresAt <= 0 {
		t.Errorf("TokenExpireAt should be derived from duration when missing, got %d", updated.ExpiresAt)
	}
}

// ---- JWT Identity ----

func TestJWTIdentityExtractsAccountID(t *testing.T) {
	// The middle section (base64url) of the JWT for {"data":{"id":"2487573687317868"}}.
	token := "eyJhbGciOiJSUzI1NiJ9.eyJkYXRhIjp7ImlkIjoiMjQ4NzU3MzY4NzMxNzg2OCJ9LCJleHAiOjE3OTM2MTM1Mjd9.sig"
	if got := jwtIdentity(token); got != "2487573687317868" {
		t.Errorf("jwtIdentity = %q, want 2487573687317868", got)
	}
	if got := jwtIdentity("not-a-jwt"); got != "" {
		t.Errorf("non-JWT should return empty, got %q", got)
	}
}
