package zcode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// OAuth CLI login flow (headless, server-side relayed) and API Key exchange.
//
// Flow:
//  1. POST oauth/cli/init (Bearer <local poll_token>) -> flow_id + authorize_url
//     + poll_token. **Starting Sep 2026, poll_token is issued by the server**, and subsequent polling must use
//     this value; falls back to a local random value when not issued (legacy protocol compatibility).
//  2. User opens authorize_url in the browser to complete authorization.
//  3. GET oauth/cli/poll/{flow_id} (Bearer <poll_token>) -> when status=ready, returns
//     token (Coding Plan JWT) and zai.access_token.
//  4. Exchange access_token for business API Key (api.z.ai chain), bypassing reCAPTCHA.
//
// Request headers are deliberately kept to the minimum set: the official CLI only carries Authorization and Content-Type,
// and including forged headers will cause upstream to impose abnormal device binding restrictions on the OAuth session.
// Refer to zcode2api/app/oauth.py.

const (
	// loginFlowTTL is the validity period of a single login session.
	loginFlowTTL = 15 * time.Minute
	// loginHTTPTimeout is the upper limit for a single login-related request.
	loginHTTPTimeout = 30 * time.Second
	// loginPollInterval is the minimum interval between two visits to the upstream poll.
	//
	// The console page polls every 2 seconds; without throttling, upstream would see request densities far exceeding the official client.
	loginPollInterval = 1500 * time.Millisecond
)

// pendingLogin is data for an ongoing login session (TTL / throttling are uniformly managed by the auth session table).
type pendingLogin struct {
	FlowID    string
	PollToken string
}

// LoginStart initiates OAuth authorization, returning the link the user needs to open in the browser.
//
// Does not accept upstream base address override parameters: OAuth init/poll are fixed at zcode.z.ai, which are different domains from
// the chat channel (api.z.ai, which can be overridden via zcode_base_url). Early versions passed chat
// override values in, causing users who only wanted to change the chat address to also break the login chain.
func LoginStart(ctx context.Context, logins core.LoginSessions) (*pluginapi.AuthLoginStartResponse, error) {
	pollToken := NewHexID(32)

	body, _ := json.Marshal(map[string]string{"provider": "zai"})
	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost,
		ZCodeOrigin+PathOAuthInit, bytes.NewReader(body))
	if errNew != nil {
		return nil, fmt.Errorf("create oauth init request: %w", errNew)
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("oauth init request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("OAuth initialization failed (HTTP %d): %s", resp.StatusCode, textPreview(string(raw)))
	}

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FlowID       string `json:"flow_id"`
			AuthorizeURL string `json:"authorize_url"`
			PollToken    string `json:"poll_token"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode oauth init response: %w", errUnmarshal)
	}
	if payload.Code != codeSuccess {
		return nil, fmt.Errorf("OAuth initialization rejected by upstream (code=%d): %s", payload.Code, payload.Msg)
	}
	if payload.Data.FlowID == "" || payload.Data.AuthorizeURL == "" {
		return nil, fmt.Errorf("Incomplete flow data returned by OAuth initialization")
	}
	// Server-issued poll_token takes priority (new protocol); falls back to local random value if not issued.
	effectiveToken := core.FirstNonEmpty(payload.Data.PollToken, pollToken)

	sessionID := "zcode_" + NewHexID(8)
	logins.Register(VendorID, sessionID, loginFlowTTL, loginPollInterval, &pendingLogin{
		FlowID:    payload.Data.FlowID,
		PollToken: effectiveToken,
	})

	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       payload.Data.AuthorizeURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(loginFlowTTL),
	}, nil
}

// LoginPoll polls authorization status once; exchanges for complete credentials upon completion.
func LoginPoll(ctx context.Context, logins core.LoginSessions, sessionID string) (*pluginapi.AuthLoginPollResponse, error) {
	session, pollState := logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session does not exist or has expired, please restart login",
		}, nil
	}
	// Polling throttling: return pending if interval is not reached, preventing the panel from spamming upstream.
	if pollState == core.PollThrottled {
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	pending, okPending := session.Data.(*pendingLogin)
	if !okPending {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session data is corrupted, please restart login",
		}, nil
	}

	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet,
		ZCodeOrigin+PathOAuthPoll+"/"+pending.FlowID, nil)
	if errNew != nil {
		return nil, fmt.Errorf("create poll request: %w", errNew)
	}
	req.Header.Set("Authorization", "Bearer "+pending.PollToken)

	resp, errDo := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		// Do not kill session on network jitter: retain session and retry on next poll.
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Status string `json:"status"`
			// JWT field names have changed across upstream versions (token / zcodejwttoken), accept both.
			Token         string `json:"token"`
			ZCodeJWTToken string `json:"zcodejwttoken"`
			AccessToken   string `json:"access_token"`
			Zai           struct {
				AccessToken string `json:"access_token"`
			} `json:"zai"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	// code=3004 is explicitly expired session from upstream, remaining 4xx are treated as terminal failures.
	if payload.Code != codeSuccess {
		if payload.Code == codeSessionExpired {
			logins.Delete(sessionID)
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusError,
				Message: "Authorization session has expired, please restart login",
			}, nil
		}
		if resp.StatusCode >= 400 {
			logins.Delete(sessionID)
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusError,
				Message: fmt.Sprintf("Upstream rejected polling (HTTP %d, code=%d): %s", resp.StatusCode, payload.Code, payload.Msg),
			}, nil
		}
	}
	if payload.Data.Status == "failed" {
		logins.Delete(sessionID)
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Authorization was denied or failed",
		}, nil
	}

	// Determination is based on "obtaining JWT" rather than the status string: upstream providing a token means authorization is complete,
	// waiting stubbornly for a specific status value would keep the session stuck in pending until timeout.
	jwt := core.FirstNonEmpty(payload.Data.Token, payload.Data.ZCodeJWTToken)
	if jwt == "" {
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	logins.Delete(sessionID)
	cred := &Credential{
		Vendor:   VendorID,
		JWTToken: jwt,
		AuthMode: "oauth",
		Profile:  NewDeviceProfile(),
		Email:    EmailFromJWT(jwt),
		UserID:   UserIDFromJWT(jwt),
	}

	// Exchange API Key: persisted together as an alternative credential.
	// Exchange failure does not block login—JWT can still be used for quota queries and plan redemption.
	apiWarning := ""
	// access_token may be at the top level of data or under data.zai, accept both.
	accessToken := core.FirstNonEmpty(payload.Data.Zai.AccessToken, payload.Data.AccessToken)
	if accessToken != "" {
		apiKey, errExchange := ExchangeAPIKey(ctx, accessToken)
		if errExchange != nil {
			apiWarning = fmt.Sprintf("(API Key exchange failed: %v, using Start Plan JWT credential)", errExchange)
		} else {
			cred.APIKey = apiKey
		}
	} else {
		apiWarning = "(Upstream did not return access_token, using Start Plan JWT credential)"
	}

	cred.Label = core.FirstNonEmpty(cred.Email, "ZCode-"+MaskedKey(core.FirstNonEmpty(cred.APIKey, cred.JWTToken)))

	storage, errJSON := cred.StorageJSON()
	if errJSON != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Failed to generate credential: " + errJSON.Error(),
		}, nil
	}

	fileName := zcodeFileName(cred)
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "Login successful" + apiWarning,
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			ID:          strings.TrimSuffix(fileName, ".json"),
			FileName:    fileName,
			Label:       cred.Label,
			StorageJSON: storage,
		},
	}, nil
}

// ExchangeAPIKey exchanges the OAuth access_token for a business API Key.
//
// Flow: z/login for business token → getCustomerInfo for org and project → find/create
// Key with name="zcode-api-key" → copy endpoint to decrypt secretKey, combined into
// "<apiKey>.<secretKey>" (Zhipu API Key format).
func ExchangeAPIKey(ctx context.Context, accessToken string) (string, error) {
	client := hostbridge.Client(ctx, loginHTTPTimeout)

	bizToken, errBiz := exchangeBizToken(ctx, client, accessToken)
	if errBiz != nil {
		return "", errBiz
	}

	orgID, projID, errOrg := lookupOrgProject(ctx, client, bizToken)
	if errOrg != nil {
		return "", errOrg
	}

	keyURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys", ZAIOrigin, orgID, projID)
	apiKey, errKey := ensureAPIKey(ctx, client, bizToken, keyURL)
	if errKey != nil {
		return "", errKey
	}

	secretKey, errSecret := decryptSecret(ctx, client, bizToken, keyURL, apiKey)
	if errSecret != nil {
		return "", errSecret
	}
	return apiKey + "." + secretKey, nil
}

// exchangeBizToken exchanges the OAuth access_token for a business token.
func exchangeBizToken(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	body, _ := json.Marshal(map[string]string{"token": accessToken})
	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost, ZAIOrigin+PathZLogin, bytes.NewReader(body))
	if errNew != nil {
		return "", errNew
	}
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("z/login request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("z/login failed (HTTP %d): %s", resp.StatusCode, textPreview(string(raw)))
	}
	var payload struct {
		Data struct {
			AccessToken    string `json:"access_token"`
			AccessTokenAlt string `json:"accessToken"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", fmt.Errorf("failed to parse z/login response: %w", errUnmarshal)
	}
	token := core.FirstNonEmpty(payload.Data.AccessToken, payload.Data.AccessTokenAlt)
	if token == "" {
		return "", fmt.Errorf("z/login response does not contain business credential")
	}
	return token, nil
}

// lookupOrgProject gets the IDs of the "default organization" and "default project".
func lookupOrgProject(ctx context.Context, client *http.Client, bizToken string) (string, string, error) {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, ZAIOrigin+PathCustomerInfo, nil)
	if errNew != nil {
		return "", "", errNew
	}
	req.Header.Set("Authorization", "Bearer "+bizToken)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", "", fmt.Errorf("getCustomerInfo request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("getCustomerInfo failed (HTTP %d)", resp.StatusCode)
	}

	var payload struct {
		Data struct {
			Organizations []struct {
				OrganizationID   string `json:"organizationId"`
				OrganizationName string `json:"organizationName"`
				Projects         []struct {
					ProjectID   string `json:"projectId"`
					ProjectName string `json:"projectName"`
				} `json:"projects"`
			} `json:"organizations"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", "", fmt.Errorf("failed to parse customer info: %w", errUnmarshal)
	}
	orgs := payload.Data.Organizations
	if len(orgs) == 0 {
		return "", "", fmt.Errorf("no available organization under the account")
	}

	// Prioritize "default organization" / "default project", otherwise fall back to the first one.
	org := orgs[0]
	for _, item := range orgs {
		if strings.Contains(item.OrganizationName, "默认机构") {
			org = item
			break
		}
	}
	if len(org.Projects) == 0 {
		return "", "", fmt.Errorf("no available project under the organization")
	}
	proj := org.Projects[0]
	for _, item := range org.Projects {
		if strings.Contains(item.ProjectName, "默认项目") {
			proj = item
			break
		}
	}
	return org.OrganizationID, proj.ProjectID, nil
}

// ensureAPIKey searches for an existing zcode-api-key, and creates one if none exists.
func ensureAPIKey(ctx context.Context, client *http.Client, bizToken, keyURL string) (string, error) {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, keyURL, nil)
	if errNew != nil {
		return "", errNew
	}
	req.Header.Set("Authorization", "Bearer "+bizToken)

	if resp, errDo := client.Do(req); errDo == nil {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		var payload struct {
			Data []struct {
				Name   string `json:"name"`
				APIKey string `json:"apiKey"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			for _, item := range payload.Data {
				if item.Name == "zcode-api-key" && strings.TrimSpace(item.APIKey) != "" {
					return item.APIKey, nil
				}
			}
		}
	}

	body, _ := json.Marshal(map[string]string{"name": "zcode-api-key"})
	createReq, errCreate := http.NewRequestWithContext(ctx, http.MethodPost, keyURL, bytes.NewReader(body))
	if errCreate != nil {
		return "", errCreate
	}
	createReq.Header.Set("Authorization", "Bearer "+bizToken)
	createReq.Header.Set("Content-Type", "application/json")

	resp, errDo := client.Do(createReq)
	if errDo != nil {
		return "", fmt.Errorf("create API Key request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Data struct {
			APIKey string `json:"apiKey"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil || payload.Data.APIKey == "" {
		return "", fmt.Errorf("failed to create API Key: %s", textPreview(string(raw)))
	}
	return payload.Data.APIKey, nil
}

// decryptSecret calls the copy endpoint to retrieve the secretKey.
//
// The list endpoint only provides the apiKey in plaintext; the secretKey must be decrypted via this endpoint, and both are combined to form the complete credential.
func decryptSecret(ctx context.Context, client *http.Client, bizToken, keyURL, apiKey string) (string, error) {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, keyURL+"/copy/"+apiKey, nil)
	if errNew != nil {
		return "", errNew
	}
	req.Header.Set("Authorization", "Bearer "+bizToken)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("decrypt Secret Key request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Data struct {
			SecretKey string `json:"secretKey"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", fmt.Errorf("failed to parse Secret Key response: %w", errUnmarshal)
	}
	if payload.Data.SecretKey == "" {
		return "", fmt.Errorf("failed to decrypt Secret Key")
	}
	return payload.Data.SecretKey, nil
}

// zcodeFileName generates the credential filename (email prioritized for account identification).
func zcodeFileName(cred *Credential) string {
	seed := core.FirstNonEmpty(cred.Email, MaskedKey(core.FirstNonEmpty(cred.APIKey, cred.JWTToken)))
	return fileNameFor(seed)
}

// base64URLDecode decodes base64url (tolerates missing padding).
func base64URLDecode(segment string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return decoded, nil
	}
	padded := segment + strings.Repeat("=", (4-len(segment)%4)%4)
	return base64.URLEncoding.DecodeString(padded)
}

// Refresh validates the credential.
//
// ZCode does not have a refresh_token flow (neither JWT nor API Key supports refresh), so this only performs
// local validation and returns refreshed=false—avoiding meaningless disk writes.
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	if errValidate := ValidateCredential(ctx, native); errValidate != nil {
		return nil, false, errValidate
	}
	return cred, false, nil
}

// ValidateCredential validates the credential format and upstream availability.
//
// Performs local format validation only, generating no upstream traffic: the host calls this method on every refresh,
// and hitting upstream with it would turn quota checking into a periodic risk control signal.
func ValidateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return transientErr("zcode credential is nil")
	}
	if strings.TrimSpace(cred.APIKey) == "" && strings.TrimSpace(cred.JWTToken) == "" {
		return transientErr("zcode credential missing both api_key and jwt_token")
	}
	return nil
}

// MaskedKey masks the credential: keeps the first 4 and last 4 characters.
func MaskedKey(val string) string {
	s := strings.TrimSpace(val)
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "***" + s[len(s)-4:]
}
