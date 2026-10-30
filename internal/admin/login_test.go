package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// postLoginCallback calls the admin route and decodes the response body.
func postLoginCallback(t *testing.T, service *Service, state, callbackURL string) map[string]any {
	t.Helper()
	request := pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/free-model-hub/login/callback",
		Body:   []byte(`{"state":"` + state + `","url":"` + callbackURL + `"}`),
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	raw, errHandle := service.Handle(raw)
	if errHandle != nil {
		t.Fatalf("handle login callback: %v", errHandle)
	}
	var envelope struct {
		Result struct {
			StatusCode int
			Body       []byte
		}
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(envelope.Result.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v (body=%s)", errUnmarshal, envelope.Result.Body)
	}
	return payload
}

// TestRegistrationDeclaresLoginCallbackRoute ensures the paste callback entrypoint must be declared in routes:
// The host allows subpaths under /plugins/<id>/... based on this table; missing registration results in a 404.
func TestRegistrationDeclaresLoginCallbackRoute(t *testing.T) {
	for _, route := range newService(t).Registration().Routes {
		if route.Method == http.MethodPost && route.Path == "/plugins/free-model-hub/login/callback" {
			return
		}
	}
	t.Fatalf("login/callback route missing: %#v", newService(t).Registration().Routes)
}

// TestHandleLoginCallbackReturnsVendorResult verifies the success path: the status and message provided by the vendor
// are returned to the page as-is; CodeArts portal login URL must also be returned (so the page can render a clickable link).
func TestHandleLoginCallbackReturnsVendorResult(t *testing.T) {
	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{
		Status:      pluginapi.AuthLoginStatusPending,
		Message:     "等待用户完成华为云登录…",
		RedirectURL: "https://codearts.huaweicloud.com/portal/login?ticket_id=x",
	}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "codearts_x",
		"http://127.0.0.1/oauth/callback?secret=s")

	if payload["status"] != string(pluginapi.AuthLoginStatusPending) {
		t.Fatalf("status = %v, want pending", payload["status"])
	}
	if payload["ok"] != false {
		t.Fatalf("ok = %v, want false", payload["ok"])
	}
	if payload["redirect_url"] == "" {
		t.Fatal("redirect_url not returned: user has no next step when CodeArts ticket fails to exchange for credentials")
	}
}

// TestHandleLoginCallbackSurfacesVendorError verifies the failure path: reasons provided by the vendor should be directly
// displayed to the user rather than turning into a technical response.
func TestHandleLoginCallbackSurfacesVendorError(t *testing.T) {
	deps := newStubDeps(t)
	deps.callbackErr = core.NewPluginError("callback_mismatch",
		"回调链接与当前登录会话不匹配，请重新发起登录后重新授权", http.StatusBadRequest)
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "traecn_x",
		"http://127.0.0.1:18080/authorize?loginTraceID=t")

	message, _ := payload["message"].(string)
	if !strings.Contains(message, "回调链接与当前登录会话不匹配") {
		t.Fatalf("message = %q, want readable reason provided by the provider", message)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusError) {
		t.Fatalf("status = %v, want error", payload["status"])
	}
}

// TestHandleLoginCallbackRejectsMissingState enforces input validation: requests without state must not be
// passed through to the vendor (otherwise a single link could be used against all sessions).
func TestHandleLoginCallbackRejectsMissingState(t *testing.T) {
	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{Status: pluginapi.AuthLoginStatusSuccess}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "", "http://127.0.0.1/oauth/callback?secret=s")
	if payload["ok"] != false {
		t.Fatalf("should not succeed when state is missing: %v", payload)
	}
}

// TestHandleLoginCallbackPersistsCredential locks down "successful login → credentials are actually persisted".
//
// Credentials for the paste path only return to the **page**: the normal polling path has the host write files
// after auth.login.poll, whereas the paste path lacks that host step. Without this, the page shows "Login successful"
// while the auth directory is empty and no new account appears in the account overview.
func TestHandleLoginCallbackPersistsCredential(t *testing.T) {
	var savedName string
	var savedJSON []byte
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthSave {
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var request pluginapi.HostAuthSaveRequest
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		savedName, savedJSON = request.Name, request.JSON
		return json.RawMessage("{}"), nil
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })

	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:    "freehub",
			FileName:    "traecn-user-1.json",
			Label:       "Trae-tester",
			StorageJSON: []byte("{\"type\":\"freehub\",\"vendor\":\"traecn\"}"),
		},
	}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "traecn_x",
		"http://127.0.0.1:18080/authorize?loginTraceID=t")

	if payload["ok"] != true {
		t.Fatalf("should report success after successful disk write: %v", payload)
	}
	if savedName != "traecn-user-1.json" {
		t.Fatalf("host.auth.save filename = %q, want traecn-user-1.json", savedName)
	}
	if !strings.Contains(string(savedJSON), "traecn") {
		t.Fatalf("persisted content lacks ownership field: %s", savedJSON)
	}
	if payload["auth_id"] != "traecn-user-1.json" {
		t.Fatalf("response did not report auth_id: %v", payload)
	}
}

// TestHandleLoginCallbackReportsPersistFailure ensures persist failures are reported honestly: when both the host callback and
// persistence fail, the page must not show "Login successful" while nothing is on disk.
func TestHandleLoginCallbackReportsPersistFailure(t *testing.T) {
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		return nil, fmt.Errorf("host unavailable")
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })

	// Fallback directory pointing to a non-existent path to force persistence to fail (without writing to the test package directory).
	original := authFileDirs
	authFileDirs = []string{filepath.Join(t.TempDir(), "missing")}
	t.Cleanup(func() { authFileDirs = original })

	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   pluginapi.AuthData{FileName: "traecn-x.json", StorageJSON: []byte("{}")},
	}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "traecn_x",
		"http://127.0.0.1:18080/authorize?loginTraceID=t")

	if payload["ok"] != false {
		t.Fatalf("a failed save must not report success: %v", payload)
	}
	message, _ := payload["message"].(string)
	if !strings.Contains(message, "failed to save credentials") {
		t.Fatalf("message = %q, want it to explain the save failure", message)
	}
}

// loginRoutePayload calls a login-related admin route and decodes the response body.
func loginRoutePayload(t *testing.T, service *Service, request pluginapi.ManagementRequest) map[string]any {
	t.Helper()
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	raw, errHandle := service.Handle(raw)
	if errHandle != nil {
		t.Fatalf("handle %s: %v", request.Path, errHandle)
	}
	var envelope struct {
		Result struct {
			StatusCode int
			Body       []byte
		}
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(envelope.Result.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v (body=%s)", errUnmarshal, envelope.Result.Body)
	}
	return payload
}

// TestRegistrationDeclaresLoginStartRoutes locks down the two in-page login entrypoints: the host permits plugin
// subpaths based on route declarations, and missing registration results in a 404 (the page only sees "Failed to get authorization link").
func TestRegistrationDeclaresLoginStartRoutes(t *testing.T) {
	found := map[string]bool{}
	for _, route := range newService(t).Registration().Routes {
		found[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		"POST /plugins/free-model-hub/login/start",
		"GET /plugins/free-model-hub/login/captcha-config",
	} {
		if !found[want] {
			t.Fatalf("route %q missing: %#v", want, found)
		}
	}
}

func TestHandleLoginStartReturnsAuthorizationURL(t *testing.T) {
	deps := newStubDeps(t)
	deps.loginStart = &pluginapi.AuthLoginStartResponse{
		URL:      "https://accounts.example/oauth",
		State:    "autoclawglobal_oauth_abc",
		Metadata: map[string]any{"callback_hint": "http://localhost:46952/auth/callback-google"},
	}
	payload := loginRoutePayload(t, newServiceWithDeps(t, deps), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/free-model-hub/login/start",
		Body:   []byte(`{"vendor_id":"autoclawglobal","metadata":{"oauth_variant":"google","ali_captcha_verify_param":"p"}}`),
	})
	if payload["ok"] != true || payload["url"] != "https://accounts.example/oauth" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["callback_hint"] != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("callback_hint = %#v", payload["callback_hint"])
	}
}

// TestHandleLoginStartSurfacesVendorError ensures upstream errors must be returned to the page as-is:
// when the captcha expires (630014), the user needs to see "Please drag again" instead of an HTTP 500.
func TestHandleLoginStartSurfacesVendorError(t *testing.T) {
	deps := newStubDeps(t)
	deps.loginStartErr = core.NewPluginError("login_failed", "风控验证未通过，请重新点击登录再试一次", http.StatusBadRequest)
	payload := loginRoutePayload(t, newServiceWithDeps(t, deps), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/free-model-hub/login/start",
		Body:   []byte(`{"vendor_id":"autoclawglobal"}`),
	})
	if payload["ok"] != false {
		t.Fatalf("ok = %v", payload["ok"])
	}
	message, _ := payload["message"].(string)
	if !strings.Contains(message, "风控验证未通过") {
		t.Fatalf("message = %q", message)
	}
}

func TestHandleLoginCaptchaConfigRequest(t *testing.T) {
	deps := newStubDeps(t)
	deps.captcha = core.CaptchaConfig{Enabled: true, SceneID: "scene-1", Region: "ga", Prefix: "sq51tr"}
	payload := loginRoutePayload(t, newServiceWithDeps(t, deps), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/free-model-hub/login/captcha-config",
		Query:  map[string][]string{"vendor": {"autoclawglobal"}},
	})
	if payload["ok"] != true {
		t.Fatalf("payload = %#v", payload)
	}
	captcha, _ := payload["captcha"].(map[string]any)
	if captcha["enabled"] != true || captcha["scene_id"] != "scene-1" {
		t.Fatalf("captcha = %#v", captcha)
	}
}

func TestHandleLoginCaptchaConfigRejectsMissingVendor(t *testing.T) {
	payload := loginRoutePayload(t, newService(t), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/free-model-hub/login/captcha-config",
	})
	if payload["ok"] != false {
		t.Fatalf("payload = %#v", payload)
	}
}

// captchaLoginVendor extends fakeVendor with two "in-page login" capabilities.
type captchaLoginVendor struct {
	fakeVendor
}

func (v *captchaLoginVendor) LoginVariants() []core.LoginVariant {
	return []core.LoginVariant{
		{ID: "zai", Label: "Zai", CallbackHint: "http://localhost:53699/auth/callback-zai"},
		{ID: "google", Label: "Google", CallbackHint: "http://localhost:46952/auth/callback-google"},
	}
}
func (v *captchaLoginVendor) LoginCaptchaConfig(context.Context) (core.CaptchaConfig, error) {
	return core.CaptchaConfig{Enabled: true, SceneID: "scene"}, nil
}
func (v *captchaLoginVendor) LoginStart(context.Context, map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return &pluginapi.AuthLoginStartResponse{}, nil
}
func (v *captchaLoginVendor) LoginPoll(context.Context, string) (*pluginapi.AuthLoginPollResponse, error) {
	return &pluginapi.AuthLoginPollResponse{}, nil
}
func (v *captchaLoginVendor) LocalCallbackHint() string {
	return "http://localhost:46952/auth/callback-google"
}
func (v *captchaLoginVendor) SubmitCallback(context.Context, string, string) (*core.CallbackSubmitResult, error) {
	return &core.CallbackSubmitResult{}, nil
}

// TestVendorsPayloadExposesCaptchaLogin locks down the two descriptions required for page rendering: only vendors
// that declare capabilities will display the "captcha then login" panel, while variants and callback URLs come from the backend
// (the page does not hardcode AutoClaw).
func TestVendorsPayloadExposesCaptchaLogin(t *testing.T) {
	deps := newStubDeps(t)
	vendor := &captchaLoginVendor{fakeVendor{Meta: core.Meta{
		VendorID: "autoclawglobal", VendorName: "AutoClaw 国际版", VendorRegion: "global",
	}}}
	if errRegister := deps.registry.Register(vendor); errRegister != nil {
		t.Fatal(errRegister)
	}
	payload := newServiceWithDeps(t, deps).buildVendorsPayload()
	vendors, _ := payload["vendors"].([]vendorDescriptor)
	if len(vendors) != 1 {
		t.Fatalf("vendors = %#v", payload["vendors"])
	}
	got := vendors[0]
	if !got.LoginCaptcha || !got.SupportsLogin || !got.PasteCallback {
		t.Fatalf("descriptor = %#v", got)
	}
	if len(got.LoginVariants) != 2 || got.LoginVariants[0].ID != "zai" {
		t.Fatalf("variants = %#v", got.LoginVariants)
	}
}
