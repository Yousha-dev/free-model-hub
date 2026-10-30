package opencodezen

// This file adapts OpenCode ZEN to core.Vendor.
//
// Key differences from other providers:
//   - **No login flow**: Credentials are static API keys applied for by users themselves. There is no OAuth,
//     no device code, and no token exchange. Therefore, LoginStart / LoginPoll explicitly return
//     "This provider does not support login, please add using an API key";
//   - **No region**: There is only one deployment (opencode.ai), and Region() returns an empty string;
//   - **No quota interface**: The upstream does not provide balance queries, and Quota returns "Unsupported";
//   - **No refresh**: The key does not change, and Refresh only performs a one-time availability check (listing models).
//
// The adapter and protocol implementation are in the same package: the adaptation layer is no longer at the root level, which is only responsible for registration.

import (
	"context"
	"strings"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// adapter is the implementation of core.Vendor on OpenCode ZEN.
type adapter struct {
	*core.NativeAdapter[*Credential]
	baseURL string
	// modelsDev is the models.dev directory cache, isolated per instance.
	modelsDev modelsDevCache
	// credits caches the multiplier table from the latest round of probing (official model ID → display text).
	credits core.CreditTable
}

// Instances constructs ZEN instances (only one, with no regional differences).
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{&adapter{
		NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
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
					label = MaskedKey(native.APIKey)
				}
				token, authMode := native.APIKey, "apikey"
				if native.Anonymous {
					token, authMode = freeTierCredential, "anonymous"
				}
				return core.CredentialSeed{
					VendorID: VendorID, Label: label, FileID: core.FileIDFromFileName(fileName),
					Token: token, AuthMode: authMode,
				}, native, nil
			},
		}),
	}}
}

// Configure parses provider-specific configuration.
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.baseURL = strings.TrimSpace(raw["opencodezen_base_url"])
}

// Match determines whether a credential belongs to this provider.
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// ---- Models ----

// StaticModels returns the model list for ZEN.
//
// A valid key is required to probe (the upstream model interface requires authentication). Borrow an available
// credential from this provider to make the call; if none can be borrowed, return an empty list—do not make up a fake list, otherwise users will see
// models whose calls are guaranteed to fail.
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	// Without a personal key the official CLI lists models with its public
	// credential; the free filter below keeps only models that lane can run.
	apiKey := v.probeAPIKey()
	if apiKey == "" {
		apiKey = freeTierCredential
	}
	models, errModels := FetchModels(ctx, v.baseURL, apiKey)
	if errModels != nil {
		return nil, errModels
	}
	// The upstream only provides IDs; metadata such as context/output/tiers are merged by the models.dev directory by ID.
	// Filtering also relies on directory unit prices and must be done after enrich.
	v.modelsDev.enrich(ctx, models)
	catalog, errCatalog := v.modelsDev.fetch(ctx)
	if errCatalog != nil {
		logger.Info("opencodezen: models.dev catalog unavailable, keep all models: %v", errCatalog)
	}
	return v.modelsToPluginAPI(freeOnly(models, catalog)), nil
}

// ModelsForAuth returns the models available for this credential.
//
// ZEN's list is global (not distinguished by account), so it shares the same source as StaticModels;
// however, it probes using **this credential's own key**, so when the key expires, the account naturally fails to get models,
// and the host eliminates it from model selection based on this.
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	listKey := native.APIKey
	if native.Anonymous {
		listKey = freeTierCredential
	}
	models, errModels := FetchModels(ctx, v.baseURL, listKey)
	if errModels != nil {
		// Probing failure returns an empty list instead of an error: the host will skip this credential due to the empty list,
		// which is precisely the result we want (avoiding hitting upstream with a bad key).
		//
		// But **Info must be used instead of Debug**: host logs default to the info level, and Debug will be
		// completely swallowed, leaving users seeing "opencodezen is not in the model list" with no way to investigate.
		logger.Info("opencodezen: model probe failed: %v", errModels)
		return nil, nil
	}
	v.modelsDev.enrich(ctx, models)
	catalog, errCatalog := v.modelsDev.fetch(ctx)
	if errCatalog != nil {
		logger.Info("opencodezen: models.dev catalog unavailable, keep all models: %v", errCatalog)
	}
	return v.modelsToPluginAPI(freeOnly(models, catalog)), nil
}

// modelsToPluginAPI converts upstream model entries to host contract types, and records the multiplier table along the way.
//
// Metadata fields may have already been filled in by the models.dev directory (see models.go); any remaining missing
// context/output limits go through the generic fallback here. Multipliers and lists must come from the same probe round.
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return model.Credits })
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
		description := strings.TrimSpace(model.Description)
		if description == "" {
			description = "OpenCode ZEN upstream model"
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        description,
			ContextLength:      contextWindow,
			MaxOutputTokens:    maxOutput,
			Efforts:            model.Efforts,
			SupportsImages:     model.SupportsImages,
			SupportsTools:      model.SupportsTools,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ModelCredits implements core.CreditsProvider: returns "official model ID → billing text".
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
	if errPaid := v.requireKeyForPaid(ctx, native, req.Payload); errPaid != nil {
		return nil, errPaid
	}
	payload, errChat := Chat(ctx, v.baseURL, ChatRequest{
		APIKey:  native.APIKey,
		Body:    req.Payload,
		Stream:  false,
		Catalog: v.catalog(ctx),
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
// Only delivers the upstream's raw SSE data payload, without `data: ` packaging and framing—packaging is done by the host
// according to the declared format. Frame splitting on the plugin side will cause inconsistent behavior with the host.
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	if errPaid := v.requireKeyForPaid(ctx, native, req.Payload); errPaid != nil {
		return errPaid
	}
	resp, errStream := ChatStream(ctx, v.baseURL, ChatRequest{
		APIKey:  native.APIKey,
		Body:    req.Payload,
		Stream:  true,
		Catalog: v.catalog(ctx),
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// requireKeyForPaid rejects paid models on a keyless credential before any
// upstream call, so the client sees a clear reason instead of a bare 401.
func (v *adapter) requireKeyForPaid(ctx context.Context, native *Credential, payload []byte) error {
	if !native.Anonymous || isFreeTierModel(extractModelName(payload), v.catalog(ctx)) {
		return nil
	}
	return core.NewPluginError("model_requires_key", "this OpenCode Zen model needs a Zen API key; the keyless account only serves free models", 403)
}

// catalog fetches the models.dev catalog, used to determine free tier by unit price.
//
// Returns an empty table when the catalog is unavailable; isFreeTierModel falls back to model name-based determination -- price unavailable.
// Request should not fail, free tier determination simply degrades to the old logic.
func (v *adapter) catalog(ctx context.Context) modelsDevCatalog {
	catalog, errCatalog := v.modelsDev.fetch(ctx)
	if errCatalog != nil {
		logger.Info("opencodezen: models.dev catalog unavailable, fall back to name-based free detection: %v", errCatalog)
		return nil
	}
	return catalog
}

// probeAPIKey borrows an available credential from this provider to get an API key (for model probing).
func (v *adapter) probeAPIKey() string {
	native, okNative := core.ProbeNativeCredential(v.Env, VendorID, func(cred *core.Credential) (*Credential, bool) {
		value, okValue := cred.Native.(*Credential)
		return value, okValue
	})
	if !okNative || native == nil {
		return ""
	}
	return native.APIKey
}

// KeylessCredentialJSON implements core.KeylessProvider: free models work
// without any account, using the official CLI's public credential.
func (v *adapter) KeylessCredentialJSON() []byte {
	return AnonymousCredentialJSON(core.ProviderKey)
}

// ConfigFields declares opencodezen-specific configuration.
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{{Name: "opencodezen_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream base address override."}}
}

// Compile-time assertion: provider capabilities actually possessed by the adapter.
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
	_ core.KeylessProvider      = (*adapter)(nil)
)
