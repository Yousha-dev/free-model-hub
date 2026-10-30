package tests

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/app"
	"free-model-hub/internal/vendors/workbuddy"
)

// TestModelsForAuthIsolatesRealms verifies the core mechanism of realm isolation.
//
// CN credentials only get cn:* models, and Global credentials only get global:*.
// Based on this, the host filters out cross-realm credentials during the credential selection phase
// (conductor_selection.go: authSupportsRouteModel -> ClientSupportsModel) -- this is the implementation point of dual-realm isolation.
func TestModelsForAuthIsolatesRealms(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.addAccount("2", "global-account", sampleCredentialJSON("global", "g1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		// CN /v3/config migrated along with ChatBase to www.workbuddy.cn (the old two realms are kept in conditions for fallback compatibility).
		if (strings.Contains(url, "workbuddy.cn") || strings.Contains(url, "codebuddy.cn") || strings.Contains(url, "copilot.tencent.com")) && strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"},{"id":"cn-exclusive","name":"CN Exclusive"}]}}`,
			}
		}
		if strings.Contains(url, "workbuddy.ai") && strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"},{"id":"global-exclusive","name":"Global Exclusive"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	// Test 1: CN credentials get the bare name glm-5.2 and cn-exclusive, never global-exclusive.
	res1 := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})
	var resp1 pluginapi.ModelResponse
	_ = json.Unmarshal(res1, &resp1)
	var ids1 []string
	for _, m := range resp1.Models {
		ids1 = append(ids1, m.ID)
	}
	if !containsString(ids1, "glm-5.2") || !containsString(ids1, "cn-exclusive") {
		t.Fatalf("cn auth missing models: %v", ids1)
	}
	if containsString(ids1, "global-exclusive") {
		t.Fatalf("cn auth must NOT contain global-exclusive: %v", ids1)
	}

	// Test 2: Global credentials get the bare name glm-5.2 and global-exclusive, never cn-exclusive.
	res2 := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "2"})
	var resp2 pluginapi.ModelResponse
	_ = json.Unmarshal(res2, &resp2)
	var ids2 []string
	for _, m := range resp2.Models {
		ids2 = append(ids2, m.ID)
	}
	if !containsString(ids2, "glm-5.2") || !containsString(ids2, "global-exclusive") {
		t.Fatalf("global auth missing models: %v", ids2)
	}
	if containsString(ids2, "cn-exclusive") {
		t.Fatalf("global auth must NOT contain cn-exclusive: %v", ids2)
	}

	// Test 3: Independent disabling by realm: after disabling cn:glm-5.2, CN credentials lose glm-5.2, while Global credentials still have glm-5.2.
	testApp.HandleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["cn:glm-5.2"],"disabled":true}`),
	})
	res1After := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})
	var resp1After pluginapi.ModelResponse
	_ = json.Unmarshal(res1After, &resp1After)
	var ids1After []string
	for _, m := range resp1After.Models {
		ids1After = append(ids1After, m.ID)
	}
	if containsString(ids1After, "glm-5.2") {
		t.Fatalf("cn auth must not contain disabled cn:glm-5.2, got: %v", ids1After)
	}

	res2After := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "2"})
	var resp2After pluginapi.ModelResponse
	_ = json.Unmarshal(res2After, &resp2After)
	var ids2After []string
	for _, m := range resp2After.Models {
		ids2After = append(ids2After, m.ID)
	}
	if !containsString(ids2After, "glm-5.2") {
		t.Fatalf("global auth should still contain glm-5.2 after disabling only cn: %v", ids2After)
	}
}

// TestStaticModelsBareAndMerged verifies that static catalogs use bare model names and automatically deduplicate and merge identical names.
func TestStaticModelsBareAndMerged(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "workbuddycn-test.json", sampleCredentialJSON("cn", "test"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body: `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2",
				  "credits":"x0.05","maxInputTokens":1000000,"maxOutputTokens":131072,
				  "supportsImages":true,"supportsToolCall":true,
				  "reasoning":{"supportedEfforts":["high","max"],"defaultEffort":"high"}}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	result := callMethod(t, pluginabi.MethodModelStatic, pluginapi.StaticModelRequest{})
	var response pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode static models: %v", errUnmarshal)
	}
	countGLM := 0
	var glmModel pluginapi.ModelInfo
	for _, model := range response.Models {
		if model.ID == "glm-5.2" {
			countGLM++
			glmModel = model
		}
		if strings.HasPrefix(model.ID, "cn:") || strings.HasPrefix(model.ID, "global:") {
			t.Fatalf("model ID must be bare, got %q", model.ID)
		}
	}
	if countGLM != 1 {
		t.Fatalf("glm-5.2 must be merged into exactly 1 model entry, got %d", countGLM)
	}
	if glmModel.ContextLength != 1000000 {
		t.Fatalf("glm-5.2 context_length = %d, want 1000000", glmModel.ContextLength)
	}
	if glmModel.Thinking == nil || len(glmModel.Thinking.Levels) == 0 {
		t.Fatal("glm-5.2 must expose reasoning levels")
	}

	// Models from providers without added credentials (such as opencodezen / cline / qoderglobal) should not appear in the model list.
	for _, m := range response.Models {
		if strings.Contains(strings.ToLower(m.ID), "cline-free") ||
			strings.Contains(strings.ToLower(m.ID), "qmodel") {
			t.Fatalf("model %q from unconfigured vendor must not be present in static models", m.ID)
		}
	}
}

// TestPrepareBodyPromptModes verifies the three prompt modes (passthrough/custom/append) in a single Pass and bare model name rewriting.
func TestPrepareBodyPromptModes(t *testing.T) {
	baseJSON := []byte(`{"model":"cn:glm-5.2","messages":[{"role":"system","content":"old sys"},{"role":"user","content":"hello"}]}`)

	// 1. passthrough
	outPass := workbuddy.PrepareBody(baseJSON, workbuddy.PrepareOptions{
		Model:      "glm-5.2",
		PromptMode: "passthrough",
		PromptText: "custom prompt",
	})
	var mapPass map[string]any
	_ = json.Unmarshal(outPass, &mapPass)
	if mapPass["model"] != "glm-5.2" {
		t.Fatalf("model must be rewritten to bare name, got: %v", mapPass["model"])
	}
	msgsPass := mapPass["messages"].([]any)
	if msgsPass[0].(map[string]any)["content"] != "old sys" {
		t.Fatalf("passthrough should keep old sys, got: %v", msgsPass[0])
	}

	// 2. custom
	outCustom := workbuddy.PrepareBody(baseJSON, workbuddy.PrepareOptions{
		Model:      "glm-5.2",
		PromptMode: "custom",
		PromptText: "pure prompt",
	})
	var mapCustom map[string]any
	_ = json.Unmarshal(outCustom, &mapCustom)
	msgsCustom := mapCustom["messages"].([]any)
	if len(msgsCustom) != 2 || msgsCustom[0].(map[string]any)["content"] != "pure prompt" {
		t.Fatalf("custom should replace sys, got: %v", msgsCustom)
	}

	// 3. append
	outAppend := workbuddy.PrepareBody(baseJSON, workbuddy.PrepareOptions{
		Model:      "glm-5.2",
		PromptMode: "append",
		PromptText: "appended rule",
	})
	var mapAppend map[string]any
	_ = json.Unmarshal(outAppend, &mapAppend)
	msgsAppend := mapAppend["messages"].([]any)
	if len(msgsAppend) != 3 || msgsAppend[1].(map[string]any)["content"] != "appended rule" {
		t.Fatalf("append should insert prompt after leading sys, got: %v", msgsAppend)
	}
}

// TestDisabledModelsConfig verifies the parsing format and enforcement behavior of the disabled_models YAML configuration.
func TestDisabledModelsConfig(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	// 1. Block list syntax, supporting three prefix forms
	yamlBlock := []byte(`
enabled_realms: cn,global
disabled_models:
  - workbuddycn:glm-5.2
  - cn:deepseek-v4-pro
  - qwen-3.5-plus
`)
	cfg, errDecode := testApp.DecodeConfig(yamlBlock)
	if errDecode != nil {
		t.Fatalf("decodeConfig failed: %v", errDecode)
	}
	if errApply := testApp.ApplyConfig(cfg); errApply != nil {
		t.Fatalf("applyConfig failed: %v", errApply)
	}

	// Provider-scoped rules only affect that specific provider
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("workbuddycn:glm-5.2 must be disabled")
	}
	if testApp.IsModelDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("workbuddyglobal:glm-5.2 should NOT be disabled")
	}
	// Region aliases expand to all providers in that region
	if !testApp.IsModelDisabled("workbuddycn", "deepseek-v4-pro") {
		t.Fatal("workbuddycn:deepseek-v4-pro must be disabled by cn: prefix")
	}
	if testApp.IsModelDisabled("workbuddyglobal", "deepseek-v4-pro") {
		t.Fatal("workbuddyglobal:deepseek-v4-pro should NOT be disabled by cn: prefix")
	}
	// Bare names apply to all providers
	if !testApp.IsModelDisabled("workbuddycn", "qwen-3.5-plus") || !testApp.IsModelDisabled("workbuddyglobal", "qwen-3.5-plus") {
		t.Fatal("bare name must be disabled on all vendors")
	}

	// 2. Flow list syntax
	yamlFlow := []byte(`
enabled_realms: cn,global
disabled_models: [workbuddyglobal:glm-5.2]
`)
	cfgFlow, errDecodeFlow := testApp.DecodeConfig(yamlFlow)
	if errDecodeFlow != nil {
		t.Fatalf("decode flow config failed: %v", errDecodeFlow)
	}
	if errApply := testApp.ApplyConfig(cfgFlow); errApply != nil {
		t.Fatalf("applyConfig flow failed: %v", errApply)
	}
	if !testApp.IsModelDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("flow list workbuddyglobal:glm-5.2 must be disabled")
	}
	// Overall replacement of new configuration: the previous round of cn restriction no longer takes effect
	if testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("workbuddycn:glm-5.2 should not be disabled after reconfigure")
	}
}

// TestStaleAliasDoesNotDisableCurrentModel reproduces a real-world bug after upgrading:
//
// A provider changed its external model ID (Qoder changed from an upstream key to a normalized slug),
// while the user state still retains the old key alias (qodercn:dfmodel -> deepseek-flash) and old key disable entry (qodercn:dfmodel).
// If restored as before, the alias would map the request back to dfmodel and hit the disabled check,
// reporting "model is disabled by plugin configuration" -- even though it is clearly enabled in the list.
// Expected: Stale keys become inactive for the current model, but configuration in the state is preserved (must not be lost when catalog probing fails).
func TestStaleAliasDoesNotDisableCurrentModel(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"qodercn:dfmodel","alias":"deepseek-flash"}`),
	})
	testApp.HandleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["qodercn:dfmodel"],"disabled":true}`),
	})
	// The upstream catalog only contains the new normalized ID: dfmodel no longer exists.
	testApp.CacheStaticModels("qodercn", []pluginapi.ModelInfo{{ID: "deepseek-flash"}})

	if got := testApp.ResolveOutboundModel("qodercn", "deepseek-flash"); got != "deepseek-flash" {
		t.Fatalf("stale alias must be ignored, resolve = %q", got)
	}
	if testApp.IsModelDisabled("qodercn", "deepseek-flash") {
		t.Fatal("stale disabled key must not disable the current model id")
	}
	// Configuration retention: user settings must not be cleared when the catalog cannot be retrieved.
	if testApp.ModelAliasFor("qodercn", "dfmodel") != "deepseek-flash" {
		t.Fatal("stale alias must stay in state")
	}
}

// TestConfigAndStateDisabledModelsUnion verifies the union of configuration declarations and page operations:
// Enabling a model on the page cannot override disabled items declared in the configuration.
func TestConfigAndStateDisabledModelsUnion(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)
	cfg.DisabledModels = []string{"cn:glm-5.2"}
	if errApply := testApp.ApplyConfig(cfg); errApply != nil {
		t.Fatalf("applyConfig failed: %v", errApply)
	}
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config-declared disable must take effect")
	}

	// The page attempts to enable the same model: the configuration declaration remains, and the disable status is not lifted.
	testApp.HandleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["cn:glm-5.2"],"disabled":false}`),
	})
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("page-side enable must NOT lift a config-declared disable")
	}

	// The page disables another model: both sources coexist, and both are intercepted.
	testApp.HandleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["workbuddycn:glm-5.3"],"disabled":true}`),
	})
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.3") {
		t.Fatal("page-side disable must take effect")
	}
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config disable must survive the page operation")
	}
}

// TestModelAliasScopedAndRouting verifies the two semantics of aliases:
//   - An alias is an external ID effective **within a provider** (unique within the same provider, used to rebind old models);
//   - Outbound requests translate back to official IDs based on the provider (cross-provider same-name aliases route back to their respective models).
func TestModelAliasScopedAndRouting(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// Bind an identical alias to two providers: the host will merge them into a single ID and schedule between the two.
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.2","alias":"glm-max"}`),
	})
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"qodercn:glm-5.3","alias":"glm-max"}`),
	})

	if got := testApp.ModelAliasFor("workbuddycn", "glm-5.2"); got != "glm-max" {
		t.Fatalf("workbuddycn alias = %q, want glm-max", got)
	}
	if got := testApp.ModelAliasFor("qodercn", "glm-5.3"); got != "glm-max" {
		t.Fatalf("qodercn alias = %q, want glm-max", got)
	}
	// The scope is a single model ID: other models of the same provider are unaffected.
	if got := testApp.ModelAliasFor("workbuddycn", "glm-5.3"); got != "" {
		t.Fatalf("unrelated model alias = %q, want empty", got)
	}

	// Outbound: the same alias is translated back to the respective official ID according to the requesting provider.
	if got := testApp.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.2" {
		t.Fatalf("testApp.ResolveOutboundModel(workbuddycn) = %q, want glm-5.2", got)
	}
	if got := testApp.ResolveOutboundModel("qodercn", "glm-max"); got != "glm-5.3" {
		t.Fatalf("testApp.ResolveOutboundModel(qodercn) = %q, want glm-5.3", got)
	}
	// Names without an alias are returned as-is (Qoder registers with a normalized display_name).
	if got := testApp.ResolveOutboundModel("qodercn", "glm-5.3"); got != "glm-5.3" {
		t.Fatalf("passthrough = %q, want glm-5.3", got)
	}

	// Aliases are unique within a provider: rebinding an alias to another model clears the old binding.
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.3","alias":"glm-max"}`),
	})
	if got := testApp.ModelAliasFor("workbuddycn", "glm-5.2"); got != "" {
		t.Fatalf("rebind must clear the old binding, got %q", got)
	}
	if got := testApp.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.3" {
		t.Fatalf("after rebind resolveOutboundModel = %q, want glm-5.3", got)
	}

	// Clearing the alias (empty string) restores the official ID.
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.3","alias":""}`),
	})
	if got := testApp.ModelAliasFor("workbuddycn", "glm-5.3"); got != "" {
		t.Fatalf("clear alias failed, got %q", got)
	}
}

// TestModelAliasSwapRegistersAliasID verifies that aliases are registered as the external ID of the model.
func TestModelAliasSwapRegistersAliasID(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	in := []pluginapi.ModelInfo{
		{ID: "qwen3.8-max", Name: "Qwen3.8-Max", DisplayName: "Qwen3.8-Max"},
		{ID: "glm-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"},
	}
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"qodercn:qwen3.8-max","alias":"qwen-max"}`),
	})

	out := testApp.ApplyModelAliases("qodercn", in)
	if out[0].ID != "qwen-max" || out[0].Name != "qwen-max" || out[0].DisplayName != "qwen-max" {
		t.Fatalf("aliased model must expose the alias as ID/Name: %+v", out[0])
	}
	// Models without set aliases keep their official IDs as-is.
	if out[1].ID != "glm-5.3" || out[1].Name != "GLM-5.3" {
		t.Fatalf("unaliased model must keep official ID: %+v", out[1])
	}
}

// TestRewritePayloadModel verifies that the model in the outbound request body is rewritten to the official ID.
func TestRewritePayloadModel(t *testing.T) {
	out := app.RewritePayloadModel([]byte(`{"model":"gmodel","messages":[]}`), "gm51model")
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(out, &decoded); errUnmarshal != nil {
		t.Fatalf("decode rewritten payload: %v", errUnmarshal)
	}
	if decoded["model"] != "gm51model" {
		t.Fatalf("payload model = %v, want gm51model", decoded["model"])
	}

	// Return invalid JSON as-is (preventing local parsing failures from interrupting a conversation).
	bad := []byte(`not json`)
	if got := app.RewritePayloadModel(bad, "gmodel"); string(got) != string(bad) {
		t.Fatalf("invalid payload must pass through unchanged, got %q", string(got))
	}
}

// TestModelChangeMarksRestartPending verifies that disabling/alias changes set the "pending" flag,
// and that the host automatically clears it when rebuilding the model registry (applyModelCatalog).
func TestModelChangeMarksRestartPending(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	if testApp.SnapshotState().RestartPending {
		t.Fatal("fresh state must not be restart-pending")
	}

	// Disable model: set the flag.
	testApp.HandleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["workbuddycn:glm-5.2"],"disabled":true}`),
	})
	if !testApp.SnapshotState().RestartPending {
		t.Fatal("toggle must mark restart-pending")
	}

	// Cleared when the host rebuilds the registry (register/reconfigure path).
	testApp.ApplyModelCatalog(cfg)
	if testApp.SnapshotState().RestartPending {
		t.Fatal("applyModelCatalog must clear restart-pending")
	}

	// Alias changes also set the flag, and the status payload includes it so the page displays a persistent prompt accordingly.
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.2","alias":"glm-max"}`),
	})
	if !testApp.SnapshotState().RestartPending {
		t.Fatal("alias change must mark restart-pending")
	}
	payload := testApp.BuildStatusPayload(pluginapi.ManagementRequest{})
	if payload["restart_pending"] != true {
		t.Fatalf("status payload restart_pending = %v, want true", payload["restart_pending"])
	}
}

// TestDefaultStateDirFallback verifies that when ~/.cli-proxy-api exists, its subdirectory is preferred.
func TestDefaultStateDirFallback(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("USERPROFILE", tempHome)
	t.Setenv("FREE_MODEL_HUB_HOME", "")

	// 1. No ~/.cli-proxy-api → fall back to ~/.free-model-hub
	dir1 := app.DefaultStateDir()
	want1 := filepath.Join(tempHome, ".free-model-hub")
	if dir1 != want1 {
		t.Fatalf("defaultStateDir = %q, want %q", dir1, want1)
	}

	// 2. ~/.cli-proxy-api exists (Docker mount scenario) → use its free-model-hub subdirectory
	cpaDir := filepath.Join(tempHome, ".cli-proxy-api")
	if errMkdir := os.MkdirAll(cpaDir, 0o755); errMkdir != nil {
		t.Fatalf("create temp cpa dir: %v", errMkdir)
	}
	dir2 := app.DefaultStateDir()
	want2 := filepath.Join(cpaDir, "free-model-hub")
	if dir2 != want2 {
		t.Fatalf("defaultStateDir with cpa dir = %q, want %q", dir2, want2)
	}
}

// containsString reports whether the slice contains a certain value.
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// keysOf returns the keys of the map (used for error messages).
func keysOf(source map[string]pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(source))
	for key := range source {
		out = append(out, key)
	}
	return out
}
