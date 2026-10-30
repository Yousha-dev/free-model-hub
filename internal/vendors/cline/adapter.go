package cline

// This file adapts Cline to core.Vendor.
//
// Key differences from other providers:
//   - **Has a complete login flow**: WorkOS OAuth 2.0 Device Code (RFC 8628), three steps to exchange for
//     Cline's own token; session state is stored in the shared auth.SessionRegistry;
//   - **No regions**: Only a single deployment (api.cline.bot), Region() returns an empty string;
//   - **No quota API**: The upstream does not provide balance checks, Quota returns "unsupported";
//   - **Tokens require the workos: prefix**: Added uniformly by the protocol layer, the adapter doesn't need to remember it.

import (
	"context"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// adapter is the core.Vendor implementation for Cline.
type adapter struct {
	*core.NativeAdapter[*Credential]
	baseURL string
	// showPass exposes Cline Pass (subscription) models; off by default
	// because they fail with ENTITLEMENT_ERROR on free accounts.
	showPass bool
	// credits is the billing tier in the latest model list (concurrency safe, see core.CreditTable).
	credits core.CreditTable
}

// pendingLogin is an ongoing Cline login session (session data).
type pendingLogin struct {
	deviceCode string
}

// Instances constructs Cline instances (only one, no regions).
func Instances(env core.Env) []core.Vendor {
	inst := &adapter{}
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
			label := native.Label
			if label == "" {
				label = native.Email
			}
			if label == "" {
				label = fileName
			}
			return core.CredentialSeed{
				VendorID: VendorID, Label: label, FileID: core.FileIDFromFileName(fileName),
				Token: native.AccessToken, RefreshToken: native.RefreshToken,
				ExpiresAt: native.ExpiresAt, AuthMode: "oauth",
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			updated, refreshed, errRefresh := RefreshCredential(ctx, inst.baseURL, native)
			if errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			return core.NativeRefresh[*Credential]{
				Native: updated, Token: updated.AccessToken, RefreshToken: updated.RefreshToken,
				ExpiresAt: updated.ExpiresAt, Changed: refreshed,
			}, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})
	return []core.Vendor{inst}
}

// Configure parses provider-specific configuration.
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.baseURL = strings.TrimSpace(raw["cline_base_url"])
	switch strings.ToLower(strings.TrimSpace(raw["cline_pass_models"])) {
	case "true", "1", "yes", "on":
		v.showPass = true
	default:
		v.showPass = false
	}
}

// Match determines whether a credential belongs to this provider.
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// ---- Models ----

// StaticModels returns Cline's model list (upstream API requires no authentication).
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx, v.baseURL)
	if errModels != nil {
		// Fall back to the built-in fallback list if probing fails: having no models registered is worse
		// than registering a potentially outdated list (clients would see no models at all).
		logger.Debug("cline: list models failed, using bundled list: %v", errModels)
		models = bundledModels()
	}
	if !v.showPass {
		free := models[:0:0]
		for _, model := range models {
			if model.Cost != CostPass {
				free = append(free, model)
			}
		}
		models = free
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI converts upstream model entries to host contract types.
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return costLabel(model.Cost) })
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return core.ModelDescriptor{}, false
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		contextWindow := model.ContextWindow
		if contextWindow == 0 {
			contextWindow = core.DefaultContextWindow(id)
		}
		maxOutput := model.MaxOutputTokens
		if maxOutput == 0 {
			maxOutput = core.DefaultMaxOutputTokens
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        describeModel(model),
			ContextLength:      contextWindow,
			MaxOutputTokens:    maxOutput,
			SupportsTools:      model.SupportsTools,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// describeModel formats the model description (indicating the billing tier).
//
// The billing tier is the information users care about most: free tiers can be used freely, subscription tiers consume plan quota.
func describeModel(model Model) string {
	description := strings.TrimSpace(model.Description)
	switch model.Cost {
	case CostFree:
		description = "[Free] " + description
	case CostPass:
		description = "[Subscription] " + description
	}
	if model.IsReasoning {
		description += " (Reasoning)"
	}
	return description
}

// ModelCredits implements core.CreditsProvider: returns "official model ID → billing tier".
//
// The console's "Cost Multiplier" column directly displays this text; free/subscription is the
// most accurate cost information Cline can provide (upstream does not provide numeric multipliers).
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// costLabel converts the billing tier to display text; returns an empty string for unknown tiers.
func costLabel(cost string) string {
	switch cost {
	case CostFree:
		return "Free"
	case CostPass:
		return "Subscription"
	}
	return ""
}

// ---- Execution ----

// Execute executes a non-streaming chat.
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, v.baseURL, ChatRequest{
		BearerToken: native.BearerToken(),
		Body:        req.Payload,
		Stream:      false,
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
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	resp, errStream := ChatStream(ctx, v.baseURL, ChatRequest{
		BearerToken: native.BearerToken(),
		Body:        req.Payload,
		Stream:      true,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// ---- Login and Refresh ----

// LoginStart initiates a WorkOS device code authorization.
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	device, errStart := LoginStart(ctx)
	if errStart != nil {
		return nil, core.ToPluginError(errStart)
	}
	sessionID := "cline_" + core.RandomHex(16)
	v.Env.Logins.Register(VendorID, sessionID, time.Until(device.ExpiresAt), device.Interval, &pendingLogin{deviceCode: device.DeviceCode})
	logger.Info("started cline login (user_code=%s)", device.UserCode)
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       device.VerificationURI,
		State:     sessionID,
		ExpiresAt: device.ExpiresAt,
		Metadata: map[string]any{
			core.VendorKey: VendorID,
			// user_code must be shown to the user: the authorization page requires manual entry of this short code
			// (verification_uri_complete sometimes does not auto-fill).
			"user_code": device.UserCode,
		},
	}, nil
}

// LoginPoll polls the login status once.
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	session, pollState := v.Env.Logins.Poll(state)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session does not exist or has expired, please restart the login process",
		}, nil
	}
	if pollState == core.PollThrottled {
		// During throttling: continue to maintain the waiting state, without hitting upstream or erroring out.
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Waiting for user authorization in the browser...",
		}, nil
	}
	pending, okPending := session.Data.(*pendingLogin)
	if !okPending {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session data is corrupted, please initiate login again",
		}, nil
	}
	credential, errPoll := LoginPoll(ctx, v.baseURL, pending.deviceCode)
	if errPoll != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errPoll.Error(),
		}, nil
	}
	if credential == nil {
		// The user has not completed authorization in the browser yet, continue waiting.
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Waiting for user authorization in the browser...",
		}, nil
	}

	v.Env.Logins.Delete(strings.TrimSpace(state))
	storageJSON, errStorage := credential.StorageJSON()
	if errStorage != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Failed to build credential: " + errStorage.Error(),
		}, nil
	}
	label := core.FirstNonEmpty(credential.Label, credential.Email, credential.AccountID)
	logger.Info("completed cline login for %s", core.FirstNonEmpty(credential.Email, credential.AccountID))
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "Login successful",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(VendorID, core.FirstNonEmpty(credential.Email, credential.AccountID, core.RandomHex(16))),
			Label:       label,
			StorageJSON: storageJSON,
		},
	}, nil
}

// ConfigFields declares cline-specific configuration.
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "cline_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream base URL override."},
		{Name: "cline_pass_models", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Also expose Cline Pass (subscription) models; needs a Cline Pass plan."},
	}
}

// Compile-time assertion: provider capabilities actually possessed by the adapter.
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
)
