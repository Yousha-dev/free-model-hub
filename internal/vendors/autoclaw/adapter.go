package autoclaw

// This file adapts the AutoClaw protocol to core.Vendor.
//
// Vendor instance = Protocol × Region. CN and Global are two independently deployed sets (userapi base
// different), tokens are not shared; the region is fixed at construction time and no longer branched within instance methods.
//
// Capability surface (aligned with Agent2API measured protocol):
//   - Credential parsing and strict attribution (accessToken/refreshToken structure);
//   - Chat goes through region proxy channel (X-Request-Model declares model);
//   - Model list: global fetches autoclaw-model-config in real-time, CN uses static table;
//   - Automatic token refresh (POST /userapi/v1/refresh);
//   - Credit quota query (GET /agent-assetmgr/api/v2/wallets);
//   - Daily check-in (POST .../autoclaw-task-complete, idempotent).
//
// Login is dispatched by region: CN uses SMS verification code; Global uses Zai / Google OAuth (completed in the console page
// complete Alibaba Cloud human-machine verification -> authorization link -> paste callback -> exchange code), and direct credential pasting is also supported.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// Validity period and polling throttling for Global OAuth login sessions.
const (
	autoclawOAuthTTL          = 10 * time.Minute
	autoclawOAuthPollInterval = 2 * time.Second
)

// adapter is the implementation of core.Vendor on AutoClaw.
type adapter struct {
	*core.NativeAdapter[*Credential]
	region Region
	// baseURL is the region base address override (populated by Configure, empty means default value).
	baseURL string

	// credits caches the rate table of the most recent catalog probe (official model ID -> display text).
	credits core.CreditTable
}

// Instances constructs the two regional instances of AutoClaw.
func Instances(env core.Env) []core.Vendor {
	// Vendor instance = protocol × region, and login methods also vary by region (interface isolation: each
	// implements only the capabilities it truly possesses, and the console renders different add forms accordingly):
	//   - CN: phone number + SMS verification code (smsLoginAdapter);
	//   - Global: Zai / Google OAuth web login (oauthLoginAdapter).
	return []core.Vendor{
		&smsLoginAdapter{newInstance(env, RegionCN).(*adapter)},
		&oauthLoginAdapter{newInstance(env, RegionGlobal).(*adapter)},
	}
}

// smsLoginAdapter is a CN instance: appends SMS verification code login to the shared adapter.
type smsLoginAdapter struct{ *adapter }

// oauthLoginAdapter is a Global instance: appends Zai / Google OAuth login to the shared adapter.
type oauthLoginAdapter struct{ *adapter }

func newInstance(env core.Env, region Region) core.Vendor {
	inst := &adapter{region: region}
	// Hook captures inst: the hook executes after construction completes, and the baseURL written by Configure overrides
	// also takes effect for refresh (no need to copy the override value into spec).
	inst.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorIDFor(region), VendorName: VendorNameFor(region), VendorRegion: string(region)},
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
			if native.Realm() != region {
				return core.CredentialSeed{}, nil, fmt.Errorf("credential region %q does not match vendor %q", native.Realm(), VendorIDFor(region))
			}
			return core.CredentialSeed{
				VendorID: VendorIDFor(region), Region: string(region),
				Label:  core.FirstNonEmpty(native.UserNameValue(), native.UIDValue(), core.MaskToken(native.AccessTokenValue())),
				FileID: core.FileIDFromFileName(fileName), UID: native.UIDValue(),
				Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAtValue(), AuthMode: "oauth",
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			accessBefore, refreshBefore := native.Snapshot()
			result, errRefresh := RefreshToken(ctx, inst.resolveBaseURL(), native)
			if errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			// Upstream response has no expires_in: conservatively record 28 days per Agent2API convention.
			expiresAt := native.ExpiresAtValue()
			if result.AccessToken != accessBefore {
				expiresAt = time.Now().Add(28 * 24 * time.Hour).Unix()
			}
			native.ApplyTokens(result.AccessToken, result.RefreshToken, expiresAt)
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
	return inst
}

// Configure parses AutoClaw-specific configuration (region base address override).
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.baseURL = strings.TrimSpace(raw[v.ID()+"_base_url"])
}

// ConfigFields declares AutoClaw-specific configuration.
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: v.ID() + "_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "AutoClaw upstream base address override (default " + DefaultBaseURLFor(v.region) + ")."},
	}
}

// AuthMode reports the credential form (the page displays semantics such as "re-login required" based on this).
func (v *adapter) AuthMode() string { return "oauth" }

// Match determines whether a credential belongs to this regional instance.
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	if !LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	if region := RegionForCredential(raw); region != "" {
		return region == string(v.region)
	}
	return core.FileNameMatchesConvention(fileName, v.ID())
}

// ---- Models ----

// StaticModels returns the model list for this region.
//
// Probing requires a valid credential; if none can be borrowed, return an error (do not compile the list).
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	token := v.probeToken()
	if token == "" {
		return nil, fmt.Errorf("no autoclaw credential available for model probe")
	}
	models, errModels := FetchModels(ctx, v.resolveBaseURL(), token, v.region)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// ModelsForAuth returns the models available for this credential.
//
// Probe using **the credential's own token**: when the token expires, the account naturally cannot get models,
// and the host eliminates it from model selection based on this.
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	models, errModels := FetchModels(ctx, v.resolveBaseURL(), native.AccessTokenValue(), v.region)
	if errModels != nil {
		// If probing fails, return an empty list instead of reporting an error: the host will skip this credential due to the empty list.
		// Must use Info: host logs default to info level, Debug will be completely swallowed.
		logger.Info("autoclaw: model probe failed: %v", errModels)
		return nil, nil
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI converts upstream model entries to host contract types, and records the rate table along the way.
//
// Multipliers and lists must come from the same probe round: fetching them separately causes mismatches
// where "the models changed, but the multipliers are from the previous batch". AutoClaw's multiplier is
// the upstream billing tier text (low/medium/high), passed through as-is without numerical conversion.
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
			Description:        core.FirstNonEmpty(model.Description, "AutoClaw upstream model"),
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxOutputTokens,
			SupportsImages:     model.SupportsImages,
			SupportsTools:      model.SupportsTools,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ModelCredits implements core.CreditsProvider: returns "official model ID -> billing tier".
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
	payload, errChat := Chat(ctx, v.resolveBaseURL(), ChatRequest{
		Token: native.AccessTokenValue(),
		Body:  req.Payload,
		Model: req.Model,
	})
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream performs a streaming chat.
//
// Only deliver the upstream's raw SSE data payload without `data: ` wrapping and framing—wrapping is done
// by the host according to the declared format.
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	resp, errStream := ChatStream(ctx, v.resolveBaseURL(), ChatRequest{
		Token: native.AccessTokenValue(),
		Body:  req.Payload,
		Model: req.Model,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// ---- Quota and Check-in ----

// Quota queries the account credit balance.
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	balance, errBalance := FetchBalance(ctx, v.resolveBaseURL(), native.AccessTokenValue())
	if errBalance != nil {
		return nil, core.ToPluginError(errBalance)
	}
	response := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     string(v.region),
			TierName: VendorNameFor(v.region),
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "Remaining Credits", Value: balance.TotalBalance, Format: "number"},
		},
	}
	return &response, nil
}

// ---- Internal Tools ----

// resolveBaseURL returns the currently effective base address of the instance.
func (v *adapter) resolveBaseURL() string {
	return BaseURL(v.baseURL, v.region)
}

// probeToken borrows an available credential of this instance to get an access token (for model probing).
func (v *adapter) probeToken() string {
	native, okNative := core.ProbeNativeCredential(v.Env, v.ID(), func(cred *core.Credential) (*Credential, bool) {
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
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.StorageMerger        = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
)

// Compile-time assertion (login capability is dispatched by instance type).
var (
	_ core.SMSLoginProvider  = (*smsLoginAdapter)(nil)
	_ core.LoginProvider     = (*oauthLoginAdapter)(nil)
	_ core.CallbackSubmitter = (*oauthLoginAdapter)(nil)
)
