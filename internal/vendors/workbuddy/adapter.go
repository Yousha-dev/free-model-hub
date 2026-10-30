package workbuddy

// This file adapts the WorkBuddy (CodeBuddy) protocol into core.Vendor.
//
// Vendor instance = protocol x region. CN and Global are two independently deployed systems: domain names, model lists,
// reasoning tier tables, check-in campaigns all differ, and credentials are not interchangeable (using a CN token against the Global
// domain returns 401 TOKEN_EXPIRE). The region is fixed at construction time, with no branching inside instance methods.

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
)

// adapter is the core.Vendor implementation for WorkBuddy.
type adapter struct {
	*core.NativeAdapter[*Credential]
	region Region
	rt     *runtime
}

// Instances constructs the two regional instances of WorkBuddy.
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{newInstance(env, RegionCN), newInstance(env, RegionGlobal)}
}

func newInstance(env core.Env, region Region) core.Vendor {
	rt := newRuntime(env)
	inst := &adapter{rt: rt, region: region}
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
			if native.Realm() != region {
				return core.CredentialSeed{}, nil, fmt.Errorf("credential realm %q does not match vendor %q", native.Realm(), VendorIDFor(region))
			}
			token := native.AccessTokenValue()
			authMode := "oauth"
			if token == "" {
				token = native.DeviceTokenValue()
				authMode = "device"
			}
			return core.CredentialSeed{
				VendorID: VendorIDFor(region), Region: string(native.Realm()),
				Label:  core.FirstNonEmpty(native.NicknameValue(), native.UIDValue(), fileName),
				FileID: core.FileIDFromFileName(fileName), UID: native.UIDValue(),
				Token: token, RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAt, AuthMode: authMode,
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			client := NewClient(rt, rt.options(ctx))
			accessBefore, refreshBefore := native.Snapshot()
			if errRefresh := client.RefreshToken(native); errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			refreshed := !native.UnchangedSince(accessBefore, refreshBefore)
			return core.NativeRefresh[*Credential]{
				Native: native, Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAt, Changed: refreshed,
			}, nil
		},
	})
	return inst
}

// Configure parses WorkBuddy-specific configurations (UA, device token, prompt, machine salt).
func (v *adapter) Configure(settings core.Settings, raw map[string]string) {
	v.rt.configure(settings, raw)
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
	if settings.RealmEnabled(string(RegionCN)) {
		return RegionCN
	}
	return RegionGlobal
}

// ---- Refresh ----

// Refresh implements core.Refresher: exchanges a refresh token for a new access token.
//
// "Refresh" and "writeback" are deliberately separated: refresh is the vendor protocol (here), and writeback is the host-to-disk
// transaction (handled uniformly by internal/tasks keep-alive orchestration). This way the vendor does not need to know about the host,
// and avoids each vendor implementing their own merge + host.auth.save + disk write.
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	if errRefresh := v.client(ctx).RefreshToken(native); errRefresh != nil {
		return nil, false, errRefresh
	}
	return cred, true, nil
}

// ---- Models ----

// StaticModels returns the model list for this region.
//
// Probing is handled by the protocol layer (up to three concurrent unions for regional routing), and this only performs type conversion:
// Upstream ModelInfo -> Neutral ModelDescriptor -> Host contract type.
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	client := v.client(ctx)
	models, errModels := client.FetchModels(v.region)
	if errModels != nil {
		return nil, errModels
	}
	v.rt.publishEffortTables(models)
	return modelsToPluginAPI(models), nil
}

// describeModel formats the model description (with a points multiplier prefix).
//
// The multiplier is the cost information users care about most, placed at the very front of the description.
func describeModel(model ModelInfo) string {
	description := strings.TrimSpace(model.Description)
	credits := strings.TrimSpace(model.Credits)
	if credits == "" {
		return description
	}
	// Upstream credits look like "x0.05"; strip the trailing "credit" suffix to avoid "x0.05 credits credit".
	credits = strings.TrimSuffix(credits, "credits")
	credits = strings.TrimSuffix(credits, "credit")
	return "[" + strings.TrimSpace(credits) + " credit] " + description
}

// modelsToPluginAPI converts protocol-layer model metadata into host contract types.
func modelsToPluginAPI(models []ModelInfo) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model ModelInfo) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        describeModel(model),
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxTokens,
			Efforts:            model.Efforts,
			DefaultEffort:      model.DefaultEffort,
			SupportsImages:     model.SupportsImages,
			SupportsTools:      model.SupportsToolCall,
			CanDisableThinking: model.CanDisableThinking,
		}, true
	})
}

// ModelsForAuth returns the models available for the given credential.
//
// This vendor's list is probed by region (rather than by account), so it shares the same source as StaticModels.
// The host still uses this list to bind "credential <-> model", thereby filtering out cross-region credentials.
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	if cred == nil || cred.RegionValue() != string(v.region) {
		// Return an empty list instead of an error when the region does not match: the host will skip this credential as a result,
		// which is precisely the desired outcome (preventing wrong-region credentials from hitting upstream).
		return nil, nil
	}
	return v.StaticModels(ctx)
}

// ---- Execution ----

// Execute performs a non-streaming chat.
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	client := v.client(ctx)
	result, errChat := client.Chat(ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Model:      req.Model,
		ClientIP:   req.ClientIP,
		Meta:       ChatMetaFromBody(req.Payload),
	})
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: result.Response,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream performs a streaming chat.
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	client := v.client(ctx)
	reader, errStream := client.ChatStream(ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Model:      req.Model,
		ClientIP:   req.ClientIP,
		Meta:       ChatMetaFromBody(req.Payload),
	})
	if errStream != nil {
		return errStream
	}
	defer func() { _ = reader.Close() }()

	_, errForward := Stream(reader, StreamOptions{Emit: sink.Emit})
	// An empty stream is not considered a failure: upstream sometimes returns a stream without any deltas,
	// and treating it as an error would cause the client to receive meaningless error frames.
	if errForward != nil && !IsEmptyStreamError(errForward) && ctx.Err() == nil {
		return errForward
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
	client := v.client(ctx)
	balance, errBalance := client.FetchBalance(native)
	if errBalance != nil {
		return nil, errBalance
	}
	response := BuildQuotaResponse(native, balance)
	return &response, nil
}

// ---- Login & Refresh ----

// LoginStart initiates a device authorization login.
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	state, authURL, errStart := RequestLoginState(ctx, v.region)
	if errStart != nil {
		return nil, errStart
	}
	sessionID := "workbuddy_" + string(v.region) + "_" + core.RandomHex(16)
	StoreLoginSession(v.Env.Logins, sessionID, &PendingLogin{
		Region: v.region,
		State:  state,
	})
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       authURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(LoginSessionTTL),
		Metadata: map[string]any{
			core.VendorKey: v.ID(),
			"region":       string(v.region),
		},
	}, nil
}

// LoginPoll polls the login status.
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	session, exists, shouldPoll := CheckLoginSession(v.Env.Logins, state)
	if !exists {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "Login session does not exist or has expired, please restart the login process",
		}, nil
	}
	if !shouldPoll {
		// During throttling: return pending and wait, without hitting upstream or returning an error
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Waiting for user to complete authorization in the browser...",
		}, nil
	}
	accessToken, refreshToken, errToken := ExchangeLoginToken(ctx, session.Region, session.State)
	if errToken != nil {
		if IsLoginPending(errToken) {
			// User has not yet completed authorization in the browser, continue waiting.
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusPending,
				Message: "Waiting for the user to finish authorization in the browser...",
			}, nil
		}
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errToken.Error()}, nil
	}
	account, errAccount := FetchLoginAccount(ctx, session.Region, session.State, accessToken)
	if errAccount != nil {
		// The token arrived but account info failed: save minimal info so the user can still use it
		// (account info is filled in on a later refresh).
		logger.Error("fetch account info failed after token exchange: %v", errAccount)
	}
	credential := &Credential{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		UID:          account.UID,
		Nickname:     account.Nickname,
		Domain:       account.Domain,
		EnterpriseID: account.EnterpriseID,
	}
	credential.SetRealm(session.Region)
	if credential.Domain == "" {
		credential.Domain = DefaultDomainFor(credential.Realm())
	}
	storageJSON, errStorage := credential.StorageJSON()
	if errStorage != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "failed to build credential: " + errStorage.Error(),
		}, nil
	}
	RemoveLoginSession(v.Env.Logins, state)
	logger.Info("completed workbuddy login for %s (vendor=%s)",
		core.FirstNonEmpty(credential.NicknameValue(), credential.UIDValue()), v.ID())
	return &pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(v.ID(), core.FirstNonEmpty(credential.UID, core.RandomHex(16))),
			Label:       core.FirstNonEmpty(credential.NicknameValue(), credential.UIDValue()),
			StorageJSON: storageJSON,
		},
	}, nil
}

// MergeStorageJSON implements core.StorageMerger: merges refreshed tokens back into the original credential JSON.
//
// WorkBuddy credentials nest auth/account objects; a generic merge would scramble top-level fields,
// so this provider must do it itself.
func (v *adapter) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, errNative := v.Native(credential)
	if errNative != nil {
		return original, errNative
	}
	return MergeStorageJSON(original, native)
}

// ---- internal helpers ----

func (v *adapter) client(ctx context.Context) *Client {
	return NewClient(v.rt, v.rt.options(ctx))
}

// LoadModelCache implements core.ModelCacheLoader: loads only this instance's cache.
func (v *adapter) LoadModelCache(cached []core.CachedModel) {
	filtered := make([]core.CachedModel, 0, len(cached))
	for _, item := range cached {
		vendorID := strings.TrimSpace(item.VendorID)
		if vendorID == "" {
			vendorID = string(VendorIDFor(v.region))
		}
		if strings.EqualFold(vendorID, v.ID()) {
			filtered = append(filtered, item)
		}
	}
	v.rt.loadCachedModelsFromState(filtered)
}

// ConsoleModels returns the details the console model page shows (including credit multipliers).
func (v *adapter) ConsoleModels() []core.ConsoleModel {
	cached := v.rt.cachedModels()[NormalizeRegion(string(v.region))]
	out := make([]core.ConsoleModel, 0, len(cached))
	for _, model := range cached {
		out = append(out, core.ConsoleModel{
			ID:              model.ID,
			Name:            model.Name,
			Description:     model.Description,
			ContextLength:   model.ContextWindow,
			MaxOutputTokens: model.MaxTokens,
			Efforts:         model.Efforts,
			DefaultEffort:   model.DefaultEffort,
			SupportsImages:  model.SupportsImages,
			SupportsTools:   model.SupportsToolCall,
			Credits:         core.FormatRate(model.Credits),
		})
	}
	return out
}

// ModelCredits implements core.CreditsProvider: normalizes upstream credit multipliers for the console.
func (v *adapter) ModelCredits() map[string]string {
	cached := v.rt.cachedModels()[NormalizeRegion(string(v.region))]
	out := make(map[string]string, len(cached))
	for _, model := range cached {
		if rate := core.FormatRate(model.Credits); rate != "" {
			out[model.ID] = rate
		}
	}
	return out
}

// This file parses WorkBuddy-specific settings and keeps the runtime snapshot outbound requests need.
//
// Ownership: UA, device token, prompt and machine salt only affect the WorkBuddy upstream protocol,
// so this package interprets them; the root only passes global settings and raw keys (core.Configurable).

const maxPromptFileSize = 1 << 20

// runtimeConfig is the configuration snapshot of one adapter instance.
type runtimeConfig struct {
	enabledRealms        []string
	sanitizeFingerprints bool
	passthroughIP        bool
	stateDir             string

	promptMode string
	promptText string

	userAgent     string
	clientVersion string
	cliVersion    string
	clientName    string

	deviceToken     string
	deviceTokenFile string
}

func defaultRuntimeConfig() runtimeConfig {
	return runtimeConfig{
		sanitizeFingerprints: true,
		clientVersion:        DefaultClientVersion,
		cliVersion:           DefaultCLIVersion,
		clientName:           DefaultClientName,
		promptMode:           PromptModePassThrough,
		promptText:           BuiltinPrompt(),
	}
}

// configure parses WorkBuddy-specific keys and updates this instance's runtime snapshot.
func (r *runtime) configure(settings core.Settings, raw map[string]string) {
	cfg := defaultRuntimeConfig()
	cfg.enabledRealms = append([]string(nil), settings.EnabledRealms...)
	cfg.sanitizeFingerprints = ParseBool(raw["workbuddy_sanitize_fingerprints"], true)
	cfg.passthroughIP = ParseBool(raw["workbuddy_passthrough_ip"], false)
	cfg.stateDir = settings.StateDir

	if value := strings.TrimSpace(raw["workbuddy_user_agent"]); value != "" {
		cfg.userAgent = value
	}
	if value := strings.TrimSpace(raw["workbuddy_client_version"]); value != "" {
		cfg.clientVersion = value
	}
	if value := strings.TrimSpace(raw["workbuddy_cli_version"]); value != "" {
		cfg.cliVersion = value
	}
	if value := strings.TrimSpace(raw["workbuddy_client_name"]); value != "" {
		cfg.clientName = value
	}
	cfg.deviceToken = strings.TrimSpace(raw["workbuddy_device_token"])
	cfg.deviceTokenFile = strings.TrimSpace(raw["workbuddy_device_token_file"])

	// Prompt filtering is a global setting (prompt_mode / prompt_file); WorkBuddy only implements it.
	if value := strings.ToLower(strings.TrimSpace(raw["prompt_mode"])); value != "" {
		cfg.promptMode = value
	}
	if path := strings.TrimSpace(raw["prompt_file"]); path != "" {
		text, errRead := readPromptFile(path)
		if errRead != nil {
			logger.Error("load prompt file failed, falling back to built-in prompt: %v", errRead)
		} else if text != "" {
			cfg.promptText = text
		}
	}
	r.cfg.Store(&cfg)
	r.prompt.Store(&promptState{Mode: cfg.promptMode, Text: cfg.promptText})
	salt := strings.TrimSpace(settings.MachineSalt)
	r.salt.Store(&salt)
}

func readPromptFile(path string) (string, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return "", errRead
	}
	if len(raw) > maxPromptFileSize {
		return "", fmt.Errorf("prompt file %s exceeds %d bytes", path, maxPromptFileSize)
	}
	return strings.TrimSpace(string(raw)), nil
}

// options returns the Options for one request (config snapshot plus current prompt degradation state).
func (r *runtime) options(ctx context.Context) Options {
	cfg := *r.cfg.Load()
	prompt := *r.prompt.Load()
	promptText := prompt.Text
	if prompt.Degraded {
		promptText = degradedPrompt
	}
	return Options{
		Context:              ctx,
		EnabledRealms:        append([]string(nil), cfg.enabledRealms...),
		PromptMode:           prompt.Mode,
		PromptText:           promptText,
		OnContentBlocked:     r.markPromptDegraded,
		SanitizeFingerprints: cfg.sanitizeFingerprints,
		PassthroughIP:        cfg.passthroughIP,
		UserAgent:            cfg.userAgent,
		ClientVersion:        cfg.clientVersion,
		CLIVersion:           cfg.cliVersion,
		ClientName:           cfg.clientName,
		DeviceToken:          cfg.deviceToken,
		DeviceTokenFile:      cfg.deviceTokenFile,
		StateDir:             cfg.stateDir,
	}
}

// ConfigFields declares WorkBuddy-specific YAML settings.
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "workbuddy_sanitize_fingerprints", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Scrub fingerprints from outbound request bodies (default on)."},
		{Name: "workbuddy_passthrough_ip", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Forward the client IP upstream (default off)."},
		{Name: "workbuddy_user_agent", Type: pluginapi.ConfigFieldTypeString, Description: "Outbound User-Agent override."},
		{Name: "workbuddy_client_version", Type: pluginapi.ConfigFieldTypeString, Description: "WorkBuddy version segment of the User-Agent."},
		{Name: "workbuddy_cli_version", Type: pluginapi.ConfigFieldTypeString, Description: "CLI version segment of the User-Agent."},
		{Name: "workbuddy_client_name", Type: pluginapi.ConfigFieldTypeString, Description: "Client name for the attribution header."},
		{Name: "workbuddy_device_token", Type: pluginapi.ConfigFieldTypeString, Description: "Global fallback X-Device-Token."},
		{Name: "workbuddy_device_token_file", Type: pluginapi.ConfigFieldTypeString, Description: "Device token file path."},
	}
}

func ParseBool(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

//go:embed assets/prompt_default.md
var defaultPromptRaw []byte

const degradedPrompt = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

const (
	PromptModePassThrough = "passthrough"
	PromptModeCustom      = "custom"
	PromptModeAppend      = "append"
)

// BuiltinPrompt returns the built-in default prompt text.
func BuiltinPrompt() string {
	return strings.TrimSpace(string(defaultPromptRaw))
}

// This is the runtime state owned by each WorkBuddy adapter instance.
//
// Config, prompt degradation state, tier table, device token cache, machine salt and the model probe host
// all live in runtime instead of package variables, so App instances stay isolated.

type promptState struct {
	Mode     string
	Text     string
	Degraded bool
}

type effortState struct {
	Defaults  map[string]string
	Supported map[string][]string
}

type runtime struct {
	env core.Env

	cfg    atomic.Pointer[runtimeConfig]
	prompt atomic.Pointer[promptState]
	effort atomic.Pointer[effortState]
	salt   atomic.Pointer[string]

	deviceMu    sync.Mutex
	devicePath  string
	deviceToken string
	deviceAt    time.Time

	modelMu    sync.Mutex
	modelCache map[Region]modelCacheEntry
}

func newRuntime(env core.Env) *runtime {
	cfg := defaultRuntimeConfig()
	rt := &runtime{env: env, modelCache: map[Region]modelCacheEntry{}}
	rt.cfg.Store(&cfg)
	rt.prompt.Store(&promptState{Mode: PromptModePassThrough, Text: BuiltinPrompt()})
	rt.effort.Store(&effortState{Defaults: map[string]string{}, Supported: map[string][]string{}})
	salt := ""
	rt.salt.Store(&salt)
	return rt
}

func (r *runtime) markPromptDegraded() {
	current := *r.prompt.Load()
	current.Degraded = true
	r.prompt.Store(&current)
}

func (r *runtime) effortTables() (map[string]string, map[string][]string) {
	state := r.effort.Load()
	return state.Defaults, state.Supported
}

func (r *runtime) setEffortTables(defaults map[string]string, supported map[string][]string) {
	old := r.effort.Load()
	next := &effortState{
		Defaults:  make(map[string]string, len(old.Defaults)+len(defaults)),
		Supported: make(map[string][]string, len(old.Supported)+len(supported)),
	}
	for key, value := range old.Defaults {
		next.Defaults[key] = value
	}
	for key, value := range old.Supported {
		next.Supported[key] = value
	}
	for key, value := range defaults {
		next.Defaults[key] = value
	}
	for key, value := range supported {
		next.Supported[key] = value
	}
	r.effort.Store(next)
}

func (r *runtime) publishEffortTables(models []ModelInfo) {
	defaults := make(map[string]string, len(models))
	supported := make(map[string][]string, len(models))
	for _, model := range models {
		if model.DefaultEffort != "" {
			defaults[model.ID] = model.DefaultEffort
		}
		if len(model.Efforts) > 0 {
			supported[model.ID] = model.Efforts
		}
	}
	r.setEffortTables(defaults, supported)
}

func (r *runtime) deriveID(uid, purpose string) string {
	return deriveIDWithSalt(uid, purpose, *r.salt.Load())
}

func (r *runtime) fingerprint(credential *Credential, purpose string) string {
	return r.deriveID(credential.UIDValue(), purpose)
}

func (r *runtime) readDeviceTokenFile(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	r.deviceMu.Lock()
	defer r.deviceMu.Unlock()
	if r.devicePath == trimmed && time.Since(r.deviceAt) < deviceTokenFileTTL {
		return r.deviceToken
	}
	r.devicePath = trimmed
	r.deviceAt = time.Now()
	r.deviceToken = ""
	info, errStat := os.Stat(trimmed)
	if errStat != nil || info.Size() > deviceTokenFileMaxLen {
		return ""
	}
	raw, errRead := os.ReadFile(trimmed)
	if errRead != nil {
		return ""
	}
	r.deviceToken = strings.TrimSpace(string(raw))
	return r.deviceToken
}

func (r *runtime) hostProbeCredential(region Region) *Credential {
	if r.env.Host == nil || r.env.Registry == nil {
		return nil
	}
	ctx := context.Background()
	entries, errList := r.env.Host.ListAuths(ctx)
	if errList != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.Usable() {
			continue
		}
		stored, okStored := r.env.Host.AuthJSON(ctx, entry.AuthIndex)
		if !okStored {
			continue
		}
		credential, okCredential := r.env.Registry.ParseCredential(stored, entry.Name, nil)
		if !okCredential || credential.RegionValue() != string(region) {
			continue
		}
		if native, okNative := credential.Native.(*Credential); okNative && native != nil {
			return native
		}
	}
	return nil
}

// Compile-time assertions: the provider capabilities this adapter implements.
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.ConsoleModelProvider = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.ModelCacheLoader     = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.StorageMerger        = (*adapter)(nil)
)
