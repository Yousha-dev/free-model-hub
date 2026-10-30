package minimaxcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// PendingLogin records ongoing OAuth device code authorization sessions.
type PendingLogin struct {
	SessionID  string
	Region     Region
	DeviceCode string
	Verifier   string
	UserCode   string
	VerifyURL  string
	Interval   time.Duration
	LastPoll   time.Time
	ExpiresAt  time.Time
}

func pkcePair() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// LoginStart requests an OAuth2 device code from the MiniMax account service.
func LoginStart(ctx context.Context, logins core.LoginSessions, r Region) (*pluginapi.AuthLoginStartResponse, error) {
	verifier, challenge, errPKCE := pkcePair()
	if errPKCE != nil {
		return nil, fmt.Errorf("generate PKCE: %w", errPKCE)
	}

	accountHost := AccountHostFor(r)
	codeURL := accountHost + EpDeviceCode

	reqBody, _ := json.Marshal(map[string]any{
		"client_id":             ClientID,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"scope":                 OAuthScope,
		"audience":              OAuthAudience,
	})

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, codeURL, bytes.NewReader(reqBody))
	if errReq != nil {
		return nil, fmt.Errorf("create device code request: %w", errReq)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, 30*time.Second).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request device code: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&payload); errDec != nil {
		return nil, fmt.Errorf("decode device code response: %w", errDec)
	}

	deviceCode := core.GetString(payload, "device_code")
	userCode := core.GetString(payload, "user_code")
	if deviceCode == "" {
		return nil, fmt.Errorf("device code response missing device_code: %v", payload)
	}

	verifyURL := core.FirstNonEmpty(
		core.GetString(payload, "verification_uri_complete"),
		core.GetString(payload, "verification_uri"),
		fmt.Sprintf("%s/oauth-authorize?user_code=%s", accountHost, userCode),
	)

	intervalSec := getInt(payload, "interval")
	if intervalSec < 3 {
		intervalSec = 3
	}
	expiresInSec := getInt(payload, "expires_in")
	if expiresInSec <= 0 {
		expiresInSec = 300
	}

	sessionID := "minimaxcode_" + string(r) + "_" + core.RandomHex(8)
	expiresAt := time.Now().Add(time.Duration(expiresInSec) * time.Second)

	pending := &PendingLogin{
		SessionID:  sessionID,
		Region:     r,
		DeviceCode: deviceCode,
		Verifier:   verifier,
		UserCode:   userCode,
		VerifyURL:  verifyURL,
		Interval:   time.Duration(intervalSec) * time.Second,
		LastPoll:   time.Now(),
		ExpiresAt:  expiresAt,
	}

	logins.Register(VendorIDFor(r), sessionID, time.Duration(expiresInSec)*time.Second, time.Duration(intervalSec)*time.Second, pending)

	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       verifyURL,
		State:     sessionID,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			"user_code":    userCode,
			core.VendorKey: VendorIDFor(r),
			"region":       string(r),
		},
	}, nil
}

// LoginPoll polls the device code authorization status.
func LoginPoll(ctx context.Context, logins core.LoginSessions, state string, baseURLOverride string) (*pluginapi.AuthLoginPollResponse, error) {
	cleanState := strings.TrimSpace(state)
	entry, pollState := logins.Poll(cleanState)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session does not exist or has expired, please initiate login again",
		}, nil
	}
	if pollState == core.PollThrottled {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Waiting for user to authorize in the browser...",
		}, nil
	}
	session, okSession := entry.Data.(*PendingLogin)
	if !okSession {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session data is corrupted, please initiate login again",
		}, nil
	}

	accountHost := AccountHostFor(session.Region)
	tokenURL := accountHost + EpToken

	data := url.Values{}
	data.Set("client_id", ClientID)
	data.Set("grant_type", DeviceGrantType)
	data.Set("device_code", session.DeviceCode)
	data.Set("code_verifier", session.Verifier)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if errReq != nil {
		return nil, fmt.Errorf("create poll request: %w", errReq)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, errDo := hostbridge.Client(ctx, 30*time.Second).Do(req)
	if errDo != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Retrying network request: " + errDo.Error(),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	var payload map[string]any
	rawBytes, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(rawBytes, &payload)

	errCode := core.GetString(payload, "error")
	switch errCode {
	case "authorization_pending":
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Waiting for user to authorize in the browser...",
		}, nil
	case "slow_down":
		session.Interval += 2 * time.Second
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Waiting for user to authorize in the browser...",
		}, nil
	case "expired_token":
		logins.Delete(cleanState)
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Authorization code has expired, please initiate login again",
		}, nil
	case "access_denied":
		logins.Delete(cleanState)
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "User denied authorization in the browser",
		}, nil
	}

	if resp.StatusCode >= 400 || errCode != "" {
		logins.Delete(cleanState)
		errDesc := core.FirstNonEmpty(core.GetString(payload, "error_description"), errCode, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Authorization failed: " + errDesc,
		}, nil
	}

	accessToken := core.GetString(payload, "access_token", "accessToken")
	if accessToken == "" {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Waiting for user to authorize in the browser...",
		}, nil
	}

	// Authorization successful, clear session and build credentials
	logins.Delete(cleanState)

	refreshToken := core.GetString(payload, "refresh_token", "refreshToken")
	expiresIn := core.GetInt64(payload, "expires_in", "expiresIn")
	if expiresIn <= 0 {
		expiresIn = 3600
	}

	cred := &Credential{
		Vendor:       VendorIDFor(session.Region),
		Region:       session.Region,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Unix() + expiresIn,
		AuthMode:     "oauth",
	}
	cred.EnsureIdentifiers()

	// Probe user real ID and Agent ID.
	//
	// Access token must be verified before persisting: upstream occasionally returns a success shape but the token is unusable,
	// resulting in a persistently 401 account once written to the credential file, leaving the user with a broken account and having to guess why.
	// Only credential-level failures reject persistence—network jitters should not invalidate an already successful authorization.
	if _, _, _, errInfo := fetchUserInfo(ctx, cred, baseURLOverride); isCredentialFailure(errInfo) {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Authorization completed but credential is not available, please initiate login again (" + errInfo.Error() + ")",
		}, nil
	}
	_ = prepare(ctx, cred, baseURLOverride, nil)

	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Failed to generate credential: " + errStorage.Error(),
		}, nil
	}

	accountIdentifier := core.FirstNonEmpty(cred.UserID, cred.Email, session.UserCode, core.RandomHex(4))
	label := cred.Label
	if label == "" {
		label = VendorNameFor(session.Region) + "-" + accountIdentifier
	}

	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "Login successful",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(VendorIDFor(session.Region), accountIdentifier),
			Label:       label,
			StorageJSON: storageJSON,
		},
	}, nil
}

// RefreshCredential uses refresh_token to refresh the MiniMax OAuth access token.
func RefreshCredential(ctx context.Context, cred *Credential, baseURLOverride string) (*Credential, bool, error) {
	if cred == nil {
		return nil, false, core.NewPluginError("credential_missing", "credential is nil", http.StatusUnauthorized)
	}

	refreshToken := strings.TrimSpace(cred.RefreshToken)
	if refreshToken == "" || cred.AuthMode == "token" {
		// Web JWT tokens without a refresh_token cannot be automatically refreshed
		return cred, false, nil
	}

	accountHost := AccountHostFor(cred.Region)
	tokenURL := accountHost + EpToken

	data := url.Values{}
	data.Set("client_id", ClientID)
	data.Set("grant_type", RefreshGrantType)
	data.Set("refresh_token", refreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if errReq != nil {
		return nil, false, fmt.Errorf("build refresh request: %w", errReq)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, false, fmt.Errorf("refresh request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&payload); errDec != nil {
		return nil, false, fmt.Errorf("decode refresh response (HTTP %d): %w", resp.StatusCode, errDec)
	}

	if resp.StatusCode >= 400 || core.GetString(payload, "error") != "" {
		errDesc := core.FirstNonEmpty(
			core.GetString(payload, "error_description"),
			core.GetString(payload, "error"),
			fmt.Sprintf("HTTP %d", resp.StatusCode),
		)
		// Must be a structured error: the host only treats 401/403/402 of core.UpstreamError as
		// "broken credential". A plain fmt.Errorf would be treated as upstream jitter and retried repeatedly—the account is neither marked broken
		// nor shows any re-authorization prompt, leaving the user unable to check their quota indefinitely.
		failure := Classify(resp.StatusCode, errDesc)
		if failure.Kind == core.FailureKindCredential {
			failure.Msg = "MiniMax authorization has expired, please re-add authorization for this account (" + errDesc + ")"
		}
		return nil, false, failure
	}

	accessToken := core.GetString(payload, "access_token", "accessToken")
	if accessToken == "" {
		return nil, false, fmt.Errorf("minimax refresh response missing access_token")
	}

	newRefreshToken := core.GetString(payload, "refresh_token", "refreshToken")
	expiresIn := core.GetInt64(payload, "expires_in", "expiresIn")
	if expiresIn <= 0 {
		expiresIn = 3600
	}

	cred.AccessToken = accessToken
	if newRefreshToken != "" {
		cred.RefreshToken = newRefreshToken
	}
	cred.ExpiresAt = time.Now().Unix() + expiresIn

	return cred, true, nil
}
