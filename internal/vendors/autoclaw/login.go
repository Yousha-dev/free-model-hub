// This file implements AutoClaw's full login capabilities: CN SMS verification code, Global OAuth web login,
// and regional instance dispatching.
//
// Login channels must be centralized in a single login.go, splitting into login_*.go is prohibited: the console,
// capability detection, and testing are all understood as "one login entry per provider".

package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// ---------------------------------------------------------------------------
// Regional dispatch
// ---------------------------------------------------------------------------

// Provider instance = protocol × region, login methods vary by region (core's interface isolation discipline:
// only implement capabilities you genuinely possess, the console renders different add forms accordingly):
//   - smsLoginAdapter (CN version): core.SMSLoginProvider (phone number + verification code);
//   - oauthLoginAdapter (Global version): core.LoginProvider + core.CallbackSubmitter
//     + core.LoginVariantProvider + core.LoginCaptchaProvider
//     (Zai / Google OAuth web login: in-page CAPTCHA + callback link pasting).
// The shared adapter itself does not implement any login interfaces—attaching SMS methods to the shared type
// would cause the Global version to also show a phone number form in the console (which would be rejected at runtime, but the UI lies).

// SendLoginCode sends an SMS verification code (CN version only, core.SMSLoginProvider).
func (sms *smsLoginAdapter) SendLoginCode(ctx context.Context, phone string) (string, error) {
	if sms.region.IsGlobal() {
		return "", core.NewPluginError("login_unsupported",
			"AutoClaw Global does not support phone number login, please use 'Web Login (Google)' or 'Enter Credentials' instead", http.StatusBadRequest)
	}
	return SendLoginCode(ctx, sms.resolveBaseURL(), phone)
}

// VerifyLoginCode completes login using the verification code (CN version only).
func (sms *smsLoginAdapter) VerifyLoginCode(ctx context.Context, sessionKey, phone, code string) ([]byte, string, error) {
	if sms.region.IsGlobal() {
		return nil, "", core.NewPluginError("login_unsupported",
			"AutoClaw Global does not support phone number login, please use 'Web Login (Google)' or 'Enter Credentials' instead", http.StatusBadRequest)
	}
	cred, errLogin := VerifyLoginCode(ctx, sms.resolveBaseURL(), sessionKey, phone, code)
	if errLogin != nil {
		return nil, "", core.ToPluginError(errLogin)
	}
	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, "", core.NewPluginError("login_failed", "failed to build credential: "+errStorage.Error(), http.StatusInternalServerError)
	}
	label := core.FirstNonEmpty(cred.UserNameValue(), MaskPhone(cred.Phone), cred.UIDValue())
	return storageJSON, label, nil
}

// autoclawPendingLogin represents an ongoing Global OAuth login session.
//
// LoginStart puts deviceID and variants into the session table; SubmitCallback retrieves them and exchanges
// the code/state from the callback for a token (state uses the value returned by the upstream, official clients pass it back as-is).
type autoclawPendingLogin struct {
	Region      Region
	Vendor      oauthVendor
	DeviceID    string
	NavigateURI string
}

// LoginStart exchanges the token obtained after completing CAPTCHA in the console for an OAuth authorization URL.
//
// Call chain (aligned with Agent2API providers/autoclaw/oauth.rs):
//
//	Console loads Alibaba Cloud SDK → user drags slider → POST {oauth-url}
//	  {source_id, device_id, navigate_uri, ali_captcha_verify_param}
//	→ data.oauth_url (which is the login page the user needs to open)
//
// The verification code in step 2 is generated on the **browser side**, so empty tokens are not accepted here: without it, upstream
// will only return a misleading 631002 ("Current version has discontinued service"), so it's better to clarify at the entry point.
// The session is registered in the public session table, and SubmitCallback uses state to retrieve this session's device_id /
// navigate_uri / variant to exchange for a code.
func (oauth *oauthLoginAdapter) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	variant, errVariant := oauthVariantFromMetadata(meta)
	if errVariant != nil {
		return nil, errVariant
	}
	captcha := core.GetString(meta, "ali_captcha_verify_param")
	if captcha == "" {
		return nil, core.NewPluginError("captcha_required",
			"Please complete the CAPTCHA (Alibaba Cloud slider) in the console first before getting the authorization link", http.StatusBadRequest)
	}
	if oauth.Env.Logins == nil {
		return nil, core.NewPluginError("login_unavailable",
			"Login session table unavailable, unable to initiate login", http.StatusInternalServerError)
	}
	deviceID := core.RandomHex(32)
	navigateURI := OAuthNavigateURI(variant, 0)
	authURL, errURL := OAuthRequestURL(ctx, oauth.resolveBaseURL(), variant, navigateURI, captcha, deviceID)
	if errURL != nil {
		return nil, core.ToPluginError(errURL)
	}
	state := oauth.ID() + "_oauth_" + core.RandomHex(16)
	oauth.Env.Logins.Register(oauth.ID(), state, autoclawOAuthTTL, autoclawOAuthPollInterval,
		&autoclawPendingLogin{
			Region:      oauth.region,
			Vendor:      variant,
			DeviceID:    deviceID,
			NavigateURI: navigateURI,
		})
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       authURL,
		State:     state,
		ExpiresAt: time.Now().Add(autoclawOAuthTTL),
		Metadata: map[string]any{
			core.VendorKey:  oauth.ID(),
			"region":        string(oauth.region),
			"oauth_variant": string(variant),
			"callback_hint": navigateURI,
		},
	}, nil
}

// LoginVariants provides the Zai and Google entries to the console (the page does not hardcode provider details).
func (oauth *oauthLoginAdapter) LoginVariants() []core.LoginVariant { return OAuthVariants() }

// LoginCaptchaConfig returns the initialization parameters for the Alibaba Cloud widget (scene_id / region / prefix).
func (oauth *oauthLoginAdapter) LoginCaptchaConfig(ctx context.Context) (core.CaptchaConfig, error) {
	return OAuthCaptchaConfig(ctx, oauth.resolveBaseURL())
}

// LoginPoll polls the Global OAuth login status once (waiting for the user to paste the callback link).
func (oauth *oauthLoginAdapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	entry, pollState := oauth.Env.Logins.Poll(state)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session does not exist or has expired, please restart login",
		}, nil
	}
	label := oauthVendorGoogle.label()
	hint := OAuthNavigateURI(oauthVendorGoogle, 0)
	if pending, okPending := entry.Data.(*autoclawPendingLogin); okPending {
		label = pending.Vendor.label()
		hint = pending.NavigateURI
	}
	return &pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusPending,
		Message: "Waiting for user to complete " + label + " login on the authorization page... After login, the browser will stop at " + hint +
			", please copy the entire link from the address bar back to the console to submit",
	}, nil
}

// LocalCallbackHint returns the local callback address to be displayed in the console.
//
// The two variants have different ports (see OAuthNavigateURI), the page displays the CallbackHint from LoginVariants
// per variant; Google's address is provided here as a fallback.
func (oauth *oauthLoginAdapter) LocalCallbackHint() string {
	return OAuthNavigateURI(oauthVendorGoogle, 0)
}

// SubmitCallback parses the callback link pasted by the user and completes the Global OAuth token exchange.
func (oauth *oauthLoginAdapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	sessionID := strings.TrimSpace(state)
	entry, pollState := oauth.Env.Logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return nil, core.NewPluginError("session_not_found",
			"Login session does not exist or has expired, please initiate login again", http.StatusNotFound)
	}
	pending, okPending := entry.Data.(*autoclawPendingLogin)
	if !okPending {
		return nil, core.NewPluginError("login_session_broken", "Login session data is corrupted, please initiate login again", http.StatusBadRequest)
	}
	vendor, code, upstreamState, okCode := ParseOAuthCallback(rawURL)
	if !okCode {
		return nil, core.NewPluginError("callback_parse_failed",
			"Failed to parse callback URL, please make sure you paste the complete URL (including code=…) where the browser stopped after login completes",
			http.StatusBadRequest)
	}
	if vendor != pending.Vendor {
		return nil, core.NewPluginError("callback_mismatch",
			"The callback URL does not match the login method used for this session, please initiate login again", http.StatusBadRequest)
	}
	cred, errExchange := OAuthExchangeCode(ctx, oauth.resolveBaseURL(), vendor, code, upstreamState, pending.NavigateURI, pending.DeviceID)
	if errExchange != nil {
		return nil, core.ToPluginError(errExchange)
	}
	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, core.NewPluginError("login_failed", "Failed to build credential: "+errStorage.Error(), http.StatusInternalServerError)
	}
	oauth.Env.Logins.Delete(sessionID)
	label := core.FirstNonEmpty(cred.UserNameValue(), cred.UIDValue())
	return &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "AutoClaw login successful",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(oauth.ID(), cred.UIDValue(), cred.AccessTokenValue()),
			Label:       label,
			StorageJSON: storageJSON,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// CN edition: Phone number + SMS verification code
// ---------------------------------------------------------------------------

// Protocol basis: Agent2API providers/autoclaw/login.rs (desktop app.asar reverse engineered + verified):
//
//	POST {userapi}/userapi/v1/agent-send-code  {"phone","source_id","device_id"}
//	  → code=0 and data.result=true means it was sent successfully;
//	POST {userapi}/userapi/v1/agent-login/     {"phone","code","platform":"web",
//	                                            "source_id","device_id"}
//	  → data {access_token, refresh_token, user_id, user_name, ...}
//
// Verified logic (do not change back):
//   - The code in the body must be a JSON number; a string will result in a 400001
//     parameter error unrelated to verification code errors;
//   - The trailing slash on the agent-login path is required by the upstream (server 307s to no slash);
//   - Both calls must use the same device_id (upstream binds the verification code to the device);
//   - The Global edition does not have this flow (SMS verification entry officially removed), reject at the entry point.

// Endpoint for the SMS login flow (under the userapi domain).
const (
	SendCodePath = "/userapi/v1/agent-send-code"
	// The trailing slash of LoginPath is required by the upstream.
	LoginPath = "/userapi/v1/agent-login/"
)

// Login request timeout: SMS interface is occasionally slow, allow 20 seconds; OAuth flow 15 seconds.
const (
	smsLoginTimeout   = 20 * time.Second
	oauthLoginTimeout = 15 * time.Second
)

// loginFlow distinguishes different handling suggestions for the same business code under SMS / OAuth flows.
type loginFlow uint8

const (
	loginFlowSMS loginFlow = iota
	loginFlowOAuth
)

// describeLoginError translates upstream business codes into user-friendly reasons.
//
// 400001 is a parameter error rather than 'incorrect verification code'—conflating the two
// misleads troubleshooting (passing code as a string consistently yields this code). 631002 / 630014 next steps
// differ between flows: SMS can only fall back to entering credentials, OAuth can re-trigger login for CAPTCHA.
func describeLoginError(flow loginFlow, code int64, upstreamMsg string) string {
	switch code {
	case 400002:
		return "Request signature verification failed (local clock may have drift), please calibrate system time and retry"
	case 631002:
		if flow == loginFlowOAuth {
			return "This login lacks risk control verification (verification code not passed or expired), please click login again and retry"
		}
		return "Upstream requires risk control verification, this login cannot be completed here; please switch to 'Enter Credentials' (can be retrieved from AutoClaw client login state file)"
	case 630014:
		if flow == loginFlowOAuth {
			return "Risk control verification failed, please click login again and retry"
		}
		return "Risk control verification failed, please try again later; if failure persists, please switch to 'Enter Credentials'"
	}
	if flow == loginFlowOAuth {
		if code == 631001 {
			return "Authorization code is invalid or expired, please click login again"
		}
	} else {
		switch code {
		case 400001:
			return "Invalid request parameters, please check the phone number and verification code format and retry"
		case 630202:
			return "Incorrect verification code, please check and retry"
		case 630101:
			return "Fetching verification code too frequently, please try again later"
		case 410000, 400000:
			return "Login state has expired, please log in again"
		}
	}
	if msg := strings.TrimSpace(upstreamMsg); msg != "" {
		return "Login failed: " + msg
	}
	return fmt.Sprintf("Login failed (upstream code=%s)", strconv.FormatInt(code, 10))
}

// normalizePhone normalizes mainland China phone numbers: tolerates +86 / 86 prefix, spaces, and hyphens,
// requires 11 digits starting with 1[2-9].
func normalizePhone(raw string) (string, error) {
	var builder strings.Builder
	for _, ch := range raw {
		if ch != ' ' && ch != '-' {
			builder.WriteRune(ch)
		}
	}
	trimmed := builder.String()
	trimmed = strings.TrimPrefix(trimmed, "+86")
	if rest, okRest := strings.CutPrefix(trimmed, "86"); okRest && len(rest) == 11 {
		trimmed = rest
	}
	if len(trimmed) == 11 && strings.HasPrefix(trimmed, "1") && trimmed[1] >= '2' && trimmed[1] <= '9' {
		valid := true
		for _, ch := range trimmed {
			if ch < '0' || ch > '9' {
				valid = false
				break
			}
		}
		if valid {
			return trimmed, nil
		}
	}
	return "", core.NewPluginError("invalid_request", "Please enter an 11-digit Mainland China phone number", http.StatusBadRequest)
}

// normalizeSMSCode validates a 6-digit numeric verification code and converts it to a number.
//
// Upstream requires code in the body to be a JSON number (passing a string yields 400001 parameter error),
// Leading zeros don't matter—the server only looks at the numeric value.
func normalizeSMSCode(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) != 6 {
		return 0, core.NewPluginError("invalid_request", "Please enter a 6-digit verification code", http.StatusBadRequest)
	}
	value, errParse := strconv.ParseInt(trimmed, 10, 64)
	if errParse != nil {
		return 0, core.NewPluginError("invalid_request", "Please enter a 6-digit verification code", http.StatusBadRequest)
	}
	return value, nil
}

// postLoginJSON sends an anonymous login POST request (no token during login).
//
// SMS and OAuth only differ in request headers and timeout; marshal, send, read response,
// and error classification are completely identical, so they are centralized here to avoid drift between two separate implementations.
func postLoginJSON(ctx context.Context, baseURL, path string, body any, what string, timeout time.Duration, applyHeaders func(*http.Request, string)) (json.RawMessage, error) {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal %s request: %w", what, errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, strings.NewReader(string(encoded)))
	if errReq != nil {
		return nil, fmt.Errorf("build %s request: %w", what, errReq)
	}
	applyHeaders(req, "")

	resp, errDo := hostbridge.Client(ctx, timeout).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: what + ": " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read " + what + " response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	return json.RawMessage(raw), nil
}

// loginEnvelope parses the business envelope {code, msg, data}; returns null when data is missing.
func loginEnvelope(raw json.RawMessage) (code int64, msg string, data json.RawMessage) {
	var envelope struct {
		Code *int64          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return 0, "", json.RawMessage("null")
	}
	if envelope.Code != nil {
		code = *envelope.Code
	}
	if len(envelope.Data) == 0 {
		envelope.Data = json.RawMessage("null")
	}
	return code, envelope.Msg, envelope.Data
}

// SendLoginCode sends an SMS verification code (CN version only).
//
// device_id is generated by this function and returned with sessionKey—the upstream binds the verification code to the device,
// and VerifyLoginCode must use the exact same value.
func SendLoginCode(ctx context.Context, baseURL, phone string) (sessionKey string, err error) {
	phone, errPhone := normalizePhone(phone)
	if errPhone != nil {
		return "", errPhone
	}
	deviceID := core.RandomHex(32)
	raw, errPost := postLoginJSON(ctx, baseURL, SendCodePath, map[string]any{
		"phone":     phone,
		"source_id": protocolName,
		"device_id": deviceID,
	}, "send verification code", smsLoginTimeout, ApplyUserAPIHeaders)
	if errPost != nil {
		return "", core.ToPluginError(errPost)
	}
	code, msg, payload := loginEnvelope(raw)
	if code != 0 {
		return "", core.NewPluginError("login_failed", describeLoginError(loginFlowSMS, code, msg), http.StatusBadRequest)
	}
	// The original implementation also requires data.result to be true (code=0 with a false result is still considered a failure).
	var data struct {
		Result bool `json:"result"`
	}
	_ = json.Unmarshal(payload, &data)
	if !data.Result {
		return "", core.NewPluginError("login_failed", "Failed to send verification code, please try again later", http.StatusBadGateway)
	}
	return deviceID, nil
}

// VerifyLoginCode completes login using phone number + verification code, returning the saved credential.
//
// sessionKey is the device_id returned by SendLoginCode; if not provided, a new one is generated
// (the upstream accepts the "direct login with new device" pattern).
func VerifyLoginCode(ctx context.Context, baseURL, sessionKey, phone, code string) (*Credential, error) {
	phone, errPhone := normalizePhone(phone)
	if errPhone != nil {
		return nil, errPhone
	}
	codeNum, errCode := normalizeSMSCode(code)
	if errCode != nil {
		return nil, errCode
	}
	deviceID := strings.TrimSpace(sessionKey)
	if deviceID == "" {
		deviceID = core.RandomHex(32)
	}
	raw, errPost := postLoginJSON(ctx, baseURL, LoginPath, map[string]any{
		"phone":     phone,
		"code":      codeNum,
		"platform":  "web",
		"source_id": protocolName,
		"device_id": deviceID,
	}, "login", smsLoginTimeout, ApplyUserAPIHeaders)
	if errPost != nil {
		return nil, core.ToPluginError(errPost)
	}
	envCode, msg, payload := loginEnvelope(raw)
	if envCode != 0 {
		return nil, core.NewPluginError("login_failed", describeLoginError(loginFlowSMS, envCode, msg), http.StatusBadRequest)
	}
	var data loginResultData
	if errUnmarshal := json.Unmarshal(payload, &data); errUnmarshal != nil {
		return nil, &core.Failure{Msg: "parse login response: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	cred, errCred := newLoginCredential(data, RegionCN, deviceID)
	if errCred != nil {
		return nil, errCred
	}
	cred.Phone = phone
	return cred, nil
}

// loginResultData defines the login response fields shared by both SMS and OAuth pipelines.
//
// It is grouped into a named type because both pipelines previously copied separate anonymous structs, making field additions prone to omissions;
// user_id captures both snake_case and camelCase: actual testing of the CN version returned userId, and reading only user_id would cause UID
// to always be empty, causing account filename collisions (#55).
type loginResultData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       any    `json:"user_id"`
	UserIDCamel  any    `json:"userId"`
	UserName     string `json:"user_name"`
}

// newLoginCredential normalizes the response fields shared by both login pipelines into a credential.
//
// When the upstream login response lacks expires_in, it conservatively defaults to 28 days; the UID is taken from the response's
// user_id / userId, falling back to reading from the access token's JWT payload if missing—the CN version's token
// is not a JWT, so the response field is the primary path.
func newLoginCredential(data loginResultData, region Region, deviceID string) (*Credential, error) {
	access := strings.TrimSpace(data.AccessToken)
	if access == "" {
		return nil, core.NewPluginError("login_failed", "Login succeeded but upstream did not return an access_token", http.StatusBadGateway)
	}
	cred := &Credential{
		AccessToken:  access,
		RefreshToken: strings.TrimSpace(data.RefreshToken),
		UserName:     strings.TrimSpace(data.UserName),
		DeviceID:     deviceID,
		Region:       region,
		ExpiresAt:    core.JWTExpiry(access),
	}
	if cred.ExpiresAt == 0 {
		cred.ExpiresAt = time.Now().Add(28 * 24 * time.Hour).Unix()
	}
	cred.UID = core.FirstNonEmpty(uidFromAny(data.UserID, data.UserIDCamel), core.JWTUserID(access))
	return cred, nil
}

// uidFromAny normalizes upstream user_id / userId (number or string) into the first non-empty string.
func uidFromAny(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		case float64:
			// 0 represents the state where the "field exists but is empty", which cannot be used as a UID—otherwise all such accounts
			// would end up colliding on <vendor>-0.json.
			if typed > 0 {
				return strconv.FormatInt(int64(typed), 10)
			}
		}
	}
	return ""
}

// MaskPhone masks a phone number (keeping the first 3 and last 4 digits).
func MaskPhone(phone string) string {
	chars := []rune(strings.TrimSpace(phone))
	if len(chars) < 7 {
		return phone
	}
	return string(chars[:3]) + "****" + string(chars[len(chars)-4:])
}

// ---------------------------------------------------------------------------
// Global version: Zai / Google OAuth web login
// ---------------------------------------------------------------------------

// Global version protocol (Agent2API providers/autoclaw/oauth.rs, desktop reverse engineering + live testing):
//
//	① POST {intl}/userapi/overseasv1/oauth-captcha-config {}
//	     → data.enabled is a toggle for whether "this build should use OAuth" (false for CN);
//	② POST {intl}/userapi/overseasv1/{zai|google}-oauth-url
//	     {source_id, device_id, navigate_uri, ali_captcha_verify_param}
//	     → data.oauth_url;
//	③ Browser login → 302 to {navigate_uri}?code=…&state=…
//	   POST {intl}/userapi/overseasv1/{zai|google}-oauth-login
//	     {source_id, device_id, code, state, navigate_uri} → token pair.
//
// Bottlenecks and solutions (Revised Oct 2026):
//   - Step ② strictly requires Alibaba Cloud risk control CAPTCHA (browser-side JS SDK, absence leads to a misleading
//     631002). The plugin console **itself is our own page**, allowing us to directly load
//     the official SDK from o.alicdn.com (the same widget set used by ZCode for "claiming plans"), passing
//     captchaVerifyParam is passed to LoginStart and then forwarded as-is to the upstream here.
//   - Google's callback whitelist uses RFC 8252 loopback loose matching (any port), and the browser
//     stops at http://localhost:46952/auth/callback-google?code=… after login completion;
//     When the plugin runs on a remote host/Docker, this address cannot reach the server, so the user
//     just copies the entire link from the address bar to exchange for the code (Step 3 does not require a captcha, tested).
//   - Zai's whitelist only recognizes the four registered ports of the official client
//     (18432/19654/19723/53699). Here 53699 is fixed: when the official client starts, it first occupies
//     the first free port (usually 18432), and picking the last one avoids the callback being captured by the desktop app.
//
// Therefore, in this implementation: both Zai and Google go through the complete flow (captcha -> auth URL -> paste callback -> exchange code).

// Global OAuth endpoint (hosted under the userapi domain).
const (
	OAuthCaptchaConfigPath = "/userapi/overseasv1/oauth-captcha-config"
	OAuthURLPathZai        = "/userapi/overseasv1/zai-oauth-url"
	OAuthURLPathGoogle     = "/userapi/overseasv1/google-oauth-url"
	OAuthLoginPathZai      = "/userapi/overseasv1/zai-oauth-login"
	OAuthLoginPathGoogle   = "/userapi/overseasv1/google-oauth-login"
)

// oauthCallbackPrefix is the callback path prefix (exact match with the official client).
const oauthCallbackPrefix = "/auth/callback-"

// oauthVendor identifies the two variants of Global OAuth.
type oauthVendor string

const (
	oauthVendorZai    oauthVendor = "zai"
	oauthVendorGoogle oauthVendor = "google"
)

// urlPath / loginPath return the two endpoints for the variant.
func (v oauthVendor) urlPath() string {
	if v == oauthVendorZai {
		return OAuthURLPathZai
	}
	return OAuthURLPathGoogle
}

func (v oauthVendor) loginPath() string {
	if v == oauthVendorZai {
		return OAuthLoginPathZai
	}
	return OAuthLoginPathGoogle
}

// label returns the name of that button on the interface.
func (v oauthVendor) label() string {
	if v == oauthVendorZai {
		return "Zai"
	}
	return "Google"
}

// OAuthCaptchaConfig gets the risk control verification configuration (the console uses this to initialize the Alibaba Cloud SDK).
//
// Returning enabled=false is not an error: upstream international builds may disable this login method, and the entry should
// honestly tell the user "this provider does not have web login", rather than leading everyone to an inevitably failing slider.
func OAuthCaptchaConfig(ctx context.Context, baseURL string) (core.CaptchaConfig, error) {
	raw, errPost := postLoginJSON(ctx, baseURL, OAuthCaptchaConfigPath, struct{}{}, "get risk control verification config", oauthLoginTimeout, ApplyOAuthHeaders)
	if errPost != nil {
		return core.CaptchaConfig{}, core.ToPluginError(errPost)
	}
	code, msg, payload := loginEnvelope(raw)
	if code != 0 {
		return core.CaptchaConfig{}, core.NewPluginError("login_failed", describeLoginError(loginFlowOAuth, code, msg), http.StatusBadGateway)
	}
	var data struct {
		Enabled bool   `json:"enabled"`
		Region  string `json:"region"`
		Prefix  string `json:"prefix"`
		SceneID string `json:"scene_id"`
	}
	if errUnmarshal := json.Unmarshal(payload, &data); errUnmarshal != nil {
		return core.CaptchaConfig{}, &core.Failure{Msg: "parse oauth captcha config: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	return core.CaptchaConfig{
		Enabled: data.Enabled,
		SceneID: strings.TrimSpace(data.SceneID),
		Region:  strings.TrimSpace(data.Region),
		Prefix:  strings.TrimSpace(data.Prefix),
	}, nil
}

// OAuthRequestURL gets the OAuth authorization URL and returns the official login page URL.
//
// captchaVerifyParam is the opaque string obtained by the console page after running the Alibaba Cloud SDK, and the gateway
// does not parse it, only forwards it. Google can pass empty in deployment modes without a captcha component — the upstream
// risk control requirements for google-oauth-url are the same as Zai's, but Google's callback whitelist is loose,
// and some deployments can skip it in practice; upon failure, fallback to "Enter credentials" following error 631002.
func OAuthRequestURL(ctx context.Context, baseURL string, vendor oauthVendor, navigateURI, captchaVerifyParam, deviceID string) (string, error) {
	captcha := strings.TrimSpace(captchaVerifyParam)
	if captcha == "" {
		// Block locally: hitting upstream without it will inevitably result in that misleading 631002,
		// so it's better to make it clear here that "the captcha step is not completed".
		return "", core.NewPluginError("captcha_required", "missing risk control verification parameter, please complete the human-machine verification first", http.StatusBadRequest)
	}
	body := map[string]any{
		"source_id":                protocolName,
		"device_id":                deviceID,
		"navigate_uri":             navigateURI,
		"ali_captcha_verify_param": captcha,
	}
	raw, errPost := postLoginJSON(ctx, baseURL, vendor.urlPath(), body, "get "+vendor.label()+" authorization URL", oauthLoginTimeout, ApplyOAuthHeaders)
	if errPost != nil {
		return "", core.ToPluginError(errPost)
	}
	code, msg, payload := loginEnvelope(raw)
	if code != 0 {
		return "", core.NewPluginError("login_failed", describeLoginError(loginFlowOAuth, code, msg), http.StatusBadRequest)
	}
	var data struct {
		OAuthURL string `json:"oauth_url"`
	}
	if errUnmarshal := json.Unmarshal(payload, &data); errUnmarshal != nil {
		return "", &core.Failure{Msg: "parse oauth url response: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	if strings.TrimSpace(data.OAuthURL) == "" {
		return "", core.NewPluginError("login_failed", "upstream did not return an authorization URL, please retry", http.StatusBadGateway)
	}
	return strings.TrimSpace(data.OAuthURL), nil
}

// OAuthExchangeCode exchanges the one-time authorization code from the callback for credentials.
//
// state is the value returned by the **upstream** callback (not generated by the plugin) — the official client
// passes it back as-is; pass an empty string when it is empty (the upstream accepts this format).
func OAuthExchangeCode(ctx context.Context, baseURL string, vendor oauthVendor, code, state, navigateURI, deviceID string) (*Credential, error) {
	body := map[string]any{
		"source_id":    protocolName,
		"device_id":    deviceID,
		"code":         strings.TrimSpace(code),
		"state":        strings.TrimSpace(state),
		"navigate_uri": navigateURI,
	}
	raw, errPost := postLoginJSON(ctx, baseURL, vendor.loginPath(), body, vendor.label()+" login to exchange for credentials", oauthLoginTimeout, ApplyOAuthHeaders)
	if errPost != nil {
		return nil, core.ToPluginError(errPost)
	}
	status, msg, payload := loginEnvelope(raw)
	if status != 0 {
		return nil, core.NewPluginError("login_failed", describeLoginError(loginFlowOAuth, status, msg), http.StatusBadRequest)
	}
	var data loginResultData
	if errUnmarshal := json.Unmarshal(payload, &data); errUnmarshal != nil {
		return nil, &core.Failure{Msg: "parse oauth login response: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	return newLoginCredential(data, RegionGlobal, deviceID)
}

// Callback port: Zai can only use those four ports registered in the official client (see file header),
// while Google uses RFC 8252 loopback matching with any port.
const (
	oauthCallbackPortZai    = 53699
	oauthCallbackPortGoogle = 46952
)

// OAuthNavigateURI constructs the callback address for this login (also used as navigate_uri sent to upstream).
//
// host must be localhost (Zai whitelist strictly validates host); when port<=0, take the default
// port according to the variant. The plugin does not listen on this port: under remote/Docker deployment, the browser stops at a "page cannot be opened" page,
// and the user can just paste back the entire link from the address bar.
func OAuthNavigateURI(vendor oauthVendor, port int) string {
	if port <= 0 {
		if vendor == oauthVendorZai {
			port = oauthCallbackPortZai
		} else {
			port = oauthCallbackPortGoogle
		}
	}
	return fmt.Sprintf("http://localhost:%d%s%s", port, oauthCallbackPrefix, vendor)
}

// OAuthVariants returns the two login entries to be rendered by the console (order matches the official client: Zai first).
func OAuthVariants() []core.LoginVariant {
	return []core.LoginVariant{
		{ID: string(oauthVendorZai), Label: "Zai", CallbackHint: OAuthNavigateURI(oauthVendorZai, 0)},
		{ID: string(oauthVendorGoogle), Label: "Google", CallbackHint: OAuthNavigateURI(oauthVendorGoogle, 0)},
	}
}

// oauthVariantFromMetadata parses the variant identifier passed from the console.
//
// Default to Google: The old host path (directly clicking "Add Authentication") won't carry this field, and Google
// is the only variant that can complete the callback on any port.
func oauthVariantFromMetadata(meta map[string]any) (oauthVendor, error) {
	raw := strings.ToLower(core.GetString(meta, "oauth_variant"))
	switch raw {
	case "", string(oauthVendorGoogle):
		return oauthVendorGoogle, nil
	case string(oauthVendorZai):
		return oauthVendorZai, nil
	}
	return "", core.NewPluginError("invalid_request", "unknown login method: "+raw, http.StatusBadRequest)
}

// ParseOAuthCallback extracts code / state / variant from the callback link.
//
// Accepts two forms: /auth/callback-zai|google (navigate_uri constructed by this plugin) and
// the official client query form. Code is one-time use, state is returned as-is (upstream's state is needed for token exchange).
func ParseOAuthCallback(raw string) (vendor oauthVendor, code, state string, ok bool) {
	raw = strings.TrimSpace(raw)
	if index := strings.IndexAny(raw, "\r\n"); index >= 0 {
		raw = raw[:index]
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		return "", "", "", false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
	default:
		// The host of navigate_uri must be a loopback address (Zai whitelist strictly validates localhost;
		// any other host with the same path is a forged callback). When users copy from the address bar,
		// some browsers/proxies normalize localhost to 127.0.0.1 or [::1], so accept those as well.
		return "", "", "", false
	}
	if !strings.HasPrefix(parsed.Path, oauthCallbackPrefix) {
		return "", "", "", false
	}
	switch strings.TrimPrefix(parsed.Path, oauthCallbackPrefix) {
	case string(oauthVendorZai):
		vendor = oauthVendorZai
	case string(oauthVendorGoogle):
		vendor = oauthVendorGoogle
	default:
		return "", "", "", false
	}
	query := parsed.Query()
	code = strings.TrimSpace(query.Get("code"))
	state = strings.TrimSpace(query.Get("state"))
	return vendor, code, state, code != ""
}
