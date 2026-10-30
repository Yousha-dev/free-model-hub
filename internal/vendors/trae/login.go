package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// This file implements Trae family authorized login link generation and polling status checks.

// BuildLoginURL constructs the authorized login URL for a given profile.
func BuildLoginURL(cfg Config, machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", cfg.ClientID)
	v.Set("redirect", "0")

	traceID := MachineTraceID(machineID, deviceID)

	return cfg.ConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(traceID) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(cfg.IDEVersion) +
		"&x_app_type=stable"
}

// LoginStart initiates the login process.
func LoginStart(ctx context.Context, cfg Config, store *SessionStore) (*pluginapi.AuthLoginStartResponse, error) {
	if errListen := store.EnsureCallbackServer(); errListen != nil {
		// Do not proceed if the listener cannot start: after the user authorizes, no process will receive the callback.
		return nil, core.NewPluginError("trae_callback_unavailable",
			"Failed to start authorization callback listener on localhost: "+errListen.Error(), http.StatusInternalServerError)
	}

	machineID := core.RandomHex(16)
	deviceID := core.RandomHex(16)
	traceID := MachineTraceID(machineID, deviceID)
	callbackURL := store.callbackURL()

	loginURL := BuildLoginURL(cfg, machineID, deviceID, callbackURL)
	sessionID := cfg.VendorID + "_" + core.RandomHex(8)

	store.register(&PendingSession{
		SessionID: sessionID,
		VendorID:  cfg.VendorID,
		Region:    cfg.Region,
		MachineID: machineID,
		DeviceID:  deviceID,
		TraceID:   traceID,
		State:     "pending",
		CreatedAt: time.Now(),
	})

	return &pluginapi.AuthLoginStartResponse{
		Provider:  "freehub",
		URL:       loginURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

// LoginPoll polls the status.
func LoginPoll(ctx context.Context, state string, store *SessionStore) (*pluginapi.AuthLoginPollResponse, error) {
	session, ok := store.get(state)
	if !ok || time.Since(session.CreatedAt) > 15*time.Minute {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session does not exist or has expired. Please restart the login and paste the callback link immediately after browser authorization",
		}, nil
	}

	switch session.State {
	case "success":
		store.remove(state)
		if session.AuthData != nil {
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusSuccess,
				Message: "Login successful",
				Auth:    *session.AuthData,
			}, nil
		}
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "Login successful",
		}, nil
	case "failed":
		store.remove(state)
		errMsg := session.ErrMsg
		if errMsg == "" {
			errMsg = "Authorized login failed"
		}
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errMsg,
		}, nil
	default:
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Waiting for user authorization in the browser...",
		}, nil
	}
}

// PendingSession records an ongoing Trae login.
type PendingSession struct {
	SessionID string
	VendorID  string // traecn / traeglobal
	Region    Region
	MachineID string
	DeviceID  string
	TraceID   string
	State     string // "pending", "success", "failed"
	AuthData  *pluginapi.AuthData
	ErrMsg    string
	CreatedAt time.Time
}

// The login form and listener all belong to SessionStore (= one App instance),
// and are not stored in package-level variables: when multiple apps coexist in the same process,
// a login initiated by one instance must not affect another instance's session, nor let their callbacks cross-claim.

// SessionStore represents the Trae login session table and callback listener for a single App instance.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*PendingSession

	// serverMu protects the listener lifecycle of this instance (lazy start, allowing retry on failure).
	serverMu   sync.Mutex
	listening  bool
	serverAddr string
}

// NewSessionStore constructs the session table.
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]*PendingSession{}}
}

func (s *SessionStore) register(p *PendingSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.sessions[p.SessionID] = p
}

func (s *SessionStore) get(sessionID string) (*PendingSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if time.Since(session.CreatedAt) > 15*time.Minute {
		delete(s.sessions, sessionID)
		return nil, false
	}
	return session, true
}

func (s *SessionStore) remove(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

func (s *SessionStore) owns(sessionID, vendorID string) bool {
	norm := strings.ToLower(strings.TrimSpace(sessionID))
	switch strings.ToLower(strings.TrimSpace(vendorID)) {
	case "traecn", "trae-cn":
		if strings.HasPrefix(norm, "traecn_") || strings.HasPrefix(norm, "trae_cn_") {
			return true
		}
	case "traeglobal", "trae-global":
		if strings.HasPrefix(norm, "traeglobal_") || strings.HasPrefix(norm, "trae_global_") {
			return true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return false
	}
	if time.Since(session.CreatedAt) > 15*time.Minute {
		delete(s.sessions, sessionID)
		return false
	}
	return strings.EqualFold(session.VendorID, vendorID)
}

// matchExact performs exact matching only by trace/machine, returning nil if not found.
func (s *SessionStore) matchExact(info *CallbackInfo) *PendingSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, session := range s.sessions {
		if session.State != "pending" {
			continue
		}
		if (info.TraceID != "" && session.TraceID == info.TraceID) ||
			(info.MachineID != "" && session.MachineID == info.MachineID) {
			return session
		}
	}
	return nil
}

// latestPending returns the most recently created pending session (fallback when no matching identifier is present in the callback).
func (s *SessionStore) latestPending() *PendingSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *PendingSession
	for _, session := range s.sessions {
		if session.State == "pending" && time.Since(session.CreatedAt) <= 15*time.Minute {
			if latest == nil || session.CreatedAt.After(latest.CreatedAt) {
				latest = session
			}
		}
	}
	return latest
}

func (s *SessionStore) gcLocked() {
	now := time.Now()
	for id, session := range s.sessions {
		if now.Sub(session.CreatedAt) > 15*time.Minute {
			delete(s.sessions, id)
		}
	}
}

// matchCallback first performs exact matching by trace/machine, then falls back to the newest pending session.
//
// Search only in **this instance's** session table: another App's login session does not belong here.
func (s *SessionStore) matchCallback(info *CallbackInfo) *PendingSession {
	if matched := s.matchExact(info); matched != nil {
		return matched
	}
	return s.latestPending()
}

// EnsureCallbackServer ensures that this instance's authorization callback listener has started.
//
// Port prefers 127.0.0.1:18080 (consistent with Trae desktop callback address); when occupied,
// it falls back to a system-assigned free port and writes the actual port into the login link—otherwise the callback will
// never reach this process. The listener lifecycle follows the instance and is not shared with other Apps.
func (s *SessionStore) EnsureCallbackServer() error {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	if s.listening {
		return nil
	}

	listener, errListen := net.Listen("tcp", "127.0.0.1:18080")
	if errListen != nil {
		logger.Debug("trae: 18080 is occupied, using random port instead: %v", errListen)
		listener, errListen = net.Listen("tcp", "127.0.0.1:0")
		if errListen != nil {
			return fmt.Errorf("listen callback: %w", errListen)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", s.handleAuthorizeCallback)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.serverAddr = "http://" + listener.Addr().String()
	s.listening = true
	go func() {
		logger.Info("trae: Local authorization callback listener started: %s/authorize", s.serverAddr)
		if errServe := srv.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
			logger.Error("trae: Callback service exited: %v", errServe)
		}
	}()
	return nil
}

// callbackURL returns this instance's callback address (valid after EnsureCallbackServer).
func (s *SessionStore) callbackURL() string {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	return s.serverAddr + "/authorize"
}

// callbackHint returns the callback address that the page should display: default port when listener is not up,
// and real port when it is up (random port if 18080 is occupied, so the prompt must change accordingly).
func (s *SessionStore) callbackHint() string {
	if addr := s.callbackURL(); addr != "" {
		return addr
	}
	return "http://127.0.0.1:18080/authorize"
}

// MachineTraceID derives a 16-bit trace ID from machineID+deviceID.
func MachineTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	if len(h) >= 16 {
		return h[len(h)-16:]
	}
	return strings.Repeat("0", 16-len(h)) + h
}

// CallbackInfo represents the callback parsing result.
type CallbackInfo struct {
	RefreshToken string
	AccessToken  string
	UID          string
	Nickname     string
	EnterpriseID string
	ExpiresAt    int64
	TraceID      string
	MachineID    string
	DeviceID     string
}

// ParseCallback parses the link received by /authorize.
func ParseCallback(rawURL string) (*CallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("empty callback url")
	}
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = "http://127.0.0.1" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &CallbackInfo{
		RefreshToken: q.Get("refreshToken"),
		TraceID:      q.Get("loginTraceID"),
		MachineID:    q.Get("machine_id"),
		DeviceID:     q.Get("device_id"),
	}

	userInfo := parseJSONParam(q.Get("userInfo"))
	info.UID = core.GetString(userInfo, "UserID")
	info.Nickname = core.GetString(userInfo, "ScreenName")
	info.EnterpriseID = core.GetString(userInfo, "TenantID")

	userJwt := parseJSONParam(q.Get("userJwt"))
	jwtToken := core.GetString(userJwt, "Token")
	jwtRefresh := core.GetString(userJwt, "RefreshToken")

	if info.RefreshToken == "" {
		info.RefreshToken = jwtRefresh
	}
	if info.RefreshToken == "" {
		info.AccessToken = jwtToken
		if exp := getInt64Param(userJwt, "TokenExpireAt"); exp > 0 {
			if exp > 1e12 {
				exp /= 1000
			}
			info.ExpiresAt = exp
		}
	}

	if info.RefreshToken == "" && info.AccessToken == "" {
		return nil, fmt.Errorf("callback missing refreshToken and userJwt.Token")
	}

	return info, nil
}

func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getInt64Param(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func (s *SessionStore) handleAuthorizeCallback(w http.ResponseWriter, r *http.Request) {
	info, errParse := ParseCallback(r.URL.String())
	if errParse != nil {
		renderCallbackHTML(w, http.StatusBadRequest, "Failed to parse authorization callback", errParse.Error())
		return
	}

	matched := s.matchCallback(info)

	if matched == nil {
		renderCallbackHTML(w, http.StatusNotFound, "Corresponding login session not found", "The login session may have expired or been processed. Please return to the console and restart the login.")
		return
	}

	if _, errComplete := s.completeLogin(r.Context(), matched, info); errComplete != nil {
		renderCallbackHTML(w, http.StatusInternalServerError, "Failed to process authorization callback", errComplete.Error())
		return
	}

	renderCallbackHTML(w, http.StatusOK, "Authorization Successful", "Account authorized successfully! You can now close this window and return to the management panel.")
}

// completeLogin completes the login using the credentials brought by the callback: exchanges for access token, assembles credentials, and sets session success.
//
// HTTP callback and console "paste callback link" share this section — the two paths only differ in "how the callback
// link is obtained", the completion logic must be identical, otherwise the same login will yield different credentials due to different entry points.
func (s *SessionStore) completeLogin(ctx context.Context, matched *PendingSession, info *CallbackInfo) (*pluginapi.AuthData, error) {
	// Execute Token exchange: host/clientID are determined by instance profile (CN version is
	// api.trae.cn + en1oxy7wnw8j9n, Global version is growsg-normal.trae.ai + ono9krqynydwx5).
	cfg, okConfig := ConfigForVendor(matched.VendorID)
	if !okConfig {
		cfg = ConfigFor(matched.Region)
	}
	authHost := cfg.AuthHost
	clientID := cfg.ClientID

	accessToken := info.AccessToken
	refreshToken := info.RefreshToken
	expiresAt := info.ExpiresAt

	// uid must be calculated before token exchange: upstream token exchange API requires UserID.
	uid := info.UID
	if uid == "" {
		uid = "user_" + matched.MachineID[:min(8, len(matched.MachineID))]
	}

	if refreshToken != "" {
		newAccess, newRefresh, exp, errEx := exchangeTraeToken(ctx, authHost, clientID, uid, refreshToken)
		if errEx != nil {
			logger.Error("trae: callback exchange token warning: %v, falling back to original", errEx)
		} else {
			accessToken = newAccess
			if newRefresh != "" {
				refreshToken = newRefresh
			}
			expiresAt = exp
		}
	}

	nickname := info.Nickname
	if nickname == "" {
		nickname = uid
	}

	label := "Trae-" + nickname
	if matched.Region == RegionGlobal {
		label = "TraeGlobal-" + nickname
	}

	storageMap := map[string]any{
		"type":          "freehub",
		"vendor":        matched.VendorID,
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"expires_at":    expiresAt,
		"uid":           uid,
		"nickname":      nickname,
		"enterprise_id": info.EnterpriseID,
		"machine_id":    matched.MachineID,
		"device_id":     matched.DeviceID,
		"label":         label,
		"region":        string(matched.Region),
	}

	rawStorage, errJSON := json.MarshalIndent(storageMap, "", "  ")
	if errJSON != nil {
		matched.State = "failed"
		matched.ErrMsg = "Failed to generate credential"
		return nil, fmt.Errorf("failed to generate credential: %w", errJSON)
	}

	fileName := matched.VendorID + "-" + uid + ".json"
	authData := &pluginapi.AuthData{
		Provider:    "freehub",
		ID:          matched.VendorID + "-" + uid,
		FileName:    fileName,
		Label:       label,
		StorageJSON: rawStorage,
	}
	matched.AuthData = authData
	matched.State = "success"

	logger.Info("trae: authorization successful, account=%s (%s), vendor=%s", uid, nickname, matched.VendorID)
	return authData, nil
}

// SubmitCallback accepts the localhost callback link pasted by the user.
//
// Upstream only accepts 127.0.0.1 callbacks. When the plugin and browser are not on the same machine, that HTTP callback will never
// arrive; the console sends back the address where the browser stopped as-is, and login is completed locally here.
//
// Unlike HTTP callbacks, pasting **strictly claims by loginTraceID / machine_id**: better to report
// a mismatch than to attribute a link to another session (HTTP callbacks have latestPending as a fallback,
// which is a convenience for same-machine scenarios; pasting must be strict).
func (s *SessionStore) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	trimmed := strings.TrimSpace(state)
	if trimmed == "" {
		return nil, core.NewPluginError("invalid_request", "Missing login session state, please initiate login again", http.StatusBadRequest)
	}

	session, ok := s.get(trimmed)
	if !ok {
		return nil, core.NewPluginError("session_not_found",
			"Login session does not exist or has expired, please restart login and paste the callback link immediately after browser authorization", http.StatusNotFound)
	}
	if session.State == "success" && session.AuthData != nil {
		return &core.CallbackSubmitResult{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "Login successful",
			Auth:    *session.AuthData,
		}, nil
	}
	if session.State != "pending" {
		msg := session.ErrMsg
		if msg == "" {
			msg = "Authorized login failed"
		}
		return &core.CallbackSubmitResult{Status: pluginapi.AuthLoginStatusError, Message: msg}, nil
	}

	// Do not echo the original link when parsing fails: it contains refreshToken / userJwt.
	info, errParse := ParseCallback(rawURL)
	if errParse != nil {
		return nil, core.NewPluginError("callback_parse_failed",
			"Failed to parse callback link, please ensure you pasted the complete link from the browser address bar (including http:// and subsequent parameters)",
			http.StatusBadRequest)
	}

	matched := s.matchExact(info)
	if matched == nil || matched.SessionID != session.SessionID {
		return nil, core.NewPluginError("callback_mismatch",
			"The callback link does not match the current login session, please restart login and re-authorize", http.StatusBadRequest)
	}

	authData, errComplete := s.completeLogin(ctx, matched, info)
	if errComplete != nil {
		return nil, core.ToPluginError(errComplete)
	}
	return &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "Login successful",
		Auth:    *authData,
	}, nil
}

// exchangeTraeToken exchanges refreshToken for accessToken (Trae OAuth ExchangeToken).
//
// Protocol (verified via joint debugging): POST {host}/cloudide/api/v3/trae/oauth/ExchangeToken,
// PascalCase payload, response takes Result.Token / Result.RefreshToken / Result.TokenExpireAt.
// host/clientID are provided by the caller according to the instance profile (CN version is api.trae.cn +
// en1oxy7wnw8j9n, Global version is growsg-normal.trae.ai + ono9krqynydwx5).
func exchangeTraeToken(ctx context.Context, authHost, clientID, userID, refreshToken string) (string, string, int64, error) {
	payload := map[string]string{
		"ClientID":     clientID,
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       userID,
	}
	bodyBytes, _ := json.Marshal(payload)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authHost+EpExchange, bytes.NewReader(bodyBytes))
	if errReq != nil {
		return "", "", 0, errReq
	}
	req.Header.Set("Content-Type", "application/json")

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", "", 0, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", 0, fmt.Errorf("upstream exchange error %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		Result struct {
			Token               string `json:"Token"`
			RefreshToken        string `json:"RefreshToken"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", "", 0, err
	}
	if res.Result.Token == "" {
		return "", "", 0, fmt.Errorf("exchange token failed: %s", string(body))
	}

	exp := res.Result.TokenExpireAt
	if exp > 1e12 {
		exp /= 1000
	}
	if exp <= 0 && res.Result.TokenExpireDuration > 0 {
		exp = time.Now().Unix() + res.Result.TokenExpireDuration
	}

	return res.Result.Token, res.Result.RefreshToken, exp, nil
}

func renderCallbackHTML(w http.ResponseWriter, code int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	color := "#3fb950"
	if code >= 400 {
		color = "#f85149"
	}
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>%s</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; background: #0d1117; color: #c9d1d9; }
    .card { background: #161b22; border: 1px solid #30363d; padding: 36px 40px; border-radius: 12px; box-shadow: 0 8px 24px rgba(0,0,0,0.5); text-align: center; max-width: 440px; }
    h2 { color: %s; margin: 0 0 16px 0; font-size: 24px; }
    p { margin: 8px 0; font-size: 15px; color: #8b949e; line-height: 1.5; }
    .msg { color: #f0f6fc; font-weight: 500; font-size: 16px; margin-bottom: 16px; }
    .tip { color: #6e7681; font-size: 13px; margin-top: 24px; }
  </style>
</head>
<body>
  <div class="card">
    <h2>%s</h2>
    <div class="msg">%s</div>
    <p class="tip">You can close this page and return to the management panel.</p>
  </div>
  <script>
    if (%d === 200) {
      setTimeout(function() { window.close(); }, 3000);
    }
  </script>
</body>
</html>`, title, color, title, message, code)
	_, _ = w.Write([]byte(html))
}

// Token refresh: calls ExchangeToken to rotate accessToken and refreshToken.

// RefreshCredential uses refreshToken to rotate accessToken (Trae OAuth ExchangeToken).
//
// Protocol is the same as exchangeTraeToken: PascalCase payload, response takes Result.Token.
func RefreshCredential(ctx context.Context, cred *Credential, cfg Config) (*Credential, bool, error) {
	if cred == nil || cred.RefreshToken == "" {
		return cred, false, nil
	}

	payload := map[string]string{
		"ClientID":     cfg.ClientID,
		"RefreshToken": cred.RefreshToken,
		"ClientSecret": "-",
		"UserID":       cred.UID,
	}
	bodyBytes, _ := json.Marshal(payload)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cfg.RefreshHost+EpExchange, bytes.NewReader(bodyBytes))
	if errReq != nil {
		return nil, false, errReq
	}
	req.Header.Set("Content-Type", "application/json")

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, false, fmt.Errorf("trae refresh token request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, false, Classify(resp.StatusCode, string(body))
	}

	var result struct {
		Result struct {
			Token               string `json:"Token"`
			RefreshToken        string `json:"RefreshToken"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, false, fmt.Errorf("decode exchange token response: %w", err)
	}
	if result.Result.Token == "" {
		return nil, false, fmt.Errorf("exchange token failed: %s", string(body))
	}

	expiresAt := result.Result.TokenExpireAt
	if expiresAt > 1e12 {
		expiresAt /= 1000
	}
	if expiresAt <= 0 && result.Result.TokenExpireDuration > 0 {
		expiresAt = time.Now().Unix() + result.Result.TokenExpireDuration
	}

	updated := *cred
	updated.AccessToken = result.Result.Token
	if result.Result.RefreshToken != "" {
		updated.RefreshToken = result.Result.RefreshToken
	}
	updated.ExpiresAt = expiresAt

	return &updated, true, nil
}
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if updated == nil {
		return original, nil
	}
	return core.MergeCredentialJSON(original, updated.StorageJSON, func(m map[string]any) {
		m["access_token"] = updated.AccessToken
		if updated.RefreshToken != "" {
			m["refresh_token"] = updated.RefreshToken
		}
		if updated.ExpiresAt > 0 {
			m["expires_at"] = updated.ExpiresAt
		}
		// Parse nested {auth:{...}} structures (see parseCredential), writing back must be symmetric,
		// otherwise nested credentials will only update at the top level after refresh, and subsequent parsing will still read the old token.
		if authObj, ok := m["auth"].(map[string]any); ok {
			authObj["accessToken"] = updated.AccessToken
			if updated.RefreshToken != "" {
				authObj["refreshToken"] = updated.RefreshToken
			}
			if updated.ExpiresAt > 0 {
				authObj["expiresAt"] = updated.ExpiresAt
			}
		}
	})
}
