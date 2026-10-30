package codearts

// Model catalog: provides the list of CodeArts built-in and benefit models.
// The API format relies on the response of /v1/model/builtin (see comments on builtinCatalog).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	// SupportsImages comes from supports_images in the built-in catalog (only vision models have this bit).
	SupportsImages bool `json:"supports_images,omitempty"`
	// Credits is the base tier text of the upstream credit[].ratio_display (e.g., "0.7x").
	// An empty string means the upstream didn't provide it—it cannot be treated as 0 (0 means "free" in the console).
	Credits string `json:"credits,omitempty"`
	// Benefit marks that the model uses the daily free benefit quota (request header maas_type: benefit).
	Benefit bool `json:"benefit,omitempty"`
}

// gatewayEnvelope is the unified envelope for the benefit gateway API.
type gatewayEnvelope struct {
	ErrorCode string          `json:"error_code"`
	ErrorMsg  string          `json:"error_msg"`
	Result    json.RawMessage `json:"result"`
}

// FetchBenefitModels fetches the model list for the daily free benefit channel.
//
// The upstream splits "daily free quota" and "subscription points" into two channels: the same model name
// may appear in both channels, distinguished only by the request header maas_type. Which models can use the free quota is entirely determined by this list
// (models in the points channel are not supported by this plugin).
func FetchBenefitModels(ctx context.Context, cred *Credential) ([]Model, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return nil, nil
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, BenefitHost+EpBenefitConfig, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 20*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts benefit models failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	var envelope gatewayEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode benefit gateway config: %w", err)
	}
	if envelope.ErrorCode != "" && envelope.ErrorCode != "0000" {
		return nil, fmt.Errorf("benefit gateway %s: %s", envelope.ErrorCode, envelope.ErrorMsg)
	}
	var config struct {
		Models []struct {
			ModelID       string `json:"model_id"`
			ModelName     string `json:"model_name"`
			ContextWindow int    `json:"context_window"`
			MaxTokens     int    `json:"max_tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(envelope.Result, &config); err != nil {
		return nil, fmt.Errorf("decode benefit model list: %w", err)
	}

	models := make([]Model, 0, len(config.Models))
	for _, item := range config.Models {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}
		contextWindow, maxTokens := applyLimitHints(id, item.ContextWindow, item.MaxTokens)
		models = append(models, Model{
			ID:            id,
			Name:          core.FirstNonEmpty(item.ModelName, id),
			Description:   "CodeArts Daily Free Benefit Quota Model",
			ContextWindow: contextWindow,
			MaxTokens:     capOutputTokens(maxTokens),
			SupportsTools: true,
			Benefit:       true,
		})
	}
	return models, nil
}

// maxOutputTokenCap is the hard cap on single-output quota from the CodeArts upstream.
//
// Empirical finding: the **value** of max_tokens participates in upstream pre-validation—65536 passes, while 65537 causes the entire request
// to return InferHub.001001005.400 (independent of the model and prompt length, it's a platform-level validation). In the catalog,
// GLM-5.2 is written as 131072 and deepseek-v4 as 393216. If copied as-is into /v1/models, the client's
// entire request will be rejected, so both the catalog and request body must be clamped.
const maxOutputTokenCap = 65536

// capOutputTokens clamps the output upper limit described by the catalog to the upstream hard cap; 0 means the catalog didn't provide it, keeping it as-is.
func capOutputTokens(declared int) int {
	if declared > maxOutputTokenCap {
		return maxOutputTokenCap
	}
	return declared
}

// creditTier is a billing tier of upstream credit[] (segmented by input length).
type creditTier struct {
	InputFrom    *int   `json:"input_from"`
	Status       string `json:"status"`
	RatioDisplay string `json:"ratio_display"`
}

// baseCreditDisplay retrieves the official multiplier text for the base tier.
//
// The upstream provides multiple tiers based on input length (one each for input_from 0 and 32001, with -1 meaning no upper limit),
// whereas the console's multiplier column has only one cell, so picking the base tier matches the official badge. Tiers with explicit status not "0"
// are skipped (missing fields are treated as active); if there is no base tier, it falls back to the first readable tier instead of returning empty. The text is kept
// as-is: ratio and ratio_display are not in the same dimension and cannot be converted into each other.
func baseCreditDisplay(tiers []creditTier) string {
	fallback := ""
	for _, tier := range tiers {
		if status := strings.TrimSpace(tier.Status); status != "" && status != "0" {
			continue
		}
		display := strings.TrimSpace(tier.RatioDisplay)
		if display == "" {
			continue
		}
		if tier.InputFrom == nil || *tier.InputFrom == 0 {
			return display
		}
		if fallback == "" {
			fallback = display
		}
	}
	return fallback
}

// builtinEntry is a builtinModels[] entry of /v1/model/builtin.
type builtinEntry struct {
	ModelID        string       `json:"model_id"`
	ModelName      string       `json:"model_name"`
	ModelDesc      string       `json:"model_desc"`
	Enable         *bool        `json:"enable"`
	SupportsImages bool         `json:"supports_images"`
	Credit         []creditTier `json:"credit"`
}

// builtinCatalog is compatible with two generations of response formats.
//
// Empirical finding (2026-09-27) the response is {"count":6,"builtinModels":[...]}. Early implementations only recognized
// the top-level models[], so every round failed to parse and silently fell back to the built-in list—the console persistently displayed
// a hardcoded old catalog. models[] is kept as a compatibility branch and is no longer used as the primary judgment criterion.
type builtinCatalog struct {
	BuiltinModels []builtinEntry `json:"builtinModels"`
	Models        []builtinEntry `json:"models"`
}

// bundledCatalog is the fallback list when there is no credential or the catalog fails.
//
// Only "ID → context/output limit + capabilities" are retained: descriptions and multipliers are strictly subject to the online catalog,
// and keeping another copy in the fallback will only silently expire when upstream adjusts prices or renames models. The limits come from agent detail's
// model_parameters (the built-in catalog itself does not provide these two numbers).
var bundledCatalog = []Model{
	{ID: "GLM-5.2", Name: "GLM-5.2", ContextWindow: 202752, MaxTokens: maxOutputTokenCap, SupportsTools: true},
	{ID: "openpangu-2.0-pro", Name: "OpenPangu-2.0-Pro", ContextWindow: 524288, MaxTokens: maxOutputTokenCap, SupportsTools: true},
	{ID: "openpangu-2.0-flash", Name: "OpenPangu-2.0-Flash", ContextWindow: 524288, MaxTokens: maxOutputTokenCap, SupportsTools: true},
	{ID: "glm-5.2-sft-harmony", Name: "GLM-5.2-ArkTS-SPARK", ContextWindow: 202752, MaxTokens: maxOutputTokenCap, SupportsTools: true},
	{ID: "Qwen3-VL-235B", Name: "Qwen3-VL-235B", SupportsTools: true, SupportsImages: true},
	{ID: "kimi-k2.6-VL", Name: "Kimi-K2.6-VL", SupportsTools: true, SupportsImages: true},
}

// bundledModels returns a copy of the fallback list, allowing callers to safely add or remove items.
func bundledModels() []Model {
	return append([]Model(nil), bundledCatalog...)
}

// fallbackLimitIndex is the fallback list's "lowercase ID → {context, output limit}" index.
//
// Queries the table on demand during parsing to avoid rebuilding the fallback list for every catalog entry.
var fallbackLimitIndex = func() map[string][2]int {
	index := make(map[string][2]int, len(bundledCatalog))
	for _, model := range bundledCatalog {
		index[strings.ToLower(model.ID)] = [2]int{model.ContextWindow, model.MaxTokens}
	}
	return index
}()

// applyLimitHints uses the fallback list to supplement missing context/output limits.
//
// Built-in catalogs do not carry these two types of numbers (true values are in agent detail), and the benefit channel may also lack them;
// they are only supplemented on the missing side, and if the upstream actually provides them, the upstream takes precedence.
func applyLimitHints(id string, contextWindow, maxTokens int) (int, int) {
	if contextWindow > 0 && maxTokens > 0 {
		return contextWindow, maxTokens
	}
	hint := fallbackLimitIndex[strings.ToLower(id)]
	if contextWindow <= 0 {
		contextWindow = hint[0]
	}
	if maxTokens <= 0 {
		maxTokens = hint[1]
	}
	return contextWindow, maxTokens
}

// parseBuiltinModels parses the built-in catalog response.
func parseBuiltinModels(body []byte) ([]Model, error) {
	var catalog builtinCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode builtin model catalog: %w", err)
	}
	entries := catalog.BuiltinModels
	if len(entries) == 0 {
		entries = catalog.Models
	}
	models := make([]Model, 0, len(entries))
	for _, entry := range entries {
		// Skip only if enable is explicitly false (treat missing field as available).
		if entry.Enable != nil && !*entry.Enable {
			continue
		}
		id := strings.TrimSpace(core.FirstNonEmpty(entry.ModelID, entry.ModelName))
		if id == "" {
			continue
		}
		contextWindow, maxTokens := applyLimitHints(id, 0, 0)
		models = append(models, Model{
			ID:   id,
			Name: core.FirstNonEmpty(entry.ModelName, id),
			// Descriptions only accept upstream model_desc: the fallback list does not invent copy, keeping both paths consistent.
			Description:    entry.ModelDesc,
			ContextWindow:  contextWindow,
			MaxTokens:      capOutputTokens(maxTokens),
			SupportsTools:  true,
			SupportsImages: entry.SupportsImages,
			Credits:        baseCreditDisplay(entry.Credit),
		})
	}
	return models, nil
}

// FetchModels tries to fetch the model list from upstream.
//
// The built-in catalog (/v1/model/builtin) is the primary criterion: it provides model names, display names, image capabilities, and
// official billing multipliers. It does not carry upper-limit numbers; context and output limits can only be supplemented from the fallback list,
// and are not fabricated when unknown (kept as 0, with the core layer deciding the fallback).
func FetchModels(ctx context.Context, cred *Credential) ([]Model, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return bundledModels(), nil
	}

	// The benefit channel list is fetched only when credentials are present: it is the sole criterion for "which models can use the daily free quota".
	benefitModels, errBenefit := FetchBenefitModels(ctx, cred)
	if errBenefit != nil {
		logger.Debug("codearts: failed to fetch benefit model list: %v", errBenefit)
		benefitModels = nil
	}

	body, errFetch := fetchBuiltinCatalog(ctx, cred)
	return modelsFromCatalog(body, errFetch, benefitModels), nil
}

// fetchBuiltinCatalog fetches the raw response body of the built-in catalog.
func fetchBuiltinCatalog(ctx context.Context, cred *Credential) ([]byte, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, SnapEngineApiHost+EpModelBuiltin, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 15*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("builtin catalog status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// modelsFromCatalog converts the catalog response into a model list: network failures, non-200 status, parsing failures,
// and empty lists all fall back to the fallback list, but are still merged into the benefit channel models.
func modelsFromCatalog(body []byte, errFetch error, benefit []Model) []Model {
	fallback := func() []Model { return mergeBenefitModels(bundledModels(), benefit) }
	if errFetch != nil {
		logger.Debug("codearts: failed to fetch built-in model catalog: %v", errFetch)
		return fallback()
	}
	models, errParse := parseBuiltinModels(body)
	if errParse != nil {
		logger.Debug("codearts: failed to parse built-in model catalog: %v", errParse)
		return fallback()
	}
	if len(models) == 0 {
		return fallback()
	}
	return mergeBenefitModels(models, benefit)
}

// mergeBenefitModels merges benefit channel models into the list, without duplicate additions for existing IDs.
func mergeBenefitModels(base, benefit []Model) []Model {
	seen := make(map[string]bool, len(base))
	for _, model := range base {
		seen[model.ID] = true
	}
	for _, model := range benefit {
		if seen[model.ID] {
			continue
		}
		base = append(base, model)
	}
	return base
}
