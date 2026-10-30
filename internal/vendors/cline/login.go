package cline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// This file implements Cline's login flow (WorkOS OAuth 2.0 Device Code, RFC 8628).
//
// Three steps (ported from cline2api's auth.go):
//  1. POST {workos}/user_management/authorize/device to get device_code and user_code;
//  2. The user completes authorization in the browser, while the plugin polls POST {workos}/user_management/authenticate
//     until obtaining the WorkOS access/refresh token;
//  3. POST {cline}/auth/register to exchange the WorkOS token for Cline's own token.
//
// Why use device code instead of PKCE callback: The plugin runs in the host process and does not have its own
// HTTP listening port, leaving no place for callback-based authorization. Device code only requires the user to manually open a link.

const (
	// workosClientID is the WorkOS client ID of the official Cline client.
	//
	// This is not a secret, but a public client ID (the official client hardcodes the same value).
	workosClientID = "client_01K3A541FN8TA3EPPHTD2325AR"
	// workosDeviceAuthURL is the device code request endpoint.
	workosDeviceAuthURL = "https://api.workos.com/user_management/authorize/device"
	// workosAuthenticateURL is the device code token exchange endpoint.
	workosAuthenticateURL = "https://api.workos.com/user_management/authenticate"

	// deviceGrantType is the grant type for the device code flow (RFC 8628 fixed value).
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// minPollInterval is the lower bound of the polling interval.
	//
	// The interval given by upstream may be as small as 1 second, but that would trigger WorkOS rate limiting and return
	// slow_down; 5 seconds is the value used by the official client.
	minPollInterval = 5 * time.Second
	// defaultDeviceTTL is the fallback for device code validity (when not provided by upstream).
	defaultDeviceTTL = 300 * time.Second

	// loginHTTPTimeout is the upper limit for a single login-related request.
	loginHTTPTimeout = 30 * time.Second
)

// DeviceAuth holds the initial information for a device authorization.
type DeviceAuth struct {
	// DeviceCode is the device code used for polling (not displayed to the user).
	DeviceCode string
	// UserCode is the short code the user needs to enter on the authorization page.
	UserCode string
	// VerificationURI is the authorization page URL.
	VerificationURI string
	// VerificationURIComplete is the full URL with user_code (can be opened directly).
	VerificationURIComplete string
	// Interval is the polling interval suggested by upstream.
	Interval time.Duration
	// ExpiresAt is the expiration time of the device code.
	ExpiresAt time.Time
}

// LoginStart requests a device code and returns the authorization link the user needs to open.
func LoginStart(ctx context.Context) (*DeviceAuth, error) {
	form := url.Values{}
	form.Set("client_id", workosClientID)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, workosDeviceAuthURL,
		strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, fmt.Errorf("build device auth request: %w", errReq)
	}
	ApplyFormHeaders(req)

	resp, errDo := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "device auth: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read device auth response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(raw))
	}

	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode device auth response: %w", errUnmarshal)
	}
	if strings.TrimSpace(payload.DeviceCode) == "" {
		return nil, fmt.Errorf("device auth response has no device_code")
	}

	interval := time.Duration(payload.Interval) * time.Second
	if interval < minPollInterval {
		interval = minPollInterval
	}
	ttl := time.Duration(payload.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultDeviceTTL
	}
	return &DeviceAuth{
		DeviceCode:              strings.TrimSpace(payload.DeviceCode),
		UserCode:                strings.TrimSpace(payload.UserCode),
		VerificationURI:         core.FirstNonEmpty(payload.VerificationURIComplete, payload.VerificationURI),
		VerificationURIComplete: strings.TrimSpace(payload.VerificationURIComplete),
		Interval:                interval,
		ExpiresAt:               time.Now().Add(ttl),
	}, nil
}

// LoginPoll polls the device authorization status once.
//
// Returning (nil, nil) indicates that the user has not completed authorization yet (the caller should continue polling);
// returning credentials indicates that authorization is complete and has been exchanged for a Cline token.
func LoginPoll(ctx context.Context, baseURL string, deviceCode string) (*Credential, error) {
	form := url.Values{}
	form.Set("grant_type", deviceGrantType)
	form.Set("device_code", deviceCode)
	form.Set("client_id", workosClientID)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, workosAuthenticateURL,
		strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, fmt.Errorf("build poll request: %w", errReq)
	}
	ApplyFormHeaders(req)

	resp, errDo := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "login poll: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read poll response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}

	var payload struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode poll response: %w", errUnmarshal)
	}

	switch payload.Error {
	case "authorization_pending":
		// The user has not completed authorization in the browser yet - this is normal during polling.
		return nil, nil
	case "slow_down":
		// Polling too fast: continue waiting, the caller can throttle according to the interval.
		return nil, nil
	case "":
		// No error: proceed to check the token.
	default:
		message := core.FirstNonEmpty(payload.ErrorDescription, payload.Error)
		return nil, fmt.Errorf("Device authorization failed: %s", message)
	}

	if strings.TrimSpace(payload.AccessToken) == "" {
		// Neither error nor token: treat as "not completed yet" and continue polling.
		return nil, nil
	}

	// Exchange the WorkOS token for Cline's own token.
	return registerWithCline(ctx, baseURL, payload.AccessToken, payload.RefreshToken)
}

// registerWithCline exchanges the WorkOS token for a Cline token.
func registerWithCline(ctx context.Context, baseURL, workosAccess, workosRefresh string) (*Credential, error) {
	endpoint := BaseURL(baseURL) + RegisterPath
	body, errMarshal := json.Marshal(map[string]string{
		"accessToken":  workosAccess,
		"refreshToken": workosRefresh,
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("encode register request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build register request: %w", errReq)
	}
	ApplyJSONHeaders(req)

	resp, errDo := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "register: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read register response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(raw))
	}

	var payload struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
			UserInfo     *struct {
				Email string `json:"email"`
			} `json:"userInfo"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode register response: %w", errUnmarshal)
	}
	refreshToken := StripWorkOSPrefix(payload.Data.RefreshToken)
	if refreshToken == "" {
		// The refresh token is the only long-term credential; without it, this login is meaningless —
		// the access token expires in a few hours, and the account would become an unrefreshable dead account.
		return nil, fmt.Errorf("cline registration missing refresh token")
	}
	cred := &Credential{
		AccessToken:  StripWorkOSPrefix(payload.Data.AccessToken),
		RefreshToken: refreshToken,
		ExpiresAt:    parseExpiry(payload.Data.ExpiresAt),
	}
	if payload.Data.UserInfo != nil {
		cred.Email = strings.TrimSpace(payload.Data.UserInfo.Email)
	}
	cred.Label = core.FirstNonEmpty(cred.Email, cred.AccountID)
	return cred, nil
}

// StorageJSON constructs the credential JSON for disk persistence.
//
// The disk format is **camelCase** (consistent with upstream): the login artifact writes and reads itself,
// No need to cater to the underline export format of cline2api (that format is accepted
// as a compatibility form in ParseCredential).
func (c *Credential) StorageJSON() ([]byte, error) {
	payload := map[string]any{
		"accessToken":  c.AccessToken,
		"refreshToken": c.RefreshToken,
	}
	if c.ExpiresAt > 0 {
		payload["expiresAt"] = c.ExpiresAt
	}
	if c.Email != "" {
		payload["email"] = c.Email
	}
	if c.AccountID != "" {
		payload["accountId"] = c.AccountID
	}
	if c.Label != "" {
		payload["label"] = c.Label
	}
	return json.MarshalIndent(payload, "", "  ")
}

// MergeStorageJSON merges the refreshed tokens back into the original credential JSON.
//
// Only update the two mutable token fields and the expiration time; all other fields
// (such as user-written labels, accountId, etc.) are kept as-is.
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if len(original) == 0 || updated == nil {
		return original, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(original, &payload); errUnmarshal != nil {
		return original, nil
	}
	// Write back key names in the form already existing in the original file: underscored files should not be changed to camelCase
	// (users might read it elsewhere in its original format).
	accessKey, refreshKey, expiresKey := "accessToken", "refreshToken", "expiresAt"
	if _, okSnake := payload["access_token"]; okSnake {
		accessKey, refreshKey, expiresKey = "access_token", "refresh_token", "expires_at"
	}
	payload[accessKey] = updated.AccessToken
	payload[refreshKey] = updated.RefreshToken
	if updated.ExpiresAt > 0 {
		payload[expiresKey] = updated.ExpiresAt
	}
	merged, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return original, errMarshal
	}
	return merged, nil
}

// This file implements Cline credential refresh.
//
// Refresh chain (ported from cline2api's refreshClineToken):
//   POST {base}/auth/refresh
//   {"refreshToken":"<rt>","grantType":"refresh_token"}
//
// Note two details, neither of which is a typo:
//   - The field is **camelCase** refreshToken, not standard OAuth's refresh_token;
//   - The authorization type field is named **grantType**, not grant_type.
//     The upstream defined this shape themselves; sending it according to standard OAuth will be rejected.

// refreshHTTPTimeout is the timeout limit for refresh requests.
const refreshHTTPTimeout = 30 * time.Second

// RefreshCredential exchanges a refresh token for a new set of tokens.
//
// Returning updated=false indicates the upstream did not issue new tokens (theoretically shouldn't happen, but upstream occasionally
// returns the same values); the caller decides whether to write to disk based on this.
func RefreshCredential(ctx context.Context, baseURL string, cred *Credential) (*Credential, bool, error) {
	if cred == nil {
		return nil, false, fmt.Errorf("credential is nil")
	}
	refreshToken := StripWorkOSPrefix(cred.RefreshToken)
	if refreshToken == "" {
		return nil, false, NewCredentialError("cline_credential_invalid", "credential has no refresh token, cannot refresh")
	}

	endpoint := BaseURL(baseURL) + RefreshPath
	body, errMarshal := json.Marshal(map[string]string{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	if errMarshal != nil {
		return nil, false, fmt.Errorf("encode refresh request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, false, fmt.Errorf("build refresh request: %w", errReq)
	}
	ApplyJSONHeaders(req)

	resp, errDo := hostbridge.Client(ctx, refreshHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, false, &core.Failure{Msg: "refresh: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, false, &core.Failure{Status: resp.StatusCode, Msg: "read refresh response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, Classify(resp.StatusCode, string(raw))
	}

	var payload struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, false, fmt.Errorf("decode refresh response: %w", errUnmarshal)
	}
	newAccess := StripWorkOSPrefix(payload.Data.AccessToken)
	if newAccess == "" {
		return nil, false, NewCredentialError("cline_credential_invalid", "refresh response does not contain accessToken")
	}
	nextRefresh := StripWorkOSPrefix(payload.Data.RefreshToken)
	if nextRefresh == "" {
		// Upstream did not rotate the refresh token: keep the old one (otherwise the next refresh will fail).
		nextRefresh = refreshToken
	}
	if newAccess == cred.AccessToken && nextRefresh == cred.RefreshToken {
		// Upstream returned the same value: considered "valid" but not "refreshed", avoiding meaningless disk writes.
		return cred, false, nil
	}

	updated := &Credential{
		AccessToken:  newAccess,
		RefreshToken: nextRefresh,
		ExpiresAt:    parseExpiry(payload.Data.ExpiresAt),
		Email:        cred.Email,
		Label:        cred.Label,
		AccountID:    cred.AccountID,
	}
	return updated, true, nil
}

// NewCredentialError constructs a credential invalidation error.
func NewCredentialError(code, message string) *core.Failure {
	return &core.Failure{Code: code, Msg: message, Kind: core.FailureKindCredential, Status: http.StatusUnauthorized}
}

// parseExpiry parses the expiration time, compatible with various upstream formats.
//
// Upstream's expiresAt is any: could be Unix milliseconds (float64 / int64),
// or an RFC3339 string. Returns 0 on parsing failure—the caller will treat it as "unknown",
// and refresh immediately once, at the cost of only one extra request.
func parseExpiry(raw any) int64 {
	switch value := raw.(type) {
	case float64:
		return millisToSeconds(int64(value))
	case int64:
		return millisToSeconds(value)
	case int:
		return millisToSeconds(int64(value))
	case json.Number:
		parsed, errParse := value.Int64()
		if errParse != nil {
			return 0
		}
		return millisToSeconds(parsed)
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return 0
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, errParse := time.Parse(layout, trimmed); errParse == nil {
				return parsed.Unix()
			}
		}
		return 0
	}
	return 0
}

// millisToSeconds converts Unix milliseconds to Unix seconds.
//
// Upstream provides milliseconds. If the value is less than 1e12, it means it is actually seconds (upstream interfaces sometimes mix them),
// in which case it is returned as-is—forcing division by milliseconds would make the expiration time fall back to 1970, causing the account to be judged as
// permanently expired and triggering repeated refreshes.
func millisToSeconds(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value < 1_000_000_000_000 {
		return value
	}
	return value / 1000
}

// AuthMode reports the credential form of this provider.
func (v *adapter) AuthMode() string { return "oauth" }
