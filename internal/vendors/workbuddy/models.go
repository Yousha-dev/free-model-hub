package workbuddy

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"free-model-hub/internal/core"
)

// This file implements the model catalog: probing the model list from the upstream, caching, and providing lookup chains for windows and tiers.

// modelCacheEntry is the model catalog cache for a specific realm.
type modelCacheEntry struct {
	models    []ModelInfo
	fetchedAt time.Time
	lastFail  time.Time
}

// Cache window. Matched with upstream probing costs: list changes are slow, but retries should be allowed as soon as possible after failures.
const (
	modelCacheTTL        = time.Hour
	modelCacheFailWindow = 5 * time.Minute
)

//go:embed assets/catalog_seed.json
var catalogSeedRaw []byte

// ModelInfo is the normalized model metadata.
type ModelInfo struct {
	// ID is the upstream model name (excluding the plugin-side realm prefix).
	ID string
	// Name is the display name (upstream display_name, falling back to ID if missing).
	Name string
	// Description is the model description (may include a credit multiplier prefix).
	Description string
	// ContextWindow is the maximum input token count.
	ContextWindow int64
	// MaxTokens is the maximum output token count (thinking and response share this budget).
	MaxTokens int64
	// Efforts is the optional reasoning tier list.
	Efforts []string
	// DefaultEffort is the default tier.
	DefaultEffort string
	// Credits is the credit multiplier (e.g., "x0.05").
	Credits string
	// SupportsImages indicates whether image input is supported.
	SupportsImages bool
	// SupportsToolCall indicates whether tool calling is supported.
	SupportsToolCall bool
	// SupportsReasoning indicates whether chain of thought is supported.
	SupportsReasoning bool
	// CanDisableThinking indicates whether disabling thinking is allowed.
	CanDisableThinking bool
	// OnlyReasoning indicates reasoning only (no text output).
	OnlyReasoning bool
	// IsDefault indicates whether it is the account default model.
	IsDefault bool
}

// catalogSeed is the embedded fallback data.
type catalogSeed struct {
	Context map[string]struct {
		Context   int64 `json:"context"`
		MaxOutput int64 `json:"max_output"`
	} `json:"context"`
	EffortsCN     map[string]effortSeed `json:"efforts_cn"`
	EffortsGlobal map[string]effortSeed `json:"efforts_global"`
}

type effortSeed struct {
	Efforts []string `json:"efforts"`
	Default string   `json:"default"`
}

var (
	seedOnce sync.Once
	seedData catalogSeed
)

func loadSeed() catalogSeed {
	seedOnce.Do(func() {
		if errUnmarshal := json.Unmarshal(catalogSeedRaw, &seedData); errUnmarshal != nil {
			// Embedded resource corruption is a build-time issue, runtime must gracefully degrade: can still work without fallback table
			// (window and tier fields will be missing, but conversation is not affected).
			seedData = catalogSeed{}
		}
	})
	return seedData
}

// dynModelEntry is the entry format for upstream /v3/config and model catalog APIs.
//
// Fields cover both CN and Global formats: Global uses modelId/model as
// a loose fallback key for id, while CN catalog does not output these two fields.
type dynModelEntry struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	ModelID         string   `json:"modelId"`
	Model           string   `json:"model"`
	Description     string   `json:"descriptionZh"`
	Credits         string   `json:"credits"`
	Tags            []string `json:"tags"`
	Vendor          string   `json:"vendor"`
	IsDefault       bool     `json:"isDefault"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	MaxAllowedSize  int64    `json:"maxAllowedSize"`
	Disabled        bool     `json:"disabled"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsReason  bool     `json:"supportsReasoning"`
	SupportsTool    bool     `json:"supportsToolCall"`
	OnlyReasoning   bool     `json:"onlyReasoning"`
	Reasoning       struct {
		Effort             string   `json:"effort"`
		Summary            string   `json:"summary"`
		DefaultEffort      string   `json:"defaultEffort"`
		CanDisableThinking bool     `json:"canDisableThinking"`
		SupportedEfforts   []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// resolvedID returns the model ID of the entry (with a fallback chain).
func (e dynModelEntry) resolvedID() string {
	for _, candidate := range []string{e.ID, e.ModelID, e.Model} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return strings.TrimSpace(e.Name)
}

// toModelInfo maps upstream entries to normalized metadata.
//
// This is the **single source of truth** for dynEntry → ModelInfo: mapping rules for both realms must be consistent,
// otherwise the same model will present different window values under CN and Global.
//
// Windows and tiers are passed through as-is from upstream values (may be 0 / empty here), and filled in by applyCatalogFallbacks
// per realm — filling in requires the realm while mapping does not, separating responsibilities makes it clearer.
func (e dynModelEntry) toModelInfo() ModelInfo {
	id := e.resolvedID()
	name := strings.TrimSpace(e.Name)
	if name == "" {
		name = id
	}
	return ModelInfo{
		ID:                 id,
		Name:               name,
		Description:        strings.TrimSpace(e.Description),
		ContextWindow:      e.MaxInputTokens,
		MaxTokens:          e.MaxOutputTokens,
		Efforts:            normalizeEfforts(e.Reasoning.SupportedEfforts),
		DefaultEffort:      core.FirstNonEmpty(e.Reasoning.DefaultEffort, e.Reasoning.Effort),
		Credits:            strings.TrimSpace(e.Credits),
		SupportsImages:     e.SupportsImages,
		SupportsToolCall:   e.SupportsTool,
		SupportsReasoning:  e.SupportsReason,
		CanDisableThinking: e.Reasoning.CanDisableThinking,
		OnlyReasoning:      e.OnlyReasoning,
		IsDefault:          e.IsDefault,
	}
}

// normalizeEfforts deduplicates and preserves order.
func normalizeEfforts(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		effort := strings.ToLower(strings.TrimSpace(item))
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		out = append(out, effort)
	}
	return out
}

// nonChatModel determines whether an entry is a non-chat model (selecting it results in 11102).
//
// Three categories: embedding/completion/code-specific (id prefix), tiny output (not a chat model),
// image generation (tagged).
func nonChatModel(entry dynModelEntry) bool {
	id := strings.ToLower(entry.resolvedID())
	for _, prefix := range []string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	if entry.MaxOutputTokens > 0 && entry.MaxOutputTokens <= 256 {
		return true
	}
	for _, tag := range entry.Tags {
		if strings.EqualFold(strings.TrimSpace(tag), "text-to-image") {
			return true
		}
	}
	return false
}

// FetchModels returns the model list for a specific realm.
//
// Returns cache on failure (even if expired) instead of an error: occasional upstream jitter should not make the entire model list disappear.
// Returns an error if it has never succeeded, leaving the caller to decide whether to expose it to the user.
func (c *Client) FetchModels(region Region) ([]ModelInfo, error) {
	rt := c.rt
	rt.modelMu.Lock()
	entry, hasCache := rt.modelCache[region]
	rt.modelMu.Unlock()

	if hasCache && len(entry.models) > 0 {
		if time.Since(entry.fetchedAt) < modelCacheTTL {
			return entry.models, nil
		}
		// Do not repeatedly hit upstream during the failure cooldown period.
		if time.Since(entry.lastFail) < modelCacheFailWindow {
			return entry.models, nil
		}
	}

	models, errFetch := c.fetchModelsFromUpstream(region)
	if errFetch != nil {
		rt.modelMu.Lock()
		cached := rt.modelCache[region]
		cached.lastFail = time.Now()
		if len(cached.models) == 0 {
			cached.models = entry.models
		}
		rt.modelCache[region] = cached
		rt.modelMu.Unlock()
		if len(cached.models) > 0 {
			return cached.models, nil
		}
		return nil, errFetch
	}

	rt.modelMu.Lock()
	rt.modelCache[region] = modelCacheEntry{models: models, fetchedAt: time.Now()}
	rt.modelMu.Unlock()
	return models, nil
}

// Model catalog cache is attached to the adapter's runtime, isolated by App instance.
//
// Client is throwaway after a single operation, cache cannot be attached to Client; nor can it be package-level,
// Otherwise, the two App instances would share the same directory. The runtime lifecycle matches the adapter,
// which avoids redundant probing while ensuring instance isolation.

// cachedModels only returns the cached catalog without triggering upstream requests (for use in the management page).
func (r *runtime) cachedModels() map[Region][]ModelInfo {
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	out := make(map[Region][]ModelInfo, len(r.modelCache))
	for region, entry := range r.modelCache {
		if len(entry.models) > 0 {
			out[region] = entry.models
		}
	}
	return out
}

// loadCachedModels loads the last cached model catalog from the state file.
//
// Pre-loading the cache on startup ensures /v1/models has content before the first upstream probe completes,
// preventing clients from seeing an empty list momentarily upon startup.
func (r *runtime) loadCachedModels(cached []ModelInfo, realm Region) {
	if len(cached) == 0 {
		return
	}
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	if len(r.modelCache[realm].models) > 0 {
		return
	}
	r.modelCache[realm] = modelCacheEntry{models: cached}
}

// applyCatalogFallbacks fills in windows and effort levels using a lookup chain.
//
// Four-tier lookup chain (window): upstream dynamic value -> static fallback table (catalog_seed) -> default value.
// Effort levels use a three-tier chain: upstream dynamic value -> realm-specific static fallback -> omitted field.
//
// The two chains are intentionally different: windows have a "better overestimate" safety bias (underestimating causes clients to prematurely truncate context),
// whereas effort levels have no safety bias (sending the wrong level results in an upstream 400 rejection), so if an effort level is not found, it is omitted.
func applyCatalogFallbacks(models []ModelInfo, region Region) []ModelInfo {
	seed := loadSeed()
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		if model.ContextWindow <= 0 {
			if cap, okCap := seed.Context[model.ID]; okCap {
				model.ContextWindow = cap.Context
				if model.MaxTokens <= 0 {
					model.MaxTokens = cap.MaxOutput
				}
			}
		}
		if model.ContextWindow <= 0 {
			model.ContextWindow = defaultContextWindow
		}
		// Effort levels: fall back to the static table only if the upstream didn't provide one. If provided by upstream, use it directly and never stack.
		if len(model.Efforts) == 0 {
			if cap, okCap := staticEffortCap(region, model.ID); okCap {
				model.Efforts = cap.Efforts
				if model.DefaultEffort == "" {
					model.DefaultEffort = cap.DefaultEffort
				}
			}
		}
		// defaultEffort must belong to efforts, otherwise the client will send an invalid effort level.
		if model.DefaultEffort != "" && !containsString(model.Efforts, model.DefaultEffort) {
			model.DefaultEffort = ""
		}
		out = append(out, model)
	}
	return out
}

// defaultContextWindow is the ultimate fallback for the window lookup chain.
//
// Upstream models generally have a 1M context; underestimating it causes clients to prematurely truncate conversations.
const defaultContextWindow int64 = 1000000

// LoadCachedModelsFromState preheats the model catalog using the cache in state.json.
//
// The cache structure is defined by core (state is just its persistence container); this function only handles regional preheating
// and avoids duplicating WorkBuddy's field mappings here.
func (r *runtime) loadCachedModelsFromState(cached []core.CachedModel) {
	byRegion := map[Region][]ModelInfo{}
	for _, item := range cached {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		region := NormalizeRegion(item.Realm)
		byRegion[region] = append(byRegion[region], ModelInfo{
			ID:                 item.ID,
			Name:               item.Name,
			ContextWindow:      item.ContextLength,
			MaxTokens:          item.MaxOutputTokens,
			Efforts:            item.Efforts,
			DefaultEffort:      item.DefaultEffort,
			SupportsImages:     item.SupportsImages,
			SupportsToolCall:   item.SupportsTool,
			CanDisableThinking: item.CanDisableThink,
			OnlyReasoning:      item.OnlyReasoning,
			Description:        item.Description,
			Credits:            item.Credits,
		})
	}
	for region, models := range byRegion {
		r.loadCachedModels(models, region)
	}
}

// staticEffortCap queries the static effort fallback table by realm.
//
// **Must be split by realm**: valid effort levels for the same model name differ between the two sides
// (deepseek-v4.1-flash is low/high/max in CN, but only high in global),
// mixing them up will cause global requests to carry low/max and be rejected by upstream with a 400.
func staticEffortCap(region Region, modelID string) (ModelInfo, bool) {
	seed := loadSeed()
	table := seed.EffortsCN
	if region.IsGlobal() {
		table = seed.EffortsGlobal
	}
	entry, okEntry := table[strings.ToLower(strings.TrimSpace(modelID))]
	if !okEntry {
		return ModelInfo{}, false
	}
	return ModelInfo{Efforts: normalizeEfforts(entry.Efforts), DefaultEffort: entry.Default}, true
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// sortModels sorts by ID to ensure stable /v1/models output.
func sortModels(models []ModelInfo) {
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
}

// This file implements upstream probing of the model catalog.
//
// Probing paths differ between CN and global, and **both use concurrent multiplexing + union merging**:
//   - CN: /v3/config (IDE UA) + enterprise model catalog;
//   - global: /v3/config (IDE UA) + /v3/config (CLI UA) + enterprise model catalog family.
//
// Neither/all paths are indispensable: actual tests show that lists returned by IDE UA and CLI UA **each contain unique models**
// (IDE has o4-mini/enhance-1.0, CLI has the deepseek family); any single path will miss models.
//
// Merging strategy: v3 entries take priority (richer fields), and enterprise endpoints only supplement IDs missing from v3.
// A failure in one path degrades to the other; an error is returned only if all fail.

// v3ConfigResponse is the response body for /v3/config.
//
// Note: doJSON has already unwrapped one envelope layer (code / msg / data),
// so this corresponds to the content of data, without the code field.
type v3ConfigResponse struct {
	Models []dynModelEntry `json:"models"`
	// ProductFeaturesConfig houses the "trial model banner": upstream places time-limited trial models
	// here rather than **in models**, but those models are actually callable.
	ProductFeaturesConfig struct {
		ModelTrialBanner struct {
			Banners []struct {
				ModelID       string `json:"modelId"`
				TargetModelID string `json:"targetModelId"`
			} `json:"banners"`
		} `json:"ModelTrialBanner"`
	} `json:"productFeaturesConfig"`
}

// enterpriseModelsResponse is the response body for the enterprise model catalog (also unwrapped from one envelope layer).
type enterpriseModelsResponse struct {
	// Agents provide the whitelist and order of models (the models list for the cli agent).
	Agents []struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	} `json:"agents"`
	// Models is the source of fields, retrieved by id.
	Models []dynModelEntry `json:"models"`
}

// fetchModelsFromUpstream concurrently probes the model catalog for a given realm.
func (c *Client) fetchModelsFromUpstream(region Region) ([]ModelInfo, error) {
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}

	type probeResult struct {
		// primary being true means this route is the v3 directory (more complete fields, prioritized during merging).
		primary bool
		models  []ModelInfo
		err     error
	}
	var (
		waitGroup sync.WaitGroup
		mu        sync.Mutex
		results   []probeResult
	)

	record := func(primary bool, models []ModelInfo, err error) {
		mu.Lock()
		results = append(results, probeResult{primary: primary, models: models, err: err})
		mu.Unlock()
	}

	// /v3/config (IDE UA): shared by both domains, most complete fields.
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		models, errProbe := c.probeV3Config(region, codeBuddyIDEUA)
		record(true, models, errProbe)
	}()

	if region.IsGlobal() {
		// /v3/config (CLI UA): Global only. Exclusive DeepSeek models are only available on this route.
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			models, errProbe := c.probeV3Config(region, codeBuddyCLIUA)
			record(true, models, errProbe)
		}()
	}

	// Enterprise model directory: fills in IDs missing from v3.
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		models, errProbe := c.probeEnterpriseModels(region)
		record(false, models, errProbe)
	}()

	waitGroup.Wait()

	// First merge the two v3 routes (IDE result first, authoritative fields), then use the enterprise endpoint to fill in missing ones.
	var primaryModels, fallbackModels []ModelInfo
	var firstErr error
	for _, result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		if result.primary {
			primaryModels = append(primaryModels, result.models...)
		} else {
			fallbackModels = append(fallbackModels, result.models...)
		}
	}

	merged := dedupeModels(primaryModels)
	// The enterprise endpoint only fills in IDs not present in v3 (does not overwrite existing fields).
	for _, model := range fallbackModels {
		if !containsModelID(merged, model.ID) {
			merged = append(merged, model)
		}
	}
	if len(merged) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("probe models for %s: %w", region, firstErr)
		}
		return nil, fmt.Errorf("probe models for %s: no models returned", region)
	}

	merged = filterChatModels(merged)
	if len(merged) == 0 {
		return nil, fmt.Errorf("probe models for %s: all entries filtered as non-chat", region)
	}
	merged = applyCatalogFallbacks(merged, region)
	sortModels(merged)
	return merged, nil
}

// probeCredential gets a credential for probing.
func (c *Client) probeCredential(region Region) *Credential {
	if cred := c.rt.hostProbeCredential(region); cred != nil {
		return cred
	}
	return &Credential{}
}

// probeV3Config fetches /v3/config and parses the model list.
func (c *Client) probeV3Config(region Region, userAgent string) ([]ModelInfo, error) {
	// Probing requires a credential: without one, upstream requests cannot be made.
	cred := c.probeCredential(region)
	if cred == nil {
		return nil, fmt.Errorf("no credential available for model probe")
	}
	endpoints := GetEndpoints(region)
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodGet, endpoints.ChatBase+v3ConfigPath, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build v3 config request: %w", errReq)
	}
	c.applyV3ConfigHeaders(req, cred, userAgent)

	data, errDo := c.doJSONWithLimit(req, configBodyLimit)
	if errDo != nil {
		return nil, errDo
	}

	var payload v3ConfigResponse
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode v3 config: %w", errUnmarshal)
	}
	models := make([]ModelInfo, 0, len(payload.Models))
	for _, entry := range payload.Models {
		if entry.Disabled || nonChatModel(entry) {
			continue
		}
		models = append(models, entry.toModelInfo())
	}
	// Trial model banner: actually callable but not in data.models, metadata is inherited from target,
	// but billing and marketing fields must be cleared (they describe the state "after official promotion").
	for _, banner := range payload.ProductFeaturesConfig.ModelTrialBanner.Banners {
		trialID := strings.TrimSpace(banner.ModelID)
		if trialID == "" || containsModelID(models, trialID) {
			continue
		}
		targetID := strings.TrimSpace(banner.TargetModelID)
		trial := ModelInfo{ID: trialID, Name: trialID}
		if base, okBase := findModel(models, targetID); okBase {
			trial = base
			trial.ID = trialID
			trial.Name = trialID
			trial.Credits = ""
		}
		models = append(models, trial)
	}
	return models, nil
}

// probeEnterpriseModels fetches the enterprise model catalog.
func (c *Client) probeEnterpriseModels(region Region) ([]ModelInfo, error) {
	cred := c.probeCredential(region)
	if cred == nil {
		return nil, fmt.Errorf("no credential available for model probe")
	}
	endpoints := GetEndpoints(region)
	paths := []string{enterpriseModelsPathCN}
	if region.IsGlobal() {
		paths = []string{enterpriseModelsPathGlobal, enterpriseModelsPathCN}
	}

	var lastErr error
	for _, path := range paths {
		req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodGet, endpoints.ChatBase+path, nil)
		if errReq != nil {
			lastErr = errReq
			continue
		}
		c.applyV3ConfigHeaders(req, cred, codeBuddyIDEUA)

		data, errDo := c.doJSONWithLimit(req, configBodyLimit)
		if errDo != nil {
			lastErr = errDo
			// Switch to the next path only on 404 (other errors cannot be recovered by switching paths).
			if upstreamErr, okErr := errDo.(*core.Failure); okErr && upstreamErr.Kind == core.FailureKindNotFound {
				continue
			}
			return nil, errDo
		}

		var payload enterpriseModelsResponse
		if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
			lastErr = fmt.Errorf("decode enterprise models: %w", errUnmarshal)
			continue
		}
		return parseEnterpriseModels(payload), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("enterprise models endpoint is unavailable")
}

// parseEnterpriseModels parses the enterprise catalog according to "agents whitelist order + models field source".
//
// The whitelist order is intentional: upstream uses it to express the recommended sorting of models, losing the order means losing the product intent.
func parseEnterpriseModels(payload enterpriseModelsResponse) []ModelInfo {
	byID := make(map[string]dynModelEntry, len(payload.Models))
	for _, entry := range payload.Models {
		if id := entry.resolvedID(); id != "" {
			byID[id] = entry
		}
	}

	order := make([]string, 0, len(byID))
	seen := make(map[string]bool, len(byID))
	for _, agent := range payload.Agents {
		if !strings.EqualFold(strings.TrimSpace(agent.Name), "cli") {
			continue
		}
		for _, id := range agent.Models {
			trimmed := strings.TrimSpace(id)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			order = append(order, trimmed)
		}
	}
	// Fall back to the original order of models when there is no agent whitelist.
	if len(order) == 0 {
		for _, entry := range payload.Models {
			if id := entry.resolvedID(); id != "" && !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
		}
	}

	out := make([]ModelInfo, 0, len(order))
	for _, id := range order {
		entry, okEntry := byID[id]
		if !okEntry {
			// Present in whitelist but missing in field table: only ID is available, leave the window to the lookup chain fallback.
			out = append(out, ModelInfo{ID: id, Name: id})
			continue
		}
		if entry.Disabled || nonChatModel(entry) {
			continue
		}
		out = append(out, entry.toModelInfo())
	}
	return out
}

// dedupeModels deduplicates by ID, keeping the first occurring entry.
func dedupeModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, model)
	}
	return out
}

// filterChatModels removes models unavailable for chat.
func filterChatModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		// Entries output by tiny are already filtered at the upstream entry level, add another fallback layer here:
		// Entries parsed by dynamic fallback lack original tags and can only be judged by the output limit.
		if model.MaxTokens > 0 && model.MaxTokens <= 256 {
			continue
		}
		out = append(out, model)
	}
	return out
}

func containsModelID(models []ModelInfo, id string) bool {
	_, okFound := findModel(models, id)
	return okFound
}

func findModel(models []ModelInfo, id string) (ModelInfo, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	return ModelInfo{}, false
}
