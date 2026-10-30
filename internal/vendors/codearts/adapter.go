package codearts

// This file adapts CodeArts to core.Vendor.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// adapter is the implementation of core.Vendor for CodeArts.
type adapter struct {
	*core.NativeAdapter[*Credential]

	// credits caches the multiplier table from the latest directory scan (official model ID → display text).
	credits core.CreditTable

	// modelsByLower is an index of lowercase model name → registered ID, used to normalize
	// old spellings in requests into official IDs in the directory.
	modelsMu      sync.RWMutex
	modelsByLower map[string]string
}

// Instances constructs a CodeArts instance (single deployment, CN region).
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{&adapter{NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorID, VendorName: VendorName, VendorRegion: "cn"},
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
				if native.UserName != "" {
					label = "CodeArts-" + native.UserName
				} else if native.UserID != "" {
					label = "CodeArts-" + native.UserID
				} else {
					label = "CodeArts-Account"
				}
			}
			return core.CredentialSeed{
				VendorID: VendorID, Region: "cn", Label: label,
				FileID: core.FileIDFromFileName(fileName), Token: native.AccessKeyID,
				ExpiresAt: native.ExpiresAt, AuthMode: "oauth",
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			updated, refreshed, errRefresh := RefreshCredential(ctx, native)
			if errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			return core.NativeRefresh[*Credential]{
				Native: updated, Token: updated.AccessKeyID, RefreshToken: updated.RefreshToken,
				ExpiresAt: updated.ExpiresAt, Changed: refreshed,
			}, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})}}
}

// Match checks whether a credential belongs to this provider.
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// ---- Models ----

// StaticModels returns the model list (no credentials required).
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx, nil)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// ModelsForAuth returns the models available for the credential.
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, _ := v.Native(cred)
	models, errModels := FetchModels(ctx, native)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI converts upstream model entries to host contract types and records the multiplier table.
//
// Multipliers and the list must come from the same scan round: fetching them separately causes a mismatch
// where "models are from one batch and multipliers are from the previous batch". CodeArts multipliers come from
// the built-in directory credit[].ratio_display; benefit channels lack multiplier fields, so benefit models are excluded (console shows "—", not assumed free).
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return model.Credits })
	ids := make(map[string]string, len(models))
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			ids[strings.ToLower(id)] = id
		}
	}
	v.modelsMu.Lock()
	v.modelsByLower = ids
	v.modelsMu.Unlock()
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:              model.ID,
			Name:            model.Name,
			Description:     model.Description,
			ContextLength:   int64(model.ContextWindow),
			MaxOutputTokens: int64(model.MaxTokens),
			SupportsTools:   model.SupportsTools,
			SupportsImages:  model.SupportsImages,
		}, true
	})
}

// prepareRequestBody performs two minimal rewrites before outbound transmission:
//
//  1. Normalize model to the registered ID: the host matches IDs precisely, but clients may use old docs/
//     old case variants of aliases (e.g., lowercase glm-5.2 maps to registered GLM-5.2). Upstream is case-sensitive
//     to model names, so sending them as-is would be rejected;
//  2. Clamp max_tokens / max_completion_tokens to the upstream hard limit (see clampTokenFields).
//
// Both share a single parse and a single serialization: doing them separately would add two extra JSON round-trips per chat request,
// and serializing based on the first parse output is purely wasteful.
func (v *adapter) prepareRequestBody(body []byte) []byte {
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return body
	}
	changed := v.canonicalizeModelField(payload)
	if clampTokenFields(payload) {
		changed = true
	}
	if !changed {
		return body
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return encoded
}

// canonicalizeModelField rewrites the model field in-place and returns whether it was actually changed.
// Unregistered names remain unchanged and are left for upstream to judge.
func (v *adapter) canonicalizeModelField(payload map[string]json.RawMessage) bool {
	raw, okRaw := payload["model"]
	if !okRaw {
		return false
	}
	var name string
	if errUnmarshal := json.Unmarshal(raw, &name); errUnmarshal != nil {
		return false
	}
	canonical := v.canonicalModelName(name)
	if canonical == name {
		return false
	}
	encoded, errMarshal := json.Marshal(canonical)
	if errMarshal != nil {
		return false
	}
	payload["model"] = encoded
	return true
}

// canonicalModelName maps a name back to its registered ID using the latest directory round; returns the original name if unregistered.
func (v *adapter) canonicalModelName(name string) string {
	v.modelsMu.RLock()
	defer v.modelsMu.RUnlock()
	if canonical, ok := v.modelsByLower[strings.ToLower(strings.TrimSpace(name))]; ok {
		return canonical
	}
	return name
}

// ModelCredits implements core.CreditsProvider: returns "official model ID → billing multiplier".
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// ---- Execution ----

// Execute executes a non-streaming chat.
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, "", ChatRequest{
		Credential: native,
		Body:       v.prepareRequestBody(req.Payload),
		Stream:     false,
		Benefit:    true,
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
	resp, errStream := ChatStream(ctx, "", ChatRequest{
		Credential: native,
		Body:       v.prepareRequestBody(req.Payload),
		Stream:     true,
		Benefit:    true,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// ---- Quota and Check-in ----

// Quota queries the account quota.
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return FetchQuota(ctx, native)
}

// ---- Login and Refresh ----

// LoginStart initiates a login.
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.Env.Logins)
}

// LoginPoll polls the login status.
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, v.Env.Logins, state)
}

// LocalCallbackHint returns the local callback URL to be displayed in the console.
//
// After successful Huawei Cloud authorization, the browser stops at this URL: if the plugin and browser are not on the same machine, it cannot be received,
// so the user can paste the entire link back into the console to continue logging in.
func (v *adapter) LocalCallbackHint() string { return LocalCallbackURL }

// SubmitCallback parses the localhost callback link pasted by the user and completes the login.
func (v *adapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	return SubmitCallback(ctx, v.Env.Logins, state, rawURL)
}

// Compile-time assertion: provider capabilities actually possessed by the adapter.
var (
	_ core.Vendor            = (*adapter)(nil)
	_ core.CallbackSubmitter = (*adapter)(nil)
	_ core.LoginProvider     = (*adapter)(nil)
	_ core.ModelsProvider    = (*adapter)(nil)
	_ core.QuotaProvider     = (*adapter)(nil)
	_ core.CreditsProvider   = (*adapter)(nil)
)
