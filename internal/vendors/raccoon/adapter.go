package raccoon

// This file adapts the Raccoon (SenseTime Raccoon) protocol into core.Vendor.
//
// Capabilities (aligned with tested Agent2API series protocols):
//   - Credential parsing and strict ownership matching (JWT access/refresh pair + uid);
//   - Chat routed through LLM gateway (OpenAI format, forwarded as-is);
//   - Real-time model catalog fetching (model_catalog), falling back to a static single entry if unavailable;
//   - Automatic token refresh (POST /refresh, refresh_token may rotate);
//   - Points quota query (GET {points_base}/balance);
//   - Daily check-in (login to claim grant, idempotent);
//   - QR code login (qrcode_code polling, no callback address needed) and authorization code ticket fallback.
//
// Raccoon has only one CN site (xiaohuanxiong.com) with no regional division.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// adapter is the implementation of core.Vendor for Raccoon.
type adapter struct {
	*core.NativeAdapter[*Credential]
	// llmBase / authBase / pointsBase / origin are domain base URL overrides (populated by Configure).
	llmBase    string
	authBase   string
	pointsBase string
	origin     string

	// credits is "official model ID -> billing rate text", sharing the same origin and lifecycle as models.
	//
	// The host contract (pluginapi.ModelInfo) does not have a rate field, and the console can only
	// access it via core.CreditsProvider, so it is produced together with the model list in the same parsing round.
	credits core.CreditTable
}

// Instances constructs Raccoon instances (only one, no regional division).
func Instances(env core.Env) []core.Vendor {
	inst := &adapter{}
	// Hook captures inst: base URL overrides written by Configure apply to token refresh as well.
	inst.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorID, VendorName: VendorName},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := ParseCredential(raw)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			uid := native.UIDOrTokenID()
			return core.CredentialSeed{
				VendorID: VendorID,
				Label:    core.FirstNonEmpty(native.NicknameValue(), uid, core.MaskToken(native.AccessTokenValue())),
				FileID:   core.FileIDFromFileName(fileName), UID: uid,
				Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAtValue(), AuthMode: "oauth",
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			accessBefore, refreshBefore := native.Snapshot()
			access, refresh, expiresAt, uid, errRefresh := RefreshToken(ctx, inst.resolveAuthBase(), native)
			if errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			if expiresAt == 0 {
				expiresAt = core.JWTExpiry(access)
			}
			native.ApplyTokens(access, refresh, expiresAt)
			native.ApplyIdentity(uid, "")
			refreshed := !native.UnchangedSince(accessBefore, refreshBefore)
			return core.NativeRefresh[*Credential]{
				Native: native, Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAtValue(), Changed: refreshed,
			}, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeCredentialJSON(original, native)
		},
	})
	return []core.Vendor{inst}
}

// Configure parses Raccoon-specific configuration (domain base URL overrides).
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.llmBase = strings.TrimSpace(raw["raccoon_llm_base_url"])
	v.authBase = strings.TrimSpace(raw["raccoon_auth_base_url"])
	v.pointsBase = strings.TrimSpace(raw["raccoon_points_base_url"])
	v.origin = strings.TrimSpace(raw["raccoon_origin"])
}

// ConfigFields declares Raccoon-specific configuration.
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "raccoon_llm_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "LLM gateway base URL override (default " + DefaultLLMBase + ")."},
		{Name: "raccoon_auth_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Auth base URL override (default " + DefaultAuthBase + ")."},
		{Name: "raccoon_points_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Points base URL override (default " + DefaultPointsBase + ")."},
		{Name: "raccoon_origin", Type: pluginapi.ConfigFieldTypeString, Description: "Main site origin override (default " + DefaultOrigin + ")."},
	}
}

// Match determines whether a credential belongs to this provider.
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// ---- Models ----

// StaticModels returns the model catalog.
//
// Probing requires a valid credential; returns an error if unavailable (does not build a catalog).
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	token := v.probeToken()
	if token == "" {
		return nil, fmt.Errorf("no raccoon credential available for model probe")
	}
	models, errModels := FetchModels(ctx, v.resolveLLMBase(), token)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// ModelsForAuth returns the models available for the credential.
//
// Probes using the credential's own token; falls back to a static fallback single entry rather than
// returning empty when the catalog fails (Raccoon catalog API is stable; the fallback ensures accounts remain usable during catalog fluctuations).
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	models, errModels := FetchModels(ctx, v.resolveLLMBase(), native.AccessTokenValue())
	if errModels != nil {
		logger.Info("raccoon: model catalog fallback to static: %v", errModels)
		models = StaticFallbackModels()
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI converts catalog entries to host contract types and records the rate table along the way.
//
// Rates and catalog must come from the same probing round: fetching them separately would cause a mismatch
// where "models changed, but rates are still from the previous batch" (consistent with cline / qoder handling).
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return model.Credits })
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               core.FirstNonEmpty(model.Name, id),
			Description:        core.FirstNonEmpty(model.Description, "Raccoon upstream model"),
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxOutputTokens,
			SupportsTools:      model.SupportsTools,
			SupportsImages:     model.SupportsImages,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ModelCredits implements core.CreditsProvider: returns "official model ID -> billing rate".
//
// Rates are taken from billing_effective_multiplier (actual payment) of catalog entries, falling back
// to billing_multiplier, and uniformly normalized via core.FormatRate into texts like "0.80X";
// models not provided upstream are omitted from the table, allowing the console to display "-" instead of rendering "not provided" as "free".
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// ---- Execution ----

// Execute performs a non-streaming chat.
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, v.resolveLLMBase(), ChatRequest{
		Token: native.AccessTokenValue(),
		Body:  req.Payload,
	})
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream executes a streaming chat.
//
// It only delivers upstream raw SSE data payloads without `data: ` wrapping and framing—the wrapping is done by the host
// according to the declared format.
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	resp, errStream := ChatStream(ctx, v.resolveLLMBase(), ChatRequest{
		Token: native.AccessTokenValue(),
		Body:  req.Payload,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// ---- Quota and Check-in ----

// Quota queries the account points balance.
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	available, errBalance := FetchBalance(ctx, v.resolvePointsBase(), native)
	if errBalance != nil {
		return nil, core.ToPluginError(errBalance)
	}
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "points", TierName: "Raccoon Points"},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "Available Points", Value: float64(available), Format: "number"},
		},
	}, nil
}

// ---- Login and Refresh ----

// LoginStart initiates a web authorization login: generates a one-time state and registers the session,
// returns the official authorization page URL (after the user logs in on the page, the browser redirects via office-raccoon://,
// paste the entire link back to the console for SubmitCallback to complete token exchange).
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	state := core.RandomHex(32)
	sessionID := VendorID + "_" + state
	StoreLoginSession(v.Env.Logins, sessionID, &PendingLogin{State: state})
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       AuthorizeURLWith(v.resolveOrigin(), loginAppName, state),
		State:     sessionID,
		ExpiresAt: time.Now().Add(qrLoginTTL),
		Metadata:  map[string]any{core.VendorKey: VendorID},
	}, nil
}

// LoginPoll polls the login status once.
//
// The authorization page mode has no pollable upstream endpoint: credentials only arrive via SubmitCallback
// when the user pastes the callback link back. Only progress is reported here; state mismatches (user pasting)
// are rejected by SubmitCallback.
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	_, exists, _ := CheckLoginSession(v.Env.Logins, state)
	if !exists {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session does not exist or has expired, please restart login",
		}, nil
	}
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: pendingMessage,
	}, nil
}

// LocalCallbackHint returns the callback format to be displayed in the console (used as a fallback for authorization code exchange).
func (v *adapter) LocalCallbackHint() string {
	return "office-raccoon://auth/callback?code=…&state=…"
}

// SubmitCallback parses the callback link (or raw authorization code) pasted by the user and exchanges it for a token to complete login.
//
// This is a fallback for QR code login: users can also use the office-raccoon:// callback on the official page,
// and paste the entire address bar link back to exchange for a token.
func (v *adapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	session, pollState := v.Env.Logins.Poll(strings.TrimSpace(state))
	if pollState == core.PollMissing {
		return nil, core.NewPluginError("session_not_found", "Login session does not exist or has expired, please restart login", http.StatusNotFound)
	}
	pending, okPending := session.Data.(*PendingLogin)
	if !okPending {
		return nil, core.NewPluginError("login_session_broken", "Login session data is corrupted, please restart login", http.StatusBadRequest)
	}
	code, callbackState, okCode := ParseCallbackCode(rawURL)
	if !okCode {
		return nil, core.NewPluginError("callback_parse_failed",
			"Failed to parse callback link, please ensure you pasted the complete callback link (office-raccoon://auth/callback?code=…)",
			http.StatusBadRequest)
	}
	if !CheckCallbackState(pending, callbackState) {
		return nil, core.NewPluginError("callback_state_mismatch",
			"Login callback state verification failed, please restart login",
			http.StatusBadRequest)
	}
	access, refresh, expiresAt, uid, errExchange := ExchangeAuthorizationCode(ctx, v.resolveAuthBase(), code)
	if errExchange != nil {
		return nil, core.ToPluginError(errExchange)
	}
	credential := &Credential{AccessToken: access, RefreshToken: refresh, ExpiresAt: expiresAt}
	if expiresAt == 0 {
		credential.ExpiresAt = core.JWTExpiry(access)
	}
	if uid != "" {
		credential.ApplyIdentity(uid, "")
	}
	if uidInfo, nickname, errInfo := FetchUserInfo(ctx, v.resolveAuthBase(), credential); errInfo == nil {
		credential.ApplyIdentity(uidInfo, nickname)
	}
	if credential.UIDOrTokenID() == "" {
		return nil, core.NewPluginError("login_failed", "Authorization code exchange did not return user identifier", http.StatusInternalServerError)
	}
	storageJSON, errStorage := credential.StorageJSON()
	if errStorage != nil {
		return nil, core.NewPluginError("login_failed", "Failed to build credential: "+errStorage.Error(), http.StatusInternalServerError)
	}
	v.Env.Logins.Delete(strings.TrimSpace(state))
	return &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "Raccoon login successful",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(VendorID, credential.UIDOrTokenID()),
			Label:       core.FirstNonEmpty(credential.NicknameValue(), credential.UIDOrTokenID()),
			StorageJSON: storageJSON,
		},
	}, nil
}

// MergeCredentialJSON implements storage merging (see credential.go).

// ---- Internal Utilities ----

func (v *adapter) resolveLLMBase() string    { return BaseURL(v.llmBase, DefaultLLMBase) }
func (v *adapter) resolveAuthBase() string   { return BaseURL(v.authBase, DefaultAuthBase) }
func (v *adapter) resolvePointsBase() string { return BaseURL(v.pointsBase, DefaultPointsBase) }
func (v *adapter) resolveOrigin() string     { return BaseURL(v.origin, DefaultOrigin) }

// probeToken borrows an available credential of this provider to get an access token (for model probing).
func (v *adapter) probeToken() string {
	native, okNative := core.ProbeNativeCredential(v.Env, VendorID, func(cred *core.Credential) (*Credential, bool) {
		value, okValue := cred.Native.(*Credential)
		return value, okValue
	})
	if !okNative || native == nil {
		return ""
	}
	return native.AccessTokenValue()
}

// Compile-time assertion: provider capabilities actually possessed by the adapter.
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.CallbackSubmitter    = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.StorageMerger        = (*adapter)(nil)
)
