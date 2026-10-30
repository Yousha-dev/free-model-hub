package opencodezen

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

// This file implements model list retrieval for OpenCode ZEN.
//
// ZEN's model list is **completely dynamic**: upstream GET {base}/v1/models returns available models
// (in practice, only ID, no metadata). Official context/output/reasoning tiers for display come from
// the models.dev directory, merged by ID. The upstream gateway project has no
// hardcoded list, and this plugin does not compile one either—a compiled list, once inconsistent with upstream, would
// cause users to encounter models that fail to call.
//
// The trade-off is that probing requires a valid API key. When probing fails, return an empty list instead of a fallback table:
// an empty list only causes /v1/models to have a few fewer entries (the page prompts to refresh), whereas a fabricated
// list leads users to think a model is available.

// modelsHTTPTimeout is the timeout limit for model list requests.
const modelsHTTPTimeout = 30 * time.Second

// Model is an upstream ZEN model.
type Model struct {
	// ID is the model identifier (upstream as-is, e.g., claude-opus-5 / gpt-6-astra).
	ID string
	// Name is the display name (falls back to ID when upstream doesn't provide one).
	Name string
	// Description is the model description (from the models.dev directory).
	Description string
	// ContextWindow is the context window (provided only if upstream supplies it).
	ContextWindow int64
	// MaxOutputTokens is the output limit (provided only if upstream supplies it).
	MaxOutputTokens int64
	// SupportsTools reports whether tool calling is supported.
	SupportsTools bool
	// IsReasoning reports whether it is a reasoning model.
	IsReasoning bool
	// SupportsImages reports whether image input is supported (from the models.dev directory).
	SupportsImages bool
	// Efforts are optional reasoning tiers (from the models.dev directory).
	Efforts []string
	// Credits is the billing display text.
	//
	// Record as zero multiplier (0.00X) only when the upstream directory explicitly states "input and output unit prices are 0"; price is not
	// a multiplier, models with prices do not enter the multiplier column (display "—"), avoiding disguising dollar prices as discount multipliers.
	Credits string
}

// FetchModels fetches the upstream model list.
func FetchModels(ctx context.Context, baseURL, apiKey string) ([]Model, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("api key is required to list models")
	}
	endpoint := ModelsBaseURLFor(baseURL) + ModelsPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build models request: %w", errReq)
	}
	ApplyModelsHeaders(req, apiKey)

	resp, errDo := hostbridge.Client(ctx, modelsHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch models: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read models response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	return parseModels(body)
}

// parseModels parses the upstream model list.
//
// Upstream returns an OpenAI-style {"data":[{"id":...}]}, but historically, bare arrays
// and {"models":[...]} have also appeared, so all three are accepted.
//
// **Do not filter by name**: The list only reflects "which models upstream has", and whether it is free is determined at request time by
// the unit price in the models.dev directory (see isFreeTierModel). Filtering out zero-priced models without "-free"
// by name would miss official default models like big-pickle.
func parseModels(body []byte) ([]Model, error) {
	type rawModel struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		DisplayName   string `json:"display_name"`
		ContextLength int64  `json:"context_length"`
		ContextWindow int64  `json:"context_window"`
		MaxOutput     int64  `json:"max_output_tokens"`
		MaxTokens     int64  `json:"max_tokens"`
		// Capability fields have different names across formats; collect all of them and normalize them below.
		SupportsTools   *bool `json:"supports_tools"`
		ToolCall        *bool `json:"tool_call"`
		FunctionCalling *bool `json:"function_calling"`
		IsReasoning     *bool `json:"is_reasoning"`
		Reasoning       *bool `json:"reasoning"`
	}

	var entries []rawModel
	// Format 1: {"data":[...]}
	var wrapper struct {
		Data   []rawModel `json:"data"`
		Models []rawModel `json:"models"`
	}
	if errWrapper := json.Unmarshal(body, &wrapper); errWrapper == nil {
		switch {
		case len(wrapper.Data) > 0:
			entries = wrapper.Data
		case len(wrapper.Models) > 0:
			entries = wrapper.Models
		}
	}
	// Format 2: Bare array.
	if len(entries) == 0 {
		if errArray := json.Unmarshal(body, &entries); errArray != nil && !json.Valid(body) {
			return nil, fmt.Errorf("models response is not valid JSON: %w", errArray)
		}
	}

	out := make([]Model, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		contextWindow := entry.ContextWindow
		if contextWindow == 0 {
			contextWindow = entry.ContextLength
		}
		maxOutput := entry.MaxOutput
		if maxOutput == 0 {
			maxOutput = entry.MaxTokens
		}
		name := core.FirstNonEmpty(entry.DisplayName, entry.Name, id)
		out = append(out, Model{
			ID:              id,
			Name:            name,
			ContextWindow:   contextWindow,
			MaxOutputTokens: maxOutput,
			SupportsTools:   boolFrom(entry.SupportsTools, entry.ToolCall, entry.FunctionCalling),
			IsReasoning:     boolFrom(entry.IsReasoning, entry.Reasoning),
		})
	}
	return out, nil
}

// boolFrom returns the first non-nil boolean value (false if all are nil).
func boolFrom(values ...*bool) bool {
	for _, value := range values {
		if value != nil {
			return *value
		}
	}
	return false
}

// This file implements model metadata enrichment for OpenCode ZEN.
//
// Upstream GET {base}/v1/models only returns model IDs (in practice, no metadata fields at all), while
// the data source for the official OpenCode client to display "context window / output limit / reasoning tiers"
// is the public models.dev directory (https://models.dev/api.json): where models under provider
// "opencode" correspond one-to-one with ZEN /v1/models and carry complete metadata.
//
// Strategy:
//   - ZEN /v1/models still determines "which models are available" (dynamic detection, requires a key);
//   - models.dev only provides "what they look like" (metadata, public, no authentication required);
//   - Catalog fetch failure does not affect the model list: original fields are retained, and the adapter uses fallback values.
//   - Catalog is cached by TTL: ModelsForAuth triggers detection per credential, and without caching,
//     every credential refresh would fetch the multi-MB catalog again.

const (
	// modelsDevURL is the public model catalog for models.dev.
	modelsDevURL = "https://models.dev/api.json"
	// modelsDevTimeout is the timeout limit for catalog requests.
	modelsDevTimeout = 15 * time.Second
	// modelsDevCacheTTL is the cache duration for successful results.
	modelsDevCacheTTL = time.Hour
	// modelsDevNegativeTTL is the cache duration for failed results: detection is triggered per credential,
	// and a short negative cache prevents upstream jitter from turning failures into a request storm for the catalog API.
	modelsDevNegativeTTL = 5 * time.Minute
	// modelsDevProvider is the provider ID corresponding to ZEN in the catalog.
	modelsDevProvider = "opencode"
)

// modelsDevEntry is a single model entry in the models.dev catalog.
type modelsDevEntry struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	Reasoning        bool   `json:"reasoning"`
	ToolCall         bool   `json:"tool_call"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Limit struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	// Cost is the price from models.dev (USD per million tokens, not a multiplier). Pointers are used to distinguish
	// between "no cost block" and "all costs are zero" — the former cannot be treated as free.
	Cost *modelsDevCost `json:"cost"`
}

// modelsDevCost is the unit price from models.dev (USD per million tokens).
//
// It is not a multiplier: converting it to a discount multiplier is meaningless, it is only used to determine "whether it is free".
type modelsDevCost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// modelsDevCatalog is the model ID to entry table under the provider.
type modelsDevCatalog map[string]modelsDevEntry

// modelsDevCache is the catalog cache (success and failure timed separately).
//
// Belongs to the adapter instance rather than package-level variables: multiple App instances cache independently without affecting each other.
// Zero value is usable; a nil receiver means no caching (one-off calls hit the upstream directly).
type modelsDevCache struct {
	mu         sync.Mutex
	catalog    modelsDevCatalog
	validUntil time.Time
	failedNext time.Time
}

// fetch fetches (or hits the cache for) the models.dev catalog.
func (c *modelsDevCache) fetch(ctx context.Context) (modelsDevCatalog, error) {
	now := time.Now()
	if c != nil {
		c.mu.Lock()
		if c.catalog != nil && now.Before(c.validUntil) {
			catalog := c.catalog
			c.mu.Unlock()
			return catalog, nil
		}
		failing := now.Before(c.failedNext)
		c.mu.Unlock()
		if failing {
			return nil, fmt.Errorf("models.dev catalog recently failed; negative cache")
		}
	}

	catalog, errFetch := loadModelsDevCatalog(ctx)
	if c != nil {
		now = time.Now()
		c.mu.Lock()
		if errFetch != nil {
			c.failedNext = now.Add(modelsDevNegativeTTL)
		} else {
			c.catalog = catalog
			c.validUntil = now.Add(modelsDevCacheTTL)
		}
		c.mu.Unlock()
	}
	return catalog, errFetch
}

// loadModelsDevCatalog initiates a catalog request and parses the provider corresponding to ZEN.
func loadModelsDevCatalog(ctx context.Context) (modelsDevCatalog, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build models.dev request: %w", errReq)
	}
	req.Header.Set("Accept", "application/json")

	resp, errDo := hostbridge.Client(ctx, modelsDevTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch models.dev: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read models.dev response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models.dev status %d", resp.StatusCode)
	}

	return parseModelsDevCatalog(body)
}

// parseModelsDevCatalog parses the catalog response and extracts the provider corresponding to ZEN.
func parseModelsDevCatalog(body []byte) (modelsDevCatalog, error) {
	var payload map[string]struct {
		Models modelsDevCatalog `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("parse models.dev response: %w", errUnmarshal)
	}
	provider, okProvider := payload[modelsDevProvider]
	if !okProvider || len(provider.Models) == 0 {
		// The ZEN provider not being in the catalog yet is a recoverable state: return an empty table instead of an error,
		// allowing the caller to continue with the fallback as "no metadata".
		return modelsDevCatalog{}, nil
	}
	return provider.Models, nil
}

// enrich enriches model metadata using the models.dev catalog.
//
// Fills in only when fields are empty (if ZEN /v1/models returns metadata in the future, it takes precedence); when the catalog is unavailable,
// only logs an info message, and the model list itself is returned as usual.
func (c *modelsDevCache) enrich(ctx context.Context, models []Model) {
	catalog, errCatalog := c.fetch(ctx)
	if errCatalog != nil {
		logger.Info("opencodezen: models.dev catalog unavailable, keep fallback metadata: %v", errCatalog)
		return
	}
	applyModelsDevCatalog(models, catalog)
}

// freeOnly keeps only free-tier models (catalog determines unit price is 0).
//
// This provider is positioned to "access free quota": the upstream /v1/models returns dozens of paid models
// and test models used for debugging simultaneously, and registering all of them would bloat the host's model list to hundreds of entries,
// and paid models will inevitably return 402 in this plugin. Therefore, filter by catalog unit price: models not in the catalog
// (such as upstream test models) are not displayed at all — without price data, it cannot be determined whether they belong to the free tier.
//
// When the catalog as a whole is unavailable, **do not filter**: it is better to display more than to let all free models disappear
// due to a single catalog jitter. In this case, isFreeTierModel falls back to determination by name.
func freeOnly(models []Model, catalog modelsDevCatalog) []Model {
	if len(catalog) == 0 {
		return models
	}
	out := make([]Model, 0, len(models))
	for _, model := range models {
		if free, known := catalogIsFree(catalog, model.ID); known && free {
			out = append(out, model)
		}
	}
	return out
}

// applyModelsDevCatalog merges catalog entries into the model manifest (pure function, fills empty fields only).
func applyModelsDevCatalog(models []Model, catalog modelsDevCatalog) {
	for i := range models {
		entry, okEntry := catalog[models[i].ID]
		if !okEntry {
			continue
		}
		model := &models[i]
		if model.ContextWindow == 0 && entry.Limit.Context > 0 {
			model.ContextWindow = entry.Limit.Context
		}
		if model.MaxOutputTokens == 0 && entry.Limit.Output > 0 {
			model.MaxOutputTokens = entry.Limit.Output
		}
		if !model.SupportsTools && entry.ToolCall {
			model.SupportsTools = true
		}
		if !model.IsReasoning && entry.Reasoning {
			model.IsReasoning = true
		}
		if len(model.Efforts) == 0 {
			model.Efforts = effortsFromModelsDev(entry)
		}
		if !model.SupportsImages {
			model.SupportsImages = hasImageInput(entry.Modalities.Input)
		}
		if (model.Name == "" || model.Name == model.ID) && entry.Name != "" {
			model.Name = entry.Name
		}
		if model.Description == "" && entry.Description != "" {
			model.Description = entry.Description
		}
		// The free signal comes from the catalog's price being all 0; only this judgment is made, and the price itself does not enter the multiplier column.
		// Displayed as a numeric multiplier "0.00X" instead of "Free": this column in the console is the fee multiplier,
		// and mixing Chinese characters in the same column would make sorting/comparison meaningless.
		if model.Credits == "" && entry.Cost != nil && entry.Cost.Input == 0 && entry.Cost.Output == 0 {
			model.Credits = "0.00X"
		}
	}
}

// effortsFromModelsDev extracts effort tiers from the catalog's reasoning_options.
//
// type=effort values are the reasoning levels officially displayed (e.g. low/high/max), keeping the order.
func effortsFromModelsDev(entry modelsDevEntry) []string {
	var efforts []string
	seen := make(map[string]bool)
	for _, option := range entry.ReasoningOptions {
		if option.Type != "effort" {
			continue
		}
		for _, value := range option.Values {
			trimmed := strings.TrimSpace(value)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			efforts = append(efforts, trimmed)
		}
	}
	return efforts
}

// hasImageInput determines whether the directory's input modality contains images.
func hasImageInput(modalities []string) bool {
	for _, modality := range modalities {
		if strings.EqualFold(modality, "image") {
			return true
		}
	}
	return false
}
