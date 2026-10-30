package trae

// This file implements chat forwarding for the Trae family (Trae CN / Trae Global):
// Request body rewriting, IDE header assembly, and SSE event translation.
//
// Protocol baseline: TraeCode CN desktop llm_raw_chat (aligned with Trae2api-cn reference implementation),
// Both instances share the same protocol implementation, with differences described by Config/profile:
//   - CN edition (traecn): raw_chat_function=chat_v3, IDE version 3.3.104
//   - Global edition (traeglobal): same protocol as CN, only domain and client_id differ;
//     Global edition has no check-in/quota campaigns (upstream 404), handled separately by tasks.go.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// ---- Endpoints and Instance Profiles ----

type Region string

const (
	RegionCN     Region = "cn"
	RegionGlobal Region = "global"

	VendorIDCN       = "traecn"
	VendorNameCN     = "Trae CN"
	VendorIDGlobal   = "traeglobal"
	VendorNameGlobal = "Trae Global"

	HostChatCN    = "https://trae-api-cn.mchost.guru"
	HostAuthCN    = "https://api.trae.cn"
	HostConsoleCN = "https://www.trae.cn"
	ClientIDCN    = "en1oxy7wnw8j9n"
	// CN IDE version number. Upstream issues different model catalogs based on the build code (X-Ide-Version-Code):
	// deepseek-v4.1-flash is only available starting from 20260806, and earlier build numbers are downgraded to the old list.
	// These two values can be overridden by <instance_id>_ide_version / <instance_id>_ide_version_code (see adapter.Configure),
	// e.g., traecn_ide_version_code; the two product lines have different versioning systems and must be configured per instance.
	IDEVersionCN = "3.3.104"
	// IDEVersionCodeCN is the build number of the CN IDE, issued along with X-Ide-Version-Code.
	IDEVersionCodeCN = "20260922"

	// Global edition (Singapore)
	HostChatGlobal       = "https://coresg-normal.trae.ai"
	HostAuthGlobal       = "https://growsg-normal.trae.ai"
	HostConsoleGlobal    = "https://www.trae.ai"
	ClientIDGlobal       = "ono9krqynydwx5"
	IDEVersionGlobal     = "3.3.67"
	IDEVersionCodeGlobal = "20260401"

	// DeviceBrandCN is the device model reported by the CN edition.
	DeviceBrandCN = "83DG"

	// Common endpoint paths (shared by both instances)
	EpChat          = "/api/ide/v2/llm_raw_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"

	AppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
)

// Config is the complete profile of a Trae instance.
type Config struct {
	VendorID   string
	VendorName string
	Region     Region

	ChatHost    string
	AuthHost    string
	RefreshHost string
	CheckinHost string
	QuotaHost   string
	ConsoleHost string

	ClientID       string
	IDEVersion     string
	IDEVersionCode string
	Function       string
	DefaultModel   string
	AppID          string
	// DeviceBrand is only used for model catalog requests (get_detail_param requires the device model).
	DeviceBrand string
}

// ConfigFor returns the default configuration for the CN/Global edition (reserved for protocol-layer testing).
func ConfigFor(r Region) Config {
	if r == RegionGlobal {
		return Config{
			VendorID:       VendorIDGlobal,
			VendorName:     VendorNameGlobal,
			Region:         RegionGlobal,
			ChatHost:       HostChatGlobal,
			AuthHost:       HostAuthGlobal,
			RefreshHost:    HostAuthGlobal,
			CheckinHost:    "", // Global edition has no check-in campaigns (upstream lacks this endpoint)
			QuotaHost:      "", // Global edition has no quota endpoint
			ConsoleHost:    HostConsoleGlobal,
			ClientID:       ClientIDGlobal,
			IDEVersion:     IDEVersionGlobal,
			IDEVersionCode: IDEVersionCodeGlobal,
			Function:       "chat_v3",
			DefaultModel:   "glm-5.2",
			AppID:          AppID,
			DeviceBrand:    "PC",
		}
	}
	return Config{
		VendorID:       VendorIDCN,
		VendorName:     VendorNameCN,
		Region:         RegionCN,
		ChatHost:       HostChatCN,
		AuthHost:       HostAuthCN,
		RefreshHost:    HostAuthCN,
		CheckinHost:    HostAuthCN,
		QuotaHost:      HostAuthCN,
		ConsoleHost:    HostConsoleCN,
		ClientID:       ClientIDCN,
		IDEVersion:     IDEVersionCN,
		IDEVersionCode: IDEVersionCodeCN,
		Function:       "chat_v3",
		DefaultModel:   "glm-5.2",
		AppID:          AppID,
		DeviceBrand:    DeviceBrandCN,
	}
}

// ConfigForVendor returns the profile for a specific provider instance.
func ConfigForVendor(vendorID string) (Config, bool) {
	switch vendorID {
	case VendorIDCN:
		return ConfigFor(RegionCN), true
	case VendorIDGlobal:
		return ConfigFor(RegionGlobal), true
	default:
		return Config{}, false
	}
}

// AllConfigs returns the profiles of both instances, ordered the same as the historical registration order.
func AllConfigs() []Config {
	return []Config{ConfigFor(RegionCN), ConfigFor(RegionGlobal)}
}

func (c Config) clientUA() string { return "Trae/" + c.IDEVersion }

func configForCredential(cred *Credential) Config {
	if cred == nil {
		return ConfigFor(RegionCN)
	}
	if cfg, okConfig := ConfigForVendor(cred.Vendor); okConfig {
		return cfg
	}
	return ConfigFor(cred.Region)
}

// ---- Chat Forwarding ----

type ChatRequest struct {
	Credential *Credential
	Profile    Config
	Body       []byte
	Stream     bool
	// catalog is the instance-level model catalog cache; nil means no caching (for tests and one-off calls).
	catalog *catalogCache
}

type StreamChunkHandler func(chunk []byte) error

// ApplyTraeHeaders assembles TraeCode IDE request headers.
//
// The header set is consistent with Trae2api-cn's build_traecode_headers: model/function bindings must
// be duplicated in X-App-Function / X-Ide-Function in addition to the request body, or the gateway may reject them.
func ApplyTraeHeaders(req *http.Request, cred *Credential, cfg Config, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "keep-alive")

	at := cred.AccessToken
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if cred.UID != "" {
		req.Header.Set("X-Uid", cred.UID)
	}
	req.Header.Set("X-App-Id", cfg.AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-App-Version-Code", cfg.IDEVersionCode)
	req.Header.Set("X-Ide-Version", cfg.IDEVersion)
	req.Header.Set("X-Ide-Version-Code", cfg.IDEVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-App-Function", cfg.Function)
	req.Header.Set("X-Ide-Function", cfg.Function)
	req.Header.Set("X-Request-Id", newUUID())
	req.Header.Set("X-Custom-Trace-Id", core.RandomHex(16))
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("Request-Traffic-Type", "prod")
	req.Header.Set("User-Agent", cfg.clientUA())
	if cred.MachineID != "" {
		req.Header.Set("X-Machine-Id", cred.MachineID)
	}
	if cred.DeviceID != "" {
		req.Header.Set("X-Device-Id", cred.DeviceID)
	}
}

// requestModelName extracts the model name from the OpenAI request body (defaults to the profile default).
func requestModelName(src []byte, cfg Config) string {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return cfg.DefaultModel
	}
	if model, ok := obj["model"].(string); ok && strings.TrimSpace(model) != "" {
		return strings.TrimSpace(model)
	}
	return cfg.DefaultModel
}

// PrepareBody rewrites an OpenAI-style request body into a TraeCode llm_raw_chat request body.
//
// Key differences (relative to the old llm_utils_chat path):
//   - model_name must be the upstream wire id (exact value in the model_detail_list catalog),
//     config_name is the configuration name in the catalog, both resolved by catalogCache.resolve;
//   - raw_chat_function goes into both the request body and request headers;
//   - conversation_id / session_id are newly generated for each request.
func PrepareBody(src []byte, cfg Config, resolved ResolvedModel) ([]byte, string, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src, resolved.ConfigName, err
	}

	sessionID := newUUID()
	obj["config_name"] = resolved.ConfigName
	obj["model_name"] = resolved.WireName
	obj["conversation_id"] = sessionID
	obj["session_id"] = sessionID
	obj["stream"] = true
	obj["raw_chat_function"] = cfg.Function
	obj["config_source"] = 1
	delete(obj, "model")
	delete(obj, "function")

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			// TraeCode's ChatMessageData uses a content block array; string content is wrapped inside.
			content, present := m["content"]
			if present {
				if s, ok := content.(string); ok {
					m["content"] = []any{map[string]any{"type": "text", "text": s}}
				}
			}
		}
	}

	data, err := json.Marshal(obj)
	return data, resolved.ConfigName, err
}

// Chat executes a non-streaming chat (internally still uses upstream SSE, aggregating before returning the OpenAI structure).
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	respBody, model, errDo := doTraeRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = respBody.Close() }()

	var (
		contentSB   strings.Builder
		reasoningSB strings.Builder
		usage       map[string]any
		finish      = "stop"
	)

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}

		switch currentEvent {
		case "output":
			if r, ok := payload["response"].(string); ok {
				contentSB.WriteString(r)
			}
			if rc, ok := payload["reasoning_content"].(string); ok {
				reasoningSB.WriteString(rc)
			}
		case "token_usage":
			usage = mapUsage(payload)
		case "error":
			code, _ := payload["code"].(float64)
			msg, _ := payload["message"].(string)
			return nil, fmt.Errorf("trae upstream error: code=%.0f msg=%s", code, msg)
		case "done":
			if fr, ok := payload["finish_reason"].(string); ok && fr != "" {
				finish = fr
			}
		}
	}

	message := map[string]any{
		"role":              "assistant",
		"content":           contentSB.String(),
		"reasoning_content": reasoningSB.String(),
	}
	out := map[string]any{
		"id":      "chatcmpl-" + core.RandomHex(6),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
	}
	if usage != nil {
		out["usage"] = usage
	}

	return json.Marshal(out)
}

// ChatStream executes a streaming chat, incrementally converting upstream SSE into OpenAI chunks.
func ChatStream(ctx context.Context, baseURL string, req ChatRequest, onChunk StreamChunkHandler) error {
	respBody, model, errDo := doTraeRequest(ctx, baseURL, req)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = respBody.Close() }()

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var currentEvent string
	chunkID := "chatcmpl-" + core.RandomHex(6)
	created := time.Now().Unix()

	emit := func(delta map[string]any, finishReason any) error {
		chunk := map[string]any{
			"id":      chunkID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         delta,
					"finish_reason": finishReason,
				},
			},
		}
		rawChunk, errMarshal := json.Marshal(chunk)
		if errMarshal != nil {
			return errMarshal
		}
		return onChunk(rawChunk)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return nil
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}

		switch currentEvent {
		case "output":
			delta := map[string]any{}
			resp, _ := payload["response"].(string)
			reasoning, _ := payload["reasoning_content"].(string)
			if resp != "" {
				delta["content"] = resp
			}
			if reasoning != "" {
				delta["reasoning_content"] = reasoning
			}
			if len(delta) > 0 {
				if errEmit := emit(delta, nil); errEmit != nil {
					return errEmit
				}
			}
		case "token_usage":
			// usage is sent separately after the last chunk (OpenAI streaming convention).
			chunk := map[string]any{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []any{},
				"usage":   mapUsage(payload),
			}
			rawChunk, errMarshal := json.Marshal(chunk)
			if errMarshal != nil {
				return errMarshal
			}
			if errEmit := onChunk(rawChunk); errEmit != nil {
				return errEmit
			}
		case "error":
			code, _ := payload["code"].(float64)
			msg, _ := payload["message"].(string)
			return fmt.Errorf("trae stream error: code=%.0f msg=%s", code, msg)
		case "done":
			return emit(map[string]any{}, "stop")
		}
	}

	return scanner.Err()
}

// mapUsage maps upstream token_usage events to the OpenAI usage structure.
func mapUsage(payload map[string]any) map[string]any {
	usage := map[string]any{}
	for src, dst := range map[string]string{
		"prompt_tokens":     "prompt_tokens",
		"completion_tokens": "completion_tokens",
		"total_tokens":      "total_tokens",
	} {
		if v, ok := payload[src]; ok {
			usage[dst] = v
		}
	}
	if v, ok := payload["reasoning_tokens"]; ok {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": v}
	}
	return usage
}

// doTraeRequest initiates an llm_raw_chat request; falls back to v1 if the v2 endpoint is unavailable.
func doTraeRequest(ctx context.Context, baseURL string, req ChatRequest) (io.ReadCloser, string, error) {
	if req.Credential == nil {
		return nil, "", fmt.Errorf("trae credential is nil")
	}

	cfg := req.Profile
	if cfg.VendorID == "" {
		cfg = configForCredential(req.Credential)
	}

	resolved, errResolve := req.catalog.resolve(ctx, requestModelName(req.Body, cfg), cfg, req.Credential)
	if errResolve != nil {
		return nil, "", errResolve
	}

	body, model, errPrep := PrepareBody(req.Body, cfg, resolved)
	if errPrep != nil {
		return nil, "", errPrep
	}

	targetHost := strings.TrimRight(baseURL, "/")
	if targetHost == "" {
		targetHost = cfg.ChatHost
	}

	resp, errDo := doRawChat(ctx, targetHost+EpChat, body, req.Credential, cfg)
	if errDo != nil {
		return nil, "", fmt.Errorf("trae chat request failed: %w", errDo)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, "", Classify(resp.StatusCode, string(raw))
	}
	return resp.Body, model, nil
}

// doRawChat initiates an llm_raw_chat request.
//
// The upstream endpoint only speaks SSE (request body stream is always true), so we uniformly route through the host stream bridge event by event
// delivery; non-streaming chat aggregates the same SSE on the caller side.
func doRawChat(ctx context.Context, url string, body []byte, cred *Credential, cfg Config) (*http.Response, error) {
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errReq != nil {
		return nil, errReq
	}
	ApplyTraeHeaders(httpReq, cred, cfg, true)

	return hostbridge.StreamClient(ctx, 0).Do(httpReq)
}

// newUUID generates a random UUID v4 string (for conversation_id / session_id).
func newUUID() string {
	b := make([]byte, 16)
	if _, errHex := hex.Decode(b, []byte(core.RandomHex(16))); errHex != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
