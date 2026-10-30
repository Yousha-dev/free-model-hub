package minimaxcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
)

// Model is the model metadata supported by MiniMax Code.
type Model struct {
	ID            string
	Name          string
	Description   string
	ContextWindow int64
	MaxOutput     int64
	SupportsTools bool
	IsReasoning   bool
	UpstreamModel string
	Variant       string
}

// bundledModels returns the built-in model list of MiniMax Code (fallback when probing fails).
func bundledModels() []Model {
	return []Model{
		{
			ID:            "minimax-agent",
			Name:          "MiniMax Agent",
			Description:   "General Agent that automatically plans and invokes tools",
			ContextWindow: 128000,
			MaxOutput:     8192,
			SupportsTools: true,
			UpstreamModel: "",
		},
		{
			ID:            "minimax-m3",
			Name:          "MiniMax M3",
			Description:   "Chat mode with faster response",
			ContextWindow: 1000000,
			MaxOutput:     16384,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M3",
		},
		{
			ID:            "minimax-m3-thinking",
			Name:          "MiniMax M3 Thinking",
			Description:   "Deep thinking mode, routing reasoning content via reasoning_content",
			ContextWindow: 1000000,
			MaxOutput:     16384,
			SupportsTools: true,
			IsReasoning:   true,
			UpstreamModel: "MiniMax-M3",
			Variant:       "thinking",
		},
		{
			ID:            "minimax-m3.1-flash",
			Name:          "MiniMax M3.1 Flash",
			Description:   "Next-gen Flash model with 512K context and faster response",
			ContextWindow: 512000,
			MaxOutput:     16384,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M3.1-Flash",
		},
		{
			ID:            "minimax-m3.1-flash-thinking",
			Name:          "MiniMax M3.1 Flash Thinking",
			Description:   "Deep thinking mode for M3.1 Flash",
			ContextWindow: 512000,
			MaxOutput:     16384,
			SupportsTools: true,
			IsReasoning:   true,
			UpstreamModel: "MiniMax-M3.1-Flash",
			Variant:       "thinking",
		},
		{
			ID:            "minimax-m2.7",
			Name:          "MiniMax M2.7",
			Description:   "Previous-generation chat model",
			ContextWindow: 128000,
			MaxOutput:     8192,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M2.7",
		},
	}
}

// FindModel looks up the model configuration based on the model ID requested by the client.
func findModel(modelID string) (Model, bool) {
	clean := strings.ToLower(strings.TrimSpace(modelID))
	// Strip the prefix if present
	if idx := strings.Index(clean, ":"); idx >= 0 {
		clean = clean[idx+1:]
	}
	for _, m := range bundledModels() {
		if strings.EqualFold(m.ID, clean) {
			return m, true
		}
	}
	// Fallback to minimax-agent by default
	return bundledModels()[0], false
}

// FetchModels returns the model list for host registration.
func FetchModels(ctx context.Context, region Region) ([]Model, error) {
	_ = ctx
	_ = region
	return bundledModels(), nil
}

// This file implements "resolving upstream model IDs per account workspace".
//
// Why it is needed: Upstream may rename/deprecate models (observed on 2026-10-25:
// M3.1-Flash in the subscription workspace was renamed from MiniMax-M3.1-Flash-Preview to MiniMax-M3.1-Flash),
// while the built-in catalog is compiled-in—continuing to send old IDs will cause the Agent session
// to reject them with 1406010011 "explicit model is not registered".
// The authoritative source is /config in the account domain (signed request), which lists currently available
// model_ids per account; falls back to the built-in catalog (old behavior) on parsing failure without breaking chats.

const (
	workspaceModelsTTL        = 10 * time.Minute
	workspaceModelsFailWindow = time.Minute
)

// workspaceModel is an available model in the workspace configuration.
type workspaceModel struct {
	ModelID      string
	DisplayName  string
	ContextLimit int64
	Variants     []string
}

type workspaceCacheEntry struct {
	models    []workspaceModel
	fetchedAt time.Time
	lastFail  time.Time
}

// workspaceCache is the account workspace catalog cache.
//
// Must be attached to the adapter instance rather than package-level variables: multiple App instances
// in the same process are independent, with the cache lifecycle bound to the instance (consistent with trae/qoder/workbuddy catalog cache). Zero value is usable.
type workspaceCache struct {
	mu      sync.Mutex
	entries map[string]workspaceCacheEntry
}

// models returns the currently available model list for the account workspace (with TTL and failure cooldown).
//
// Retains the last successful result on failure; returns nil if it has never succeeded, and the caller falls back to the built-in catalog.
func (c *workspaceCache) models(ctx context.Context, cred *Credential, baseURLOverride string) []workspaceModel {
	if cred == nil {
		return nil
	}
	key := workspaceAccountKey(cred, baseURLOverride)
	if c != nil {
		c.mu.Lock()
		entry, okEntry := c.entries[key]
		c.mu.Unlock()
		if okEntry {
			if time.Since(entry.fetchedAt) < workspaceModelsTTL {
				return entry.models
			}
			// Do not repeatedly hit upstream during the failure cooldown period: when quotas/model lists are refreshed per account,
			// upstream jitter should not cause every message to incur an extra timeout penalty.
			if !entry.lastFail.IsZero() && time.Since(entry.lastFail) < workspaceModelsFailWindow {
				return entry.models
			}
		}
	}

	models, errFetch := fetchWorkspaceModels(ctx, cred, baseURLOverride)
	if c == nil {
		if errFetch != nil {
			return nil
		}
		return models
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]workspaceCacheEntry{}
	}
	entry := c.entries[key]
	if errFetch != nil || len(models) == 0 {
		logger.Debug("minimax workspace models: fetch failed for %s: %v", key, errFetch)
		entry.lastFail = now
		c.entries[key] = entry
		return entry.models
	}
	for cachedKey, cached := range c.entries {
		if cachedKey == key {
			continue
		}
		if now.Sub(cached.fetchedAt) >= workspaceModelsTTL &&
			(cached.lastFail.IsZero() || now.Sub(cached.lastFail) >= workspaceModelsFailWindow) {
			delete(c.entries, cachedKey)
		}
	}
	c.entries[key] = workspaceCacheEntry{models: models, fetchedAt: now}
	return models
}

// workspaceAccountKey uses stable account identity as the cache key (without leaving raw tokens in map keys),
// and includes the baseURL: switching mirrors will not hit the old site's catalog.
func workspaceAccountKey(cred *Credential, baseURLOverride string) string {
	account := core.FirstNonEmpty(cred.UserID, cred.Email)
	if account == "" {
		sum := sha256.Sum256([]byte(cred.AccessToken))
		account = hex.EncodeToString(sum[:8])
	}
	return account + "|" + AgentHostFor(cred.Region, baseURLOverride)
}

// fetchWorkspaceModels pulls and parses the models list from /config (signed account domain request).
func fetchWorkspaceModels(ctx context.Context, cred *Credential, baseURLOverride string) ([]workspaceModel, error) {
	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildAgentTarget(cred, base, EpConfig, http.MethodGet, "", false)
	resp, errDo := doRequest(ctx, cred, target, base, 20*time.Second)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, core.NewPluginError("minimax_config_unavailable",
			"config endpoint returned "+resp.Status, resp.StatusCode)
	}
	var body struct {
		Models []struct {
			ModelID           string   `json:"model_id"`
			DisplayName       string   `json:"display_name"`
			ContextLimit      int64    `json:"context_limit"`
			SupportedVariants []string `json:"supported_variants"`
		} `json:"models"`
	}
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); errDecode != nil {
		return nil, errDecode
	}
	models := make([]workspaceModel, 0, len(body.Models))
	for _, item := range body.Models {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}
		models = append(models, workspaceModel{
			ModelID:      id,
			DisplayName:  item.DisplayName,
			ContextLimit: item.ContextLimit,
			Variants:     item.SupportedVariants,
		})
	}
	return models, nil
}

// mergeWorkspaceModels uses the built-in catalog as a skeleton, supplementing it with newly added models from the workspace.
//
// Models with the same name in the built-in catalog do not replace metadata: the workspace only declares ID/display name/context,
// and directly overwriting would lose MaxOutput, tool capabilities, and -thinking variants. A new model explicitly declaring
// thinking in the workspace registers an additional <id>-thinking entry, matching the naming convention of the built-in list.
func mergeWorkspaceModels(static []pluginapi.ModelInfo, workspace []workspaceModel) []pluginapi.ModelInfo {
	if len(workspace) == 0 {
		return static
	}
	out := make([]pluginapi.ModelInfo, 0, len(static)+len(workspace))
	out = append(out, static...)
	present := make(map[string]bool, len(out))
	for _, model := range out {
		present[strings.ToLower(strings.TrimSpace(model.ID))] = true
	}
	for _, item := range workspace {
		id := strings.ToLower(strings.TrimSpace(item.ModelID))
		if id == "" || present[id] {
			continue
		}
		present[id] = true
		name := core.FirstNonEmpty(strings.TrimSpace(item.DisplayName), item.ModelID)
		out = append(out, workspaceModelInfo(id, name, "", item.ContextLimit))
		if !workspaceDeclaresVariant(item, "thinking") {
			continue
		}
		thinkingID := id + "-thinking"
		if present[thinkingID] {
			continue
		}
		present[thinkingID] = true
		out = append(out, workspaceModelInfo(thinkingID, name+" Thinking",
			"Deep thinking mode, routing reasoning content via reasoning_content", item.ContextLimit))
	}
	return out
}

// workspaceModelInfo complements the workspace model into a complete external entry.
//
// /config only provides ID/display name/context, missing output limits and tool capabilities; retaining the fallback values
// from the built-in catalog avoids issues after registration such as MaxCompletionTokens=0, SupportsTools=false, and inconsistencies
// with other model descriptions in the same package.
func workspaceModelInfo(id, displayName, description string, contextLimit int64) pluginapi.ModelInfo {
	if contextLimit <= 0 {
		contextLimit = core.DefaultContextWindowTokens
	}
	return core.ModelInfoToPluginAPI("", core.ModelDescriptor{
		ID:              id,
		Name:            displayName,
		Description:     description,
		ContextLength:   contextLimit,
		MaxOutputTokens: core.DefaultMaxOutputTokens,
		SupportsTools:   true,
	})
}

// resolveWorkspaceModel resolves the model requested by the client into the model_id currently registered in the workspace.
//
// Match order: Request ID (try removing -thinking suffix first) -> Built-in directory UpstreamModel.
// When hitting a workspace model, verify the variant at the same time (include it only if the workspace declares support).
// If not matched, returns ok=false, and the caller falls back to the built-in directory's UpstreamModel (legacy behavior).
func resolveWorkspaceModel(ctx context.Context, cache *workspaceCache, cred *Credential, baseURLOverride, modelID string, bundled Model) (string, string, bool) {
	models := cache.models(ctx, cred, baseURLOverride)
	if len(models) == 0 {
		return "", "", false
	}
	variant := bundled.Variant
	base := normalizeModelProbe(modelID)
	if strings.HasSuffix(base, "thinking") {
		// The thinking variant is expressed as a suffix on the request side, and as supported_variants on the workspace side.
		if variant == "" {
			variant = "thinking"
		}
		base = strings.TrimSuffix(strings.TrimSuffix(base, "thinking"), "-")
	}
	for _, item := range models {
		if !strings.EqualFold(item.ModelID, base) && !strings.EqualFold(item.ModelID, bundled.UpstreamModel) {
			continue
		}
		resolved := item.ModelID
		if variant != "" && !workspaceSupportsVariant(item, variant) {
			// Workspace explicitly does not support this variant: treat it as having no variant (better to respond directly than to send invalid parameters).
			variant = ""
		}
		return resolved, variant, true
	}
	return "", "", false
}

// workspaceDeclaresVariant reports whether the workspace **explicitly** declares a variant.
func workspaceDeclaresVariant(item workspaceModel, variant string) bool {
	for _, declared := range item.Variants {
		if strings.EqualFold(strings.TrimSpace(declared), variant) {
			return true
		}
	}
	return false
}

// workspaceSupportsVariant reports whether the workspace model supports a variant.
//
// If supported_variants is not declared, it is considered supported: built-in -thinking always carries the variant,
// and it won't be disabled without negative evidence from the workspace (preserving legacy behavior).
func workspaceSupportsVariant(item workspaceModel, variant string) bool {
	return len(item.Variants) == 0 || workspaceDeclaresVariant(item, variant)
}

// normalizeModelProbe normalizes the request ID for workspace matching: strips provider prefix and lowercases.
func normalizeModelProbe(modelID string) string {
	clean := strings.ToLower(strings.TrimSpace(modelID))
	if idx := strings.Index(clean, ":"); idx >= 0 {
		clean = clean[idx+1:]
	}
	return clean
}
