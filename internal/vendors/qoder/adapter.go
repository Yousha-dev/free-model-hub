package qoder

// This file adapts the Qoder protocol to core.Vendor.
//
// Boundary:
//   - The rest of this package is the **pure protocol layer** (cosy signing, bridge protocol conversion, endpoint table,
//     check-in, quota, login), unaware of the host;
//   - This file is the **adapter layer**, connecting global configuration and host ports.
//
// Key difference from WorkBuddy: Every upstream call in Qoder requires a cosy session
// (including an RSA-encrypted cosyKey and device fingerprint), and session creation requires a jobToken exchange,
// so the Bridge must be cached here—it cannot be rebuilt for every request.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
)

const (
	// qoderBridgeTTL determines the maximum reuse duration of the Bridge (including the cosy session).
	//
	// Once the upstream changes its signing rules, it will automatically rebuild after this duration at most, without restarting the plugin.
	qoderBridgeTTL = 30 * time.Minute
	// qoderBridgeTimeout limits a single connection establishment time (including jobToken exchange).
	qoderBridgeTimeout = 30 * time.Second
)

// adapter is the implementation of core.Vendor on Qoder.
type adapter struct {
	*core.NativeAdapter[*Credential]
	region Region
	// bridges caches cosy sessions by credential fingerprint (creating a session requires a jobToken exchange, which is expensive).
	bridges sync.Map
	// models is an index of "external model ID -> upstream key", replaced entirely upon each model list refresh.
	//
	// Must be attached to the instance: upstream keys with the same name in CN and Global versions may correspond to different models.
	models modelIndex
	// credits is "external model ID -> billing multiplier text", sharing the same source and lifecycle as models.
	//
	// The console needs to display multipliers, but the host contract (pluginapi.ModelInfo) lacks this field,
	// so this instance must record it during model conversion for ConsoleModels to read back.
	credits core.CreditTable
	// settings are raw configuration keys (device identity needs to read qoder_desktop_version).
	settings atomic.Value // map[string]string
}

// Instances constructs the two regional instances for Qoder.
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{newInstance(env, RegionCN), newInstance(env, RegionGlobal)}
}

func newInstance(env core.Env, region Region) core.Vendor {
	instance := &adapter{
		NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
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
				if native.Region != region {
					return core.CredentialSeed{}, nil, fmt.Errorf("credential region %q does not match vendor %q", native.Region, VendorIDFor(region))
				}
				label := native.Label
				if label == "" {
					label = native.Email
				}
				if label == "" {
					label = fileName
				}
				return core.CredentialSeed{
					VendorID: VendorIDFor(region), Region: string(native.Region), Label: label,
					FileID: core.FileIDFromFileName(fileName), Token: native.Token,
					RefreshToken: native.RefreshToken, AuthMode: "oauth",
				}, native, nil
			},
			RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
				updated, refreshed, errRefresh := RefreshCredential(ctx, native)
				if errRefresh != nil {
					return core.NativeRefresh[*Credential]{}, errRefresh
				}
				return core.NativeRefresh[*Credential]{
					Native: updated, Token: updated.Token, RefreshToken: updated.RefreshToken, Changed: refreshed,
				}, nil
			},
		}),
		region: region,
	}
	instance.settings.Store(map[string]string{})
	return instance
}

// Configure saves Qoder-specific configuration (desktop version override for device identity).
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	snapshot := make(map[string]string, len(raw))
	for key, value := range raw {
		snapshot[key] = value
	}
	v.settings.Store(snapshot)
}

// Match determines whether a credential belongs to this region.
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	if !LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	if region := RegionForCredential(raw); region != "" {
		return region == v.region
	}
	return v.defaultRegionForParse() == v.region
}

func (v *adapter) defaultRegionForParse() Region {
	settings := v.Env.Settings()
	for _, vendor := range v.Env.Registry.Vendors() {
		if vendor.Region() == string(RegionGlobal) && settings.RealmEnabled(vendor.Region()) {
			return RegionGlobal
		}
	}
	return RegionCN
}

// ---- Models ----

// StaticModels returns the model list for this region.
//
// Requires one upstream call (to list available models), so it borrows a same-region account to build a Bridge.
// Returns an error on failure rather than fabricating a list: registering a hardcoded model catalog will only become outdated with the upstream,
// exposing non-existent models to the client.
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := v.fetchModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// fetchModels borrows a probe credential to fetch the upstream model list.
func (v *adapter) fetchModels(ctx context.Context) ([]Model, error) {
	cred, okCred := core.ProbeNativeCredential(v.Env, v.ID(), func(c *core.Credential) (*Credential, bool) {
		native, okNative := c.Native.(*Credential)
		return native, okNative
	})
	if !okCred || cred == nil {
		return nil, fmt.Errorf("no credential available for model probe")
	}
	b, errBridge := v.bridgeFor(ctx, cred)
	if errBridge != nil {
		return nil, errBridge
	}
	return b.ListAvailableModels(ctx)
}

// ModelsForAuth returns the models available for the credential.
//
// Returns an error on failure rather than a fallback list: the host will skip this credential, which is more honest
// than exposing a hardcoded, potentially non-existent model catalog to the client.
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	if cred == nil || cred.RegionValue() != string(v.region) {
		// Returns an empty list instead of an error when regions do not match: the host will thus skip this credential,
		// which is precisely the desired result (avoiding hitting upstream with credentials of the wrong domain).
		return nil, nil
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	b, errBridge := v.bridgeFor(ctx, native)
	if errBridge != nil {
		return nil, errBridge
	}
	models, errModels := b.ListAvailableModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// ---- Execution ----

// Execute executes a non-streaming chat.
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	b, errBridge := v.bridgeFor(ctx, native)
	if errBridge != nil {
		return nil, errBridge
	}
	// Reuses the chat-completions construction logic of the bridge, but writes the result into memory instead of
	// http.ResponseWriter—the plugin side does not handle HTTP responses, only producing payloads.
	recorder := newPayloadRecorder()
	if errRun := RunChatCompletions(b, req.Payload, recorder); errRun != nil {
		return nil, errRun
	}
	return &pluginapi.ExecutorResponse{
		Payload: recorder.Payload(),
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream executes a streaming chat.
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	b, errBridge := v.bridgeFor(ctx, native)
	if errBridge != nil {
		return errBridge
	}
	return StreamChatCompletions(b, req.Payload, sink.Emit)
}

// ---- Quota and Check-in ----

// Quota queries the account quota.
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	quota, errQuota := FetchQuota(ctx, native)
	if errQuota != nil {
		return nil, errQuota
	}
	response := BuildQuotaResponse(quota)
	return &response, nil
}

// ConfigFields declares Qoder specific YAML configuration.
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "qoder_desktop_version", Type: pluginapi.ConfigFieldTypeString, Description: "cosy-version desktop version override."},
		{Name: "qoder_machine_token", Type: pluginapi.ConfigFieldTypeString, Description: "machineToken issued by official components, used to unlock the Global daily claim event."},
		{Name: "qoder_machine_type", Type: pluginapi.ConfigFieldTypeString, Description: "machineType paired with qoder_machine_token, must be filled in pairs."},
		{Name: "qoder_machine_code", Type: pluginapi.ConfigFieldTypeString, Description: "Optional machineCode (leaving it blank does not affect event visibility)."},
	}
}

// ---- Login and Refresh ----

// LoginStart initiates a PKCE device authorization login.
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	loginURL, state, expiresAt, errStart := LoginStart(ctx, v.Env.Logins, v.region)
	if errStart != nil {
		return nil, errStart
	}
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       loginURL,
		State:     state,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			core.VendorKey: v.ID(),
			"region":       string(v.region),
		},
	}, nil
}

// LoginPoll polls the login status once.
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	result, errPoll := LoginPoll(ctx, v.Env.Logins, state)
	if errPoll != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errPoll.Error(),
		}, nil
	}
	if result.Pending {
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	plan := FetchPlanName(ctx, result.Token, result.Region)
	storage, cred := BuildLoginStorage(result.Region, result.Token, result.RefreshToken, result.Profile, plan)
	authID := LoginAuthID(result.Profile, state)
	fileName := core.FileNameFor(v.ID(), strings.TrimPrefix(authID, "qoder-"))
	logger.Info("completed qoder login (vendor=%s email=%s)", v.ID(), cred.Email)
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "Login successful",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			ID:          strings.TrimSuffix(fileName, ".json"),
			FileName:    fileName,
			Label:       cred.Label,
			StorageJSON: storage,
		},
	}, nil
}

// OwnsLoginSession reports whether the session was created by this provider.
func (v *adapter) OwnsLoginSession(sessionID string) bool {
	return OwnsLoginSession(v.Env.Logins, sessionID)
}

// MergeStorageJSON implements core.StorageMerger: merges the refreshed tokens back into the original credential JSON.
//
// Qoder's credentials are flat fields (device_token / refresh_token / region / label),
// only updating the two volatile token fields while keeping the rest (user-written label, secret, etc.) intact.
func (v *adapter) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	if len(original) == 0 {
		return original, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(original, &payload); errUnmarshal != nil {
		return original, nil
	}
	token := credential.TokenValue()
	refreshToken := credential.RefreshTokenValue()
	if token != "" {
		// Only modify existing keys, do not add new ones: the user might be using one of the seven candidate names
		// (token / device_token / access_token ...); creating a device_token
		// out of thin air would result in two tokens with the same meaning in the file.
		if _, okDevice := payload["device_token"]; okDevice {
			payload["device_token"] = token
		} else if _, okToken := payload["token"]; okToken {
			payload["token"] = token
		} else {
			payload["device_token"] = token
		}
	}
	if refreshToken != "" {
		payload["refresh_token"] = refreshToken
	}
	merged, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return original, errMarshal
	}
	return merged, nil
}

// ---- Bridge Cache ----

// bridgeEntry is a cached Bridge instance.
type bridgeEntry struct {
	bridge    *Bridge
	createdAt time.Time
}

// bridgeFor retrieves (or establishes) the cosy session for this credential.
//
// Caching is required: establishing a session requires a jobToken exchange + RSA encryption, and rebuilding it per request
// would expose the upstream to a large volume of repetitive token exchanges.
func (v *adapter) bridgeFor(ctx context.Context, cred *Credential) (*Bridge, error) {
	key := v.bridgeCacheKey(cred)
	if cached, okCached := v.bridges.Load(key); okCached {
		entry, okEntry := cached.(bridgeEntry)
		if okEntry && time.Since(entry.createdAt) < qoderBridgeTTL {
			return entry.bridge, nil
		}
		v.bridges.Delete(key)
	}

	createCtx, cancel := context.WithTimeout(ctx, qoderBridgeTimeout)
	defer cancel()
	templateBase, errTemplate := TemplateBase()
	if errTemplate != nil {
		return nil, errTemplate
	}
	b, errBridge := NewBridge(createCtx, BridgeSecret(cred), v.region, templateBase, &v.models)
	if errBridge != nil {
		return nil, ClassifyCredentialError(errBridge)
	}
	v.bridges.Store(key, bridgeEntry{bridge: b, createdAt: time.Now()})
	return b, nil
}

// bridgeCacheKey generates the cache key for the Bridge.
//
// Uses "credential fingerprint + region": the session must be rebuilt after credential rotation (old signing keys become invalid).
// Does not store token plaintext directly -- the cache key may enter logs and memory dumps.
func (v *adapter) bridgeCacheKey(cred *Credential) string {
	return v.ID() + "|" + CredentialFingerprint(cred)
}

// ---- Internal Utilities ----

// modelsToPluginAPI converts upstream model entries into host contract types.
//
// Public model IDs use slugs normalized from display_name (DeepSeek-V4-Pro -> deepseek-v4-pro),
// while rebuilding the slug -> upstream key index for this instance, which is precisely restored by the Bridge upon outbound
// calls to model_config.key / x-model-key required by the Qoder encryption gateway.
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	slugByKey := v.models.replace(models)
	core.FillCredits(&v.credits, models,
		func(model Model) string { return slugByKey[strings.TrimSpace(model.Key)] },
		func(model Model) string {
			if model.PriceFactor == nil {
				return ""
			}
			return strconv.FormatFloat(*model.PriceFactor, 'f', -1, 64)
		})
	registered := core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		key := strings.TrimSpace(model.Key)
		if key == "" {
			return core.ModelDescriptor{}, false
		}
		id, okSlug := slugByKey[key]
		if !okSlug {
			return core.ModelDescriptor{}, false
		}
		name := strings.TrimSpace(model.DisplayName)
		if name == "" {
			name = key
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        describeModel(model),
			ContextLength:      int64(model.ContextWindow),
			MaxOutputTokens:    int64(model.MaxOutputTokens),
			SupportsTools:      true,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
	return registered
}

// ModelCredits implements core.CreditsProvider: returns "public model ID -> billing multiplier".
//
// Only reports the multiplier dimension; the model list itself is derived by the catalog layer from registration results.
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// describeModel formats the model description (with price multiplier).
func describeModel(model Model) string {
	description := "Qoder upstream model"
	if model.IsDefault {
		description += " (default)"
	}
	if model.IsReasoning {
		description += " (reasoning)"
	}
	if model.PriceFactor != nil {
		description += fmt.Sprintf(", billing multiplier x%.2f", *model.PriceFactor)
	}
	return description
}

// payloadRecorder writes the HTTP response into memory (the plugin side does not handle real HTTP responses).
type payloadRecorder struct {
	status  int
	headers http.Header
	body    []byte
}

func newPayloadRecorder() *payloadRecorder {
	return &payloadRecorder{status: http.StatusOK, headers: http.Header{}}
}

func (r *payloadRecorder) Header() http.Header { return r.headers }

func (r *payloadRecorder) Write(data []byte) (int, error) {
	r.body = append(r.body, data...)
	return len(data), nil
}

func (r *payloadRecorder) WriteHeader(statusCode int) { r.status = statusCode }

// Payload returns the recorded response body.
func (r *payloadRecorder) Payload() []byte { return r.body }

// Compile-time assertion: provider capabilities actually supported by the adapter.
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.LoginSessionOwner    = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.StorageMerger        = (*adapter)(nil)
)
