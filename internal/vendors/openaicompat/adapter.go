// Package openaicompat connects OpenAI-compatible APIs that offer free tiers.
//
// Every provider here is the vendor's own first-party endpoint and offers
// free models or a recurring free tier; pay-only providers are left out. Unknown
// relays and resellers from the reviewed upstream catalogs (FleetRelay,
// OmniRush, anonymous gateways) are deliberately absent: they would receive
// every prompt and every key.
package openaicompat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// FreeFilter selects which upstream models a provider exposes.
type FreeFilter string

const (
	// FilterAll exposes every listed model (free tiers are rate limited, not
	// model limited, on these providers).
	FilterAll FreeFilter = "all"
	// FilterFlagged keeps entries marked isFree, or ending in ":free" (Kilo).
	FilterFlagged FreeFilter = "flagged"
	// FilterZeroPrice keeps ":free" ids and zero-priced entries (OpenRouter).
	FilterZeroPrice FreeFilter = "zero_price"
	// FilterFlash keeps the free "-flash" models (Z.ai); "-flashx" is paid.
	FilterFlash FreeFilter = "flash"
)

// Provider describes one OpenAI-compatible upstream.
type Provider struct {
	// ID is the vendor ID, credential file prefix and model ID namespace.
	ID string
	// Name is the console display name.
	Name string
	// BaseURL is the OpenAI-compatible base; chat goes to BaseURL+"/chat/completions".
	BaseURL string
	// ModelsURL overrides BaseURL+"/models" when the catalog lives elsewhere.
	ModelsURL string
	// KeyURL is where users create a key; shown in the console.
	KeyURL string
	// Keyless providers answer free models without any credential.
	Keyless bool
	// Filter selects the exposed models.
	Filter FreeFilter
	// Note is a one-line hint about the free tier.
	Note string
	// AppHeaders identify the calling app to providers that gate some free
	// models on it (OpenRouter serves certain :free models only to coding
	// agents it recognises by HTTP-Referer / X-Title). Overridable through
	// <id>_app_url and <id>_app_title.
	AppHeaders map[string]string
}

// clineAppHeaders is the attribution the Cline coding agent sends to OpenRouter.
var clineAppHeaders = map[string]string{"HTTP-Referer": "https://cline.bot", "X-Title": "Cline"}

// Providers is the built-in provider list, in console order.
var Providers = []Provider{
	{ID: "kilo", Name: "Kilo (free pool, no key)", BaseURL: "https://api.kilo.ai/api/gateway", KeyURL: "https://app.kilo.ai", Keyless: true, Filter: FilterFlagged, Note: "Free pool needs no account; prompts may be logged by the upstream."},
	{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1", KeyURL: "https://openrouter.ai/keys", Filter: FilterZeroPrice, AppHeaders: clineAppHeaders, Note: "Only :free models are exposed unless openrouter_paid_models is on. Requests identify as the Cline coding agent so agent-only free models work."},
	{ID: "github", Name: "GitHub Models", BaseURL: "https://models.github.ai/inference", ModelsURL: "https://models.github.ai/catalog/models", KeyURL: "https://github.com/settings/personal-access-tokens", Filter: FilterAll, Note: "Fine-grained token with models:read; free tier is rate limited."},
	{ID: "nvidia", Name: "NVIDIA NIM", BaseURL: "https://integrate.api.nvidia.com/v1", KeyURL: "https://build.nvidia.com/settings/api-keys", Filter: FilterAll},
	{ID: "groq", Name: "Groq", BaseURL: "https://api.groq.com/openai/v1", KeyURL: "https://console.groq.com/keys", Filter: FilterAll},
	{ID: "cerebras", Name: "Cerebras", BaseURL: "https://api.cerebras.ai/v1", KeyURL: "https://cloud.cerebras.ai", Filter: FilterAll},
	{ID: "mistral", Name: "Mistral", BaseURL: "https://api.mistral.ai/v1", KeyURL: "https://console.mistral.ai/api-keys", Filter: FilterAll},
	{ID: "codestral", Name: "Mistral Codestral", BaseURL: "https://codestral.mistral.ai/v1", KeyURL: "https://console.mistral.ai/codestral", Filter: FilterAll},
	{ID: "sambanova", Name: "SambaNova", BaseURL: "https://api.sambanova.ai/v1", KeyURL: "https://cloud.sambanova.ai/apis", Filter: FilterAll},
	{ID: "llm7", Name: "LLM7", BaseURL: "https://api.llm7.io/v1", KeyURL: "https://token.llm7.io", Filter: FilterAll},
	{ID: "cohere", Name: "Cohere", BaseURL: "https://api.cohere.ai/compatibility/v1", KeyURL: "https://dashboard.cohere.com/api-keys", Filter: FilterAll},
	{ID: "ollama", Name: "Ollama Cloud", BaseURL: "https://ollama.com/v1", KeyURL: "https://ollama.com/settings/keys", Filter: FilterAll},
	{ID: "vercel", Name: "Vercel AI Gateway", BaseURL: "https://ai-gateway.vercel.sh/v1", KeyURL: "https://vercel.com/docs/ai-gateway", Filter: FilterAll},
	{ID: "zai", Name: "Z.ai", BaseURL: "https://api.z.ai/api/paas/v4", KeyURL: "https://z.ai/manage-apikey/apikey-list", Filter: FilterFlash, Note: "Only the free GLM *-Flash models are exposed."},
}

const (
	modelsTimeout    = 30 * time.Second
	chatTimeout      = 10 * time.Minute
	modelsCacheTTL   = 10 * time.Minute
	maxResponseBytes = 32 << 20
)

// Instances builds one vendor per built-in provider.
func Instances(env core.Env) []core.Vendor {
	vendors := make([]core.Vendor, 0, len(Providers))
	for _, provider := range Providers {
		vendors = append(vendors, newAdapter(env, provider))
	}
	return vendors
}

type adapter struct {
	*core.NativeAdapter[*Credential]
	provider Provider

	mu         sync.Mutex
	showPaid   bool
	appHeaders map[string]string
	cache      map[string]cachedModels
}

type cachedModels struct {
	models  []upstreamModel
	fetched time.Time
}

func newAdapter(env core.Env, provider Provider) *adapter {
	a := &adapter{provider: provider, appHeaders: provider.AppHeaders, cache: map[string]cachedModels{}}
	a.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: provider.ID, VendorName: provider.Name},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := parseCredential(raw, provider.Keyless)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			label, token, mode := native.Label, native.APIKey, "apikey"
			if native.Anonymous {
				token, mode = "anonymous:"+provider.ID, "anonymous"
			}
			if label == "" {
				label = maskKey(native.APIKey)
			}
			return core.CredentialSeed{
				VendorID: provider.ID, Label: label, FileID: core.FileIDFromFileName(fileName),
				Token: token, AuthMode: mode,
			}, native, nil
		},
	})
	return a
}

// Match accepts only files named after this provider; the registry already
// resolves files that carry an explicit vendor field.
func (a *adapter) Match(fileName, _ string, _ map[string]any) bool {
	return core.FileNameMatchesConvention(fileName, a.provider.ID)
}

// Configure reads provider options.
func (a *adapter) Configure(_ core.Settings, raw map[string]string) {
	value := strings.ToLower(strings.TrimSpace(raw[a.provider.ID+"_paid_models"]))
	showPaid := a.provider.Filter == FilterZeroPrice && (value == "true" || value == "1" || value == "yes" || value == "on")
	headers := map[string]string{}
	for name, header := range a.provider.AppHeaders {
		headers[name] = header
	}
	if len(headers) > 0 {
		if appURL := strings.TrimSpace(raw[a.provider.ID+"_app_url"]); appURL != "" {
			headers["HTTP-Referer"] = appURL
		}
		if appTitle := strings.TrimSpace(raw[a.provider.ID+"_app_title"]); appTitle != "" {
			headers["X-Title"] = appTitle
		}
	}
	a.mu.Lock()
	a.showPaid = showPaid
	a.appHeaders = headers
	a.mu.Unlock()
}

// applyAppHeaders adds the configured app identity to an upstream request.
func (a *adapter) applyAppHeaders(req *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for name, value := range a.appHeaders {
		req.Header.Set(name, value)
	}
}

// ConfigFields declares provider options.
func (a *adapter) ConfigFields() []pluginapi.ConfigField {
	var fields []pluginapi.ConfigField
	if a.provider.Filter == FilterZeroPrice {
		fields = append(fields, pluginapi.ConfigField{
			Name:        a.provider.ID + "_paid_models",
			Type:        pluginapi.ConfigFieldTypeBoolean,
			Description: a.provider.Name + ": also expose paid models (uses your credits).",
		})
	}
	if len(a.provider.AppHeaders) > 0 {
		fields = append(fields,
			pluginapi.ConfigField{Name: a.provider.ID + "_app_url", Type: pluginapi.ConfigFieldTypeString, Description: a.provider.Name + ": app URL sent as HTTP-Referer (default " + a.provider.AppHeaders["HTTP-Referer"] + ")."},
			pluginapi.ConfigField{Name: a.provider.ID + "_app_title", Type: pluginapi.ConfigFieldTypeString, Description: a.provider.Name + ": app name sent as X-Title (default " + a.provider.AppHeaders["X-Title"] + ")."},
		)
	}
	return fields
}

// KeylessCredentialJSON implements core.KeylessProvider for keyless providers.
func (a *adapter) KeylessCredentialJSON() []byte {
	if !a.provider.Keyless {
		return nil
	}
	return []byte(`{"type":"` + core.ProviderKey + `","vendor":"` + a.provider.ID + `","anonymous":true,"label":"Free pool (no account)"}`)
}

// ---- models ----

// StaticModels lists models using any usable credential of this provider, or
// none for keyless providers.
func (a *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	native, ok := core.ProbeNativeCredential(a.Env, a.provider.ID, func(cred *core.Credential) (*Credential, bool) {
		value, okValue := cred.Native.(*Credential)
		return value, okValue
	})
	if !ok || native == nil {
		if !a.provider.Keyless {
			return nil, fmt.Errorf("no %s credential available for model probe", a.provider.ID)
		}
		native = &Credential{Anonymous: true}
	}
	models, errModels := a.models(ctx, native)
	if errModels != nil {
		return nil, errModels
	}
	return a.toModelInfo(models), nil
}

// ModelsForAuth lists the models this credential can call. A failed probe
// returns an empty list so the host drops the credential instead of routing
// traffic to a bad key.
func (a *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := a.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	models, errModels := a.models(ctx, native)
	if errModels != nil {
		logger.Info("%s: model probe failed: %v", a.provider.ID, errModels)
		return nil, nil
	}
	return a.toModelInfo(models), nil
}

func (a *adapter) models(ctx context.Context, native *Credential) ([]upstreamModel, error) {
	cacheKey := keyFingerprint(native.APIKey)
	a.mu.Lock()
	if entry, ok := a.cache[cacheKey]; ok && time.Since(entry.fetched) < modelsCacheTTL {
		a.mu.Unlock()
		return entry.models, nil
	}
	showPaid := a.showPaid
	a.mu.Unlock()

	body, errFetch := a.fetchModels(ctx, native.APIKey)
	if errFetch != nil {
		return nil, errFetch
	}
	listed, errParse := parseModels(body)
	if errParse != nil {
		return nil, errParse
	}
	models := filterModels(listed, a.provider.Filter, showPaid)
	if len(models) == 0 {
		return nil, fmt.Errorf("%s listed no usable models", a.provider.ID)
	}
	a.mu.Lock()
	a.cache[cacheKey] = cachedModels{models: models, fetched: time.Now()}
	a.mu.Unlock()
	return models, nil
}

func (a *adapter) fetchModels(ctx context.Context, apiKey string) ([]byte, error) {
	endpoint := a.provider.ModelsURL
	if endpoint == "" {
		endpoint = strings.TrimRight(a.provider.BaseURL, "/") + "/models"
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	a.applyAppHeaders(req)
	resp, errDo := hostbridge.Client(ctx, modelsTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("list models: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if errRead != nil {
		return nil, fmt.Errorf("read models: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classify(resp.StatusCode, body)
	}
	return body, nil
}

func (a *adapter) toModelInfo(models []upstreamModel) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model upstreamModel) (core.ModelDescriptor, bool) {
		contextWindow := model.ContextLength
		if contextWindow == 0 {
			contextWindow = core.DefaultContextWindow(model.ID)
		}
		maxOutput := model.MaxOutput
		if maxOutput == 0 {
			maxOutput = core.DefaultMaxOutputTokens
		}
		name := model.Name
		if name == "" {
			name = model.ID
		}
		return core.ModelDescriptor{
			ID:                 a.PublicModelID(model.ID),
			Name:               name + " (" + a.provider.Name + ")",
			Description:        a.provider.Name + " model " + model.ID,
			ContextLength:      contextWindow,
			MaxOutputTokens:    maxOutput,
			SupportsImages:     model.SupportsImages,
			SupportsTools:      true,
			CanDisableThinking: true,
		}, true
	})
}

// PublicModelID namespaces an upstream model ID with the provider ID so it
// never collides with the host's own providers or another vendor.
func (a *adapter) PublicModelID(upstreamID string) string {
	return a.provider.ID + "/" + upstreamID
}

// UpstreamModelID strips the provider namespace from a public model ID.
func (a *adapter) UpstreamModelID(publicID string) string {
	return strings.TrimPrefix(strings.TrimSpace(publicID), a.provider.ID+"/")
}

func keyFingerprint(key string) string {
	if key == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
)
