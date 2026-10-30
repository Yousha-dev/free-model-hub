package trae

// This file adapts Trae CN and Trae Global to core.Vendor.
//
// Both instances share the same protocol implementation, with differences described by Config/profile.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
)

type adapter struct {
	*core.NativeAdapter[*Credential]
	// config is the construction-time profile, read-only.
	config Config
	// ideVersion is the configuration override for the IDE version; an empty string means using the compile-time default from config.
	// The two instances have different version lines (CN / global are each 3.3.x), so configuration keys are distinguished by instance ID
	// (see Configure) and cannot share family-level keys.
	ideVersionMu   sync.RWMutex
	ideVersion     string
	ideVersionCode string
	sessions       *SessionStore
	// catalog is the model catalog cache (per account), isolated per instance.
	catalog catalogCache
	// credits caches the rate table from the latest round of catalog probing (official model ID → display text).
	credits core.CreditTable
}

// profile returns the current profile snapshot (compile-time profile + runtime version override).
//
// The version number determines the model catalog sent by the upstream and can be modified at runtime, so all read points must go through here.
func (v *adapter) profile() Config {
	v.ideVersionMu.RLock()
	defer v.ideVersionMu.RUnlock()
	cfg := v.config
	if v.ideVersion != "" {
		cfg.IDEVersion = v.ideVersion
	}
	if v.ideVersionCode != "" {
		cfg.IDEVersionCode = v.ideVersionCode
	}
	return cfg
}

// Instances constructs all instances of the Trae family.
func Instances(env core.Env) []core.Vendor {
	configs := AllConfigs()
	sessions := NewSessionStore()
	out := make([]core.Vendor, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, newInstance(env, cfg, sessions))
	}
	return out
}

func newInstance(env core.Env, cfg Config, sessions *SessionStore) core.Vendor {
	return &adapter{
		NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
			Meta: core.Meta{VendorID: cfg.VendorID, VendorName: cfg.VendorName, VendorRegion: string(cfg.Region)},
			Env:  env,
			Extract: func(c *core.Credential) (*Credential, bool) {
				native, ok := c.Native.(*Credential)
				return native, ok
			},
			ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
				native, errParse := ParseCredentialFor(raw, cfg)
				if errParse != nil {
					return core.CredentialSeed{}, nil, errParse
				}
				native.Vendor = cfg.VendorID
				label := native.Label
				if label == "" {
					prefix := "TraeCN-"
					if cfg.Region == RegionGlobal {
						prefix = "TraeGlobal-"
					}
					if native.Nickname != "" {
						label = prefix + native.Nickname
					} else if native.UID != "" {
						label = prefix + native.UID
					} else {
						label = prefix + "Account"
					}
				}
				return core.CredentialSeed{
					VendorID: cfg.VendorID, Region: string(cfg.Region), Label: label,
					FileID: core.FileIDFromFileName(fileName), Token: native.AccessToken,
					ExpiresAt: native.ExpiresAt, AuthMode: "oauth",
				}, native, nil
			},
			RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
				updated, refreshed, errRefresh := RefreshCredential(ctx, native, cfg)
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
		}),
		config: cfg, sessions: sessions,
	}
}

func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredentialFor(raw, fileName, provider, v.profile())
}

// Configure parses Trae-specific configurations.
//
// Upstream delivers different model catalogs based on X-Ide-Version-Code: when the CN build number is lower than 20260806,
// deepseek-v4.1-flash cannot be obtained. When the gating is raised further, the user can modify the configuration without waiting for a release.
// Key names are prefixed by instance (traecn_ide_version / traeglobal_ide_version / …)
// because the version numbering systems of the three product lines differ, sharing a key would cause conflicts.
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	prefix := v.config.VendorID + "_"
	version := strings.TrimSpace(raw[prefix+"ide_version"])
	versionCode := strings.TrimSpace(raw[prefix+"ide_version_code"])
	v.ideVersionMu.Lock()
	v.ideVersion = version
	v.ideVersionCode = versionCode
	v.ideVersionMu.Unlock()
}

// ---- Models ----

func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	// Borrow an available credential to probe the upstream catalog (Catalog.StaticModels calls this method only when credentials are available).
	cred, okCred := core.ProbeNativeCredential(v.Env, v.ID(), func(c *core.Credential) (*Credential, bool) {
		native, okNative := c.Native.(*Credential)
		return native, okNative
	})
	if !okCred || cred == nil {
		return nil, fmt.Errorf("trae: no available credentials, unable to fetch model catalog")
	}
	models, errModels := v.catalog.fetchModels(ctx, cred, v.profile())
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI converts upstream model entries into host contract types, while recording the rate table.
//
// Rates and manifests must come from the same probing round: fetching them separately would cause a mismatch where
// "the model batch has changed, but the rates are still from the previous batch".
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return model.Credits })
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
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ModelCredits implements core.CreditsProvider: returns "official model ID → billing rate".
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// ---- Execution ----

func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, "", ChatRequest{
		Credential: native,
		Profile:    v.profile(),
		Body:       req.Payload,
		Stream:     false,
		catalog:    &v.catalog,
	})
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	return ChatStream(ctx, "", ChatRequest{
		Credential: native,
		Profile:    v.profile(),
		Body:       req.Payload,
		Stream:     true,
		catalog:    &v.catalog,
	}, sink.Emit)
}

// ---- Quota and Check-in ----

func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	response, errQuota := FetchQuota(ctx, native, v.profile())
	if errQuota != nil {
		return nil, core.ToPluginError(errQuota)
	}
	return response, nil
}

// ---- Login and Refresh ----

func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.profile(), v.sessions)
}

func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, state, v.sessions)
}

// OwnsLoginSession reports whether this login session is held by the current instance.
//
// Trae's session table is self-maintained (callback listening requires instance isolation) and is not registered in public LoginSessions.
// When the root layer routes login requests, it relies on this self-certification interface to recognize the state.
func (v *adapter) OwnsLoginSession(sessionID string) bool {
	return v.sessions.owns(sessionID, v.profile().VendorID)
}

// LocalCallbackHint returns the local callback address to be displayed in the console.
func (v *adapter) LocalCallbackHint() string { return v.sessions.callbackHint() }

// SubmitCallback parses the localhost callback URL pasted by the user and completes the login.
func (v *adapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	return v.sessions.SubmitCallback(ctx, state, rawURL)
}

// Compile-time assertion: vendor capabilities actually possessed by the adapter.
var (
	_ core.Vendor            = (*adapter)(nil)
	_ core.CallbackSubmitter = (*adapter)(nil)
	_ core.Configurable      = (*adapter)(nil)
	_ core.LoginProvider     = (*adapter)(nil)
	_ core.LoginSessionOwner = (*adapter)(nil)
	_ core.QuotaProvider     = (*adapter)(nil)
	_ core.CreditsProvider   = (*adapter)(nil)
)
