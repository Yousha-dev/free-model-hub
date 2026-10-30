package openaicompat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// upstreamModel is one normalized catalog entry.
type upstreamModel struct {
	ID             string
	Name           string
	ContextLength  int64
	MaxOutput      int64
	SupportsImages bool
	free           bool
}

// parseModels accepts {"data":[...]}, {"models":[...]} and bare arrays, the
// shapes used by OpenAI-compatible gateways and the GitHub Models catalog.
func parseModels(body []byte) ([]upstreamModel, error) {
	var entries []map[string]any
	if errArray := json.Unmarshal(body, &entries); errArray != nil {
		var envelope struct {
			Data   []map[string]any `json:"data"`
			Models []map[string]any `json:"models"`
		}
		if errEnvelope := json.Unmarshal(body, &envelope); errEnvelope != nil {
			return nil, fmt.Errorf("models response is not JSON: %w", errEnvelope)
		}
		entries = envelope.Data
		if len(entries) == 0 {
			entries = envelope.Models
		}
	}
	models := make([]upstreamModel, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		id := stringField(entry, "id")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		model := upstreamModel{
			ID:            id,
			Name:          firstString(entry, "name", "display_name", "friendly_name"),
			ContextLength: firstInt(entry, "context_length", "context_window", "max_context_length", "max_model_len"),
			MaxOutput:     firstInt(entry, "max_output_tokens", "max_completion_tokens"),
		}
		if limits, ok := entry["limits"].(map[string]any); ok {
			if model.ContextLength == 0 {
				model.ContextLength = firstInt(limits, "max_input_tokens", "context_length")
			}
			if model.MaxOutput == 0 {
				model.MaxOutput = firstInt(limits, "max_output_tokens")
			}
		}
		if provider, ok := entry["top_provider"].(map[string]any); ok && model.MaxOutput == 0 {
			model.MaxOutput = firstInt(provider, "max_completion_tokens")
		}
		model.SupportsImages = mentionsImage(entry)
		if flag, ok := entry["isFree"].(bool); ok {
			model.free = flag
		}
		if pricing, ok := entry["pricing"].(map[string]any); ok {
			if !model.free {
				model.free = zeroPrice(pricing["prompt"]) && zeroPrice(pricing["completion"])
			}
		}
		if strings.HasSuffix(id, ":free") || strings.HasSuffix(id, "/free") {
			model.free = true
		}
		models = append(models, model)
	}
	return models, nil
}

// flashModel matches Z.ai's free "-flash" ids (not the paid "-flashx").
var flashModel = regexp.MustCompile(`-flash($|-)`)

// filterModels applies the provider's free filter and sorts by ID.
func filterModels(models []upstreamModel, filter FreeFilter, showPaid bool) []upstreamModel {
	out := make([]upstreamModel, 0, len(models))
	for _, model := range models {
		switch filter {
		case FilterFlagged:
			if !model.free {
				continue
			}
		case FilterZeroPrice:
			if !showPaid && !model.free {
				continue
			}
		case FilterFlash:
			if !flashModel.MatchString(strings.ToLower(model.ID)) {
				continue
			}
		}
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func zeroPrice(value any) bool {
	switch typed := value.(type) {
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return err == nil && parsed == 0
	case float64:
		return typed == 0
	}
	return false
}

func mentionsImage(entry map[string]any) bool {
	for _, key := range []string{"supported_input_modalities", "input_modalities"} {
		if list, ok := entry[key].([]any); ok {
			for _, item := range list {
				if value, okValue := item.(string); okValue && strings.EqualFold(value, "image") {
					return true
				}
			}
		}
	}
	if architecture, ok := entry["architecture"].(map[string]any); ok {
		if list, okList := architecture["input_modalities"].([]any); okList {
			for _, item := range list {
				if value, okValue := item.(string); okValue && strings.EqualFold(value, "image") {
					return true
				}
			}
		}
	}
	return false
}

func stringField(entry map[string]any, key string) string {
	value, _ := entry[key].(string)
	return strings.TrimSpace(value)
}

func firstString(entry map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringField(entry, key); value != "" {
			return value
		}
	}
	return ""
}

func firstInt(entry map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch typed := entry[key].(type) {
		case float64:
			if typed > 0 {
				return int64(typed)
			}
		case string:
			if parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}
