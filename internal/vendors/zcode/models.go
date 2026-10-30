package zcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// Model directory.
//
// The authoritative upstream manifest is in client/configs (unauthenticated public endpoint). Currently, there are only two models:
// GLM-5.3 and GLM-5.3-Flash; earlier versions of GLM-5.x / GLM-4.7 have been taken offline from plans. Continuing
// to expose them would cause clients to trigger a 3006 error (model not allowed) when selected. Therefore, we rely on
// configs and only fall back to the built-in manifest if fetching fails.

// Model represents metadata for an available model.
type Model struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	ContextWindow  int    `json:"context_window"`
	MaxTokens      int    `json:"max_tokens"`
	SupportsTools  bool   `json:"supports_tools"`
	SupportsImages bool   `json:"supports_images"`
	IsReasoning    bool   `json:"is_reasoning"`
}

// bundledModels is the built-in manifest used when fetching from configs fails (consistent with the current upstream).
func bundledModels() []Model {
	return []Model{
		{
			ID:            "GLM-5.3",
			Name:          "GLM-5.3",
			Description:   "ZCode flagship model, suitable for complex code and long-context tasks",
			ContextWindow: 1000000,
			MaxTokens:     128000,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:             "GLM-5.3-Flash",
			Name:           "GLM-5.3-Flash",
			Description:    "ZCode high-speed model, supporting image input",
			ContextWindow:  1000000,
			MaxTokens:      128000,
			SupportsTools:  true,
			SupportsImages: true,
			IsReasoning:    true,
		},
	}
}

// FetchModels fetches the current model manifest from upstream, falling back to the built-in manifest on failure.
func FetchModels(ctx context.Context) ([]Model, error) {
	url := ZCodeOrigin + PathClientConfigs + "?app_version=" + ClientAppVersion
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errNew != nil {
		return bundledModels(), nil
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, errDo := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if errDo != nil {
		return bundledModels(), nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil || resp.StatusCode >= 400 {
		return bundledModels(), nil
	}

	models := parseConfigModels(body)
	if len(models) == 0 {
		return bundledModels(), nil
	}
	return models, nil
}

// parseConfigModels extracts builtinModels from the client/configs response.
func parseConfigModels(body []byte) []Model {
	var payload struct {
		Data struct {
			BuiltinModels []struct {
				ModelID             string `json:"modelId"`
				Name                string `json:"name"`
				Description         string `json:"description"`
				ContextWindow       int    `json:"contextWindow"`
				MaxCompletionTokens int    `json:"maxCompletionTokens"`
				Capabilities        struct {
					Vision bool `json:"vision"`
				} `json:"capabilities"`
				Reasoning struct {
					Levels map[string]any `json:"levels"`
				} `json:"reasoning"`
			} `json:"builtinModels"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil
	}

	models := make([]Model, 0, len(payload.Data.BuiltinModels))
	for _, item := range payload.Data.BuiltinModels {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}
		models = append(models, Model{
			ID:             id,
			Name:           core.FirstNonEmpty(item.Name, id),
			Description:    item.Description,
			ContextWindow:  item.ContextWindow,
			MaxTokens:      item.MaxCompletionTokens,
			SupportsTools:  true,
			SupportsImages: item.Capabilities.Vision,
			IsReasoning:    len(item.Reasoning.Levels) > 0,
		})
	}
	return models
}

// NormalizeModelName normalizes the casing of model names (upstream is case-sensitive for model names).
//
// Clients often pass lowercase aliases, so we map them back to the official spelling; unknown names are returned as is,
// letting upstream provide a clear "model does not exist" error rather than being silently rewritten.
func NormalizeModelName(model string) string {
	trimmed := strings.TrimSpace(model)
	switch strings.ToLower(trimmed) {
	case "glm-5.3", "glm-5.3-pro":
		return "GLM-5.3"
	case "glm-5.3-flash":
		return "GLM-5.3-Flash"
	default:
		return trimmed
	}
}
