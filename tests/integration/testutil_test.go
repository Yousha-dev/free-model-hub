package tests

// This file provides a "fake host": replacing host callbacks with in-memory implementations,
// allowing the plugin's complete execution chain (credential parsing, model registration, outbound HTTP,
// task reporting, streaming forwarding) to be tested without a CPA process.
//
// Key design:
//   - installFakeHost uses t.Cleanup for complete restoration (including reducing retry backoff to milliseconds);
//   - resetPluginGlobals centrally clears package-level globals—the biggest pitfall in white-box testing is cache leakage across test cases;
//   - non-streaming responses are intentionally returned in the host's "untagged" format to cover the compatibility logic of rpcHostHTTPResponse.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/app"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/vendors/workbuddy"
	"free-model-hub/tests/testhost"
)

// abiEnvelope is the plugin response envelope (for testing; production side has unified to core).
type abiEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// testCallbackID is the host callback ID used for testing.
const testCallbackID = "test-callback"

// fakeHost is a thin wrapper around tests/testhost.Host: preserving existing lowercase method names
// to avoid rewriting test cases on a large scale; new packages/test cases should directly use testhost's exported API.
type fakeHost struct{ *testhost.Host }

// fakeUpstreamResponse is an alias for fake host responses (retaining compatibility with existing test cases).
type fakeUpstreamResponse = testhost.UpstreamResponse

func newFakeHost() *fakeHost { return &fakeHost{testhost.New()} }

func (h *fakeHost) call(callbackID, method string, payload any) (json.RawMessage, error) {
	return h.Host.Call(callbackID, method, payload)
}

func (h *fakeHost) addAccount(authIndex, name, credentialJSON string) {
	h.AddAccount(authIndex, name, credentialJSON)
}

func (h *fakeHost) setUpstream(fn func(method, url, body string) fakeUpstreamResponse) {
	h.SetUpstream(fn)
}

func (h *fakeHost) emittedChunks() []string { return h.EmittedChunks() }

func (h *fakeHost) streamErrors() []string { return h.StreamErrors() }

func (h *fakeHost) closedStreams() []string { return h.ClosedStreams() }

var testApp *app.App

// installFakeHost connects the fake host to the plugin's host call entry point.
func installFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	host := newFakeHost()
	hostbridge.Configure(host.call)
	t.Cleanup(func() { hostbridge.Configure(nil) })
	return host
}

// setupTestPlugin prepares an App instance using a temporary state directory.
func setupTestPlugin(t *testing.T, mutate ...func(*app.Config)) app.Config {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.LogLevel = "error"
	cfg.LogToFile = false
	for _, apply := range mutate {
		apply(&cfg)
	}
	instance, errNew := app.New(app.Options{Version: "test", Factories: app.BuiltinFactories()})
	if errNew != nil {
		t.Fatalf("new app: %v", errNew)
	}
	testApp = instance
	t.Cleanup(func() {
		instance.StopBackgroundWork()
		instance.WaitPluginStreams(0)
		testApp = nil
	})
	if errApply := instance.ApplyConfig(cfg); errApply != nil {
		t.Fatalf("apply config: %v", errApply)
	}
	if _, errState := instance.LoadState(cfg); errState != nil {
		t.Fatalf("load state: %v", errState)
	}
	return cfg
}

// resetPluginGlobals is kept empty: each test case creates an independent App, eliminating the need to clear globals.
func resetPluginGlobals(*testing.T) {}

// buildConfigYAML generates the configuration YAML that the host passes to plugin.register.
//
// Includes the enabled and priority keys appended by the host, verifying the plugin's tolerance for unknown fields.
func buildConfigYAML(cfg app.Config) []byte {
	var builder strings.Builder
	builder.WriteString("enabled: true\npriority: 1\n")
	fmt.Fprintf(&builder, "state_dir: %q\n", cfg.StateDir)
	fmt.Fprintf(&builder, "log_level: %s\n", cfg.LogLevel)
	fmt.Fprintf(&builder, "enabled_realms: %s\n", strings.Join(cfg.EnabledRealms, ","))
	if len(cfg.DisabledModels) > 0 {
		fmt.Fprintf(&builder, "disabled_models: %s\n", strings.Join(cfg.DisabledModels, ","))
	}
	if mode := cfg.Raw["prompt_mode"]; mode != "" {
		fmt.Fprintf(&builder, "prompt_mode: %s\n", mode)
	}
	return []byte(builder.String())
}

// registerRequest constructs the request body for plugin.register.
func registerRequest(t *testing.T, cfg app.Config) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(app.LifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	if errMarshal != nil {
		t.Fatalf("marshal register request: %v", errMarshal)
	}
	return raw
}

// sampleCredentialJSON constructs a credential JSON (nested format).
func sampleCredentialJSON(realm, uid string) string {
	return credentialJSON(realm, uid, "ent-1")
}

// personalCredentialJSON constructs a standard account credential without an enterprise tenant.
//
// The key difference between enterprise and personal accounts lies in enterpriseId: enterprise accounts have no check-in activities,
// and quotas go through the enterprise usage API, so "personal account" test cases must explicitly omit this field.
func personalCredentialJSON(realm, uid string) string {
	return credentialJSON(realm, uid, "")
}

func credentialJSON(realm, uid, enterpriseID string) string {
	region := workbuddy.NormalizeRegion(realm)
	enterprise := ""
	if enterpriseID != "" {
		enterprise = fmt.Sprintf(`"enterpriseId": %q, `, enterpriseID)
	}
	return fmt.Sprintf(`{
  "auth": {"accessToken": "at-%s", "refreshToken": "rt-%s", "expiresAt": %d, "domain": "%s", "realm": "%s"},
  "account": {%s"uid": "%s", "nickname": "账号%s"},
  "device_token": "dt-%s"
}`, uid, uid, time.Now().Add(24*time.Hour).Unix(), workbuddy.DefaultDomainFor(region), region, enterprise, uid, uid, uid)
}

// Utilities.

func mustMarshal(value any) []byte {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}

// decodeEnvelopeResultForTest unwraps the plugin response envelope (test helper).
func decodeEnvelopeResultForTest(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	if !env.OK {
		message := "unknown"
		if env.Error != nil {
			message = env.Error.Code + ": " + env.Error.Message
		}
		t.Fatalf("envelope is not ok: %s", message)
	}
	return env.Result
}

// callMethod invokes an ABI method and unwraps the result.
func callMethod(t *testing.T, method string, payload any) json.RawMessage {
	t.Helper()
	var request []byte
	if payload != nil {
		request = mustMarshal(payload)
	}
	raw, errHandle := testApp.HandleMethod(method, request)
	if errHandle != nil {
		t.Fatalf("%s: %v", method, errHandle)
	}
	return decodeEnvelopeResultForTest(t, raw)
}

// waitFor polls until a condition is met (avoiding flaky tests caused by fixed sleeps).
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}

// managementCall invokes a plugin management interface and returns the response body (body can be nil).
func managementCall(t *testing.T, method, path string) pluginapi.ManagementResponse {
	t.Helper()
	return managementCallWithBody(t, method, path, nil)
}

// managementCallWithBody invokes a plugin management interface with a JSON body.
func managementCallWithBody(t *testing.T, method, path string, body any) pluginapi.ManagementResponse {
	t.Helper()
	request := pluginapi.ManagementRequest{
		Method:  method,
		Path:    "/v0/management/plugins/free-model-hub" + path,
		Headers: http.Header{"X-Host-Callback-Id": []string{testCallbackID}},
	}
	if body != nil {
		request.Body = mustMarshal(body)
	}
	raw, errHandle := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(request))
	if errHandle != nil {
		t.Fatalf("%s %s: %v", method, path, errHandle)
	}
	var response pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelopeResultForTest(t, raw), &response); errUnmarshal != nil {
		t.Fatalf("decode management response: %v", errUnmarshal)
	}
	return response
}
