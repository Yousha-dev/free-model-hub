package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// This file implements Qoder's OAuth device authorization login (PKCE).
//
// Flow (consistent with the upstream Qoder desktop client):
//  1. Generate PKCE verifier/challenge and nonce, assemble the authorization URL (DeviceLoginBase);
//  2. The user completes authorization in the browser;
//  3. Poll the poll endpoint: 404 = not yet authorized, 200 = device_token acquired;
//  4. Fetch account information and plan name, and assemble the credential JSON.
//
// Differences from WorkBuddy: WorkBuddy uses a custom state/token three-interface approach, while Qoder uses standard
// PKCE device code flow. Their structures are completely different, so the login protocol belongs to the provider layer.

const (
	// oauthClientID is the public client id used by the Qoder desktop client (consistent with upstream implementation).
	oauthClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	// loginFlowTTL is the validity period of a single login session (consistent with the upstream 10 minutes).
	loginFlowTTL = 10 * time.Minute
	// loginHTTPTimeout is the limit for login-related requests.
	loginHTTPTimeout = 30 * time.Second
	// loginPollInterval is the minimum interval between two visits to the upstream poll endpoint for the same session.
	//
	// The console page's polling frequency is much higher than upstream recommendations (upstream client is once per second),
	// without throttling, opening the panel would flood upstream immediately.
	loginPollInterval = 1200 * time.Millisecond
)

// pendingLogin represents data for an ongoing login session (TTL / throttling is centrally managed by the auth session table).
type pendingLogin struct {
	nonce    string
	verifier string
	region   Region
}

// LoginProfile is the account information (remains zero value if it cannot be retrieved).
type LoginProfile struct {
	UserID string
	Email  string
	Name   string
	Type   string
}

// LoginStart generates PKCE parameters and login URL, and registers a login session pending polling.
func LoginStart(ctx context.Context, logins core.LoginSessions, region Region) (loginURL, state string, expiresAt time.Time, err error) {
	verifier, challenge, errPKCE := core.NewPKCEPair()
	if errPKCE != nil {
		return "", "", time.Time{}, errPKCE
	}
	nonce := core.RandomHex(16)
	state = core.RandomHex(16)

	endpoints := GetEndpoints(region)
	params := url.Values{}
	params.Set("nonce", nonce)
	params.Set("challenge", challenge)
	params.Set("challenge_method", "S256")
	params.Set("client_id", oauthClientID)
	loginURL = endpoints.DeviceLoginBase + "?" + params.Encode()

	deadline := time.Now().Add(loginFlowTTL)
	logins.Register(VendorIDFor(region), state, time.Until(deadline), loginPollInterval, &pendingLogin{
		nonce:    nonce,
		verifier: verifier,
		region:   region,
	})
	return loginURL, state, deadline, nil
}

// LoginPollResult is the result of a single poll.
type LoginPollResult struct {
	// Pending being true indicates that it is still waiting for user authorization, and the caller should continue polling.
	Pending bool
	// Token / RefreshToken / Profile are valid when credentials are obtained.
	Token        string
	RefreshToken string
	Profile      LoginProfile
	// Region is the region of this login (used to assemble persistence content when getting credentials).
	Region Region
}

// LoginPoll polls a login session once.
//
// Returning an error indicates that the session is invalid (non-existent or timed out), and the caller should terminate polling;
// Returning Pending=true indicates that the user has not completed authorization yet, just continue polling.
func LoginPoll(ctx context.Context, logins core.LoginSessions, state string) (LoginPollResult, error) {
	session, pollState := logins.Poll(state)
	if pollState == core.PollMissing {
		return LoginPollResult{}, fmt.Errorf("Login session does not exist or has expired, please initiate login again")
	}
	// Throttling: Panel polling frequency is higher than upstream recommendations (upstream client is once per second).
	if pollState == core.PollThrottled {
		return LoginPollResult{Pending: true}, nil
	}
	entry, okEntry := session.Data.(*pendingLogin)
	if !okEntry {
		return LoginPollResult{}, fmt.Errorf("Login session data is corrupted, please initiate login again")
	}

	token, refreshToken, waiting, errPoll := pollDeviceToken(ctx, entry)
	if errPoll != nil {
		// Network jitter should not directly kill the session: keep the session and try again on the next poll.
		return LoginPollResult{Pending: true}, nil
	}
	if waiting {
		return LoginPollResult{Pending: true}, nil
	}

	profile := fetchLoginProfile(ctx, token, entry.region)
	logins.Delete(state)
	return LoginPollResult{
		Token:        token,
		RefreshToken: refreshToken,
		Profile:      profile,
		Region:       entry.region,
	}, nil
}

// OwnsLoginSession reports whether the session was created by this provider and is still valid.
func OwnsLoginSession(logins core.LoginSessions, sessionID string) bool {
	_, okEntry := logins.Owns(sessionID)
	return okEntry
}

// pollDeviceToken accesses the upstream poll endpoint.
//
// waiting=true indicates that the user has not authorized yet (upstream 404), and the caller should continue waiting.
func pollDeviceToken(ctx context.Context, entry *pendingLogin) (token, refreshToken string, waiting bool, err error) {
	endpoints := GetEndpoints(entry.region)
	pollURL := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		endpoints.PollEndpoint, url.QueryEscape(entry.nonce), url.QueryEscape(entry.verifier))
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if errRequest != nil {
		return "", "", false, errRequest
	}
	resp, errDo := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", false, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return "", "", false, fmt.Errorf("failed to read upstream poll response: %w", errRead)
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		// Upstream semantics: User has not authorized yet.
		return "", "", true, nil
	case http.StatusOK:
	default:
		return "", "", false, fmt.Errorf("upstream poll returned HTTP %d: %s", resp.StatusCode, summarizeBody(body))
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return "", "", false, fmt.Errorf("upstream poll response is not valid JSON: %w", errUnmarshal)
	}
	deviceToken, _ := payload["token"].(string)
	deviceToken = strings.TrimSpace(deviceToken)
	if deviceToken == "" {
		return "", "", false, fmt.Errorf("no token in upstream poll response")
	}
	refreshToken, _ = payload["refresh_token"].(string)
	return deviceToken, strings.TrimSpace(refreshToken), false, nil
}

// fetchLoginProfile fetches account information (failure only degrades: can still be used without email/name).
func fetchLoginProfile(ctx context.Context, token string, region Region) LoginProfile {
	endpoints := GetEndpoints(region)
	result, errProfile := httpGetBearerJSON(ctx, endpoints.UserinfoBase, token)
	if errProfile != nil {
		return LoginProfile{}
	}
	return LoginProfile{
		UserID: core.StringField(result, "userId"),
		Email:  strings.ToLower(core.StringField(result, "email")),
		Name:   core.StringField(result, "name"),
		Type:   core.StringField(result, "userType"),
	}
}

// FetchPlanName fetches the plan name (leaves blank on failure).
func FetchPlanName(ctx context.Context, token string, region Region) string {
	endpoints := GetEndpoints(region)
	result, errPlan := httpGetBearerJSON(ctx, endpoints.PlanEndpoint, token)
	if errPlan != nil {
		return ""
	}
	return core.StringField(result, "plan_tier_name")
}

// BuildLoginStorage generates the credential JSON for persistence (flat format).
//
// Isomorphic with import path: the refresh code (auth.refresh) can read back these fields as-is.
func BuildLoginStorage(region Region, token, refreshToken string, profile LoginProfile, plan string) ([]byte, *Credential) {
	payload := map[string]any{
		"device_token": token,
		"region":       string(region),
		"auth_mode":    "oauth",
	}
	if refreshToken != "" {
		payload["refresh_token"] = refreshToken
	}
	label := profile.Name
	if label == "" {
		label = profile.Email
	}
	if label != "" {
		payload["label"] = label
	}
	if profile.Email != "" {
		payload["email"] = profile.Email
	}
	if plan != "" {
		payload["plan"] = plan
	}
	if profile.UserID != "" {
		payload["qoder_account_id"] = profile.UserID
	}
	if profile.Type != "" {
		payload["qoder_user_type"] = profile.Type
	}
	storage, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		// map only contains strings, theoretically it won't fail; fallback to compact encoding to ensure login is not interrupted.
		storage, _ = json.Marshal(payload)
	}
	return storage, &Credential{
		Token:        token,
		RefreshToken: refreshToken,
		Region:       region,
		Label:        label,
		Email:        profile.Email,
	}
}

// LoginAuthID generates the host-side auth identifier (used for the persisted file name, must be safe for file names).
func LoginAuthID(profile LoginProfile, state string) string {
	base := profile.Email
	if base == "" {
		base = profile.UserID
	}
	if base == "" {
		base = "login-" + shortState(state)
	}
	return "qoder-" + sanitizeAuthID(base)
}

// shortState truncates the state for logging (the full state is part of the credential material).
func shortState(state string) string {
	if len(state) <= 8 {
		return state
	}
	return state[:8]
}

// sanitizeAuthID keeps only filename-safe characters.
func sanitizeAuthID(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '_', r == '-', r == '@':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	out := strings.Trim(builder.String(), "-.@")
	if out == "" {
		out = "account"
	}
	return out
}

// summarizeBody truncates the response body for error messages.
func summarizeBody(body []byte) string {
	return core.Truncate(string(body), 200)
}

// Compile-time check to ensure bridge's credential parsing is available in this package (login persistence needs it to decrypt back to token).

// This file implements credential renewal (validation + refresh) for Qoder.
//
// Semantic differences between the two paths:
//   - PAT (pt-...) goes through jobToken exchange, **will get a new refreshToken**;
//   - Device token (dt-...) uses userinfo for keep-alive, does not write to disk when content remains unchanged
//     (otherwise mtime would change meaninglessly, and users cannot tell if it was actually renewed).
//
// refreshed=false indicates only validation passed, no new credentials issued by the upstream.

// RefreshCredential validates and refreshes credentials.
func RefreshCredential(ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if cred == nil {
		return nil, false, fmt.Errorf("credential is nil")
	}
	deviceToken, refreshToken := ParseOAuthSecret(cred.Token)
	if strings.HasPrefix(deviceToken, "dt-") {
		// Device token: use userinfo for keep-alive, content remains unchanged.
		if _, errInfo := FetchUserInfoWithToken(ctx, deviceToken, cred.Region); errInfo != nil {
			return nil, false, ClassifyCredentialError(errInfo)
		}
		return cred, false, nil
	}

	// PAT: exchange for jobToken to get new securityOauthToken / refreshToken.
	seed := FingerprintSeed("", cred.Token)
	endpoints := GetEndpoints(cred.Region)
	jobToken, errExchange := ExchangeJobToken(ctx, cred.Token,
		DeriveMachineID(seed), DeriveMachineToken(seed), DeriveMachineType(seed),
		endpoints.JobTokenURL)
	if errExchange != nil {
		return nil, false, ClassifyCredentialError(errExchange)
	}
	oauthToken := StrVal(jobToken, "securityOauthToken")
	if oauthToken == "" {
		return nil, false, NewError("qoder_credential_invalid",
			"jobToken response missing securityOauthToken", 401)
	}
	nextRefresh := StrVal(jobToken, "refreshToken")
	if nextRefresh == "" {
		nextRefresh = refreshToken
	}
	if oauthToken == cred.Token && nextRefresh == cred.RefreshToken {
		// Upstream returned the same value: considered 'valid' but not 'refreshed', avoiding meaningless disk writes.
		return cred, false, nil
	}
	updated := &Credential{
		Token:        oauthToken,
		RefreshToken: nextRefresh,
		Region:       cred.Region,
		Label:        cred.Label,
		Email:        cred.Email,
	}
	return updated, true, nil
}
