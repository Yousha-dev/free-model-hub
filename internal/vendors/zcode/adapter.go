package zcode

import (
	"context"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
)

// This file adapts ZCode into core.Vendor.
//
// The credential form is oauth: accounts are generated via console-initiated OAuth login (obtaining Coding Plan
// JWT and the exchanged API Key at once). It is declared as oauth instead of apikey because the UI
// determines the "Add Account" action based on auth_mode—apikey directly pops up an input box without triggering login.
//
// Channel selection: When JWT is available, prioritize the Start Plan channel (zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages);
// pure API Key accounts use the ZCode official gateway forwarding channel (zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages).

// adapter is the core.Vendor implementation for ZCode.
type adapter struct {
	*core.NativeAdapter[*Credential]
	baseURL string
	// claims is the claim throttle for this instance; unshared, independent when multiple Apps coexist.
	claims *claimGate
	// activations is the active reporting deduplication table for this instance (once per account per day).
	activations *activationState
	// gate is the Start Plan outbound gate for this instance; unshared, independent when multiple Apps coexist.
	gate *chatGate
}

// Instances constructs a ZCode instance.
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{&adapter{claims: newClaimGate(), activations: newActivationState(), gate: newChatGate(), NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorID, VendorName: VendorName, VendorRegion: "global"},
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
			label := native.Label
			if label == "" {
				label = core.FirstNonEmpty(native.Email, "ZCode-"+MaskedKey(native.OutboundToken()))
			}
			return core.CredentialSeed{
				VendorID: VendorID, Region: "global", Label: label,
				FileID: core.FileIDFromFileName(fileName), UID: core.FirstNonEmpty(native.UserID, native.Email),
				Token: native.APIKey, AuthMode: native.AuthMode,
			}, native, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})}}
}

// AuthMode returns the credential form: oauth.
func (v *adapter) AuthMode() string { return "oauth" }

// Configure parses vendor-specific configurations.
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.baseURL = strings.TrimSpace(raw["zcode_base_url"])
	if interval, errParse := time.ParseDuration(strings.TrimSpace(raw["zcode_claim_min_interval"])); errParse == nil && interval >= 0 {
		v.claims.setInterval(interval)
	}
}

// Match determines whether a credential belongs to this vendor.
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// ---- Models ----

// StaticModels returns the model list.
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}
func modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        model.Description,
			ContextLength:      int64(model.ContextWindow),
			MaxOutputTokens:    int64(model.MaxTokens),
			SupportsTools:      model.SupportsTools,
			SupportsImages:     model.SupportsImages,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ---- Execution ----

// Execute executes a non-streaming chat.
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	release, errGate := v.holdStartPlanSlot(ctx, native)
	if errGate != nil {
		return nil, errGate
	}
	defer release()

	payload, errChat := Chat(ctx, v.baseURL, ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     false,
	})
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream executes a streaming chat.
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	release, errGate := v.holdStartPlanSlot(ctx, native)
	if errGate != nil {
		return errGate
	}
	defer release()

	return ChatStream(ctx, v.baseURL, ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     true,
	}, func(chunk []byte) error {
		return sink.Emit(chunk)
	})
}

// ---- Quota & Check-in ----

// Quota queries the account quota.
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return v.fetchQuota(ctx, native)
}

// ---- Login & Refresh ----

// LoginStart initiates a login.
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.Env.Logins)
}

// LoginPoll polls the login status.
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, v.Env.Logins, state)
}

// ConfigFields declares ZCode-specific configurations.
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "zcode_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream base URL override."},
		{Name: "zcode_claim_min_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Minimum interval between plan claims (Go duration, e.g., 15s; 0 means no throttling)."},
	}
}

// Compile-time assertion: vendor capabilities actually possessed by the adapter.
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.ActivityProbe        = (*adapter)(nil)
	_ core.PlanClaimer          = (*adapter)(nil)
)
