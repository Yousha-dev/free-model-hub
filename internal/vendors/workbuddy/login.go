package workbuddy

// This file implements the device authorization login flow for WorkBuddy.
//
// Flow (consistent with cmd/login in the original project):
//  1. Request state from /v2/plugin/auth/state to get the authorization URL for the user to open;
//  2. The user completes authorization in the browser;
//  3. Poll /v2/plugin/auth/token to exchange for a token, then use /v2/plugin/login/account to fetch account information.
//
// The host only lists its own hardcoded OAuth providers, and plugins do not appear in that list,
// so the login entry point is provided by the plugin's console page (see auth_login.go at the root layer).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// PendingLogin represents data for an ongoing login session.
//
// TTL and polling throttling are centrally managed by the internal/auth session table; only protocol state is kept here.
type PendingLogin struct {
	Region Region
	State  string
}

// StoreLoginSession registers an ongoing login session (TTL and throttling are delegated to the shared registration table).
func StoreLoginSession(logins core.LoginSessions, sessionID string, session *PendingLogin) {
	if session == nil {
		return
	}
	logins.Register(VendorIDFor(session.Region), sessionID, LoginSessionTTL, loginPollInterval, session)
}

// RemoveLoginSession cleans up the session after a successful login or termination.
func RemoveLoginSession(logins core.LoginSessions, sessionID string) {
	logins.Delete(sessionID)
}

// CheckLoginSession checks the login session state and throttling.
// Returns (session, exists, shouldPoll):
//   - exists: whether the session exists and is within its TTL;
//   - shouldPoll: whether the throttle interval has passed and an upstream token request can be made.
func CheckLoginSession(logins core.LoginSessions, sessionID string) (session *PendingLogin, exists bool, shouldPoll bool) {
	entry, pollState := logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return nil, false, false
	}
	pending, okPending := entry.Data.(*PendingLogin)
	if !okPending {
		return nil, false, false
	}
	return pending, true, pollState == core.PollReady
}

// loginAccount is the account information retrieved during the login flow.
type LoginAccount struct {
	UID          string
	Nickname     string
	Domain       string
	EnterpriseID string
}

// requestLoginState fetches the state and constructs the user authorization URL.
func RequestLoginState(ctx context.Context, region Region) (state, authURL string, err error) {
	endpoints := GetEndpoints(region)
	url := endpoints.ChatBase + "/v2/plugin/auth/state?platform=" + loginUAPlatform
	// The upstream requires a JSON body for POST requests (an empty object is sufficient); without it, some nodes return errors.
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if errReq != nil {
		return "", "", fmt.Errorf("build login state request: %w", errReq)
	}
	applyLoginHeaders(req, region)

	raw, errCall := doLoginRequest(req)
	if errCall != nil {
		return "", "", errCall
	}
	var payload struct {
		State string `json:"state"`
		URL   string `json:"url"`
		// Some formats place the link in authUrl / loginUrl.
		AuthURL  string `json:"authUrl"`
		LoginURL string `json:"loginUrl"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", "", fmt.Errorf("decode login state: %w", errUnmarshal)
	}
	state = strings.TrimSpace(payload.State)
	authURL = core.FirstNonEmpty(payload.URL, payload.AuthURL, payload.LoginURL)
	if state == "" || authURL == "" {
		return "", "", fmt.Errorf("login state response is incomplete")
	}
	return state, authURL, nil
}

// exchangeLoginToken exchanges the state for a token.
func ExchangeLoginToken(ctx context.Context, region Region, state string) (accessToken, refreshToken string, err error) {
	endpoints := GetEndpoints(region)
	url := endpoints.ChatBase + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return "", "", fmt.Errorf("build login token request: %w", errReq)
	}
	applyLoginHeaders(req, region)

	raw, errCall := doLoginRequest(req)
	if errCall != nil {
		return "", "", errCall
	}
	var payload struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", "", fmt.Errorf("decode login token: %w", errUnmarshal)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", "", fmt.Errorf("login token response has no accessToken")
	}
	return strings.TrimSpace(payload.AccessToken), strings.TrimSpace(payload.RefreshToken), nil
}

// fetchLoginAccount retrieves account information.
func FetchLoginAccount(ctx context.Context, region Region, state, accessToken string) (LoginAccount, error) {
	endpoints := GetEndpoints(region)
	url := endpoints.ChatBase + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return LoginAccount{}, fmt.Errorf("build login account request: %w", errReq)
	}
	applyLoginHeaders(req, region)
	// This endpoint uses the newly acquired token for authentication (the original project also uses Bearer).
	if strings.TrimSpace(accessToken) != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	raw, errCall := doLoginRequest(req)
	if errCall != nil {
		return LoginAccount{}, errCall
	}
	var payload struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		Domain       string `json:"domain"`
		EnterpriseID string `json:"enterpriseId"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return LoginAccount{}, fmt.Errorf("decode login account: %w", errUnmarshal)
	}
	return LoginAccount{
		UID:          strings.TrimSpace(payload.UID),
		Nickname:     strings.TrimSpace(payload.Nickname),
		Domain:       strings.TrimSpace(payload.Domain),
		EnterpriseID: strings.TrimSpace(payload.EnterpriseID),
	}, nil
}

// doLoginRequest initiates a login-related request and unwraps the envelope.
func doLoginRequest(req *http.Request) (json.RawMessage, error) {
	client := hostbridge.Client(req.Context(), loginHTTPTimeout)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("login request: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read login response: %w", errRead)
	}
	if resp.StatusCode >= 400 {
		return nil, enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		// Some login APIs directly return a bare object (without an envelope).
		return body, nil
	}
	if envelope.Code != 0 {
		// Crucial: **Do not** treat all non-zero business codes as failures.
		// The upstream uses 11217 + "login ing..." to indicate "authorization not yet complete", which is a normal
		// state during polling; treating it as a failure causes users to see "authorization failed" when they are just one step away.
		return nil, &core.Failure{
			Kind:   core.FailureKindClient,
			Status: resp.StatusCode,
			Msg:    fmt.Sprintf("%s %s", strconv.Itoa(envelope.Code), strings.TrimSpace(envelope.Msg)),
		}
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		return envelope.Data, nil
	}
	return body, nil
}

// applyLoginHeaders writes request headers for the device authorization chain (consistent with the upstream client).
//
// The format of X-Requested-With and Accept must match the official client: upstream performs UA/header validation
// on device authorization endpoints, and behavior is undefined if X-Requested-With is missing.
func applyLoginHeaders(req *http.Request, region Region) {
	endpoints := GetEndpoints(region)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", endpoints.Origin)
	req.Header.Set("Referer", endpoints.Origin+"/")
	req.Header.Set("User-Agent", loginClientUA)
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// isLoginPending determines whether an error indicates that "the user has not yet completed authorization".
//
// Upstream returns 404 or business errors during the pending authorization period; neither of these should terminate polling.
func IsLoginPending(err error) bool {
	if err == nil {
		return false
	}
	var upstreamErr *core.Failure
	if !errors.As(err, &upstreamErr) {
		return false
	}
	if upstreamErr.Status == http.StatusNotFound {
		return true
	}
	message := strings.ToLower(upstreamErr.Msg)
	// 11217 is the upstream business code for "login in progress"; "login ing" is its fixed message.
	// Both indicate that the user has not yet completed authorization in the browser, so polling should continue instead of reporting a failure.
	if strings.Contains(message, loginPendingCode) {
		return true
	}
	for _, marker := range []string{"login ing", "pending", "not authorized", "waiting", "authorizing"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// loginPendingCode is the upstream business code for "authorization not yet completed".
const loginPendingCode = "11217"

// Constants for the login chain (originally private constants at the root layer, migrated to this package along with the login logic).
const (
	// LoginSessionTTL is the validity period of the login session.
	LoginSessionTTL = 15 * time.Minute
	// loginPollInterval is the minimum interval for the plugin side to poll the upstream (to prevent excessive frequency).
	loginPollInterval = 2 * time.Second
	// loginHTTPTimeout is the timeout limit for a single login-related request.
	loginHTTPTimeout = 30 * time.Second
	// loginUAPlatform is the platform identifier for the login chain.
	loginUAPlatform = "CLI"
	// loginClientUA is the UA for the login flow.
	loginClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// DefaultDomainFor returns the default domain for a given domain.
func DefaultDomainFor(region Region) string {
	if region.IsGlobal() {
		return "www.workbuddy.ai"
	}
	return "www.codebuddy.cn"
}

// RefreshToken refreshes the access token using the refresh token.
//
// Security red line: The X-Refresh-Token header **is only allowed on the refresh endpoint**, and chat requests must never carry it.
//
// Concurrency uses a two-phase approach: take snapshot inside lock -> send request outside lock -> validate inside lock that the snapshot was not concurrently modified before writing back.
func (c *Client) RefreshToken(cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("credential is nil")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	refreshToken := cred.RefreshTokenValue()
	if strings.TrimSpace(refreshToken) == "" {
		return fmt.Errorf("credential has no refresh token")
	}
	accessBefore, refreshBefore := cred.Snapshot()

	endpoints := GetEndpoints(region)
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost,
		endpoints.ChatBase+tokenRefreshPath, nil)
	if errReq != nil {
		return fmt.Errorf("build refresh request: %w", errReq)
	}
	applyCommonHeaders(req, c, cred)
	req.Header.Set("X-Refresh-Token", refreshToken)
	req.Header.Set("X-Auth-Refresh-Source", authRefreshSource)

	resp, errDo := c.refreshClient().Do(req)
	if errDo != nil {
		return fmt.Errorf("refresh request: %w", errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
	if errRead != nil {
		return fmt.Errorf("read refresh response: %w", errRead)
	}
	if resp.StatusCode >= 400 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	}

	var envelope struct {
		Code int                  `json:"code"`
		Msg  string               `json:"msg"`
		Data refreshTokenResponse `json:"data"`
	}
	var token refreshTokenResponse
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal == nil && envelope.Code == 0 && strings.TrimSpace(envelope.Data.AccessToken) != "" {
		token = envelope.Data
	} else if errUnmarshal == nil && envelope.Code != 0 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	} else if errDirect := json.Unmarshal(body, &token); errDirect != nil || strings.TrimSpace(token.AccessToken) == "" {
		return &core.Failure{Kind: core.FailureKindSessionDead, Status: http.StatusUnauthorized, Msg: "refresh failed: no accessToken in response — re-login required"}
	}

	// Snapshot validation: if a concurrent refresh has already written, abandon this write-back to avoid overwriting new values with old responses.
	if !cred.UnchangedSince(accessBefore, refreshBefore) {
		return nil
	}
	var expiresAt int64
	if token.ExpiresIn > 0 && token.ExpiresIn <= maxRefreshTokenExpiresIn {
		expiresAt = time.Now().Unix() + token.ExpiresIn
	}
	cred.ApplyTokenRefresh(token.AccessToken, token.RefreshToken, token.Domain, expiresAt)
	if cred.FilePath != "" {
		if errSave := SaveCredentialFile(cred.FilePath, cred); errSave != nil {
			return fmt.Errorf("save refreshed credential: %w", errSave)
		}
	}
	return nil
}
