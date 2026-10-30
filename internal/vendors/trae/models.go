package trae

// This file provides model catalog parsing for the Trae family.
//
// The TraeCode gateway has two namespaces (refer to Trae2api-cn):
//   - config_name: configuration name in the catalog (e.g., glm-5.2), used in request body config_name field;
//   - wire id: catalog model_detail_list[].model_name (e.g., glm-5.2__dev),
//     used in request body model_name field with case-sensitive matching.
//
// The sole source of the catalog: POST /api/ide/v1/get_detail_param (function is consistent with the chat side,
// see fetchCatalog). Cached per account; refreshed after cache expiration. Falls back to old entries on fetch failure
// (stale-while-error), and errors only if no old entries exist.
// Entries with usage=custom_model/summary are routing configs and are not selectable chat models.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	SupportsTools bool   `json:"supports_tools"`
	IsReasoning   bool   `json:"is_reasoning"`
	// Credits is the upstream billing multiplier from consumption_rate (raw text, e.g., "0.94").
	//
	// empty means upstream has no multiplier (enable=false or missing field), console displays "—".
	Credits string `json:"credits,omitempty"`
}

// ResolvedModel is the model identifier pair used in a chat request.
type ResolvedModel struct {
	ConfigName string // request body config_name
	WireName   string // request body model_name (upstream wire id)
}

// ---- Upstream catalog (cached per account) ----

type catalogEntry struct {
	models    map[string]ResolvedModel // config_name (lowercase) -> parsing result
	metadata  map[string]Model         // config_name -> metadata
	fetchedAt time.Time
}

const catalogTTL = 10 * time.Minute

// catalogCache is the model catalog cache, keyed by account + function + build number.
//
// Belongs to the adapter instance rather than package-level variables: multiple App instances in the same process are independent, and cache
// lifecycle follows the instance. Zero value is usable; nil receiver means no caching (one-off calls hit upstream directly).
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]catalogEntry
}

// resolve uses the account catalog to parse the requested model name into (config_name, wire id).
//
// Model names are passed through as-is (no alias normalization): whatever is in the catalog is requested; missing models cause an error,
// avoiding guessing an incorrect wire id which would trigger upstream 4023.
func (c *catalogCache) resolve(ctx context.Context, model string, cfg Config, cred *Credential) (ResolvedModel, error) {
	requested := strings.TrimSpace(model)
	if requested == "" {
		requested = cfg.DefaultModel
	}
	entry, okEntry := c.load(ctx, cred, cfg)
	if !okEntry {
		return ResolvedModel{}, fmt.Errorf("Trae model catalog is temporarily unavailable, please try again later")
	}
	if resolved, okResolved := entry.models[strings.ToLower(requested)]; okResolved {
		return resolved, nil
	}
	return ResolvedModel{}, fmt.Errorf("model %s is not in the available catalog for this account", requested)
}

// load reads the account's model catalog cache; refreshes if expired, falls back to old entries on fetch failure.
//
// The cache key includes function and build number: catalog content is determined by both, and caching only by account would
// continue hitting the old list after version numbers are modified by config (up to catalogTTL).
// catalogEntries filters raw entries into "selectable chat models" and display metadata.
//
// Kept as a separate function to allow feeding two generations of keyed responses directly in tests without starting an HTTP service.
func catalogEntries(items []catalogItem) (map[string]ResolvedModel, map[string]Model) {
	models := make(map[string]ResolvedModel, len(items))
	metadata := make(map[string]Model, len(items))
	for _, item := range items {
		name := strings.TrimSpace(item.ConfigName)
		if name == "" || !item.ConfigSwitch {
			continue
		}
		// Entries with usage=custom_model/summary are routing configs or internal summaries, not selectable chat models.
		if usage := strings.ToLower(strings.TrimSpace(item.Usage)); usage != "" && usage != "chat_completion" {
			continue
		}
		if item.CustomModels != nil {
			continue
		}
		wire, maxTokens := selectWireModel(item.ModelDetailList)
		if wire == "" {
			continue
		}
		display := strings.TrimSpace(item.DisplayConfig.DisplayName)
		if display == "" {
			display = name
		}
		models[strings.ToLower(name)] = ResolvedModel{ConfigName: name, WireName: wire}
		metadata[name] = Model{
			ID:            name,
			Name:          display,
			Description:   "Trae upstream model",
			ContextWindow: item.ContextWindow.Dev,
			MaxTokens:     maxTokens,
			SupportsTools: strings.Contains(item.ExtraConfig, `"native_function_call":true`),
			IsReasoning:   item.DisplayConfig.ModelCapability == "reasoning_model",
			Credits:       consumptionRate(item.DisplayContactConfig),
		}
	}
	return models, metadata
}

func (c *catalogCache) load(ctx context.Context, cred *Credential, cfg Config) (catalogEntry, bool) {
	key := strings.Join([]string{cfg.VendorID, cred.UID, cfg.Function, cfg.IDEVersionCode}, "|")
	var (
		entry    catalogEntry
		okCached bool
	)
	if c != nil {
		c.mu.Lock()
		entry, okCached = c.entries[key]
		c.mu.Unlock()
		if okCached && time.Since(entry.fetchedAt) < catalogTTL {
			return entry, true
		}
	}

	fresh, okFresh := fetchCatalog(ctx, cred, cfg)
	if !okFresh {
		// Fall back to old entries when upstream is temporarily unavailable; fail only if never fetched successfully.
		return entry, okCached
	}
	if c != nil {
		now := time.Now()
		c.mu.Lock()
		if c.entries == nil {
			c.entries = map[string]catalogEntry{}
		}
		// Clean up expired entries along the way: accounts persist long-term, and append-only caches would grow indefinitely.
		for cachedKey, cached := range c.entries {
			if now.Sub(cached.fetchedAt) >= catalogTTL {
				delete(c.entries, cachedKey)
			}
		}
		c.entries[key] = fresh
		c.mu.Unlock()
	}
	return fresh, true
}

// catalogResponse is the response structure for get_detail_param (taking only required fields).
type catalogResponse struct {
	ConfigInfoList []catalogItem `json:"config_info_list"`
}

type catalogItem struct {
	ConfigName    string `json:"config_name"`
	ConfigSwitch  bool   `json:"config_switch"`
	Usage         string `json:"usage"`
	CustomModels  any    `json:"custom_models"`
	ContextWindow struct {
		Dev int `json:"dev"`
	} `json:"context_window_tokens"`
	DisplayConfig struct {
		DisplayName     string `json:"display_name"`
		ModelCapability string `json:"model_capability"`
	} `json:"display_config"`
	ExtraConfig     string          `json:"extra_config"`
	ModelDetailList []catalogDetail `json:"model_detail_list"`
	// DisplayContactConfig contains billing multipliers, but field casing is inconsistent across response generations
	// (display_contact_config / displayContactConfig, with both camelCase
	// and snake_case states internally), so capture the whole block and retrieve case-insensitively by key.
	DisplayContactConfig json.RawMessage `json:"display_contact_config"`
}

// UnmarshalJSON is compatible with both outer key naming styles.
//
// The old version uses snake_case, and the new version changed the outer layer to camelCase (displayContactConfig,
// extraConfig). Recognizing only one would silently turn the entire multiplier/tool capability into "unknown".
func (item *catalogItem) UnmarshalJSON(data []byte) error {
	type alias catalogItem
	var aux struct {
		*alias
		DisplayContactConfigCamel json.RawMessage `json:"displayContactConfig"`
		ExtraConfigCamel          json.RawMessage `json:"extraConfig"`
	}
	aux.alias = (*alias)(item)
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(item.DisplayContactConfig) == 0 {
		item.DisplayContactConfig = aux.DisplayContactConfigCamel
	}
	if item.ExtraConfig == "" && len(aux.ExtraConfigCamel) > 0 {
		var text string
		if err := json.Unmarshal(aux.ExtraConfigCamel, &text); err == nil {
			item.ExtraConfig = text
		}
	}
	return nil
}

type catalogDetail struct {
	ModelName string `json:"model_name"`
	MaxTokens int    `json:"max_tokens"`
}

// selectWireModel picks the wire id from model_detail_list: prioritizing the __dev tier,
// and taking the first entry for configurations without __dev. Note that the wire id belongs to a function's namespace,
// and the return value is only valid under that function (see fetchCatalog).
func selectWireModel(details []catalogDetail) (string, int) {
	fallback := ""
	fallbackTokens := 0
	for _, detail := range details {
		name := strings.TrimSpace(detail.ModelName)
		if name == "" {
			continue
		}
		if strings.HasSuffix(strings.ToLower(name), "__dev") {
			return name, detail.MaxTokens
		}
		if fallback == "" {
			fallback = name
			fallbackTokens = detail.MaxTokens
		}
	}
	return fallback, fallbackTokens
}

// fetchCatalog calls the upstream get_detail_param to pull the account's available model catalog.
//
// Critical constraint: **function must match the chat side** (cfg.Function). The upstream maintains
// two sets of namespaces based on function, and the same model has different wire ids under different functions:
//   - chat_v3:        DeepSeek-V4-Pro -> DeepSeek-V4-Pro__dev
//   - solo_work_lite: DeepSeek-V4-Pro -> deepseek_v4_pro__dev
//
// Taking the wire id from solo_work_lite and chatting under chat_v3 will cause the upstream to return
// 4023 model is unknown. Therefore, the request body, X-App-Function, and X-Ide-Function
// must all send cfg.Function.
func fetchCatalog(ctx context.Context, cred *Credential, cfg Config) (catalogEntry, bool) {
	body, errBody := json.Marshal(map[string]any{
		"function":            cfg.Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	if errBody != nil {
		return catalogEntry{}, false
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cfg.ChatHost+EpModels, strings.NewReader(string(body)))
	if errReq != nil {
		return catalogEntry{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
	req.Header.Set("X-Cloudide-Token", cred.AccessToken)
	req.Header.Set("X-Ide-Token", cred.AccessToken)
	if cred.UID != "" {
		req.Header.Set("X-Uid", cred.UID)
	}
	req.Header.Set("X-App-Id", cfg.AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-App-Version-Code", cfg.IDEVersionCode)
	req.Header.Set("X-Ide-Version", cfg.IDEVersion)
	req.Header.Set("X-Ide-Version-Code", cfg.IDEVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-App-Function", cfg.Function)
	req.Header.Set("X-Ide-Function", cfg.Function)
	req.Header.Set("X-Device-Brand", cfg.DeviceBrand)
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("Request-Traffic-Type", "prod")

	client := hostbridge.Client(ctx, 15*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		logger.Info("trae catalog: %s request failed: %v", cfg.VendorID, errDo)
		return catalogEntry{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		logger.Info("trae catalog: %s status=%d body=%s", cfg.VendorID, resp.StatusCode, core.Truncate(string(raw), 512))
		return catalogEntry{}, false
	}
	if errRead != nil {
		logger.Info("trae catalog: %s read body failed: %v", cfg.VendorID, errRead)
		return catalogEntry{}, false
	}
	var parsed catalogResponse
	if errJSON := json.Unmarshal(raw, &parsed); errJSON != nil {
		logger.Info("trae catalog: %s decode failed: %v body=%s", cfg.VendorID, errJSON, core.Truncate(string(raw), 512))
		return catalogEntry{}, false
	}

	models, metadata := catalogEntries(parsed.ConfigInfoList)
	entry := catalogEntry{models: models, metadata: metadata, fetchedAt: time.Now()}
	if len(entry.models) == 0 {
		// Upstream occasionally downgrades globally (e.g., returning only old manifests when the build number is below the gate). Printing
		// out the entry summary helps distinguish between "catalog structure changed" and "version number downgraded".
		logger.Info("trae catalog: %s filtered out is empty (function=%s code=%s, original %d entries): %s",
			cfg.VendorID, cfg.Function, cfg.IDEVersionCode, len(parsed.ConfigInfoList),
			summarizeCatalog(parsed.ConfigInfoList))
		return catalogEntry{}, false
	}
	return entry, true
}

// consumptionRate retrieves the billing multiplier from display_contact_config.
//
// Upstream shape: display_contact_config.consumption_rate{enable, data{enable, rate}},
// key names have both snake_case and camelCase states, retrieved level by level case-insensitively. enable=false
// means the model does not participate in billing (equivalent to "no multiplier"); rate=0 is a valid limited free tier and is retained.
func consumptionRate(raw json.RawMessage) string {
	contact, okContact := decodeObject(raw)
	if !okContact {
		return ""
	}
	rate, okRate := decodeObject(lookupRaw(contact, "consumption_rate"))
	if !okRate {
		return ""
	}
	if disabled(rate) {
		return ""
	}
	data, okData := decodeObject(lookupRaw(rate, "data"))
	if !okData || disabled(data) {
		return ""
	}
	value := lookupRaw(data, "rate")
	if len(value) == 0 {
		return ""
	}
	var text string
	if errText := json.Unmarshal(value, &text); errText == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if errNumber := json.Unmarshal(value, &number); errNumber == nil {
		return number.String()
	}
	return ""
}

// decodeObject decodes JSON into a key-value map; returns false for empty values/non-objects.
func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var doc map[string]json.RawMessage
	if errDoc := json.Unmarshal(raw, &doc); errDoc != nil || doc == nil {
		return nil, false
	}
	return doc, true
}

// lookupRaw gets a value by key name, ignoring case and underscores (compatible with mixed camelCase and snake_case).
//
// Doing EqualFold alone is not enough: consumptionRate and consumption_rate differ in separators,
// and remain unequal after folding case, causing the multiplier to be silently missed.
func lookupRaw(doc map[string]json.RawMessage, key string) json.RawMessage {
	want := normalizeKey(key)
	for name, value := range doc {
		if normalizeKey(name) == want {
			return value
		}
	}
	return nil
}

// normalizeKey normalizes key names to "lowercase + underscore removed".
func normalizeKey(key string) string {
	return strings.ToLower(strings.ReplaceAll(key, "_", ""))
}

// disabled reports whether enable in the object is explicitly false (missing fields are treated as enabled).
func disabled(doc map[string]json.RawMessage) bool {
	raw := lookupRaw(doc, "enable")
	if len(raw) == 0 {
		return false
	}
	var enabled bool
	if errEnable := json.Unmarshal(raw, &enabled); errEnable != nil {
		return false
	}
	return !enabled
}

// summarizeCatalog generates a raw entry summary, containing only category fields and no credentials.
func summarizeCatalog(items []catalogItem) string {
	const limit = 20
	parts := make([]string, 0, min(len(items), limit))
	for index, item := range items {
		if index >= limit {
			parts = append(parts, fmt.Sprintf("...(+%d)", len(items)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s[usage=%s,switch=%t,custom=%t]",
			item.ConfigName, item.Usage, item.ConfigSwitch, item.CustomModels != nil))
	}
	return strings.Join(parts, ", ")
}

// FetchModels returns the available model list for this credential account (directly from the upstream catalog, without cross-call caching).
func FetchModels(ctx context.Context, cred *Credential, cfg Config) ([]Model, error) {
	return new(catalogCache).fetchModels(ctx, cred, cfg)
}

// fetchModels is the instance-cached version of FetchModels: avoids hitting upstream when catalogCache hits.
func (c *catalogCache) fetchModels(ctx context.Context, cred *Credential, cfg Config) ([]Model, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("trae: no available credentials, unable to pull model catalog")
	}
	entry, okEntry := c.load(ctx, cred, cfg)
	if !okEntry {
		return nil, fmt.Errorf("trae: failed to pull model catalog")
	}
	out := make([]Model, 0, len(entry.metadata))
	for _, model := range entry.metadata {
		out = append(out, model)
	}
	return out, nil
}
