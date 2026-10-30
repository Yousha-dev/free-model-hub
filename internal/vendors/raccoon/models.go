package raccoon

// This file implements fetching the model catalog for Raccoon (GET {llm_base}/model_catalog).
//
// Upstream format {"code":0,"data":{"categories":[{type,models:[…]}]}},
// Entry {name, display_name, description, display_description, visible,
// context_window, max_tokens, billing_multiplier, params:{...}}:
// name is the model ID, description is the internal code name (the name that should be shown in the model selector),
// display_description is the marketing copy; context_window / max_tokens might be
// strings or numbers, and can also be embedded in params (both are recognized); first occurrence of the same name wins;
// visible defaults to true.
//
// Falls back to a static single entry (DefaultConfigName) when the catalog cannot be fetched, ensuring /v1/models is not empty.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// modelsHTTPTimeout is the timeout limit for catalog requests.
const modelsHTTPTimeout = 15 * time.Second

// Model is a normalized catalog entry.
type Model struct {
	ID              string
	Name            string
	Description     string
	ContextWindow   int64
	MaxOutputTokens int64
	IsReasoning     bool
	SupportsTools   bool
	// SupportsImages is inferred from the catalog entry tags (vision / image / image-understanding).
	SupportsImages bool
	// Credits is the raw billing multiplier given by the upstream (e.g. "0.8", "x0.05");
	// An empty string means the upstream didn't provide one — it cannot be treated as 0, let alone "free".
	Credits string
}

// FetchModels fetches the model catalog.
func FetchModels(ctx context.Context, llmBase, token string) ([]Model, error) {
	if strings.TrimSpace(llmBase) == "" {
		return nil, fmt.Errorf("upstream LLM base URL is not configured")
	}
	endpoint := llmBase + CatalogPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build catalog request: %w", errReq)
	}
	ApplyAPIHeaders(req, token)

	resp, errDo := hostbridge.Client(ctx, modelsHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch model catalog: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read model catalog response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	models, errParse := parseCatalog(body)
	if errParse != nil {
		return nil, errParse
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("model catalog is empty")
	}
	return models, nil
}

// parseCatalog parses the catalog response (if root has no data, treats root as data; flattens categories[].models).
func parseCatalog(body []byte) ([]Model, error) {
	var envelope struct {
		Code    *int64          `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("parse model catalog: %w", errUnmarshal)
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return nil, ClassifyBusiness(http.StatusOK, *envelope.Code, envelope.Message)
	}
	root := json.RawMessage(body)
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		root = envelope.Data
	}
	var parsed struct {
		Categories []struct {
			Models []catalogEntry `json:"models"`
		} `json:"categories"`
	}
	if errUnmarshal := json.Unmarshal(root, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("parse model catalog: %w", errUnmarshal)
	}
	out := make([]Model, 0)
	seen := map[string]bool{}
	for _, category := range parsed.Categories {
		for _, entry := range category.Models {
			model, okModel := entry.normalize()
			if !okModel || seen[model.ID] {
				continue
			}
			seen[model.ID] = true
			out = append(out, model)
		}
	}
	return out, nil
}

// catalogEntry is the raw structure of a catalog entry.
type catalogEntry struct {
	Name               string    `json:"name"`
	ModelName          string    `json:"model_name"`
	DisplayName        string    `json:"display_name"`
	Description        string    `json:"description"`
	DisplayDescription string    `json:"display_description"`
	Visible            *bool     `json:"visible"`
	Tags               []string  `json:"tags"`
	ContextWindow      *flexInt  `json:"context_window"`
	MaxTokens          *flexInt  `json:"max_tokens"`
	BillingMultiplier  *flexText `json:"billing_multiplier"`
	// BillingEffectiveMultiplier is the actual billing multiplier after promotions and free periods:
	// the amount the user will actually be charged; falls back to the base multiplier when omitted.
	BillingEffectiveMultiplier *flexText `json:"billing_effective_multiplier"`
	Params                     *struct {
		ContextWindow     *flexInt  `json:"context_window"`
		MaxTokens         *flexInt  `json:"max_tokens"`
		BillingMultiplier *flexText `json:"billing_multiplier"`
	} `json:"params"`
}

// flexInt supports both numeric and string representations for numbers (upstream has sent both).
type flexInt float64

// UnmarshalJSON supports both JSON numbers and strings.
func (f *flexInt) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		return nil
	}
	trimmed = strings.Trim(trimmed, "\"")
	if trimmed == "" {
		return nil
	}
	var value float64
	if _, err := fmt.Sscanf(trimmed, "%g", &value); err != nil {
		return fmt.Errorf("flexInt: %w", err)
	}
	*f = flexInt(value)
	return nil
}

// flexText supports both numeric and string formats, and preserves the raw text.
//
// billing_multiplier upstream has sent both 0.8 (number) and "x0.05" (string).
// The multiplier needs to be both normalized into display text like "0.80X"
// and preserved as-is when upstream gives non-numeric text, so it is not parsed into float64.
type flexText string

// UnmarshalJSON supports JSON numbers and strings; all other types are treated as "not provided".
//
// Upstream has written missing data as true / {}; if left as is, the console would display "true" as text.
func (f *flexText) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return fmt.Errorf("flexText: %w", err)
		}
		*f = flexText(strings.TrimSpace(text))
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		*f = ""
		return nil
	}
	*f = flexText(number.String())
	return nil
}

// value returns the raw text; nil receiver returns an empty string.
func (f *flexText) value() string {
	if f == nil {
		return ""
	}
	return strings.TrimSpace(string(*f))
}

// normalize normalizes a raw entry into a Model (discards if id is missing; visible defaults to true).
func (e *catalogEntry) normalize() (Model, bool) {
	id := strings.TrimSpace(core.FirstNonEmpty(e.Name, e.ModelName))
	if id == "" {
		return Model{}, false
	}
	visible := true
	if e.Visible != nil {
		visible = *e.Visible
	}
	if !visible {
		return Model{}, false
	}
	contextWindow := firstFloat(e.ContextWindow, e.nestedContext())
	maxTokens := firstFloat(e.MaxTokens, e.nestedMaxTokens())
	if contextWindow == 0 {
		contextWindow = core.DefaultContextWindow(id)
	}
	if maxTokens == 0 {
		maxTokens = core.DefaultMaxOutputTokens
	}
	return Model{
		ID:              id,
		Name:            e.displayName(id),
		Description:     strings.TrimSpace(e.DisplayDescription),
		ContextWindow:   contextWindow,
		MaxOutputTokens: maxTokens,
		IsReasoning:     true,
		SupportsTools:   true,
		SupportsImages:  supportsImageTags(e.Tags),
		// Actual billing multiplier takes precedence: this is the amount the user will actually be charged (differs from base price during discounts/free periods).
		Credits: firstText(e.BillingEffectiveMultiplier, e.BillingMultiplier, e.nestedBillingMultiplier()),
	}, true
}

// maxModelDisplayNameRunes is the length limit for allowing upstream internal code names to be used as display names.
//
// description can be a code name like "Raccoon-Work" or a full sentence description;
// putting sentences into the model selector will break the list, so revert to wire id when exceeding the limit.
const maxModelDisplayNameRunes = 64

// displayName selects the model display name.
//
// Upstream field semantics are counter-intuitive: name is the model ID, description is the internal code name
// (the name that should be shown in the list), and marketing copy is in display_description. Early implementations took
// display_description as the display name, causing the model list to show remarks like "suitable for simple Q&A, content rewriting".
// Explicit display_name takes highest priority, followed by code names of reasonable length.
func (e *catalogEntry) displayName(id string) string {
	if name := strings.TrimSpace(e.DisplayName); name != "" {
		return name
	}
	if codename := strings.TrimSpace(e.Description); codename != "" && utf8.RuneCountInString(codename) <= maxModelDisplayNameRunes {
		return codename
	}
	return id
}

func (e *catalogEntry) nestedContext() *flexInt {
	if e.Params == nil {
		return nil
	}
	return e.Params.ContextWindow
}

func (e *catalogEntry) nestedMaxTokens() *flexInt {
	if e.Params == nil {
		return nil
	}
	return e.Params.MaxTokens
}

func (e *catalogEntry) nestedBillingMultiplier() *flexText {
	if e.Params == nil {
		return nil
	}
	return e.Params.BillingMultiplier
}

// firstFloat returns the first non-nil numeric value (rounded).
func firstFloat(values ...*flexInt) int64 {
	for _, value := range values {
		if value != nil && *value > 0 {
			return int64(*value)
		}
	}
	return 0
}

// firstText returns the first non-empty text.
func firstText(values ...*flexText) string {
	for _, value := range values {
		if text := value.value(); text != "" {
			return text
		}
	}
	return ""
}

// supportsImageTags checks whether the tags of a directory entry declare vision capabilities.
//
// The upstream directory does **not** have a supportImage field, so the source implementation recognizes these three words from tags;
// otherwise it results in a missing report (vision models appear empty in the console "Capabilities" column).
func supportsImageTags(tags []string) bool {
	for _, tag := range tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "vision", "image", "image-understanding":
			return true
		}
	}
	return false
}

// StaticFallbackModels is a fallback single entry when the directory cannot be fetched (ensuring /v1/models is not an empty list).
func StaticFallbackModels() []Model {
	return []Model{{
		ID:              DefaultConfigName,
		Name:            "Raccoon Chat",
		Description:     "Raccoon default model (fallback when directory fetch fails)",
		ContextWindow:   180000,
		MaxOutputTokens: 80000,
		IsReasoning:     true,
		SupportsTools:   true,
	}}
}
