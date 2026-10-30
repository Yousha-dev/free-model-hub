package opencodezen

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// This file implements chat-completions forwarding for OpenCode ZEN.
//
// Differences from WorkBuddy / Qoder: ZEN upstream is standard OpenAI chat-completions.
// Special requirements (referencing opencode2api and the official OpenCode implementation):
//   1. Session ID must be in canonical session format: `ses_` + 12-digit hex + 14-character Base62.
//   2. Free-tier models (IDs containing "free") require the full official Agent request format: route via inference lane,
//      authenticate with anonymous public credentials, stream=true, and declare the official OpenCode toolset
//      (see assets/free-tier-tools.json).
//      Non-streaming calls automatically enable stream in the plugin and combine into a standard JSON response via CollapseSSEToJSON after upstream returns.

// chatHTTPTimeout is the limit for non-streaming chats.
const chatHTTPTimeout = 10 * time.Minute

// ChatRequest is the input for a chat forwarding operation.
type ChatRequest struct {
	// APIKey is the outbound credential for paid models; free tier uses anonymous public credentials instead.
	APIKey string
	// Body is the chat-completions request body translated by the host.
	Body []byte
	// Stream reports whether it is streaming.
	Stream bool
	// Catalog is the models.dev catalog, used to determine the free tier by unit price.
	// Falls back to determining by model name when empty.
	Catalog modelsDevCatalog
}

// Chat executes a non-streaming chat, returning the complete response body from upstream.
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	modelName := extractModelName(req.Body)
	isFree := isFreeTierModel(modelName, req.Catalog)

	if isFree {
		// Free tier only accepts streaming: force stream open, then aggregate SSE back into a complete JSON document.
		req.Stream = true
		resp, errStream := ChatStream(ctx, baseURL, req)
		if errStream != nil {
			return nil, errStream
		}
		defer func() { _ = resp.Body.Close() }()

		return CollapseSSEToJSON(resp.Body, modelName)
	}

	resp, errDo := doChat(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	return body, nil
}

// ChatStream executes a streaming chat, returning the upstream response body (caller is responsible for closing).
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	httpReq, errReq := newChatRequest(ctx, baseURL, req, req.Catalog)
	if errReq != nil {
		return nil, errReq
	}

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, string(body))
	}
	return resp, nil
}

// doChat initiates a non-streaming chat request.
func doChat(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	httpReq, errReq := newChatRequest(ctx, baseURL, req, req.Catalog)
	if errReq != nil {
		return nil, errReq
	}

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// newChatRequest assembles a chat-completions request (shared by streaming and non-streaming).
//
// Free and paid use the same inference lane, differing only in credentials: free uses anonymous public,
// and paid uses the user key. Whether it is free is determined by the models.dev catalog unit price, see isFreeTierModel.
func newChatRequest(ctx context.Context, baseURL string, req ChatRequest, catalog modelsDevCatalog) (*http.Request, error) {
	body := req.Body
	credential := req.APIKey
	if isFreeTierModel(extractModelName(body), catalog) {
		body = prepareFreeTierBody(body)
		credential = freeTierCredential
	} else {
		body = normalizeMaxTokens(body)
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, chatEndpoint(baseURL), bytesReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, credential, sessionIDFor(body))
	return httpReq, nil
}

// normalizeMaxTokens normalizes max_tokens to max_completion_tokens.
//
// Official 2.1.4 uniformly sends max_completion_tokens for the opencode family of models: the catalog
// only declares compatibility.reasoning_field without max_tokens_field, so
// detectMaxTokensField (packages/ai/src/protocols/openai-chat.ts) falls back to the default
// branch. The host transparently passes the client's raw field name, which is patched here to prevent upstream from ignoring the limit due to unknown fields.
//
// When both fields are provided, max_completion_tokens takes precedence and max_tokens is dropped, preventing
// two limit fields from appearing in the same request. If parsing fails, return as-is and let upstream report an error.
func normalizeMaxTokens(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	value, hasMax := payload["max_tokens"]
	if !hasMax {
		return body
	}
	if _, hasCompletion := payload["max_completion_tokens"]; !hasCompletion {
		payload["max_completion_tokens"] = value
	}
	delete(payload, "max_tokens")
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// chatEndpoint returns the full URL for chat-completions.
//
// Free and paid share the same egress (this is how official v2 splits credentials), so it is no longer tiered by
// whether it is free.
func chatEndpoint(override string) string {
	return BaseURL(override) + ChatPath
}

// extractModelName extracts the model string from the request body.
func extractModelName(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}

// isFreeTierModel determines whether it is a free-tier model.
//
// The judgment criteria align with official 2.1.4: check if the model unit price is 0, **not the model name**.
// Officially, the same dimension uses `!model.cost.some(c => c.input > 0)`
// (packages/core/src/plugin/provider/opencode.ts), but zero-priced models whose names do not contain "free"
// exist in the catalog (such as big-pickle, which is the default model when officially logged out).
// Determining by name would incorrectly push such models onto the paid lane.
//
// Unit price comes from the models.dev catalog; when the catalog is unavailable, it falls back to name-based determination (see
// catalogIsFree). An explicit "-free" suffix is accepted here as a fallback signal.
func isFreeTierModel(model string, catalog modelsDevCatalog) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if free, known := catalogIsFree(catalog, model); known {
		return free
	}
	// big-pickle is the official logged-out default and is free, but its name
	// carries no "free" marker for the offline fallback to see.
	lower := strings.ToLower(model)
	return strings.Contains(lower, "free") || lower == "big-pickle"
}

// catalogIsFree answers "is it free" using the unit price of the model in the catalog.
//
// known=false indicates that the catalog lacks this model (or lacks the cost block), requiring the caller to fall back:
// Missing data cannot be treated as "free" nor as "paid".
func catalogIsFree(catalog modelsDevCatalog, model string) (free bool, known bool) {
	entry, okEntry := catalog[model]
	if !okEntry || entry.Cost == nil {
		return false, false
	}
	return entry.Cost.Input == 0 && entry.Cost.Output == 0, true
}

// prepareFreeTierBody normalizes the free tier request body: forces streaming and declares usage statistics.
//
// **Does not inject any tool definitions**. The official client only passes through tools truly enabled in the current session
// (packages/core/src/session/model-request.ts). There is no branch for "supplementing tools by model"
// across the entire repository; fabricating a toolset contradicts official behavior and adds a dozen
// KB of payload to every request. The caller sends whatever it brings.
func prepareFreeTierBody(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	payload["stream"] = true
	if opt, ok := payload["stream_options"].(map[string]any); ok {
		opt["include_usage"] = true
	} else {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// chatCompletionResponse is used to merge an SSE stream into a complete chat.completion JSON.
type chatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type chatMessage struct {
	Role             string `json:"role"`
	Content          any    `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	ToolCalls        []any  `json:"tool_calls,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type sseDeltaChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []any  `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// CollapseSSEToJSON consumes the upstream SSE stream and assembles it into a complete OpenAI-format chat.completion JSON response.
func CollapseSSEToJSON(r io.Reader, defaultModel string) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	resp := chatCompletionResponse{
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   defaultModel,
		Choices: []chatChoice{
			{
				Index: 0,
				Message: chatMessage{
					Role: "assistant",
				},
				FinishReason: "stop",
			},
		},
	}

	var contentBuilder strings.Builder
	var reasoningBuilder strings.Builder
	var toolCalls []any

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk sseDeltaChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.ID != "" {
			resp.ID = chunk.ID
		}
		if chunk.Model != "" {
			resp.Model = chunk.Model
		}
		if chunk.Created > 0 {
			resp.Created = chunk.Created
		}
		if chunk.Usage != nil {
			resp.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				contentBuilder.WriteString(choice.Delta.Content)
			}
			if choice.Delta.Reasoning != "" {
				reasoningBuilder.WriteString(choice.Delta.Reasoning)
			} else if choice.Delta.ReasoningContent != "" {
				reasoningBuilder.WriteString(choice.Delta.ReasoningContent)
			}
			if len(choice.Delta.ToolCalls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, choice.Delta.ToolCalls)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				resp.Choices[0].FinishReason = *choice.FinishReason
			}
		}
	}

	contentStr := contentBuilder.String()
	resp.Choices[0].Message.Content = contentStr
	resp.Choices[0].Message.ReasoningContent = reasoningBuilder.String()

	toolCalls = filterValidToolCalls(toolCalls)
	if len(toolCalls) > 0 {
		resp.Choices[0].Message.ToolCalls = toolCalls
		resp.Choices[0].FinishReason = "tool_calls"
		if contentStr == "" {
			resp.Choices[0].Message.Content = nil
		}
	}

	return json.Marshal(resp)
}

func mergeToolCalls(accumulated []any, incoming []any) []any {
	for _, raw := range incoming {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		idx := core.IntFromAny(call["index"])
		var target map[string]any
		for _, item := range accumulated {
			if existing, okMap := item.(map[string]any); okMap {
				if core.IntFromAny(existing["index"]) == idx {
					target = existing
					break
				}
			}
		}
		if target == nil {
			target = cloneToolCall(call)
			target["index"] = idx
			accumulated = append(accumulated, target)
			continue
		}
		if id, okID := call["id"].(string); okID && id != "" {
			target["id"] = id
		}
		if tp, okTp := call["type"].(string); okTp && tp != "" {
			target["type"] = tp
		}
		if fnIncoming, okFn := call["function"].(map[string]any); okFn {
			fnTarget, _ := target["function"].(map[string]any)
			if fnTarget == nil {
				fnTarget = map[string]any{}
				target["function"] = fnTarget
			}
			if name, okName := fnIncoming["name"].(string); okName && name != "" {
				fnTarget["name"] = name
			}
			if args, okArgs := fnIncoming["arguments"].(string); okArgs && args != "" {
				prevArgs, _ := fnTarget["arguments"].(string)
				fnTarget["arguments"] = prevArgs + args
			}
		}
	}
	return accumulated
}

func filterValidToolCalls(calls []any) []any {
	var valid []any
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, okFn := call["function"].(map[string]any)
		if !okFn {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		valid = append(valid, call)
	}
	for i := 1; i < len(valid); i++ {
		for j := i; j > 0; j-- {
			left, _ := valid[j-1].(map[string]any)
			right, _ := valid[j].(map[string]any)
			if core.IntFromAny(left["index"]) > core.IntFromAny(right["index"]) {
				valid[j-1], valid[j] = valid[j], valid[j-1]
			}
		}
	}
	return valid
}

func cloneToolCall(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		if k == "function" {
			if fn, ok := v.(map[string]any); ok {
				fnCopy := make(map[string]any, len(fn))
				for fk, fv := range fn {
					fnCopy[fk] = fv
				}
				dst[k] = fnCopy
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// canonicalSessionPattern matches the official OpenCode session format:
// "ses_" + 12 lowercase hex characters + 14 Base62 characters.
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// descendingTimePartPattern matches the official client Identifier.descending() generated
// time part: 48-bit value of ~(ts_ms<<12|counter) in hex. Bitwise NOT sets the lower 12 bits
// to 0xFFF-counter, so the actual time part ends with f[c-f].
//
// **This section is a hard threshold for the free tier**: if the format is correct but random hex fills the time part, the upstream
// still returns 403 FreeTierError—it validates the encoding scheme, not just the length.
var descendingTimePartPattern = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

// sessionIDFor derives the session ID for the current request from the request body.
//
// The same conversation (same initial user message) gets the same session, passing the free tier's
// encoding validation while maintaining upstream prompt cache affinity; falls back to random when no signal can be derived.
func sessionIDFor(body []byte) string {
	return canonicalSessionID(conversationSeed(body))
}

// canonicalSessionID deterministically re-encodes any signal into the official session format.
// Signals already officially minted (correct format and time part encoding) are preserved as-is to avoid breaking affinity.
func canonicalSessionID(signal string) string {
	if canonicalSessionPattern.MatchString(signal) &&
		descendingTimePartPattern.MatchString(signal[4:16]) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := descendingTimePart(sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// conversationSeed takes the serialized content of the first user message as the conversation seed.
//
// Uses the "beginning" instead of the entire history: multi-turn conversations grow longer, and using the full text as a seed would cause
// every turn of the same conversation to change its session, preventing prompt cache hits.
func conversationSeed(body []byte) string {
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return randomSessionSignal()
	}
	for _, message := range payload.Messages {
		if message.Role != "user" || message.Content == nil {
			continue
		}
		encoded, errMarshal := json.Marshal(message.Content)
		if errMarshal == nil && string(encoded) != "null" {
			return string(encoded)
		}
	}
	if trimmed := strings.TrimSpace(payload.PreviousResponseID); trimmed != "" {
		return trimmed
	}
	return randomSessionSignal()
}

// randomSessionSignal provides a one-time seed when the request body provides no signal.
func randomSessionSignal() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// When crypto/rand fails, a valid session must still be produced; this only affects cache affinity.
		return "fallback"
	}
	return hex.EncodeToString(buf)
}

// descendingTimePart renders six hash bytes into the official Identifier.descending()
// layout: 36-bit timestamp pushed to the high bits of a 48-bit word, OR'd with counter 1, then bitwise NOT'd,
// so the lower 12 bits read as ffe.
func descendingTimePart(hash []byte) string {
	current := (uint64(hash[0])<<28 | uint64(hash[1])<<20 | uint64(hash[2])<<12 |
		uint64(hash[3])<<4 | uint64(hash[4])>>4) << 12
	current |= 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return hex.EncodeToString([]byte{
		byte(inverted >> 40), byte(inverted >> 32), byte(inverted >> 24),
		byte(inverted >> 16), byte(inverted >> 8), byte(inverted),
	})
}

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	remainder := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}

// base62Alphabet is the Base62 character set.
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// bytesReader wraps a byte slice into an io.Reader.
func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

// This file defines the upstream endpoints and request headers for OpenCode ZEN.
//
// Endpoints are taken from the model catalog of the official 2.1.4 client (models.dev provider "opencode"
// api field): both paid and free models use the inference lane, with the only difference being credentials—free uses
// anonymous public, and paid uses user key. The historical https://opencode.ai/zen was merely a web
// path and usage query endpoint, and is no longer the inference base address.
//
// **This plugin only handles chat-completions** (Claude / Codex clients are translated by the host),
// so only the chat path is retained here. The upstream also provides /v1/responses, /v1/messages,
// /v1/systemone: those are covered by multi-protocol translation on the host side, so the plugin does not implement them repeatedly.

const (
	// DefaultBaseURL is the upstream base URL for chat-completions.
	DefaultBaseURL = "https://opencode.ai/inference/openai"
	// ChatPath is the path for chat-completions.
	ChatPath = "/v1/chat/completions"
	// ModelsBaseURL is the base URL for the model list, which has one less /openai layer than chat:
	// /inference/openai/v1/models returns 404 in practice; the list is at /inference/v1/models.
	ModelsBaseURL = "https://opencode.ai/inference"
	// ModelsPath is the path for the model list.
	ModelsPath = "/v1/models"
	// freeTierCredential is the anonymous credential used by the official client for the free tier:
	// Paid keys will be rejected on this lane (observed 401 Missing API key in practice).
	freeTierCredential = "public"

	// clientVersion is the spoofed OpenCode client version, aligned with v2 stable.
	//
	// The value comes from the official repository packages/cli/package.json (2.1.4), not guessed;
	// 1.x has been rejected by upstream (see issue #43).
	clientVersion = "2.1.4"
	// clientChannel is the official stable channel name, see official packages/script/src/index.ts:
	// The stable channel CHANNEL is always "latest", and other values are preview channels.
	clientChannel = "latest"
	// clientName is the client identifier for the official CLI (shared by x-opencode-client and UA).
	clientName = "cli"
	// userAgentFormat is the UA format of v2: opencode/<channel>/<version>/<name>,
	// see App.useragent in official packages/core/src/app.ts. The two-segment 1.x
	// "opencode/<version> (<os> <arch>; <go version>)" is no longer recognized.
	userAgentFormat = "opencode/%s/%s/%s"
)

// BaseURL returns the upstream base URL for chat-completions (without trailing slash).
//
// Allows configuration override: no need to change code when the upstream changes domain or users want to use a self-hosted gateway.
func BaseURL(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return DefaultBaseURL
	}
	return trimmed
}

// ModelsBaseURLFor returns the upstream base URL for the model list (without trailing slash).
//
// Different layer from chat endpoint: chat is at /inference/openai, manifest is at /inference.
// When the override value only specifies the chat layer (including the /openai suffix), the model list layer strips this layer
// so that a self-hosted gateway still only requires a single configuration item.
func ModelsBaseURLFor(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return ModelsBaseURL
	}
	return strings.TrimSuffix(trimmed, "/openai")
}

// UserAgent returns the UA spoofed as the official OpenCode client.
//
// The format matches the official v2 client: opencode/<channel>/<version>/<name>.
func UserAgent() string {
	return fmt.Sprintf(userAgentFormat, clientChannel, clientVersion, clientName)
}

// ApplyChatHeaders writes all headers required for chat-completions requests.
//
// Authentication and client identification are **hard thresholds**: the ZEN upstream determines
// request sources by x-opencode-client and UA, and missing them will be rejected as an unknown client.
func ApplyChatHeaders(req *http.Request, apiKey, sessionID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("x-opencode-client", clientName)
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-opencode-session-id", sessionID)
	req.Header.Set("x-session-affinity", sessionID)
	req.Header.Set("X-Session-Id", sessionID)
	req.Header.Set("x-opencode-project", "global")
}

// ApplyModelsHeaders writes the headers for model list requests (authentication only).
func ApplyModelsHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("x-opencode-client", clientName)
}
