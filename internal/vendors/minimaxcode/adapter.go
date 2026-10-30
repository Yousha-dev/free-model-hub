package minimaxcode

// This file adapts MiniMax Code (CN/Global) to core.Vendor.

import (
	"context"
	"strings"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
)

// adapter is the core.Vendor implementation for MiniMax Code (one instance per region).
type adapter struct {
	*core.NativeAdapter[*Credential]
	region  Region
	baseURL string
	// workspace is the account workspace model directory cache. Attached to the instance rather than package-level variables:
	// multiple App instances in the same process are independent, and the cache lifecycle follows the instance.
	workspace workspaceCache
}

// Instances constructs two regional instances.
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{newInstance(env, RegionCN), newInstance(env, RegionGlobal)}
}

func newInstance(env core.Env, region Region) core.Vendor {
	inst := &adapter{region: region}
	inst.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorIDFor(region), VendorName: VendorNameFor(region), VendorRegion: string(region)},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := ParseCredential(raw, region)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			label := native.Label
			if label == "" {
				if native.Email != "" {
					label = native.Email
				} else if native.UserID != "" {
					label = VendorNameFor(region) + "-" + native.UserID
				} else {
					label = fileName
				}
			}
			uid := native.UserID
			if uid == "" {
				uid = core.FirstNonEmpty(native.Email, fileName)
			}
			return core.CredentialSeed{
				VendorID: VendorIDFor(region), Region: string(region), Label: label,
				FileID: core.FileIDFromFileName(fileName), UID: uid,
				Token: native.AccessToken, RefreshToken: native.RefreshToken,
				ExpiresAt: native.ExpiresAt, AuthMode: native.AuthMode,
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			updated, refreshed, errRefresh := RefreshCredential(ctx, native, inst.baseURL)
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
	return inst
}

// Configure parses provider-specific configuration.
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	if v.region == RegionGlobal {
		v.baseURL = strings.TrimSpace(raw["minimaxcode_global_base_url"])
		return
	}
	v.baseURL = strings.TrimSpace(raw["minimaxcode_cn_base_url"])
}

// Match determines whether a credential belongs to this region.
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider, v.region)
}

// ---- Models ----

// ModelsForAuth returns the model list for this region ("built-in catalog ∪ account workspace").
//
// The built-in catalog is the registry skeleton: it carries complete context/output/tool metadata, and plugin-side
// constructed -thinking variant IDs. The workspace /config is the authoritative source for currently available models upstream,
// but is only used to supplement new models missing from the built-in catalog—replacing it entirely would cause existing IDs (especially
// -thinking) and metadata to disappear, resulting in compatibility regressions.
//
// When fetching fails, only the built-in catalog is used: better to serve old IDs than an empty list (an empty list would cause the host
// to discard the credential, whereas old IDs will at most error out during chat).
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	static, errStatic := v.StaticModels(ctx)
	if errStatic != nil {
		return nil, errStatic
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return static, nil
	}
	return mergeWorkspaceModels(static, v.workspace.models(ctx, native, v.baseURL)), nil
}

// StaticModels returns the model list for this region.
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx, v.region)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}

// modelsToPluginAPI converts upstream model entries into host contract types.
func modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        model.Description,
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxOutput,
			SupportsTools:      model.SupportsTools,
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
	payload, errChat := chat(ctx, native, req, v.baseURL, &v.workspace)
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
	errStream := chatStream(ctx, native, req, v.baseURL, func(chunk []byte) error {
		return sink.Emit(chunk)
	}, &v.workspace)
	if errStream != nil {
		return errStream
	}
	return nil
}

// ---- Quota & Check-in ----

// Quota queries the account quota.
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return FetchQuota(ctx, native, v.baseURL)
}

// ---- Login & Refresh ----

// LoginStart initiates a login.
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.Env.Logins, v.region)
}

// LoginPoll polls the login status.
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, v.Env.Logins, state, v.baseURL)
}

// ConfigFields declares MiniMax Code specific configuration.
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "minimaxcode_cn_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "CN Agent domain base URL override (Token Plan window queries fixedly connect directly to the open platform domain)."},
		{Name: "minimaxcode_global_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Global Agent domain base URL override (Token Plan window queries fixedly connect directly to the open platform domain)."},
	}
}

// Compile-time assertion: vendor capabilities actually possessed by the adapter.
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
)
