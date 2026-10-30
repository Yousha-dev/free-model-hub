package zcode

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// Chat forwarding: Two-way conversion between OpenAI chat-completions and Anthropic Messages.
//
// The upstream uses the Anthropic Messages protocol (model names are case-sensitive), so this layer's responsibility is
// to translate the chat-completions request passed from the host and translate the response back. Refer to
// zcode2api/app/openai_compat.py and app/body_transform.py.
//
// Tool calling is mandatory rather than an enhancement: The plugin declaration only handles chat-completions, and Claude Code /
// Codex tool calls are all converted through this. Without it, only plain text QA remains.

// ChatRequest is the input for a chat forwarding operation.
type ChatRequest struct {
	Credential *Credential
	// Body is the chat-completions request body translated by the host.
	Body   []byte
	Stream bool
}

// Chat executes a non-streaming chat and returns a full response in OpenAI format.
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	httpReq, model, errBuild := buildHTTPRequest(ctx, baseURL, req)
	if errBuild != nil {
		return nil, errBuild
	}

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, transientErr("zcode chat request failed: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, transientErr("read zcode response: " + errRead.Error())
	}
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	// The upstream sometimes wraps business errors in HTTP 200, which cannot be treated as a successful response.
	if errUpstream := classifyEmbeddedError(body); errUpstream != nil {
		return nil, errUpstream
	}
	return anthropicToOpenAICompletion(body, model)
}

// ChatStream executes a streaming chat and delivers OpenAI chunks one by one.
func ChatStream(ctx context.Context, baseURL string, req ChatRequest, onChunk func([]byte) error) error {
	httpReq, model, errBuild := buildHTTPRequest(ctx, baseURL, req)
	if errBuild != nil {
		return errBuild
	}

	// The total duration of a streaming response is unpredictable; timeouts are delegated to upstream idle control.
	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return transientErr("zcode stream request failed: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Classify(resp.StatusCode, string(body))
	}

	// The upstream sometimes ignores the stream parameter and returns a full JSON document. Validation errors must be intercepted here,
	// otherwise the SSE parser will treat it as an empty stream—resulting in the client receiving "zero content" with no visible reason.
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "event-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if errUpstream := classifyEmbeddedError(body); errUpstream != nil {
			return errUpstream
		}
		// Normal response: converted as non-streaming, then split and delivered as individual content chunks.
		completion, errConvert := anthropicToOpenAICompletion(body, model)
		if errConvert != nil {
			return errConvert
		}
		return emitCollapsedCompletion(completion, model, onChunk)
	}

	converter := newStreamConverter(model)
	if errStart := converter.emitStart(onChunk); errStart != nil {
		return errStart
	}
	if errParse := parseAnthropicSSE(resp.Body, converter, onChunk); errParse != nil {
		return errParse
	}
	// Compatible with upstream closing the stream without sending message_stop: Symmetric with emitStart, the caller holding
	// the stream lifecycle completes the final chunk. emitFinish is idempotent, so normal completion paths won't send repeatedly.
	return converter.emitFinish("", onChunk)
}

// ---- Start Plan Outbound Gate ----

// planQueueWaitTimeout is the upper limit for queuing to wait for in-flight chats of the same account to clear. Covers typical streaming
// response lengths; if exceeded, it returns to the host via SoftRate for route switching without hanging the request.
const planQueueWaitTimeout = 90 * time.Second

// errPlanQueueTimeout indicates that the queue limit was exceeded and outbound eligibility was not acquired.
var errPlanQueueTimeout = errors.New("zcode start plan queue timeout")

// chatGate serializes chat outbounds for the Start Plan channel per account.
//
// Why it is needed: Start Plan is a single-user interactive plan, and the upstream explicitly declared
// concurrency limits via 3008/3009/3010. High-frequency concurrency also triggers 3012 "unusual activity"; official clients are naturally
// serial (one user typing at a time). This plugin directly pumps concurrent agent requests forwarded by the host into the upstream,
// which is equivalent to cramming dozens of sessions into a single interactive plan—the upstream rejects this by design. At any moment,
// only one in-flight chat is allowed for the same account, and queued requests wait for the previous one to clear before going outbound, matching official client behavior.
//
// This is not bypassing risk control: the upstream declares concurrency limits via business error codes, and it is standard practice
// for clients to respect and queue for the limits they receive; when the queue times out, it returns to the host via SoftRate for route switching.
//
// State belongs to the vendor instance (same convention as claimGate): independent when multiple Apps coexist.
type chatGate struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

// newChatGate constructs an instance-level gate.
func newChatGate() *chatGate {
	return &chatGate{slots: map[string]chan struct{}{}}
}

// acquire obtains outbound eligibility for the account and returns a return function: returns immediately if there are no in-flight chats; otherwise
// bounded queuing exits early and avoids occupying the queue when the request context is canceled (host disconnects client).
func (g *chatGate) acquire(ctx context.Context, accountKey string) (func(), error) {
	slot := g.slot(accountKey)
	timer := time.NewTimer(planQueueWaitTimeout)
	defer timer.Stop()
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errPlanQueueTimeout
	}
}

// slot returns the gate corresponding to the account. The number of accounts is bounded by core.MaxAccountsPerVendor, no recycling needed.
func (g *chatGate) slot(accountKey string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	slot, ok := g.slots[accountKey]
	if !ok {
		slot = make(chan struct{}, 1)
		g.slots[accountKey] = slot
	}
	return slot
}

// holdStartPlanSlot acquires Start Plan outbound eligibility for a chat. The return value is guaranteed to be a
// callable function upon success, facilitating defer. Pure API Key channels are not bound by this concurrency limit and are allowed directly.
func (v *adapter) holdStartPlanSlot(ctx context.Context, cred *Credential) (func(), error) {
	if cred == nil || !cred.UsesPlanChannel() {
		return func() {}, nil
	}
	release, errAcquire := v.gate.acquire(ctx, cred.AccountKey())
	if errAcquire == nil {
		return release, nil
	}
	if errors.Is(errAcquire, errPlanQueueTimeout) {
		return nil, &core.Failure{
			Kind:   core.FailureKindSoftRate,
			Status: 0,
			Msg:    "zcode Start Plan channel concurrent queue timeout (in-flight chats for the same account have not cleared), returned to host for route switching",
		}
	}
	// Host already disconnected: no retry, and not counted as an upstream failure.
	return nil, &core.Failure{
		Kind:   core.FailureKindClient,
		Status: 0,
		Msg:    "zcode chat cancelled while queued: " + errAcquire.Error(),
	}
}

// emitCollapsedCompletion folds a non-streaming response into a sequence of OpenAI chunks.
//
// Fallback path when upstream ignores the stream parameter: the client sent a streaming request and must still receive
// Complete chunk sequence (including the end chunk), otherwise it will wait indefinitely for subsequent content.
func emitCollapsedCompletion(completion []byte, model string, onChunk func([]byte) error) error {
	var resp struct {
		Usage   map[string]any `json:"usage"`
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal(completion, &resp); errUnmarshal != nil || len(resp.Choices) == 0 {
		return transientErr("upstream returned a non-streaming response that could not be parsed")
	}

	converter := newStreamConverter(model)
	if errStart := converter.emitStart(onChunk); errStart != nil {
		return errStart
	}
	message := resp.Choices[0].Message
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		if errEmit := onChunk(converter.chunk(map[string]any{"reasoning_content": reasoning}, nil)); errEmit != nil {
			return errEmit
		}
	}
	if content, ok := message["content"].(string); ok && content != "" {
		chunk := converter.chunk(map[string]any{"content": content}, nil)
		if errEmit := onChunk(chunk); errEmit != nil {
			return errEmit
		}
	}
	if toolCalls, ok := message["tool_calls"].([]any); ok {
		for index, item := range toolCalls {
			call, _ := item.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			delta := map[string]any{"tool_calls": []any{map[string]any{
				"index": index,
				"id":    core.RawStringField(call, "id"),
				"type":  "function",
				"function": map[string]any{
					"name":      core.RawStringField(fn, "name"),
					"arguments": core.RawStringField(fn, "arguments"),
				},
			}}}
			if errEmit := onChunk(converter.chunk(delta, nil)); errEmit != nil {
				return errEmit
			}
		}
	}
	reason := core.FirstNonEmpty(resp.Choices[0].FinishReason, "stop")
	return onChunk(converter.chunkWithUsage(map[string]any{}, &reason, resp.Usage))
}

// buildHTTPRequest converts a chat-completions request body into an upstream Anthropic request.
func buildHTTPRequest(ctx context.Context, baseURL string, req ChatRequest) (*http.Request, string, error) {
	if req.Credential == nil {
		return nil, "", fmt.Errorf("zcode credential is nil")
	}

	anthropicBody, model, errConvert := openAIToAnthropicBody(req.Body, req.Stream, req.Credential)
	if errConvert != nil {
		return nil, "", errConvert
	}

	target, errTarget := resolveTargetURL(baseURL, req.Credential)
	if errTarget != nil {
		return nil, "", errTarget
	}

	httpReq, errNew := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(anthropicBody))
	if errNew != nil {
		return nil, "", fmt.Errorf("create http request: %w", errNew)
	}
	applyChatHeaders(httpReq, req.Credential)
	return httpReq, model, nil
}

// resolveTargetURL determines which channel to use for this chat session.
//
// Prioritize the Start Plan channel (zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages, Bearer JWT);
// Accounts with only an API Key use the ZCode official gateway forwarding channel (zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages,
// aligning with the official client official-coding-plan-gateway.ts to prevent DNS resolution failures when directly connecting to api.z.ai under CN networks).
func resolveTargetURL(baseURL string, cred *Credential) (string, error) {
	if cred == nil {
		return "", fmt.Errorf("zcode credential is nil")
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if cred.UsesPlanChannel() {
		if base == "" {
			base = ZCodeOrigin
		}
		return base + PathPlanMessages, nil
	}
	if strings.TrimSpace(cred.APIKey) != "" {
		if base == "" {
			return ZCodeOrigin + PathUltraZAIMessages, nil
		}
		return base + PathAPIMessages, nil
	}
	return "", fmt.Errorf("zcode: credential missing both jwt_token and api_key")
}

// applyChatHeaders assembles outbound request headers: auth + identity header + tracking header.
func applyChatHeaders(req *http.Request, cred *Credential) {
	headers := map[string]string{
		"Content-Type":      "application/json",
		"Accept":            "text/event-stream, application/json",
		"anthropic-version": AnthropicVersion,
	}
	if cred.UsesPlanChannel() {
		headers["Authorization"] = "Bearer " + cred.JWTToken
		headers["x-api-key"] = cred.JWTToken
	} else if strings.TrimSpace(cred.APIKey) != "" {
		headers["x-api-key"] = cred.APIKey
		headers["Authorization"] = "Bearer " + cred.APIKey
	}
	BuildIdentityHeaders(cred.Profile).Apply(headers)
	for key, value := range BuildTraceHeaders() {
		headers[key] = value
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
}

// openAIToAnthropicBody converts an OpenAI request body into the Anthropic Messages format.
//
// Overrides: system/developer extraction, multimodal content chunking, tool_calls → tool_use,
// role=tool → tool_result, tools/tool_choice mapping, and the body transformations
// required by the Plan channel (system identity block / cache_control / metadata.user_id).
func openAIToAnthropicBody(src []byte, stream bool, cred *Credential) ([]byte, string, error) {
	var in struct {
		Model               string   `json:"model"`
		Messages            []any    `json:"messages"`
		MaxTokens           int      `json:"max_tokens"`
		MaxCompletionTokens int      `json:"max_completion_tokens"`
		Temperature         *float64 `json:"temperature,omitempty"`
		TopP                *float64 `json:"top_p,omitempty"`
		Stream              bool     `json:"stream"`
		Stop                any      `json:"stop"`
		Tools               []any    `json:"tools"`
		ToolChoice          any      `json:"tool_choice"`
	}
	if errUnmarshal := json.Unmarshal(src, &in); errUnmarshal != nil {
		return nil, "", fmt.Errorf("decode chat request: %w", errUnmarshal)
	}

	model := NormalizeModelName(in.Model)
	if model == "" {
		return nil, "", fmt.Errorf("zcode: model is required")
	}

	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = in.MaxCompletionTokens
	}
	if maxTokens <= 0 || maxTokens > MaxTokensLimit {
		// Clients (such as auto-compact resume) may carry out-of-bounds values, and the upstream will return 400/1210, so clamp them uniformly.
		maxTokens = DefaultMaxTokens
	}

	systemParts, outMessages := convertMessages(in.Messages)

	out := map[string]any{
		"model":      model,
		"messages":   outMessages,
		"max_tokens": maxTokens,
		"stream":     stream,
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if stopSequences := normalizeStop(in.Stop); len(stopSequences) > 0 {
		out["stop_sequences"] = stopSequences
	}
	applyTools(out, in.Tools, in.ToolChoice)

	applyPlanBodyTransforms(out, model, cred)

	encoded, errMarshal := json.Marshal(out)
	if errMarshal != nil {
		return nil, "", fmt.Errorf("encode anthropic request: %w", errMarshal)
	}
	return encoded, model, nil
}

// convertMessages splits OpenAI messages into (system text, Anthropic messages).
func convertMessages(messages []any) ([]string, []map[string]any) {
	var systemParts []string
	outMessages := make([]map[string]any, 0, len(messages))

	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		switch strings.TrimSpace(role) {
		case "system", "developer":
			if text := textFromContent(content); strings.TrimSpace(text) != "" {
				systemParts = append(systemParts, text)
			}

		case "tool":
			// Tool results are tool_result blocks under the user message in the Anthropic protocol; consecutive tool/user messages are merged.
			outMessages = appendAnthropicMessage(outMessages, "user", []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": strings.TrimSpace(core.RawStringField(msg, "tool_call_id")),
				"content":     textFromContent(content),
			}})

		case "assistant":
			outMessages = appendAnthropicMessage(outMessages, "assistant", assistantBlocks(content, msg["tool_calls"]))

		default:
			outMessages = appendAnthropicMessage(outMessages, "user", userBlocks(content))
		}
	}
	return systemParts, outMessages
}

// appendAnthropicMessage appends an Anthropic message; if it has the same role as the previous one, merge their content blocks
// to satisfy the strict alternation requirement for user / assistant in the Anthropic Messages protocol (e.g., concurrent tools returning multiple tool results).
func appendAnthropicMessage(out []map[string]any, role string, blocks []any) []map[string]any {
	if len(blocks) == 0 {
		return out
	}
	if n := len(out); n > 0 && core.RawStringField(out[n-1], "role") == role {
		if prev, ok := out[n-1]["content"].([]any); ok {
			out[n-1]["content"] = append(prev, blocks...)
			return out
		}
	}
	return append(out, map[string]any{
		"role":    role,
		"content": blocks,
	})
}

// assistantBlocks assembles the assistant's content blocks (text + tool_use).
func assistantBlocks(content any, rawToolCalls any) []any {
	blocks := make([]any, 0, 4)
	if text := textFromContent(content); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	toolCalls, _ := rawToolCalls.([]any)
	for _, item := range toolCalls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name := strings.TrimSpace(core.RawStringField(fn, "name"))
		if name == "" {
			continue
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    core.FirstNonEmpty(core.RawStringField(call, "id"), "toolu_"+NewHexID(12)),
			"name":  name,
			"input": parseToolArguments(fn["arguments"]),
		})
	}
	if len(blocks) == 0 {
		// Anthropic requires content to be non-empty.
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks
}

// userBlocks assembles the user's content blocks (text + images).
func userBlocks(content any) []any {
	switch v := content.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": v}}
	case []any:
		blocks := make([]any, 0, len(v))
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch core.RawStringField(part, "type") {
			case "text":
				blocks = append(blocks, map[string]any{"type": "text", "text": core.RawStringField(part, "text")})
			case "image_url":
				if block := imageBlock(part["image_url"]); block != nil {
					blocks = append(blocks, block)
				}
			}
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}
		return blocks
	default:
		return []any{map[string]any{"type": "text", "text": ""}}
	}
}

// imageBlock converts an OpenAI image_url into an Anthropic image block.
//
// Only data: URLs are supported: external image links require the gateway to download them itself, as the upstream does not accept direct references.
func imageBlock(raw any) map[string]any {
	url := ""
	switch v := raw.(type) {
	case string:
		url = v
	case map[string]any:
		url = core.RawStringField(v, "url")
	}
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	head, data, found := strings.Cut(url, ",")
	if !found || strings.TrimSpace(data) == "" {
		return nil
	}
	mediaType := strings.TrimPrefix(head, "data:")
	if idx := strings.Index(mediaType, ";"); idx >= 0 {
		mediaType = mediaType[:idx]
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		},
	}
}

// applyTools maps OpenAI tools / tool_choice to Anthropic format.
func applyTools(out map[string]any, tools []any, toolChoice any) {
	if len(tools) > 0 {
		mapped := make([]any, 0, len(tools))
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tool["function"].(map[string]any)
			if fn == nil {
				fn = tool
			}
			name := strings.TrimSpace(core.RawStringField(fn, "name"))
			if name == "" {
				continue
			}
			entry := map[string]any{"name": name}
			if desc := core.RawStringField(fn, "description"); desc != "" {
				entry["description"] = desc
			}
			if schema, ok := fn["parameters"].(map[string]any); ok && schema != nil {
				entry["input_schema"] = schema
			} else {
				entry["input_schema"] = map[string]any{"type": "object"}
			}
			mapped = append(mapped, entry)
		}
		if len(mapped) > 0 {
			out["tools"] = mapped
		}
	}

	switch choice := toolChoice.(type) {
	case string:
		switch choice {
		case "none":
			delete(out, "tools")
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		}
		// "auto" / default: Anthropic defaults to auto, no explicit setting needed.
	case map[string]any:
		fn, _ := choice["function"].(map[string]any)
		if name := strings.TrimSpace(core.RawStringField(fn, "name")); name != "" {
			out["tool_choice"] = map[string]any{"type": "tool", "name": name}
		}
	}
}

// applyPlanBodyTransforms applies the request body transformations required by the Plan channel.
//
// The upstream gateway performs content moderation, and lacking the official identity block will result in rejection as 3012; cache_control and
// metadata.user_id are standard behaviors of the official client. These transformations are only applied when a JWT is present—
// the API Key channel is not the official client format, and injecting them would instead create conflicting signals.
func applyPlanBodyTransforms(body map[string]any, model string, cred *Credential) {
	if cred == nil || !cred.UsesPlanChannel() {
		return
	}
	applyStartPlanSystem(body, model)
	applyCacheControl(body)
	if userID := core.FirstNonEmpty(cred.UserID, UserIDFromJWT(cred.JWTToken)); userID != "" {
		metadata, _ := body["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["user_id"] = userID
		body["metadata"] = metadata
	}
}

// anthropicToOpenAICompletion converts an Anthropic response into an OpenAI chat.completion.
func anthropicToOpenAICompletion(raw []byte, model string) ([]byte, error) {
	var resp struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		Usage anthropicUsage `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", errUnmarshal)
	}

	var text strings.Builder
	var reasoning strings.Builder
	var toolCalls []any
	for _, block := range resp.Content {
		switch block.Type {
		case "thinking":
			reasoning.WriteString(block.Thinking)
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, buildToolCall(block.ID, block.Name, block.Input))
		}
	}

	message := map[string]any{"role": "assistant"}
	if text.Len() > 0 {
		message["content"] = text.String()
	} else {
		message["content"] = nil
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	out := map[string]any{
		"id":      core.FirstNonEmpty(resp.ID, "chatcmpl-"+NewHexID(12)),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   core.FirstNonEmpty(resp.Model, model),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason(resp.StopReason, len(toolCalls) > 0),
		}},
	}
	if usage := resp.Usage.openAI(); len(usage) > 0 {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

// buildToolCall assembles tool calls in the OpenAI format.
func buildToolCall(id, name string, rawInput json.RawMessage) map[string]any {
	input := "{}"
	if len(rawInput) > 0 {
		input = string(rawInput)
	}
	return map[string]any{
		"id":   core.FirstNonEmpty(id, "toolu_"+NewHexID(12)),
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": input,
		},
	}
}

// finishReason maps Anthropic stop_reason to OpenAI finish_reason.
func finishReason(stopReason string, hasToolCalls bool) string {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}

// streamConverter is a stateful Anthropic SSE to OpenAI chunk converter.
//
// Tool calls arrive in two parts during streaming: content_block_start provides id/name,
// and subsequent input_json_delta provides argument fragments in batches, so an index mapping must be maintained.
type streamConverter struct {
	model     string
	chunkID   string
	created   int64
	toolIndex int
	usage     anthropicUsage
	finished  bool
}

// anthropicUsage stores cumulative Anthropic counts. input_tokens does not include cache reads and writes,
// so both must be included when converted to OpenAI prompt_tokens; cache details are a subset and are not accumulated additionally.
// Pointers are used to distinguish between missing fields and explicit zeros.
type anthropicUsage struct {
	InputTokens         *int64 `json:"input_tokens"`
	OutputTokens        *int64 `json:"output_tokens"`
	CacheReadTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens *int64 `json:"cache_creation_input_tokens"`
}

func (u *anthropicUsage) merge(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	var update anthropicUsage
	if json.Unmarshal(data, &update) != nil {
		return
	}
	// message_delta counts are cumulative snapshots and cannot be added as increments.
	if update.InputTokens != nil {
		u.InputTokens = update.InputTokens
	}
	if update.OutputTokens != nil {
		u.OutputTokens = update.OutputTokens
	}
	if update.CacheReadTokens != nil {
		u.CacheReadTokens = update.CacheReadTokens
	}
	if update.CacheCreationTokens != nil {
		u.CacheCreationTokens = update.CacheCreationTokens
	}
}

func (u anthropicUsage) openAI() map[string]any {
	if u.InputTokens == nil && u.OutputTokens == nil && u.CacheReadTokens == nil && u.CacheCreationTokens == nil {
		return nil
	}
	value := func(p *int64) int64 {
		if p != nil {
			return *p
		}
		return 0
	}
	read, write := value(u.CacheReadTokens), value(u.CacheCreationTokens)
	input, output := value(u.InputTokens)+read+write, value(u.OutputTokens)
	out := map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
	if u.CacheReadTokens != nil || u.CacheCreationTokens != nil {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": read, "cache_creation_tokens": write}
	}
	return out
}

func newStreamConverter(model string) *streamConverter {
	return &streamConverter{
		model:   model,
		chunkID: "chatcmpl-" + NewHexID(12),
		created: time.Now().Unix(),
	}
}

// emitStart delivers the first chunk (carrying the role).
func (c *streamConverter) emitStart(onChunk func([]byte) error) error {
	return onChunk(c.chunk(map[string]any{"role": "assistant"}, nil))
}

// feed processes an upstream event, yielding zero to multiple chunks.
func (c *streamConverter) feed(event map[string]any, onChunk func([]byte) error) error {
	switch core.RawStringField(event, "type") {
	case "message_start":
		if msg, ok := event["message"].(map[string]any); ok {
			if id := core.RawStringField(msg, "id"); id != "" {
				c.chunkID = id
			}
			if model := core.RawStringField(msg, "model"); model != "" {
				c.model = model
			}
			c.usage.merge(msg["usage"])
		}

	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		if block != nil && core.RawStringField(block, "type") == "tool_use" {
			index := c.toolIndex
			c.toolIndex++
			return onChunk(c.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": index,
					"id":    core.RawStringField(block, "id"),
					"type":  "function",
					"function": map[string]any{
						"name":      core.RawStringField(block, "name"),
						"arguments": "",
					},
				}},
			}, nil))
		}

	case "content_block_delta":
		delta, _ := event["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		switch core.RawStringField(delta, "type") {
		case "thinking_delta":
			if thinking := core.RawStringField(delta, "thinking"); thinking != "" {
				return onChunk(c.chunk(map[string]any{"reasoning_content": thinking}, nil))
			}
		case "text_delta":
			if text := core.RawStringField(delta, "text"); text != "" {
				return onChunk(c.chunk(map[string]any{"content": text}, nil))
			}
		case "input_json_delta":
			if partial := core.RawStringField(delta, "partial_json"); partial != "" {
				return onChunk(c.chunk(map[string]any{
					"tool_calls": []any{map[string]any{
						"index":    core.MaxInt(c.toolIndex-1, 0),
						"function": map[string]any{"arguments": partial},
					}},
				}, nil))
			}
		}

	case "message_delta":
		c.usage.merge(event["usage"])
		delta, _ := event["delta"].(map[string]any)
		stop := core.RawStringField(delta, "stop_reason")
		if stop == "" {
			return nil
		}
		return c.emitFinish(stop, onChunk)

	case "message_stop":
		// Partially compatible with upstream providing only usage without stop_reason; complete the final chunk upon explicit completion.
		// The normal path already sent the final chunk in message_delta, and emitFinish ensures it is not duplicated.
		return c.emitFinish("", onChunk)

	case "error":
		errObj, _ := event["error"].(map[string]any)
		return fmt.Errorf("upstream streaming error: %s", core.FirstNonEmpty(core.RawStringField(errObj, "message"), "unknown"))
	}
	return nil
}

// emitFinish delivers the final chunk only once; it does not forge zero counts when upstream has no usage.
// This is the CPA internal executor payload: the host collects usage first, then performs protocol conversion.
// Delivers the full counts and finish_reason in the same chunk, without sending intermediate choices:[] snapshots;
// The public output policy for client include_usage remains decided by the host.
func (c *streamConverter) emitFinish(stop string, onChunk func([]byte) error) error {
	if c.finished {
		return nil
	}
	reason := finishReason(stop, c.toolIndex > 0)
	if err := onChunk(c.chunkWithUsage(map[string]any{}, &reason, c.usage.openAI())); err != nil {
		return err
	}
	c.finished = true
	return nil
}

// chunk assembles an OpenAI chat.completion.chunk.
func (c *streamConverter) chunk(delta map[string]any, finishReason *string) []byte {
	return c.chunkWithUsage(delta, finishReason, nil)
}

func (c *streamConverter) chunkWithUsage(delta map[string]any, finishReason *string, usage map[string]any) []byte {
	var finish any
	if finishReason != nil {
		finish = *finishReason
	}
	payload := map[string]any{
		"id":      c.chunkID,
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if len(usage) > 0 {
		payload["usage"] = usage
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

// parseAnthropicSSE parses upstream SSE line by line and passes it to the converter.
func parseAnthropicSSE(r io.Reader, converter *streamConverter, onChunk func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	// Single events can be very large (large text_delta / base64 images), so relax the limit to 8MB.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if errUnmarshal := json.Unmarshal([]byte(data), &event); errUnmarshal != nil {
			continue
		}
		if errFeed := converter.feed(event, onChunk); errFeed != nil {
			return errFeed
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return transientErr("read zcode stream: " + errScan.Error())
	}
	return nil
}

// classifyEmbeddedError identifies business errors wrapped in HTTP 200.
//
// Upstream still returns 200 + {"code":N,...} for some errors, and treating them as successful responses
// would spit the error body out to the client as model output.
func classifyEmbeddedError(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var envelope struct {
		Type  string `json:"type"`
		Code  int    `json:"code"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(trimmed, &envelope); errUnmarshal != nil {
		return nil
	}
	// Anthropic error envelope: {"type":"error","error":{...}}
	if envelope.Type == "error" && envelope.Error.Type != "" {
		return Classify(http.StatusBadRequest, string(trimmed))
	}
	// Business code envelope: has a code that is non-zero, and does not contain Anthropic response flags (content).
	if envelope.Code != codeSuccess && envelope.Code != 0 && !bytes.Contains(trimmed, []byte(`"content"`)) {
		return Classify(http.StatusOK, string(trimmed))
	}
	return nil
}

// parseToolArguments parses tool arguments into an object; invalid JSON is wrapped in _raw to preserve the original text.
func parseToolArguments(raw any) any {
	switch v := raw.(type) {
	case map[string]any:
		return v
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return map[string]any{}
		}
		var parsed map[string]any
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &parsed); errUnmarshal == nil {
			return parsed
		}
		return map[string]any{"_raw": v}
	default:
		return map[string]any{}
	}
}

// normalizeStop normalizes the stop parameter to Anthropic's stop_sequences.
func normalizeStop(stop any) []string {
	switch v := stop.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// textFromContent extracts plain text from OpenAI content (string or array of chunks).
func textFromContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if core.RawStringField(part, "type") == "text" {
				if text := core.RawStringField(part, "text"); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// chatHTTPTimeout is the upper limit for non-streaming chat.
const chatHTTPTimeout = 10 * time.Minute

// transientErr wraps local/transport-layer failures into a shared upstream error type.
//
// Difference from business errors produced by Classify: Status is 0, so HTTPStatus() falls back
// to KindServer's 502—the host will retry with another account, but will not mark the credential as bad due to a partial response.
// Reusing core.Failure is deliberate: core.ToPluginError uses core.UpstreamError
// to recognize this classification vocabulary,
// and custom types would be downgraded to a generic 502 and lose all semantics (see notes in errors.go).
func transientErr(msg string) *core.Failure {
	return &core.Failure{Kind: core.FailureKindServer, Status: 0, Msg: msg}
}

// Request body transformations for the Plan channel (aligning with official client behavior).
//
// The role and necessity of the three transformations:
//  1. System identity block: Upstream gateway performs content review, and requests missing the official identity block
//     will be rejected as 3012 "unusual activity"; the official client consistently prepends these three blocks.
//  2. cache_control: Appends an ephemeral marker to the last block of the last non-system message.
//     Upstream silently ignores this when below the cache threshold, so unconditional appending is safe.
//  3. metadata.user_id: The official client injects the logged-in user identifier.
//
// All transformations remain no-op for malformed input: transformation failure must never be magnified into a request failure.
// Refer to zcode2api/app/body_transform.py.

//go:embed assets/zcode_system.json
var zcodeSystemJSON []byte

type systemBlockTemplate struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

var (
	zcodeSystemOnce   sync.Once
	zcodeSystemBlocks []systemBlockTemplate
)

// systemBlocks returns the official identity block template (empty if parsing fails, skipping injection in that case).
func systemBlocks() []systemBlockTemplate {
	zcodeSystemOnce.Do(func() {
		var blocks []systemBlockTemplate
		if errUnmarshal := json.Unmarshal(zcodeSystemJSON, &blocks); errUnmarshal != nil {
			blocks = nil
		}
		zcodeSystemBlocks = blocks
	})
	return zcodeSystemBlocks
}

// applyStartPlanSystem assembles a three-part system (cli_prefix / stable / dynamic) according to the ZCode ContextBuilder specification.
//
// The upstream gateway strictly validates this three-part structure: appending a fourth system block triggers an HTTP 405 code=3012 interception.
// Therefore, client-provided system prompts are embedded into the third dynamic block according to the official Output Style slot.
// Idempotent: returns directly if the first block is already the official identifier, and repeated calls will not repeatedly concatenate.
func applyStartPlanSystem(body map[string]any, model string) {
	blocks := systemBlocks()
	if len(blocks) == 0 {
		return
	}

	existing := body["system"]
	if list, ok := existing.([]any); ok && len(list) > 0 {
		if first, ok := list[0].(map[string]any); ok {
			if core.RawStringField(first, "text") == blocks[0].Text {
				return
			}
		}
	}

	modelLine := ""
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		modelLine = "\n- You are powered by the model named account:zai-start-plan/" + trimmed + "."
	}
	outputStyle := ""
	if userSys := extractUserSystemText(existing); userSys != "" {
		outputStyle = "\n\n# Output Style: custom\n" + userSys
	}

	official := make([]any, 0, len(blocks))
	for _, tmpl := range blocks {
		text := strings.ReplaceAll(tmpl.Text, "{{MODEL_LINE}}", modelLine)
		text = strings.ReplaceAll(text, "{{OUTPUT_STYLE}}", outputStyle)
		entry := map[string]any{
			"type": tmpl.Type,
			"text": text,
		}
		if len(tmpl.CacheControl) > 0 {
			cc := make(map[string]any, len(tmpl.CacheControl))
			for k, v := range tmpl.CacheControl {
				cc[k] = v
			}
			entry["cache_control"] = cc
		}
		official = append(official, entry)
	}
	body["system"] = official
}

// extractUserSystemText extracts the original client system text.
func extractUserSystemText(system any) string {
	switch v := system.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch block := item.(type) {
			case string:
				if s := strings.TrimSpace(block); s != "" {
					parts = append(parts, s)
				}
			case map[string]any:
				if core.RawStringField(block, "type") == "text" {
					if s := strings.TrimSpace(core.RawStringField(block, "text")); s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// applyCacheControl adds the ephemeral cache marker to the last block of the last non-system message.
//
// Idempotent: does nothing if cache_control already exists.
func applyCacheControl(body map[string]any) {
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok || core.RawStringField(msg, "role") == "system" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			msg["content"] = []any{map[string]any{
				"type":          "text",
				"text":          content,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
		case []any:
			if len(content) == 0 {
				return
			}
			last, ok := content[len(content)-1].(map[string]any)
			if !ok || last["cache_control"] != nil {
				return
			}
			last["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		return
	}
}

// Upstream constants centralization: URLs, endpoints, client fingerprints, and model names.
//
// Convention consistent with other providers: **never hardcode upstream constants elsewhere**, always reference this file.

const (
	// VendorID is the unique identifier of this provider within the free-model-hub plugin.
	VendorID = "zcode"
	// VendorName is the display name for the management console and lists.
	VendorName = "ZCode"

	// ── Upstream origin ─────────────────────────────────────────────────────────

	// ZCodeOrigin hosts the Start Plan chat channel, Ultra gateway, billing family, and OAuth CLI.
	ZCodeOrigin = "https://zcode.z.ai"
	// ZAIOrigin hosts the API Key redemption chain.
	ZAIOrigin = "https://api.z.ai"

	// ── Chat endpoints ────────────────────────────────────────────────────────────

	// PathPlanMessages is the Start Plan channel (Bearer JWT).
	PathPlanMessages = "/api/v1/zcode-plan/anthropic/v1/messages"
	// PathUltraZAIMessages is the proxy path of the official ZCode gateway to api.z.ai (aligned with official-coding-plan-gateway.ts).
	PathUltraZAIMessages = "/api/v1/ultra-zai/anthropic/v1/messages"
	// PathAPIMessages is the direct Anthropic Messages path when using a custom baseURL.
	PathAPIMessages = "/api/anthropic/v1/messages"

	// ── OAuth CLI (headless, server-side relay) ─────────────────────────────
	//
	// Full path starting from origin (used after concatenating with ZCodeOrigin).

	PathOAuthInit = "/api/v1/oauth/cli/init"
	PathOAuthPoll = "/api/v1/oauth/cli/poll" // + /{flow_id}

	// ── API Key redemption chain (api.z.ai) ───────────────────────────

	PathZLogin       = "/api/auth/z/login"
	PathCustomerInfo = "/api/biz/customer/getCustomerInfo"

	// ── Billing / quota (zcode.z.ai) ────────────────────────────────────────────

	// BillingBase is the billing family base (balance/preview/claim).
	//
	// billing/current is deprecated (marked in official source code codingPlanProviderAvailability.ts and
	// bigmodelUsageQuotaProvider.ts), holding plans is now determined by billing/balance's
	// data.plans, see fetchPlanSnapshot in tasks.go.
	BillingBase = ZCodeOrigin + "/api/v1/zcode-plan"

	// PathBillingBalance returns both data.plans (held plans) and data.balances (remaining quotas).
	PathBillingBalance = "/billing/balance"
	// PathBillingPreview lists the currently offered plans by upstream (unfiltered for held ones).
	PathBillingPreview = "/billing/preview"
	// PathBillingClaim claims a plan, requiring a CAPTCHA token.
	PathBillingClaim  = "/billing/claim"
	PathClientConfigs = "/api/v1/client/configs"

	// PathEventReport is the client telemetry reporting endpoint.
	//
	// **Not under zcode-plan** (concatenating with BillingBase would result in 404), and without Authorization.
	// Upstream treats it as a qualification signal for promotional plans: the official desktop app sends app_launch on every startup,
	// and app_daily_active once a day. Without these two, billing/preview will always be empty.
	PathEventReport = "/api/v1/event/report"

	// ── Client fingerprint ──────────────────────────────────────────────────────────

	// ClientAppVersion is the current version of the official desktop app.
	//
	// Upstream judges compatibility based on client version (outdated versions are rejected), so when following official version upgrades,
	// update here simultaneously.
	ClientAppVersion = "3.16.2"

	// UserAgent is sourced from ClientAppVersion to prevent drift between the two.
	UserAgent = "ZCode/" + ClientAppVersion

	AnthropicVersion = "2023-06-01"
	ClientAgent      = "glm"
	HTTPReferer      = ZCodeOrigin + "/"
	ClientTitle      = "Z Code@electron"
	ReleaseChannel   = "stable"

	// DefaultPlatform/DefaultOSVersion/DefaultLanguage/DefaultTimezone are
	// fallback values when **no account context** is present (model probing, testing). When an account is present, always use that account's
	// device profile—one independent device per account, preventing multiple accounts from clustering into "the same machine".
	DefaultPlatform  = "darwin-arm64"
	DefaultOSVersion = "25.5.0"
	DefaultLanguage  = "zh-CN"
	DefaultTimezone  = "Asia/Shanghai"

	// ── Limits ────────────────────────────────────────────────────────────────

	// MaxTokensLimit is the legal upper limit for upstream max_tokens (exceeding it returns 400 code 1210).
	MaxTokensLimit = 131072

	// DefaultMaxTokens is the value used when max_tokens is not specified in the request.
	DefaultMaxTokens = 8192

	// ── Verification headers ────────────────────────────────────────────────────────────

	// CaptchaHeader is the verification token for the Plan channel; REGION is issued alongside it, and missing it easily causes 3007.
	CaptchaHeader       = "X-Aliyun-Captcha-Verify-Param"
	CaptchaRegionHeader = "X-Aliyun-Captcha-Verify-Region"
)

// Fallback values (prefix / region / scene_id) when client/configs fails to fetch CAPTCHA configuration.
//
// Same source as CAPTCHA_DEFAULTS in the reference implementation zcode2api/app/constants.py: these three values are
// the current official desktop client configuration, allowing the slider to still be rendered when the configs endpoint is unavailable.
const (
	captchaFallbackPrefix = "no8xfe"
	captchaFallbackRegion = "cn"
	captchaFallbackScene  = "11xygtvd"
)

// This file parses ZCode's provider-specific configuration.
