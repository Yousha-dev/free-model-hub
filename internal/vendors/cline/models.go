package cline

// This file implements Cline's model manifest.
//
// Upstream has an **unauthenticated** recommended models endpoint that returns three arrays:
// recommended / free / clinePass.
// Actual testing shows that free models' tags may be empty, so "is free" must be determined by **which array it is in**,
// and relying solely on tags would misjudge free models as paid.
//
// Fall back to the built-in fallback manifest when detection fails: having no models registered is worse than registering a potentially outdated manifest
// (the client would not see any models).

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
)

// modelsHTTPTimeout is the request timeout limit for the model manifest.
const modelsHTTPTimeout = 15 * time.Second

// Model represents a Cline upstream model.
type Model struct {
	// ID is the model identifier, in the form provider/model (upstream requires a slash).
	ID string
	// Name is the display name.
	Name string
	// Description is the description.
	Description string
	// ContextWindow is the context window.
	ContextWindow int64
	// MaxOutputTokens is the maximum output tokens.
	MaxOutputTokens int64
	// Cost is the billing tier: free / pass.
	Cost string
	// SupportsTools reports whether tool calling is supported.
	SupportsTools bool
	// IsReasoning reports whether it is a reasoning model.
	IsReasoning bool
}

// Billing tier constants.
const (
	CostFree = "free"
	CostPass = "pass"
)

// FetchModels pulls the upstream recommended model manifest (unauthenticated).
func FetchModels(ctx context.Context, baseURL string) ([]Model, error) {
	endpoint := BaseURL(baseURL) + ModelsPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build models request: %w", errReq)
	}
	req.Header.Set("Accept", "application/json")

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
	models, errParse := parseModels(body)
	if errParse != nil {
		return nil, errParse
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models response contained no usable entries")
	}
	return models, nil
}

// rawModel is the raw format of an upstream model entry.
//
// Field names are **upstream camelCase as-is**, and fields appearing in different arrays vary slightly,
// so we collect all of them and then normalize.
type rawModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	DisplayName   string   `json:"displayName"`
	Description   string   `json:"description"`
	ContextWindow int64    `json:"contextWindow"`
	MaxTokens     int64    `json:"maxTokens"`
	MaxOutput     int64    `json:"maxOutputTokens"`
	Tags          []string `json:"tags"`
}

// parseModels parses the three upstream arrays and normalizes them.
func parseModels(body []byte) ([]Model, error) {
	var payload struct {
		Recommended []rawModel `json:"recommended"`
		Free        []rawModel `json:"free"`
		ClinePass   []rawModel `json:"clinePass"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("models response is not valid JSON: %w", errUnmarshal)
	}

	// Deduplication key uses the model ID: the same model may appear in multiple arrays simultaneously.
	seen := map[string]bool{}
	out := make([]Model, 0, len(payload.Recommended)+len(payload.Free)+len(payload.ClinePass))

	appendAll := func(entries []rawModel, cost string, isFreeArray bool) {
		for _, entry := range entries {
			id := strings.TrimSpace(entry.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			model := Model{
				ID:              id,
				Name:            core.FirstNonEmpty(entry.DisplayName, entry.Name, id),
				Description:     strings.TrimSpace(entry.Description),
				ContextWindow:   entry.ContextWindow,
				MaxOutputTokens: core.FirstNonZero(entry.MaxOutput, entry.MaxTokens),
				Cost:            cost,
				SupportsTools:   true,
			}
			// "Is free" is determined by the array: actual testing shows tags in the free array may be empty,
			// and relying solely on tags would misjudge free models as paid.
			if isFreeArray || hasFreeTag(entry.Tags) {
				model.Cost = CostFree
			}
			model.IsReasoning = hasReasoningTag(entry.Tags)
			out = append(out, model)
		}
	}

	// The order matters: free enters the table first, so models with the same name default to the "free" tier
	// (users care more about whether it is available for free).
	appendAll(payload.Free, CostFree, true)
	appendAll(payload.ClinePass, CostPass, false)
	appendAll(payload.Recommended, CostPass, false)
	return out, nil
}

// hasFreeTag reports whether FREE is marked in tags.
func hasFreeTag(tags []string) bool {
	for _, tag := range tags {
		if strings.EqualFold(strings.TrimSpace(tag), "free") {
			return true
		}
	}
	return false
}

// hasReasoningTag reports whether reasoning is marked in tags.
func hasReasoningTag(tags []string) bool {
	for _, tag := range tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "reasoning", "thinking":
			return true
		}
	}
	return false
}

// bundledModels is the built-in fallback model manifest.
//
// Source: cline2api's builtinModels constant (offline fallback), not constructed out of thin air.
// The truly available set is subject to the real-time manifest fetched via "refresh models".
func bundledModels() []Model {
	return []Model{
		{ID: "cline-free/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Cost: CostFree, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-free/gemini-3.8-flash", Name: "Gemini 3.8 Flash", Cost: CostFree, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-free/mimo-v2.6-flash", Name: "MiMo v2.6 Flash", Cost: CostFree, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-pass/glm-5.3", Name: "GLM-5.3", Cost: CostPass, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-pass/kimi-k3", Name: "Kimi K3", Cost: CostPass, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-pass/deepseek-v4-pro", Name: "DeepSeek V4 Pro", Cost: CostPass, ContextWindow: 200000, MaxOutputTokens: 32768, SupportsTools: true, IsReasoning: true},
		{ID: "cline-pass/qwen3.8-max", Name: "Qwen3.8 Max", Cost: CostPass, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
	}
}
