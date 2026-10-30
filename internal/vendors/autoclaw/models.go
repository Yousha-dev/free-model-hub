package autoclaw

// This file implements AutoClaw model list and account information retrieval.
//
// List source (consistent with Agent2API):
//   - global: GET {base}/autoclaw-proxy/proxy/autoclaw-model-config (real-time directory,
//     containing metadata like contextWindow / maxTokens / reasoning);
//   - cn: static table (models for sandbox relay channel, upstream has no corresponding real-time directory API to
//     pull directly from userapi; real-time status is subject to measured model availability).
//
// Probing requires a valid credential; probe failure returns an empty list rather than a fabricated one.

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

// modelsHTTPTimeout is the limit for model list requests.
const modelsHTTPTimeout = 30 * time.Second

// Model is a normalized model entry.
type Model struct {
	ID              string
	Name            string
	Description     string
	ContextWindow   int64
	MaxOutputTokens int64
	IsReasoning     bool
	SupportsImages  bool
	SupportsTools   bool
	// Credits is the upstream creditConsumptionLevel tier text ("Low"/"Medium"/"High"),
	// or it may be a numeric tier number — numbers are converted to "Tier N" and do not participate in rate normalization.
	//
	// It is the text tier of "how many points to deduct for this tier", not a discount multiplier, and **cannot** be converted
	// into a numeric multiplier; if upstream doesn't provide it, it is an empty string (console displays "—", not falsely assumed free).
	Credits string
}

// modelConfigEntry is a model record in autoclaw-model-config.
type modelConfigEntry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Tooltip       string   `json:"tooltip"`
	ContextWindow int64    `json:"contextWindow"`
	MaxTokens     int64    `json:"maxTokens"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"`
	// CreditConsumptionLevel is the billing tier, measured as Chinese tier text; numeric tiers have also been seen,
	// so RawMessage is used to receive it and then decode it by shape, preventing the entire list parsing from failing if upstream changes its shape.
	CreditConsumptionLevel json.RawMessage `json:"creditConsumptionLevel"`
}

// modelConfigPayload is compatible with two upstream response shapes:
//   - old envelope: {"code":0,"msg":"SUCCESS","data":{"models":[...]}};
//   - measured Global: top-level {"models":[...]} (no code/msg/data wrapper).
//
// Recognizing only the latter would cause everything except the former to be silently parsed into an empty list — the host then
// discards the credential with "0 models", and the entire page fails to show AutoClaw models.
type modelConfigPayload struct {
	Code   *int64             `json:"code"`
	Msg    string             `json:"msg"`
	Models []modelConfigEntry `json:"models"`
	Data   struct {
		Models []modelConfigEntry `json:"models"`
	} `json:"data"`
}

// entries returns the valid model list (top-level preferred, fallback to envelope).
func (p modelConfigPayload) entries() []modelConfigEntry {
	if len(p.Models) > 0 {
		return p.Models
	}
	return p.Data.Models
}

// cnStaticModels is the model table for the CN sandbox relay channel (Agent2API 2026-09 snapshot).
var cnStaticModels = []string{
	"openclaw", "openclaw/default", "glm-5.3-flash", "glm-2", "zai_auto",
	"zai_auto-fast", "zai_glm-5.3-flash", "zaicoding_glm-5.3",
	"tdpsk_deepseek-v4-flash-202605", "tdpsk_deepseek-v4-pro-202606",
}

// FetchModels pulls the regional model list.
func FetchModels(ctx context.Context, baseURL string, token string, region Region) ([]Model, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("access token is required to list models")
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured")
	}
	if region.IsGlobal() {
		return fetchModelConfig(ctx, baseURL, token)
	}
	return staticCNModels(), nil
}

// fetchModelConfig pulls the Global real-time model configuration.
func fetchModelConfig(ctx context.Context, baseURL, token string) ([]Model, error) {
	endpoint := baseURL + ModelsConfigPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build model config request: %w", errReq)
	}
	ApplyModelsHeaders(req, token)

	resp, errDo := hostbridge.Client(ctx, modelsHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch model config: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read model config response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(body))
	}
	var payload modelConfigPayload
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("parse model config: %w", errUnmarshal)
	}
	if payload.Code != nil && *payload.Code != 0 {
		return nil, Classify(resp.StatusCode, *payload.Code, payload.Msg)
	}
	entries := payload.entries()
	if len(entries) == 0 {
		// Successfully parsed but zero models: most likely the response shape changed again. Must report an error — returning
		// an empty list + nil error would leave only "0 model(s)" in the upload interface, and the host would
		// eliminate the credential as having no available models, masking the issue as an "empty model list".
		return nil, fmt.Errorf("model config returned 0 model(s)")
	}
	out := make([]Model, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		contextWindow := entry.ContextWindow
		if contextWindow == 0 {
			contextWindow = core.DefaultContextWindow(id)
		}
		maxTokens := entry.MaxTokens
		if maxTokens == 0 {
			maxTokens = core.DefaultMaxOutputTokens
		}
		out = append(out, Model{
			ID:              id,
			Name:            core.FirstNonEmpty(entry.Name, id),
			Description:     entry.Tooltip,
			ContextWindow:   contextWindow,
			MaxOutputTokens: maxTokens,
			IsReasoning:     entry.Reasoning,
			SupportsImages:  modelAcceptsImages(entry.Input),
			SupportsTools:   true,
			Credits:         levelText(entry.CreditConsumptionLevel),
		})
	}
	return out, nil
}

// levelText decodes the upstream billing tier into display text.
//
// Measured as a string ("Low"/"Medium"/"High"); numbers are **tier numbers** rather than multipliers,
// and must carry the "Tier" prefix, otherwise core.FormatRate will normalize 3 to "3.00X",
// turning into a visible pseudo-multiplier in the console. Unknown shapes like objects return an empty string: better to display "—".
func levelText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if errText := json.Unmarshal(raw, &text); errText == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if errNumber := json.Unmarshal(raw, &number); errNumber == nil {
		return "Tier " + number.String()
	}
	return ""
}

// modelAcceptsImages reports whether the upstream-declared input modalities contain images.
func modelAcceptsImages(input []string) bool {
	for _, modality := range input {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

// staticCNModels returns the CN static model table.
func staticCNModels() []Model {
	out := make([]Model, 0, len(cnStaticModels))
	for _, id := range cnStaticModels {
		out = append(out, Model{
			ID:              id,
			Name:            id,
			Description:     "AutoClaw CN Sandbox Model",
			ContextWindow:   core.DefaultContextWindow(id),
			MaxOutputTokens: core.DefaultMaxOutputTokens,
			SupportsTools:   true,
		})
	}
	return out
}

// RefreshTokenResult is the result of a token refresh.
type RefreshTokenResult struct {
	AccessToken  string
	RefreshToken string
}

// RefreshToken exchanges the refresh token for a new access token
// (POST {base}/userapi/v1/refresh, body {source_id, device_id, refresh_token}).
//
// An empty refresh_token indicates no upstream rotation, retaining the old value.
func RefreshToken(ctx context.Context, baseURL string, cred *Credential) (*RefreshTokenResult, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured")
	}
	payload := map[string]any{
		"source_id":     protocolName,
		"device_id":     cred.DeviceIDValue(),
		"refresh_token": cred.RefreshTokenValue(),
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal refresh request: %w", errMarshal)
	}
	endpoint := baseURL + RefreshPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if errReq != nil {
		return nil, fmt.Errorf("build refresh request: %w", errReq)
	}
	ApplyUserAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, 30*time.Second).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "refresh: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read refresh response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	var envelope struct {
		Code *int64 `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, int64Value(envelope.Code), string(raw))
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return nil, Classify(resp.StatusCode, *envelope.Code, envelope.Msg)
	}
	if strings.TrimSpace(envelope.Data.AccessToken) == "" {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "refresh succeeded but no access_token", Kind: core.FailureKindSessionDead}
	}
	return &RefreshTokenResult{
		AccessToken:  envelope.Data.AccessToken,
		RefreshToken: envelope.Data.RefreshToken,
	}, nil
}

// WalletBalance is the points wallet balance.
type WalletBalance struct {
	TotalBalance float64
}

// FetchBalance queries the points balance (GET {base}/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw).
func FetchBalance(ctx context.Context, baseURL string, token string) (*WalletBalance, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured")
	}
	endpoint := baseURL + WalletsPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build wallets request: %w", errReq)
	}
	ApplyUserAPIHeaders(req, token)

	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "wallets: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read wallets response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	var envelope struct {
		Code *int64 `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			TotalBalance any `json:"total_balance"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return nil, Classify(resp.StatusCode, *envelope.Code, envelope.Msg)
	}
	return &WalletBalance{TotalBalance: float64(core.IntFromAny(envelope.Data.TotalBalance))}, nil
}

// int64Value returns the first non-nil int64 pointer value (0 if all are nil).
func int64Value(values ...*int64) int64 {
	for _, value := range values {
		if value != nil {
			return *value
		}
	}
	return 0
}
