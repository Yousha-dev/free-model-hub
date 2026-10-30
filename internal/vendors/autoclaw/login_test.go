package autoclaw

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"13812345678":    "13812345678",
		" 138-1234-5678": "13812345678",
		"+8613812345678": "13812345678",
		"8613812345678":  "13812345678",
	}
	for raw, want := range cases {
		got, err := normalizePhone(raw)
		if err != nil || got != want {
			t.Fatalf("normalizePhone(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"12345", "23812345678", "1381234567a", ""} {
		if _, err := normalizePhone(bad); err == nil {
			t.Fatalf("normalizePhone(%q) should fail", bad)
		}
	}
}

func TestNormalizeSMSCode(t *testing.T) {
	if got, err := normalizeSMSCode(" 123456 "); err != nil || got != 123456 {
		t.Fatalf("normalizeSMSCode = %d, %v", got, err)
	}
	// Leading zeros don't matter: the server only looks at the numeric value.
	if got, err := normalizeSMSCode("000000"); err != nil || got != 0 {
		t.Fatalf("normalizeSMSCode(000000) = %d, %v", got, err)
	}
	if _, err := normalizeSMSCode("12345"); err == nil {
		t.Fatal("5-digit code should fail")
	}
	if _, err := normalizeSMSCode("12345a"); err == nil {
		t.Fatal("non-digit code should fail")
	}
}

func TestDescribeSMSLoginError(t *testing.T) {
	// 400001 is a parameter error rather than "incorrect verification code"—conflating them misdirects troubleshooting.
	if got := describeLoginError(loginFlowSMS, 400001, "请求数据有问题"); !strings.Contains(got, "parameters") {
		t.Fatalf("400001 message = %q", got)
	}
	if got := describeLoginError(loginFlowSMS, 630202, ""); !strings.Contains(got, "Incorrect verification code") {
		t.Fatalf("630202 message = %q", got)
	}
}

func TestMaskPhone(t *testing.T) {
	if got := MaskPhone("13812345678"); got != "138****5678" {
		t.Fatalf("MaskPhone = %q", got)
	}
	if got := MaskPhone("123"); got != "123" {
		t.Fatalf("MaskPhone(short) = %q", got)
	}
}

func TestSMSLoginProviderCapability(t *testing.T) {
	instances := Instances(core.Env{})
	// Login capability is dispatched by instance type: cn -> smsLoginAdapter, global -> oauthLoginAdapter.
	if _, cnOK := core.As[core.SMSLoginProvider](instances[0]); !cnOK {
		t.Fatal("cn instance should implement SMSLoginProvider")
	}
	if _, glSMS := core.As[core.SMSLoginProvider](instances[1]); glSMS {
		t.Fatal("global instance must NOT implement SMSLoginProvider (it has OAuth instead)")
	}
	if _, glLogin := core.As[core.LoginProvider](instances[1]); !glLogin {
		t.Fatal("global instance should implement LoginProvider")
	}
	if _, cnLogin := core.As[core.LoginProvider](instances[0]); cnLogin {
		t.Fatal("cn instance must NOT implement LoginProvider (it has SMS login instead)")
	}
}

func TestParseOAuthCallback(t *testing.T) {
	vendor, code, state, ok := ParseOAuthCallback("http://localhost:46952/auth/callback-google?code=ac1&state=up1")
	if !ok || vendor != oauthVendorGoogle || code != "ac1" || state != "up1" {
		t.Fatalf("google callback = %v/%v/%v/%v", vendor, code, state, ok)
	}
	vendor, code, _, ok = ParseOAuthCallback("http://localhost:18432/auth/callback-zai?code=ac2")
	if !ok || vendor != oauthVendorZai || code != "ac2" {
		t.Fatalf("zai callback = %v/%v/%v", vendor, code, ok)
	}
	if _, _, _, ok := ParseOAuthCallback("http://evil.com/auth/callback-google?code=x"); ok {
		t.Fatal("foreign host should be rejected")
	}
	if _, _, _, ok := ParseOAuthCallback("http://localhost:46952/other?code=x"); ok {
		t.Fatal("wrong path should be rejected")
	}
}

func TestOAuthNavigateURI(t *testing.T) {
	if got := OAuthNavigateURI(oauthVendorGoogle, 0); got != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("OAuthNavigateURI = %q", got)
	}
	if got := OAuthNavigateURI(oauthVendorZai, 18432); got != "http://localhost:18432/auth/callback-zai" {
		t.Fatalf("OAuthNavigateURI zai = %q", got)
	}
}

func TestLoginStartRegionGuards(t *testing.T) {
	instances := Instances(core.Env{})
	cn := instances[0]
	_ = instances[1]
	// Login capability is dispatched by instance type: cn only has SMS login (SMSLoginProvider),
	// global only has OAuth (LoginProvider + CallbackSubmitter).
	if _, okSMS := core.As[core.SMSLoginProvider](cn); !okSMS {
		t.Fatal("cn should implement SMSLoginProvider")
	}
	if _, okLogin := core.As[core.LoginProvider](cn); okLogin {
		t.Fatal("cn must NOT implement LoginProvider")
	}
	// Global OAuth step 2 enforces risk-control verification code (browser-side SDK), which cannot be completed on the plugin side—
	// LoginStart returns a guidance text page and does not declare LoginProvider (console falls back to credential entry).
}

// testContext returns a test background context.
func testContext() context.Context { return context.Background() }

// TestOAuthDefaultCallbackPorts locks the callback ports of two variants: Zai can only use one of the four
// official client-registered ports, while Google uses loose loopback matching.
func TestOAuthDefaultCallbackPorts(t *testing.T) {
	if got := OAuthNavigateURI(oauthVendorZai, 0); got != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("zai navigate uri = %q", got)
	}
	if got := OAuthNavigateURI(oauthVendorGoogle, 0); got != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("google navigate uri = %q", got)
	}
}

func TestParseOAuthCallbackAcceptsLoopbackAliases(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:46952/auth/callback-google?code=c1&state=s1",
		"http://[::1]:46952/auth/callback-google?code=c1&state=s1",
	} {
		if _, code, _, ok := ParseOAuthCallback(raw); !ok || code != "c1" {
			t.Fatalf("ParseOAuthCallback(%q) = %q, %v", raw, code, ok)
		}
	}
}

func TestOAuthCaptchaConfigParsesEnvelope(t *testing.T) {
	useLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != OAuthCaptchaConfigPath {
			t.Errorf("path = %q, want %q", r.URL.Path, OAuthCaptchaConfigPath)
		}
		if r.Header.Get("X-Auth-Sign") == "" {
			t.Error("captcha-config request missing signature header")
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"enabled":true,"region":"ga",
			"prefix":"sq51tr","scene_id":"18vhnjxl","captcha_supplier":"aliyun"}}`))
	}))
	defer server.Close()

	config, errConfig := OAuthCaptchaConfig(context.Background(), server.URL)
	if errConfig != nil {
		t.Fatalf("OAuthCaptchaConfig: %v", errConfig)
	}
	if !config.Enabled || config.SceneID != "18vhnjxl" || config.Region != "ga" || config.Prefix != "sq51tr" {
		t.Fatalf("config = %#v", config)
	}
}

func TestOAuthCaptchaConfigSurfacesUpstreamCode(t *testing.T) {
	useLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":630014,"msg":"Verification failed","data":null}`))
	}))
	defer server.Close()

	_, errConfig := OAuthCaptchaConfig(context.Background(), server.URL)
	if errConfig == nil || !strings.Contains(errConfig.Error(), "Risk control verification") {
		t.Fatalf("err = %v, want a risk-control verification hint", errConfig)
	}
}

func TestLoginStartRequiresCaptcha(t *testing.T) {
	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	// It does not matter if the request address is unreachable: missing verification code must be blocked before sending the request.
	oauth.baseURL = "http://127.0.0.1:1"
	_, errStart := oauth.LoginStart(testContext(), map[string]any{"oauth_variant": "google"})
	if errStart == nil || !strings.Contains(errStart.Error(), "CAPTCHA") {
		t.Fatalf("err = %v, want a CAPTCHA hint", errStart)
	}
}

func TestLoginStartRequestsOAuthURLAndRegistersSession(t *testing.T) {
	useLoopbackHost(t)
	var gotPath string
	var gotBody map[string]any
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"oauth_url":"https://accounts.example/oauth?x=1"}}`))
	}))
	defer server.Close()

	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	oauth.baseURL = server.URL
	response, errStart := oauth.LoginStart(testContext(), map[string]any{
		"oauth_variant":            "zai",
		"ali_captcha_verify_param": "captcha-token-1",
	})
	if errStart != nil {
		t.Fatalf("LoginStart: %v", errStart)
	}
	if response.URL != "https://accounts.example/oauth?x=1" {
		t.Fatalf("url = %q", response.URL)
	}
	if !strings.HasPrefix(response.State, oauth.ID()+"_oauth_") {
		t.Fatalf("state = %q", response.State)
	}
	if response.Metadata["callback_hint"] != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("metadata = %#v", response.Metadata)
	}
	if gotPath != OAuthURLPathZai {
		t.Fatalf("path = %q, want %q", gotPath, OAuthURLPathZai)
	}
	if gotBody["ali_captcha_verify_param"] != "captcha-token-1" {
		t.Fatalf("body = %#v", gotBody)
	}
	if gotBody["navigate_uri"] != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("navigate_uri = %#v", gotBody["navigate_uri"])
	}
	if gotBody["source_id"] != protocolName {
		t.Fatalf("source_id = %#v", gotBody["source_id"])
	}
	// Alibaba Cloud WAF blocks Go's default UA with a 405 HTML page (observed to recover just by changing the UA);
	// OAuth must also carry the official desktop fingerprint, otherwise the verification code token generated in the browser is rejected.
	if ua := gotHeaders.Get("User-Agent"); !strings.HasPrefix(ua, "AutoClaw/") || strings.Contains(ua, "Go-http-client") {
		t.Fatalf("User-Agent = %q, want AutoClaw desktop UA", ua)
	}
	if gotHeaders.Get("X-Client-Type") != "pc" || gotHeaders.Get("X-Harness-Type") != "zcode" {
		t.Fatalf("desktop fingerprint headers missing: %#v", gotHeaders)
	}

	entry, pollState := oauth.Env.Logins.Poll(response.State)
	if pollState != core.PollReady || entry == nil {
		t.Fatalf("session not registered: %v", pollState)
	}
	pending, okPending := entry.Data.(*autoclawPendingLogin)
	if !okPending {
		t.Fatal("session data type mismatch")
	}
	if pending.Vendor != oauthVendorZai || pending.NavigateURI != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("pending = %#v", pending)
	}
	if pending.DeviceID == "" || pending.DeviceID != gotBody["device_id"] {
		t.Fatalf("device id mismatch: pending=%q body=%#v", pending.DeviceID, gotBody["device_id"])
	}
}

func TestLoginStartRejectsUnknownVariant(t *testing.T) {
	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	_, errStart := oauth.LoginStart(testContext(), map[string]any{
		"oauth_variant": "github", "ali_captcha_verify_param": "x",
	})
	if errStart == nil || !strings.Contains(errStart.Error(), "unknown login method") {
		t.Fatalf("err = %v", errStart)
	}
}

func TestOAuthLoginCapabilitiesAndVariants(t *testing.T) {
	instances := Instances(envWithLogins())
	if _, okCaptcha := core.As[core.LoginCaptchaProvider](instances[1]); !okCaptcha {
		t.Fatal("global instance should implement LoginCaptchaProvider")
	}
	if _, okCaptcha := core.As[core.LoginCaptchaProvider](instances[0]); okCaptcha {
		t.Fatal("cn instance must NOT implement LoginCaptchaProvider")
	}
	variants, okVariants := core.As[core.LoginVariantProvider](instances[1])
	if !okVariants {
		t.Fatal("global instance should implement LoginVariantProvider")
	}
	list := variants.LoginVariants()
	if len(list) != 2 || list[0].ID != "zai" || list[1].ID != "google" {
		t.Fatalf("variants = %#v", list)
	}
	if list[1].CallbackHint != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("google hint = %q", list[1].CallbackHint)
	}
}

// TestSubmitCallbackExchangesCodeWithSessionNavigateURI locks the "paste callback → exchange code"
// chain: the 2nd and 3rd hops must use the same navigate_uri / device_id, otherwise Zai rejects
// it due to mismatched callback URL or invalid state.
func TestSubmitCallbackExchangesCodeWithSessionNavigateURI(t *testing.T) {
	useLoopbackHost(t)
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != OAuthLoginPathZai {
			t.Errorf("path = %q, want %q", r.URL.Path, OAuthLoginPathZai)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"access_token":"access-1",` +
			`"refresh_token":"refresh-1","user_id":42,"user_name":"tester"}}`))
	}))
	defer server.Close()

	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	oauth.baseURL = server.URL
	sessionID := oauth.ID() + "_oauth_test"
	navigateURI := "http://localhost:53699/auth/callback-zai"
	oauth.Env.Logins.Register(oauth.ID(), sessionID, time.Hour, 0, &autoclawPendingLogin{
		Region: RegionGlobal, Vendor: oauthVendorZai, DeviceID: "device-1", NavigateURI: navigateURI,
	})

	result, errSubmit := oauth.SubmitCallback(testContext(), sessionID,
		"http://localhost:53699/auth/callback-zai?code=code-1&state=upstream-state")
	if errSubmit != nil {
		t.Fatalf("SubmitCallback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, message = %q", result.Status, result.Message)
	}
	if result.Auth.FileName != "autoclawglobal-42.json" {
		t.Fatalf("file name = %q", result.Auth.FileName)
	}
	if gotBody["code"] != "code-1" || gotBody["state"] != "upstream-state" {
		t.Fatalf("body = %#v", gotBody)
	}
	if gotBody["navigate_uri"] != navigateURI || gotBody["device_id"] != "device-1" {
		t.Fatalf("navigate/device mismatch: %#v", gotBody)
	}
	var storage map[string]any
	if errUnmarshal := json.Unmarshal(result.Auth.StorageJSON, &storage); errUnmarshal != nil {
		t.Fatalf("decode storage: %v", errUnmarshal)
	}
	if storage["accessToken"] != "access-1" || storage["refreshToken"] != "refresh-1" {
		t.Fatalf("storage = %#v", storage)
	}
	if _, pollState := oauth.Env.Logins.Poll(sessionID); pollState != core.PollMissing {
		t.Fatalf("session should be consumed after success, state = %v", pollState)
	}
}

// TestLoginCredentialUIDChainCoversCamelCaseUserID locks the UID extraction chain: when a CN login
// response only returns camelCase userId and token is not a JWT, the UID must still fall on the credential—otherwise the credential filename
// would have to rely on a fallback, causing the second account to overwrite the first (#55).
func TestLoginCredentialUIDChainCoversCamelCaseUserID(t *testing.T) {
	cases := []struct {
		name       string
		data       loginResultData
		wantUID    string
		wantStored string
	}{
		{
			name:       "驼峰 userId 且 token 非 JWT",
			data:       loginResultData{AccessToken: "opaque-token", UserIDCamel: "8a22f603"},
			wantUID:    "8a22f603",
			wantStored: "8a22f603",
		},
		{
			name:       "蛇形 user_id 数字",
			data:       loginResultData{AccessToken: "opaque-token", UserID: float64(42)},
			wantUID:    "42",
			wantStored: "42",
		},
		{
			name:       "驼峰 userId 在蛇形 user_id 为空时补位",
			data:       loginResultData{AccessToken: "opaque-token", UserID: "", UserIDCamel: "u-camel"},
			wantUID:    "u-camel",
			wantStored: "u-camel",
		},
		{
			name:       "user_id 为 0 时不当作 UID",
			data:       loginResultData{AccessToken: "opaque-token", UserID: float64(0), UserIDCamel: "u-camel"},
			wantUID:    "u-camel",
			wantStored: "u-camel",
		},
		{
			name:       "两种写法同时出现时以蛇形为准",
			data:       loginResultData{AccessToken: "opaque-token", UserID: "u-snake", UserIDCamel: "u-camel"},
			wantUID:    "u-snake",
			wantStored: "u-snake",
		},
	}
	for _, testCase := range cases {
		cred, errCred := newLoginCredential(testCase.data, RegionCN, "device-1")
		if errCred != nil {
			t.Fatalf("%s: newLoginCredential: %v", testCase.name, errCred)
		}
		if got := cred.UIDValue(); got != testCase.wantUID {
			t.Fatalf("%s: UID = %q, want %q", testCase.name, got, testCase.wantUID)
		}
		// Persisted JSON's userId shares the same source as UID: identity is not lost when the host rereads the credential.
		storage, errStorage := cred.StorageJSON()
		if errStorage != nil {
			t.Fatalf("%s: StorageJSON: %v", testCase.name, errStorage)
		}
		var payload map[string]any
		if errUnmarshal := json.Unmarshal(storage, &payload); errUnmarshal != nil {
			t.Fatalf("%s: decode storage: %v", testCase.name, errUnmarshal)
		}
		if payload["userId"] != testCase.wantStored {
			t.Fatalf("%s: storage userId = %#v, want %q", testCase.name, payload["userId"], testCase.wantStored)
		}
	}
}

// TestLoginFileNamesAreStableAndUniqueWithoutJWTToken locks login file persistence names:
// same account re-login gives the same name (idempotent), different accounts give different names (no mutual overwrites), and none are fixed placeholder names.
func TestLoginFileNamesAreStableAndUniqueWithoutJWTToken(t *testing.T) {
	useLoopbackHost(t)
	userIDs := map[string]string{"first": "8a22f603", "second": "9b33a704"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Deliberately returning only camelCase userId and non-JWT token: this is precisely the observed response format for the CN version.
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"access_token":"opaque-` + body.Code +
			`","refresh_token":"refresh-` + body.Code + `","userId":"` + userIDs[body.Code] +
			`","user_name":"同名用户"}}`))
	}))
	defer server.Close()

	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	oauth.baseURL = server.URL
	submit := func(sessionSuffix, code string) string {
		t.Helper()
		sessionID := oauth.ID() + "_oauth_" + sessionSuffix
		oauth.Env.Logins.Register(oauth.ID(), sessionID, time.Hour, 0, &autoclawPendingLogin{
			Region: RegionGlobal, Vendor: oauthVendorGoogle, DeviceID: "device-1",
			NavigateURI: "http://localhost:46952/auth/callback-google",
		})
		result, errSubmit := oauth.SubmitCallback(testContext(), sessionID,
			"http://localhost:46952/auth/callback-google?code="+code)
		if errSubmit != nil {
			t.Fatalf("SubmitCallback(%s): %v", code, errSubmit)
		}
		return result.Auth.FileName
	}

	firstLogin := submit("s1", "first")
	secondAccount := submit("s2", "second")
	sameAccountAgain := submit("s3", "first")

	if firstLogin != sameAccountAgain {
		t.Fatalf("re-logging in the same account changed the filename: %q / %q (will result in duplicate accounts)", firstLogin, sameAccountAgain)
	}
	if firstLogin == secondAccount {
		t.Fatalf("different accounts fell into the same filename: %q (the latter login overwrites the previous one)", firstLogin)
	}
	if firstLogin != "autoclawglobal-8a22f603.json" {
		t.Fatalf("filename = %q, want autoclawglobal-8a22f603.json", firstLogin)
	}
	if secondAccount != "autoclawglobal-9b33a704.json" {
		t.Fatalf("filename = %q, want autoclawglobal-9b33a704.json", secondAccount)
	}
}

// ---------------------------------------------------------------------------
// Login test helpers: loopback host and session table
// ---------------------------------------------------------------------------

// This file provides loopback stubs for HTTP unit tests in the login chain: hostbridge.Client defaults to routing outbound
// through the host, but is replaced in tests with a forwarder connecting directly to httptest (same as useLoopbackHost in the codearts package).
//
// Both requests and responses are parsed using maps to avoid duplicating the host's JSON field names in test code;
// see internal/hostbridge/http.go for rpcHostHTTPRequest/Response field definitions.

func useLoopbackHost(t *testing.T) {
	t.Helper()
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, nil
		}
		rawPayload, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req map[string]any
		if errDecode := json.Unmarshal(rawPayload, &req); errDecode != nil {
			return nil, errDecode
		}
		methodText, _ := req["method"].(string)
		urlText, _ := req["url"].(string)
		var bodyReader io.Reader
		if encoded, okEncoded := req["body"].(string); okEncoded && encoded != "" {
			body, errBody := base64.StdEncoding.DecodeString(encoded)
			if errBody != nil {
				return nil, errBody
			}
			bodyReader = strings.NewReader(string(body))
		}
		httpReq, errNew := http.NewRequest(methodText, urlText, bodyReader)
		if errNew != nil {
			return nil, errNew
		}
		if rawHeaders, okHeaders := req["headers"].(map[string]any); okHeaders {
			for key, rawValues := range rawHeaders {
				values, _ := rawValues.([]any)
				for _, rawValue := range values {
					if value, okValue := rawValue.(string); okValue {
						httpReq.Header.Add(key, value)
					}
				}
			}
		}
		resp, errDo := http.DefaultClient.Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()
		body, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return nil, errRead
		}
		wrapped, errWrap := json.Marshal(map[string]any{
			"status_code": resp.StatusCode,
			"headers":     resp.Header,
			"body":        body,
		})
		if errWrap != nil {
			return nil, errWrap
		}
		return wrapped, nil
	})
}

// stubLoginSessions is an in-memory implementation of core.LoginSessions (for unit testing).
type stubLoginSessions struct {
	sessions map[string]*core.LoginSession
}

func newStubLoginSessions() *stubLoginSessions {
	return &stubLoginSessions{sessions: map[string]*core.LoginSession{}}
}

func (s *stubLoginSessions) Register(vendorID, sessionID string, ttl, interval time.Duration, data any) {
	s.sessions[sessionID] = &core.LoginSession{VendorID: vendorID, Data: data}
}

func (s *stubLoginSessions) Poll(sessionID string) (*core.LoginSession, core.PollState) {
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, core.PollMissing
	}
	return session, core.PollReady
}

func (s *stubLoginSessions) Owns(sessionID string) (string, bool) {
	session, ok := s.sessions[sessionID]
	if !ok {
		return "", false
	}
	return session.VendorID, true
}

func (s *stubLoginSessions) Delete(sessionID string) { delete(s.sessions, sessionID) }

// envWithLogins returns an Env with a session table (for login chain unit tests).
func envWithLogins() core.Env { return core.Env{Logins: newStubLoginSessions()} }

var _ = testing.Short
