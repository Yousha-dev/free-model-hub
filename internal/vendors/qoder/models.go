package qoder

// This file implements Qoder's model list.
//
// The list has only one source: the upstream model/list API (requires cosy session). **No built-in
// fallback list is maintained**—hardcoded model names become outdated as upstream changes, making clients see
// a non-existent catalog instead. If fetching fails, return an empty list and prompt the console to "Refresh models".

import (
	"strings"
	"sync"

	"free-model-hub/internal/logger"
)

// modelIndex is the "external model ID / upstream key -> upstream key" index for a regional instance.
//
// Why instance state instead of a package-level table: Qoder CN and Global are two adapter instances,
// The same upstream key may correspond to different models in two regions (e.g., mmodel in CN is
// and the same upstream key may correspond to different models in each region (e.g., mmodel is
// MiniMax-M2.7 in CN, and MiniMax-M3 in Global). Putting it in the instance allows independent maintenance per side,
//
// Why dynamically built rather than a hardcoded static table: The slug -> key mapping exists only in the upstream
// list's {key, display_name} pairs. Hardcoding a table means modifying code every time upstream changes a model,
// and easily misbinds two different models to the same name; deriving from the list naturally tracks upstream.
type modelIndex struct {
	mu     sync.RWMutex
	byName map[string]string
}

// replace rebuilds the index with a batch of upstream model entries, returning "upstream key -> external ID".
//
// For duplicate IDs (upstream has duplicate display_names), only the first is kept, and the rest are omitted from the return value,
// to be skipped by the caller—hosts match models by ID, and registering two identical IDs ensures one can never be selected.
func (i *modelIndex) replace(models []Model) map[string]string {
	byName := make(map[string]string, len(models)*2)
	slugByKey := make(map[string]string, len(models))
	for _, model := range models {
		key := strings.TrimSpace(model.Key)
		if key == "" {
			continue
		}
		id := canonicalModelID(model.DisplayName, key)
		if id == "" {
			continue
		}
		if _, exists := byName[id]; exists {
			logger.Info("qoder: duplicate model id %q (key %s), skipping", id, key)
			continue
		}
		slugByKey[key] = id
		byName[id] = key
		// Older clients may still send upstream keys directly (dmodel / gmodel …): accept them as well.
		// External ID takes precedence—in case a slug happens to equal another model's upstream key,
		// the slug declared in the list takes priority.
		lowerKey := strings.ToLower(key)
		if _, exists := byName[lowerKey]; !exists {
			byName[lowerKey] = key
		}
	}
	i.mu.Lock()
	i.byName = byName
	i.mu.Unlock()
	return slugByKey
}

// UpstreamKey resolves an external model name to the upstream model_config.key. Case-insensitive.
func (i *modelIndex) UpstreamKey(name string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	if trimmed == "" {
		return "", false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	key, ok := i.byName[trimmed]
	return key, ok
}

// canonicalModelID normalizes the upstream display_name into an external model ID:
// lowercase, collapse whitespace into a single "-" ("DeepSeek-V4-Pro" -> "deepseek-v4-pro").
//
// When display_name is missing, fall back to the lowercase form of the upstream key, ensuring a usable ID is always available.
func canonicalModelID(displayName, key string) string {
	slug := strings.ToLower(strings.TrimSpace(displayName))
	slug = strings.Join(strings.Fields(slug), "-")
	if slug == "" {
		slug = strings.ToLower(strings.TrimSpace(key))
	}
	return slug
}
