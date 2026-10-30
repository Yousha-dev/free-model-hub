package qoder

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"

	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
)

// This file is Qoder's transport and chat layer: HTTP/session infrastructure (cosy signing, Bearer client),
// upstream message and delta conversion, and chat-completions entry/exit points.

// This file provides the chat entry point taking payloads as inputs and outputs.
//
// Upstream bridge handlers write directly to http.ResponseWriter (ported from the gateway service),
// while this plugin is a CPA executor: it produces no HTTP responses, only payloads for the host.
//
// We deliberately do **not** reuse HandleChatCompletions here, but rewrite its assembly logic
// based on payload forms: that handler directly runs `fmt.Fprintf(w, "data: %s\n\n", ...)` for SSE frames,
// which duplicates the host's framing responsibilities (the host adds `data: ` itself according to the declared format).
// Therefore, this only produces raw JSON payloads, leaving frame packaging to the host.

// RunChatCompletions executes a non-streaming chat-completions request, writing the response body into w.
func RunChatCompletions(b *Bridge, payload []byte, w io.Writer) error {
	req, errDecode := decodeChatRequest(payload)
	if errDecode != nil {
		return errDecode
	}
	reqID := "chatcmpl-" + NewRequestID()
	created := UnixSec()
	messages := BuildQoderMessages(b.templateMessages(), req.Messages, req.Prompt, req.ToolsUsed)

	var content strings.Builder
	var toolCallBuf []interface{}
	var inputTokens, outputTokens int
	errCall := b.CallQoder(context.Background(), InferAgent(req.Model), messages, req.Model, req.Tools, func(d Delta) {
		if d.InputTokens > 0 || d.OutputTokens > 0 {
			inputTokens, outputTokens = d.InputTokens, d.OutputTokens
		}
		if d.Content != "" {
			content.WriteString(d.Content)
		}
		if d.ToolCalls != nil {
			toolCallBuf = mergeToolCallDeltas(toolCallBuf, d.ToolCalls)
		}
	})
	if errCall != nil {
		return errCall
	}

	toolCallBuf = filterValidToolCalls(toolCallBuf)

	finishReason := "stop"
	message := map[string]interface{}{"role": "assistant", "content": content.String()}
	if len(toolCallBuf) > 0 {
		finishReason = "tool_calls"
		message["tool_calls"] = toolCallBuf
		if content.Len() == 0 {
			// OpenAI semantics: content must be null only during tool calls, not an empty string.
			message["content"] = nil
		}
	}
	response := map[string]interface{}{
		"id": reqID, "object": "chat.completion",
		"created": created, "model": req.Model,
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "message": message, "finish_reason": finishReason},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	}
	encoded, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		return fmt.Errorf("encode chat response: %w", errMarshal)
	}
	_, errWrite := w.Write(encoded)
	return errWrite
}

// StreamChatCompletions executes a streaming chat-completions request, passing frames to emit one by one.
//
// emit receives raw JSON payloads, and the outer layer uniformly formats them into valid SSE format via hostStreamSink.Emit.
func StreamChatCompletions(b *Bridge, payload []byte, emit func([]byte) error) error {
	req, errDecode := decodeChatRequest(payload)
	if errDecode != nil {
		return errDecode
	}
	reqID := "chatcmpl-" + NewRequestID()
	created := UnixSec()
	messages := BuildQoderMessages(b.templateMessages(), req.Messages, req.Prompt, req.ToolsUsed)

	var hasToolCalls bool
	var inputTokens, outputTokens int
	errCall := b.CallQoder(context.Background(), InferAgent(req.Model), messages, req.Model, req.Tools, func(d Delta) {
		if d.InputTokens > 0 || d.OutputTokens > 0 {
			inputTokens, outputTokens = d.InputTokens, d.OutputTokens
		}
		chunk := MakeChatChunk(reqID, created, req.Model)
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) > 0 {
			entry, _ := choices[0].(map[string]interface{})
			delta, _ := entry["delta"].(map[string]interface{})
			if delta != nil {
				if d.Content != "" {
					delta["role"] = "assistant"
					delta["content"] = d.Content
				}
				if d.Reasoning != "" {
					// reasoning_content is a convention field for DeepSeek-family clients.
					delta["reasoning_content"] = d.Reasoning
				}
				if d.ToolCalls != nil {
					delta["tool_calls"] = d.ToolCalls
					hasToolCalls = true
				}
			}
		}
		encoded, errMarshal := json.Marshal(chunk)
		if errMarshal != nil {
			return
		}
		_ = emit(encoded)
	})
	if errCall != nil {
		return errCall
	}

	// Wrapping frame: finish_reason and usage.
	finishReason := "stop"
	if hasToolCalls {
		finishReason = "tool_calls"
	}
	done := MakeChatChunk(reqID, created, req.Model)
	choices, _ := done["choices"].([]interface{})
	if len(choices) > 0 {
		entry, _ := choices[0].(map[string]interface{})
		entry["finish_reason"] = finishReason
		entry["delta"] = map[string]interface{}{}
	}
	if inputTokens > 0 || outputTokens > 0 {
		done["usage"] = map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		}
	}
	encoded, errMarshal := json.Marshal(done)
	if errMarshal != nil {
		return errMarshal
	}
	return emit(encoded)
}

// mergeToolCallDeltas accumulates streaming tool chunks by index, concatenating argument deltas into a complete tool call.
func mergeToolCallDeltas(accumulated []interface{}, incoming []interface{}) []interface{} {
	for _, raw := range incoming {
		call, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		idx := core.IntFromAny(call["index"])
		var target map[string]interface{}
		for _, item := range accumulated {
			if existing, okMap := item.(map[string]interface{}); okMap {
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
		if fnIncoming, okFn := call["function"].(map[string]interface{}); okFn {
			fnTarget, _ := target["function"].(map[string]interface{})
			if fnTarget == nil {
				fnTarget = map[string]interface{}{}
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

// filterValidToolCalls removes incomplete tool calls without a valid function.name and sorts them by index in ascending order.
func filterValidToolCalls(calls []interface{}) []interface{} {
	var valid []interface{}
	for _, item := range calls {
		call, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fn, okFn := call["function"].(map[string]interface{})
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
			left, _ := valid[j-1].(map[string]interface{})
			right, _ := valid[j].(map[string]interface{})
			if core.IntFromAny(left["index"]) > core.IntFromAny(right["index"]) {
				valid[j-1], valid[j] = valid[j], valid[j-1]
			}
		}
	}
	return valid
}

func cloneToolCall(src map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		if k == "function" {
			if fn, ok := v.(map[string]interface{}); ok {
				fnCopy := make(map[string]interface{}, len(fn))
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

// chatRequest defines key fields for a chat request.
type chatRequest struct {
	Model     string
	Stream    bool
	Messages  []interface{}
	Tools     interface{}
	ToolsUsed bool
	Prompt    string
}

// decodeChatRequest parses the request body.
func decodeChatRequest(payload []byte) (chatRequest, error) {
	var raw map[string]interface{}
	if errUnmarshal := json.Unmarshal(payload, &raw); errUnmarshal != nil {
		return chatRequest{}, fmt.Errorf("decode chat request: %w", errUnmarshal)
	}
	messages, _ := raw["messages"].([]interface{})
	tools := raw["tools"]
	stream, _ := raw["stream"].(bool)
	return chatRequest{
		Model:     StrValDefault(raw, "model", "auto"),
		Stream:    stream,
		Messages:  messages,
		Tools:     tools,
		ToolsUsed: tools != nil,
		Prompt:    ExtractLatestUserPrompt(messages),
	}, nil
}

func BuildQoderMessages(templateMsgs []interface{}, incoming []interface{}, prompt string, toolsEnabled bool) []interface{} {
	var rebuilt []interface{}

	hasIncomingSystem := false
	for _, m := range incoming {
		if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
			hasIncomingSystem = true
			break
		}
	}
	if !hasIncomingSystem && templateMsgs != nil {
		for _, m := range templateMsgs {
			if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
				rebuilt = append(rebuilt, DeepCopyMap(mm))
			}
		}
	}

	for _, m := range incoming {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		// Handle multi-tool-result: a single Claude user message may contain multiple tool_results
		if role, _ := mm["role"].(string); role == "user" {
			if contentArr, ok := mm["content"].([]interface{}); ok && len(contentArr) > 0 {
				if first, ok := contentArr[0].(map[string]interface{}); ok && first["type"] == "tool_result" {
					for _, block := range contentArr {
						b, _ := block.(map[string]interface{})
						if b == nil || b["type"] != "tool_result" {
							continue
						}
						toolCallId, _ := b["tool_use_id"].(string)
						resultContent := ""
						switch c := b["content"].(type) {
						case string:
							resultContent = c
						case []interface{}:
							for _, part := range c {
								if pm, ok := part.(map[string]interface{}); ok {
									if t, ok := pm["text"].(string); ok {
										resultContent += t
									}
								}
							}
						}
						rebuilt = append(rebuilt, map[string]interface{}{
							"role":         "tool",
							"tool_call_id": toolCallId,
							"content":      resultContent,
						})
					}
					continue
				}
			}
		}
		converted := ConvertIncomingMessage(mm, toolsEnabled)
		if converted != nil {
			rebuilt = append(rebuilt, converted)
		}
	}

	if len(rebuilt) == 0 && prompt != "" {
		rebuilt = append(rebuilt, BuildUserMessage(prompt))
	}
	return rebuilt
}

func ConvertIncomingMessage(msg map[string]interface{}, toolsEnabled bool) map[string]interface{} {
	role, _ := msg["role"].(string)
	content := msg["content"]

	// Handle Claude Messages API content blocks (array of typed blocks)
	if contentArr, ok := content.([]interface{}); ok && len(contentArr) > 0 {
		firstBlock, _ := contentArr[0].(map[string]interface{})
		blockType, _ := firstBlock["type"].(string)

		// assistant message with tool_use blocks
		if role == "assistant" && HasBlockType(contentArr, "tool_use") {
			out := map[string]interface{}{"role": "assistant", "content": ""}
			var toolCalls []interface{}
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil {
					continue
				}
				switch b["type"] {
				case "tool_use":
					inputJSON, _ := json.Marshal(b["input"])
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   b["id"],
						"type": "function",
						"function": map[string]interface{}{
							"name":      b["name"],
							"arguments": string(inputJSON),
						},
					})
				case "text":
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts != "" {
				out["content"] = textParts
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			return out
		}

		// user message with tool_result blocks
		if role == "user" && blockType == "tool_result" {
			var results []map[string]interface{}
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil || b["type"] != "tool_result" {
					continue
				}
				toolCallId, _ := b["tool_use_id"].(string)
				resultContent := ""
				switch c := b["content"].(type) {
				case string:
					resultContent = c
				case []interface{}:
					for _, part := range c {
						if pm, ok := part.(map[string]interface{}); ok {
							if t, ok := pm["text"].(string); ok {
								resultContent += t
							}
						}
					}
				}
				results = append(results, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": toolCallId,
					"content":      resultContent,
				})
			}
			if len(results) == 1 {
				return results[0]
			}
			// Multiple tool results: return first, caller should handle multi
			// Actually we need to return all - use a special marker
			if len(results) > 0 {
				return results[0]
			}
			return nil
		}

		// Handle thinking blocks - skip them
		if blockType == "thinking" || blockType == "redacted_thinking" {
			// Extract only text blocks
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b != nil && b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts == "" {
				return nil
			}
			return BuildStructuredMessage(role, textParts)
		}
	}

	text := NormalizeMessageContent(msg)

	if role == "assistant" && toolsEnabled {
		if tc, ok := msg["tool_calls"]; ok {
			out := BuildStructuredMessage("assistant", text)
			out["tool_calls"] = tc
			return out
		}
	}

	if role == "tool" {
		out := BuildStructuredMessage("tool", text)
		if name, ok := msg["name"].(string); ok {
			out["name"] = name
		}
		if tcid, ok := msg["tool_call_id"].(string); ok {
			out["tool_call_id"] = tcid
		}
		return out
	}

	if text == "" {
		return nil
	}

	if role == "user" {
		return BuildUserMessage(text)
	}
	return BuildStructuredMessage(role, text)
}

func HasBlockType(blocks []interface{}, blockType string) bool {
	for _, block := range blocks {
		if b, ok := block.(map[string]interface{}); ok {
			if b["type"] == blockType {
				return true
			}
		}
	}
	return false
}

func BuildUserMessage(text string) map[string]interface{} {
	return map[string]interface{}{
		"role":    "user",
		"content": "",
		"contents": []interface{}{
			map[string]interface{}{"type": "text", "text": text},
		},
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BuildStructuredMessage(role, text string) map[string]interface{} {
	return map[string]interface{}{
		"role":                        role,
		"content":                     text,
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BlankResponseMeta() map[string]interface{} {
	return map[string]interface{}{
		"id": "",
		"usage": map[string]interface{}{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
			"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 0},
			"prompt_tokens_details":     map[string]interface{}{"cached_tokens": 0},
		},
	}
}

func NormalizeMessageContent(msg map[string]interface{}) string {
	c := msg["content"]
	if s, ok := c.(string); ok {
		return s
	}
	if c == nil {
		return ""
	}
	arr, ok := c.([]interface{})
	if !ok {
		data, _ := json.Marshal(c)
		return string(data)
	}
	var sb string
	for _, block := range arr {
		b, _ := block.(map[string]interface{})
		if t, ok := b["text"].(string); ok {
			if sb != "" {
				sb += "\n\n"
			}
			sb += t
		}
	}
	return sb
}

type Delta struct {
	Role         string
	Content      string
	Reasoning    string // Upstream reasoning process (reasoning_content)
	ToolCalls    []interface{}
	InputTokens  int
	OutputTokens int
	Err          error // Non-nil when upstream returns a business error
}

func (d Delta) isEmpty() bool {
	return d.Role == "" && d.Content == "" && d.Reasoning == "" && d.ToolCalls == nil && d.InputTokens == 0 && d.OutputTokens == 0 && d.Err == nil
}

func ExtractDelta(dataLine string) Delta {
	var wrapper map[string]interface{}
	if err := json.Unmarshal([]byte(dataLine), &wrapper); err != nil {
		logger.Debug("[delta] unmarshal wrapper failed: %v, raw=%s", err, dataLine)
		return Delta{}
	}
	// ---- Envelope status check (Step 3, hub iter_inner_sse format) ----
	// Upstream frame: data:{"headers":{...},"body":"<inner chunk>","statusCodeValue":200}
	// Key pattern: after upstream HTTP 200 stream establishment, provider errors are wrapped and delivered in envelopes
	// statusCodeValue=418/5xx (access logs record 200 while the root cause is a business error).
	// Strictly distinguished from inner business codes (e.g., {"code":"115"} in body): envelope errors carry real
	// HTTP status codes and can participate in transient retry decisions; gate reopening occurs in CallQoderWithOpts.
	if raw, ok := wrapper["statusCodeValue"]; ok {
		if status := toEnvelopeStatus(raw); status != 200 {
			ue := NewUpstreamError(status, envelopeDetail(wrapper))
			logger.Error("[delta] envelope statusCodeValue=%v -> %s", raw, ue.Message)
			return Delta{Err: ue}
		}
	}
	inner, _ := wrapper["body"].(string)
	if inner == "" {
		logger.Debug("[delta] no body field, keys=%v, raw=%s", mapKeys(wrapper), dataLine)
		return Delta{}
	}
	var innerJSON map[string]interface{}
	if err := json.Unmarshal([]byte(inner), &innerJSON); err != nil {
		logger.Debug("[delta] unmarshal inner failed: %v", err)
		return Delta{}
	}
	// First capture top-level usage (upstream may return it in the final chunk along with choices),
	// without returning early: if the same frame also contains choice content, merge it into the same Delta to prevent data loss
	var usageIn, usageOut int
	if usage, ok := innerJSON["usage"].(map[string]interface{}); ok {
		usageIn = int(FloatVal(usage, "prompt_tokens"))
		usageOut = int(FloatVal(usage, "completion_tokens"))
	}
	choices, _ := innerJSON["choices"].([]interface{})
	for _, ch := range choices {
		chMap, _ := ch.(map[string]interface{})
		delta, _ := chMap["delta"].(map[string]interface{})
		if delta == nil {
			continue
		}
		role, _ := delta["role"].(string)
		content, _ := delta["content"].(string)
		reasoning, _ := delta["reasoning_content"].(string)
		var toolCalls []interface{}
		if tc, ok := delta["tool_calls"].([]interface{}); ok && len(tc) > 0 {
			toolCalls = tc
		}
		if role != "" || content != "" || reasoning != "" || toolCalls != nil {
			return Delta{Role: role, Content: content, Reasoning: reasoning, ToolCalls: toolCalls,
				InputTokens: usageIn, OutputTokens: usageOut}
		}
	}
	// Upstream business error: {"code":"115","message":"..."}
	// In-stream business error (within HTTP 200 frame): no transient retry guidance unless it's content moderation,
	// and content moderation is separately mapped to content_policy_rejected.
	// Note: Business error checks still precede usage-only returns, preventing error frames with usage from being misidentified as normal frames.
	if code, ok := innerJSON["code"].(string); ok && code != "" && code != "0" {
		msg, _ := innerJSON["message"].(string)
		err := NewStreamBusinessError(fmt.Sprintf("upstream error code=%s: %s", code, msg))
		logger.Error("[delta] %v", err)
		return Delta{Err: err}
	}
	// Neither choices nor business code matched, and usage > 0: continue returning usage-only Delta (behavior identical to pre-fix)
	if usageIn > 0 || usageOut > 0 {
		return Delta{InputTokens: usageIn, OutputTokens: usageOut}
	}
	logger.Debug("[delta] no valid choices found, inner=%s", inner)
	return Delta{}
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// toEnvelopeStatus: envelope statusCodeValue might be int/float64/str, standardize it to int
// (parsing failure returns 502, treated as a transient failure).
func toEnvelopeStatus(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, err := strconv.Atoi(x)
		if err != nil {
			return 502
		}
		return n
	}
	return 502
}

// envelopeDetail extracts envelope error details: uses body directly when it's a string (error details are inside),
// otherwise truncates the full JSON frame for easy troubleshooting.
func envelopeDetail(wrapper map[string]interface{}) string {
	if body, ok := wrapper["body"].(string); ok && body != "" {
		return body
	}
	b, _ := json.Marshal(wrapper)
	return core.Truncate(string(b), 400)
}

func ExtractLatestUserPrompt(msgs []interface{}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]interface{})
		if m["role"] == "user" {
			return NormalizeMessageContent(m)
		}
	}
	return ""
}

func MakeChatChunk(id string, created int64, model string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "object": "chat.completion.chunk",
		"created": created, "model": model,
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "delta": map[string]interface{}{}, "finish_reason": nil},
		},
	}
}

func (b *Bridge) templateMessages() []interface{} {
	if msgs, ok := b.templateBase["messages"].([]interface{}); ok {
		return msgs
	}
	return nil
}

type BearerClient struct {
	sess *SessionContext
}

func NewBearerClient(sess *SessionContext) *BearerClient {
	return &BearerClient{sess: sess}
}

func (c *BearerClient) buildHeaders(pathSig, body, accept string, extra map[string]string) (map[string]string, error) {
	payloadB64, err := BuildPayloadB64(c.sess.Info)
	if err != nil {
		return nil, err
	}
	date := fmt.Sprintf("%d", UnixSec())
	sig := SignRequest(payloadB64, c.sess.CosyKey, date, body, pathSig)
	bearer := ComposeBearer(payloadB64, sig)

	h := map[string]string{
		"cosy-data-policy":      "agree",
		"content-type":          "application/json",
		"cosy-machinetype":      c.sess.MachineType,
		"cosy-clienttype":       "5",
		"cosy-date":             date,
		"cosy-user":             c.sess.Identity.Uid,
		"cosy-key":              c.sess.CosyKey,
		"cache-control":         "no-cache",
		"accept":                accept,
		"authorization":         bearer,
		"cosy-version":          Version,
		"cosy-machineid":        c.sess.MachineId,
		"cosy-machinetoken":     c.sess.MachineToken,
		"login-version":         "v2",
		"user-agent":            "Go-http-client/2.0",
		"cosy-scene":            "assistant",
		"cosy-business-product": "ide",
		"cosy-business-type":    "agent",
	}
	for k, v := range extra {
		h[k] = v
	}
	return h, nil
}

func PathSigFrom(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	p := u.Path
	if strings.HasPrefix(p, "/algo") {
		p = p[len("/algo"):]
	}
	return p, nil
}

// shortResponseMaxBytes is the read limit for Qoder short responses (model catalog, quota, check-in, exchanging jobTokens,
// userinfo, login polling). Normal responses for these APIs are within tens of KB; exceeding this indicates
// The upstream returned abnormal content (HTML error page, proxy injection), which must be rejected instead of being read indefinitely into memory.
const shortResponseMaxBytes int64 = 1 << 20

// readQoderResponseBody reads a short Qoder response and rejects bodies exceeding the limit.
// Read limit+1 bytes to distinguish between "exactly reaching the limit" and "truncated", avoiding silent parsing of half-cut JSON.
func readQoderResponseBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, shortResponseMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > shortResponseMaxBytes {
		return nil, fmt.Errorf("qoder response body exceeds %d bytes", shortResponseMaxBytes)
	}
	return data, nil
}

// callGet sends a GET request using cosy signature, with the body part being an empty string when participating in the signature.
// Used for pure query interfaces like /algo/api/v2/model/list.
func (c *BearerClient) callGet(ctx context.Context, fullURL string) (map[string]interface{}, error) {
	pathSig, err := PathSigFrom(fullURL)
	if err != nil {
		return nil, err
	}
	headers, err := c.buildHeaders(pathSig, "", "application/json", nil)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read qoder response: %w", errRead)
	}
	if resp.StatusCode != 200 {
		return nil, NewUpstreamError(resp.StatusCode, string(data))
	}
	preview := string(data)
	if len(preview) > 2000 {
		preview = preview[:2000]
	}
	logger.Info("callGet %s response (%d bytes): %s", fullURL, len(data), preview)
	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	return result, err
}

// openStreamLines sends a POST, reads SSE lines, calls onLine for each non-empty line.
// onLine returning false indicates that the consumer requests to stop reading (e.g., deltaDispatcher has caught an envelope error frame),
// at which point this stream read ends immediately and returns, and the upper layer (CallQoderWithOpts envelope retry gate) decides whether to reopen.
// ctx can be used to cancel streaming reads (for example, when the client disconnects).
//
// Connection phase (before stream establishment) with same-account transient retry: 418/5xx/transport jitter → 1s/2s backoff up to
// TransientMaxRetries times. In-stream errors after successful stream establishment are handled by the upper layer (Step 3 envelope retry).
func (c *BearerClient) openStreamLines(ctx context.Context, fullURL string, jsonBody interface{}, extra map[string]string, onLine func(string) bool) error {
	pathSig, err := PathSigFrom(fullURL)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(jsonBody)
	if err != nil {
		return err
	}
	bodyStr, err := Encode(plain)
	if err != nil {
		return err
	}
	// No overall Timeout is set; the lifecycle is controlled by the context instead to prevent long streaming responses from being truncated
	client := hostbridge.StreamClient(ctx, 0)

	var lastErr error
	queueWaits := 0
	maxQueueWaits := QueueMaxWaits()
	for attempt := 0; attempt <= TransientMaxRetries+maxQueueWaits; attempt++ {
		if attempt > 0 {
			if !core.SleepContext(ctx, RetryBackoff(attempt)) {
				return ctx.Err()
			}
		}
		headers, err := c.buildHeaders(pathSig, bodyStr, "text/event-stream", extra)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", fullURL, strings.NewReader(bodyStr))
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if IsTransientTransport(err) && attempt < TransientMaxRetries {
				logger.Info("transient transport error opening stream %s (try %d/%d): %s - retry in %ds",
					fullURL, attempt+1, TransientMaxRetries+maxQueueWaits+1, err.Error(), attempt+1)
				continue
			}
			return WrapTransportError(err)
		}
		if resp.StatusCode != 200 {
			body, errRead := readQoderResponseBody(resp.Body)
			resp.Body.Close()
			if errRead != nil {
				return fmt.Errorf("read qoder stream response: %w", errRead)
			}
			detail := string(body)
			if signal, queued := ParseQueueSignal(detail); queued && queueWaits < maxQueueWaits && ctx.Err() == nil {
				wait := queueWaitFor(signal)
				queueWaits++
				logger.Info("upstream queues model %s opening stream %s (%s); waiting %s before retry %d/%d",
					signal.ModelKey, fullURL, signal.QueueType, wait, queueWaits, maxQueueWaits)
				if !core.SleepContext(ctx, wait) {
					return ctx.Err()
				}
				continue
			}
			if signal, queued := ParseQueueSignal(detail); queued {
				return QueueSignalError(signal)
			}
			if IsTransientUpstream(resp.StatusCode, detail) && attempt < TransientMaxRetries {
				logger.Info("transient upstream HTTP %d opening stream %s (try %d/%d) - retry in %ds",
					resp.StatusCode, fullURL, attempt+1, TransientMaxRetries+maxQueueWaits+1, attempt+1)
				continue
			}
			return NewUpstreamError(resp.StatusCode, detail)
		}

		// Stream establishment successful: enter streaming read, no more retries at this layer
		defer resp.Body.Close()
		lineCh := make(chan string)
		errCh := make(chan error, 1)
		// done: closed when the main loop exits early (onLine returns false), unlocking the scanner goroutine
		// to prevent it from blocking on lineCh send and leaving only ctx.Done as an outlet causing a goroutine leak
		done := make(chan struct{})
		defer close(done)
		go func() {
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
			for scanner.Scan() {
				select {
				case lineCh <- scanner.Text():
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				case <-done:
					return
				}
			}
			if err := scanner.Err(); err != nil {
				errCh <- err
				return
			}
			errCh <- nil
		}()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-errCh:
				logger.Debug("stream read complete")
				return err
			case line := <-lineCh:
				if line != "" {
					// onLine returns false: consumer has set upstreamErr (envelope error frame),
					// stop reading immediately to prevent request hanging when the upstream does not close the stream;
					// errors are reopened by the emitted gate of CallQoderWithOpts + isRetryableStreamError
					if !onLine(line) {
						return nil
					}
				}
			}
		}
	}
	// Defensive fallback: all failure branches return directly within the loop, normally unreachable
	return WrapTransportError(lastErr)
}

// ExchangeJobToken converts a PAT or refresh token into a job token.
// Outbound requests go through the host HTTP bridge (ctx carries host_callback_id).
func ExchangeJobToken(ctx context.Context, token, machineId, machineToken, machineType string, jobTokenURL ...string) (map[string]interface{}, error) {
	date := CurrentDate()
	sig := SignLegacy(date)

	var personalToken, refreshToken string
	if strings.HasPrefix(token, "drt-") {
		refreshToken = token
		personalToken = ""
	} else {
		personalToken = token
		refreshToken = ""
	}

	inner := map[string]interface{}{
		"personalToken":      personalToken,
		"securityOauthToken": "",
		"refreshToken":       refreshToken,
		"needRefresh":        refreshToken != "",
		"authInfo":           map[string]interface{}{},
	}
	innerJSON, _ := json.Marshal(inner)

	outer := map[string]interface{}{
		"payload":       string(innerJSON),
		"encodeVersion": "1",
	}
	plain, _ := json.Marshal(outer)
	body, err := Encode(plain)
	if err != nil {
		return nil, err
	}

	endpoint := "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1"
	if len(jobTokenURL) > 0 && jobTokenURL[0] != "" {
		endpoint = jobTokenURL[0]
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("cosy-machinetoken", machineToken)
	req.Header.Set("cosy-machinetype", machineType)
	req.Header.Set("login-version", "v2")
	req.Header.Set("appcode", AppCode)
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("cosy-version", Version)
	req.Header.Set("cosy-clienttype", "5")
	req.Header.Set("date", date)
	req.Header.Set("signature", sig)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("cosy-machineid", machineId)
	req.Header.Set("user-agent", "Go-http-client/2.0")

	resp, err := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read qoder job token response: %w", errRead)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d at jobToken body=%s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	return result, err
}

const (
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customPad      = '$'
)

var c2s [128]int
var s2c [128]int

func init() {
	for i := range c2s {
		c2s[i] = -1
		s2c[i] = -1
	}
	for i := 0; i < 64; i++ {
		c2s[customAlphabet[i]] = int(stdAlphabet[i])
		s2c[stdAlphabet[i]] = int(customAlphabet[i])
	}
	c2s[customPad] = '='
	s2c['='] = customPad
}

func Encode(plaintext []byte) (string, error) {
	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		c := int(rearranged[i])
		if c >= 128 || s2c[c] < 0 {
			return "", fmt.Errorf("char out of alphabet: %d", c)
		}
		out[i] = byte(s2c[c])
	}
	return string(out), nil
}

func Decode(encoded string) ([]byte, error) {
	n := len(encoded)
	mapped := make([]byte, n)
	for i := 0; i < n; i++ {
		c := int(encoded[i])
		if c >= 128 || c2s[c] < 0 {
			return nil, fmt.Errorf("char out of custom alphabet: %d", c)
		}
		mapped[i] = byte(c2s[c])
	}
	a := n / 3
	std := string(mapped[n-a:]) + string(mapped[a:n-a]) + string(mapped[:a])
	return base64.StdEncoding.DecodeString(std)
}

// saltedSeed reserved interface; this implementation does not use deployment salt (consistent with existing behavior).
func saltedSeed(seed string) string { return seed }

// FingerprintSeed selects the fingerprint seed: prioritizing the account UID;
// when the UID is unknown (jobToken exchange happens before getting the uid), it falls back to the credential token,
// ensuring that derivation results for the same credential across calls are consistent and do not drift with process restarts.
func FingerprintSeed(uid, credential string) string {
	if uid != "" {
		return uid
	}
	return "cred:" + credential
}

func deriveID(seed, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + saltedSeed(seed)))
	return fmt.Sprintf("%x", sum)
}

// DeriveMachineID derives a stable cosy-machineid (32-bit hex).
func DeriveMachineID(seed string) string {
	return deriveID(seed, "machine")
}

// DeriveMachineType derives a stable 18-bit machine type (isomorphic to hub derive_machine_type).
func DeriveMachineType(seed string) string {
	return deriveID(seed, "machinetype")[:18]
}

// DeriveMachineToken derives a stable machine token.
//
// Format aligned with official component output: 88-character base64url (65 bytes of entropy). SHA-512 has only 64
// bytes, so another round of digest with a different prefix is appended to fill it up—still deterministic, invariant for the same seed.
func DeriveMachineToken(seed string) string {
	primary := sha512.Sum512([]byte("machinetoken:" + saltedSeed(seed)))
	secondary := sha512.Sum512([]byte("machinetoken2:" + saltedSeed(seed)))
	raw := make([]byte, 66)
	copy(raw, primary[:])
	raw[64] = secondary[0]
	raw[65] = secondary[1]
	return base64.RawURLEncoding.EncodeToString(raw)[:88]
}

const Version = "1.0.10"

const ServerPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

type AuthIdentity struct {
	Name               string
	Aid                string
	Uid                string
	YxUid              string
	OrganizationId     string
	OrganizationName   string
	UserType           string
	SecurityOauthToken string
	RefreshToken       string
}

type SessionContext struct {
	TempKey      []byte
	CosyKey      string
	Info         string
	Identity     AuthIdentity
	MachineId    string
	MachineToken string
	MachineType  string
}

func NewSession(id AuthIdentity, machineId, machineToken, machineType string) (*SessionContext, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	tempKey := []byte(fmt.Sprintf("%x", raw)[:16])

	cosyKeyBytes, err := rsaEncrypt(tempKey)
	if err != nil {
		return nil, err
	}
	cosyKey := base64.StdEncoding.EncodeToString(cosyKeyBytes)

	payloadBytes, err := authPayloadJSON(id)
	if err != nil {
		return nil, err
	}
	encPayload, err := aesEncrypt(payloadBytes, tempKey)
	if err != nil {
		return nil, err
	}
	info := base64.StdEncoding.EncodeToString(encPayload)

	return &SessionContext{
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         info,
		Identity:     id,
		MachineId:    machineId,
		MachineToken: machineToken,
		MachineType:  machineType,
	}, nil
}

func SignRequest(payloadB64, cosyKey, cosyDate, body, pathWithoutAlgo string) string {
	s := payloadB64 + "\n" + cosyKey + "\n" + cosyDate + "\n" + body + "\n" + pathWithoutAlgo
	return Md5Hex(s)
}

func BuildPayloadB64(info string) (string, error) {
	m := map[string]string{
		"cosyVersion": Version,
		"ideVersion":  "",
		"info":        info,
		"requestId":   NewUUID(),
		"version":     "v1",
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func ComposeBearer(payloadB64, sig string) string {
	return "Bearer COSY." + payloadB64 + "." + sig
}

func authPayloadJSON(id AuthIdentity) ([]byte, error) {
	m := map[string]string{
		"name":                 id.Name,
		"aid":                  id.Aid,
		"uid":                  id.Uid,
		"yx_uid":               id.YxUid,
		"organization_id":      id.OrganizationId,
		"organization_name":    id.OrganizationName,
		"user_type":            id.UserType,
		"security_oauth_token": id.SecurityOauthToken,
		"refresh_token":        id.RefreshToken,
	}
	return json.Marshal(m)
}

func rsaEncrypt(plaintext []byte) ([]byte, error) {
	pemData := strings.ReplaceAll(ServerPubKeyPEM, "\r", "")
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not RSA public key")
	}
	return rsa.EncryptPKCS1v15(rand.Reader, rsaPub, plaintext)
}

func aesEncrypt(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	pad := bs - len(plain)%bs
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	out := make([]byte, len(padded))
	iv := key[:bs]
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(out, padded)
	return out, nil
}

func NewUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func NewRequestID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)[:24]
}
func UnixSec() int64 {
	return time.Now().Unix()
}

func UnixMs() int64 {
	return time.Now().UnixMilli()
}

func FloatVal(m map[string]interface{}, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// FloatPtr gets an optional numeric value: returns nil if field is missing, and a pointer to 0 if explicitly 0.
func FloatPtr(m map[string]interface{}, key string) *float64 {
	v, ok := m[key].(float64)
	if !ok {
		return nil
	}
	return &v
}

const (
	AppCode   = "cosy"
	SecretB64 = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==" // base64("war, war never changes")
	sep       = "&"
)

func CurrentDate() string {
	return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

func SignLegacy(date string) string {
	return Md5Hex(AppCode + sep + SecretB64 + sep + date)
}

func Md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

// ==================== Proprietary Protocol Bridge Engine & Endpoints ====================

// This file is the protocol layer of Qoder: region and endpoint tables, proprietary protocol engine (Bridge connection,
// model listing, CallQoderWithOpts), queuing signals, and request templates.

// baseprompt.json is the request template for Qoder upstream agent_chat_generation (ported from qoder2api).
// The template retains {UUIDn}/{TIME1} placeholders: replaced with actual values upon each Bridge creation and then parsed into JSON.
//
//go:embed assets/baseprompt.json
var basePromptRaw []byte

// TemplateBase constructs a request template.
//
// Difference from qoder2api: errors are returned here for parsing failures instead of being silently ignored. Template parsing failure means
// all upstream requests will be distorted, and silently continuing will manifest the problem as "the model giving irrelevant answers", which is hard to locate.
func TemplateBase() (map[string]interface{}, error) {
	template := string(basePromptRaw)
	for _, placeholder := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
		template = strings.ReplaceAll(template, placeholder, NewUUID())
	}
	template = strings.ReplaceAll(template, "{TIME1}", fmt.Sprintf("%d", UnixMs()))

	var templateBase map[string]interface{}
	if errUnmarshal := json.Unmarshal([]byte(template), &templateBase); errUnmarshal != nil {
		return nil, fmt.Errorf("baseprompt.json template parsing failed (embedded resource corrupted or placeholder not replaced): %w", errUnmarshal)
	}
	if templateBase == nil {
		return nil, fmt.Errorf("baseprompt.json template is empty")
	}
	return templateBase, nil
}

// BridgeSecret returns the credential string to be passed to NewBridge.
//
// The upstream NewBridge internally calls ParseOAuthSecret: passing dt- plaintext will drop
// the refresh_token (session cannot be refreshed), so when a refresh token exists, it must be passed as qoder2api
// exported JSON format, consistent with upstream desktop behavior.
func BridgeSecret(cred *Credential) string {
	if cred == nil {
		return ""
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return cred.Token
	}
	payload, errMarshal := json.Marshal(map[string]string{
		"device_token":  cred.Token,
		"refresh_token": cred.RefreshToken,
	})
	if errMarshal != nil {
		return cred.Token
	}
	return string(payload)
}

// CredentialFingerprint generates a credential fingerprint used for the Bridge cache key.
//
// Do not use token plaintext directly as the cache key: cache keys end up in logs and memory dumps.
func CredentialFingerprint(cred *Credential) string {
	if cred == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cred.Token + "|" + cred.RefreshToken + "|" + string(cred.Region)))
	return hex.EncodeToString(sum[:16])
}

func qoderChatStreamURL(region Region) string {
	return GetEndpoints(region).ChatStreamURL
}

func qoderModelListURL(region Region) string {
	return GetEndpoints(region).ModelListURL
}

func ParseOAuthSecret(secret string) (deviceToken, refreshToken string) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", ""
	}
	if strings.HasPrefix(secret, "dt-") || strings.HasPrefix(secret, "drt-") {
		return secret, ""
	}
	var payload struct {
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(secret), &payload); err != nil {
		return secret, ""
	}
	return payload.DeviceToken, payload.RefreshToken
}

func ResolveOAuthUserID(userInfo map[string]interface{}) string {
	if id := StrVal(userInfo, "id"); id != "" {
		return id
	}
	if id := StrVal(userInfo, "userId"); id != "" {
		return id
	}
	return StrVal(userInfo, "uid")
}

func FetchUserInfoWithToken(ctx context.Context, token string, region Region) (map[string]interface{}, error) {
	ep := GetEndpoints(region)
	req, err := http.NewRequestWithContext(ctx, "GET", ep.UserinfoBase, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read qoder userinfo: %w", errRead)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	// DEBUG purpose to inspect raw userinfo response structure: downgrade to Debug level and truncate to 500 bytes,
	// default log level no longer writes PII like usernames/organizations to disk, retaining low-level troubleshooting capability
	logger.Debug("userinfo raw (%d bytes): %s", len(raw), core.Truncate(string(raw), 500))
	return result, nil
}

type Bridge struct {
	sess         *SessionContext
	client       *BearerClient
	region       Region
	templateBase map[string]interface{}
	// models is the model index of the owning adapter instance: restores external slugs back to upstream keys.
	// When nil (constructible only in tests) falls back to the built-in mapping table.
	models *modelIndex
	// chatStreamURL for test injection: when non-empty, overrides the upstream chat stream URL corresponding to the region
	chatStreamURL string
}

// Model is a streamlined model entry returned to the frontend (retaining only fields necessary for dropdown selection)
type Model struct {
	Key             string `json:"key"`
	DisplayName     string `json:"display_name"`
	Enable          bool   `json:"enable"`
	IsDefault       bool   `json:"is_default"`
	IsReasoning     bool   `json:"is_reasoning,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	MaxInputTokens  int    `json:"max_input_tokens,omitempty"`
	// PriceFactor is the upstream billing multiplier; uses a pointer to distinguish "not provided" from "explicit 0" (limited-time free).
	PriceFactor *float64 `json:"price_factor,omitempty"`
}

// NewBridge creates the API conversion bridge.
// Supports two authentication methods:
//  1. OAuth device token (dt-xxx): used directly, calls /api/v1/userinfo to get user info
//  2. Personal Access Token (PAT): calls ExchangeJobToken to exchange for a session token
//
// models is the model index of the owning adapter instance, used to restore external slugs back to upstream keys on outbound;
// When nil is passed, Bridge falls back to the built-in mapping table (test scenarios without an index yet).
func NewBridge(ctx context.Context, pat string, region Region, templateBase map[string]interface{}, models *modelIndex) (*Bridge, error) {
	// Note: Do not print credentials here (even the prefix). Plugin logs enter the CPA process logs,
	// and account identifiers are only allowed in non-sensitive fields after a session is successfully established.
	var identity AuthIdentity
	var name, id string

	deviceToken, refreshToken := ParseOAuthSecret(pat)
	if strings.HasPrefix(deviceToken, "dt-") {
		userInfo, err := FetchUserInfoWithToken(ctx, deviceToken, region)
		if err != nil {
			return nil, fmt.Errorf("fetch user info: %w", err)
		}
		name = StrVal(userInfo, "name")
		id = ResolveOAuthUserID(userInfo)
		identity = AuthIdentity{
			Name:               name,
			Aid:                id,
			Uid:                id,
			OrganizationId:     StrVal(userInfo, "organization_id"),
			OrganizationName:   StrVal(userInfo, "organization_name"),
			UserType:           StrValDefault(userInfo, "userType", "personal_standard"),
			SecurityOauthToken: deviceToken,
			RefreshToken:       refreshToken,
		}
	} else {
		// jobToken exchange happens before uid is obtained: machine header is stably derived from credential seed
		// (hub exchange requests do not carry machine headers; QCCG's original behavior is preserved here but random drift is eliminated)
		seed := FingerprintSeed("", pat)
		jt, err := ExchangeJobToken(ctx, pat, DeriveMachineID(seed), DeriveMachineToken(seed), DeriveMachineType(seed), GetEndpoints(region).JobTokenURL)
		if err != nil {
			return nil, fmt.Errorf("exchangeJobToken: %w", err)
		}
		name = StrVal(jt, "name")
		id = StrVal(jt, "id")
		identity = AuthIdentity{
			Name:               name,
			Aid:                id,
			Uid:                id,
			UserType:           StrValDefault(jt, "userType", "personal_standard"),
			SecurityOauthToken: StrVal(jt, "securityOauthToken"),
			RefreshToken:       StrVal(jt, "refreshToken"),
		}
	}

	logger.Info("Bridge session for %s (%s)", name, id)
	// Stable device fingerprint: prioritized uid derivation (dual-zone unified, persists across restarts), falls back to credential seed if uid is missing
	seed := FingerprintSeed(id, pat)
	mid := DeriveMachineID(seed)
	mtoken := DeriveMachineToken(seed)
	mtype := DeriveMachineType(seed)
	logger.Info("Bridge fingerprint machineid=%s (stable derive)", mid)
	sess, err := NewSession(identity, mid, mtoken, mtype)
	if err != nil {
		return nil, err
	}
	client := NewBearerClient(sess)

	return &Bridge{
		sess:         sess,
		client:       client,
		region:       region,
		templateBase: templateBase,
		models:       models,
	}, nil
}

// ListAvailableModels calls /algo/api/v2/model/list via cosy signature to pull the upstream model list.
// Returns models with enable=true in the top-level assistant array, sorted by is_default desc + display_name asc.
func (b *Bridge) ListAvailableModels(ctx context.Context) ([]Model, error) {
	modelListURL := qoderModelListURL(b.region)
	resp, err := b.client.callGet(ctx, modelListURL)
	if err != nil {
		return nil, err
	}
	// debug: print response top-level keys
	keys := make([]string, 0, len(resp))
	for k := range resp {
		keys = append(keys, k)
	}
	logger.Info("model list response keys: %v (region=%s)", keys, b.region)
	models := parseModels(resp)
	if len(models) == 0 {
		return nil, fmt.Errorf("%s -> empty model list, keys=%v", modelListURL, keys)
	}
	return models, nil
}

func parseModels(resp map[string]interface{}) []Model {
	// Try multiple categories by priority
	categories := []string{"assistant", "developer", "chat"}
	for _, cat := range categories {
		rawList, _ := resp[cat].([]interface{})
		out := extractModels(rawList)
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func extractModels(rawList []interface{}) []Model {
	out := make([]Model, 0, len(rawList))
	for _, it := range rawList {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		enable, _ := m["enable"].(bool)
		model := Model{
			Key:            StrVal(m, "key"),
			DisplayName:    StrVal(m, "display_name"),
			Enable:         enable,
			IsDefault:      func() bool { v, _ := m["is_default"].(bool); return v }(),
			IsReasoning:    func() bool { v, _ := m["is_reasoning"].(bool); return v }(),
			MaxInputTokens: int(FloatVal(m, "max_input_tokens")),
			PriceFactor:    FloatPtr(m, "price_factor"),
		}
		if cc, ok := m["context_config"].(map[string]interface{}); ok {
			var defaultCfg map[string]interface{}
			for _, cfg := range cc {
				cfgMap, _ := cfg.(map[string]interface{})
				if cfgMap == nil {
					continue
				}
				isDefault, _ := cfgMap["is_default"].(bool)
				if isDefault {
					defaultCfg = cfgMap
					break
				}
			}
			if defaultCfg == nil {
				for _, cfg := range cc {
					cfgMap, _ := cfg.(map[string]interface{})
					if cfgMap != nil {
						defaultCfg = cfgMap
						break
					}
				}
			}
			if defaultCfg != nil {
				model.ContextWindow = int(FloatVal(defaultCfg, "token_count"))
			}
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = model.MaxInputTokens
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = model.MaxInputTokens
		}
		if model.IsReasoning {
			model.MaxOutputTokens = 32768
		} else {
			model.MaxOutputTokens = 16384
		}
		out = append(out, model)
	}
	return out
}

// DeepCopyMap does a JSON round-trip deep copy.
func DeepCopyMap(m map[string]interface{}) map[string]interface{} {
	data, _ := json.Marshal(m)
	var out map[string]interface{}
	json.Unmarshal(data, &out)
	return out
}

// CallOpts controls optional behavior for a single CallQoder call.
type CallOpts struct {
	IsReasoning bool // Whether to enable reasoning mode (corresponds to upstream model_config.is_reasoning)
	MaxTokens   int  // max_tokens requested by the client, 0 means using template default
}

func (b *Bridge) CallQoder(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, onDelta func(Delta)) error {
	return b.CallQoderWithOpts(ctx, agent, messages, model, tools, CallOpts{}, onDelta)
}

// deltaDispatcher dispatches upstream SSE lines into Delta callbacks, the return value is used by openStreamLines'
// onLine usage: false means stop reading the upstream stream.
// Constraint (review P1): Once an in-stream business error is captured (*upstreamErr is not nil),
// immediately stop dispatching any subsequent deltas to onDelta, and return false starting from the line where the error was set,
// causing openStreamLines to exit promptly — otherwise if the stream is not closed after the upstream emits an error frame and the gate is reopened,
// it will never trigger and the request will hang; errors are uniformly returned by CallQoder after the stream ends.
func deltaDispatcher(onDelta func(Delta), upstreamErr *error) func(string) bool {
	return func(line string) bool {
		if *upstreamErr != nil {
			return false
		}
		if !strings.HasPrefix(line, "data:") {
			return true
		}
		dataPayload := strings.TrimSpace(line[5:])
		if dataPayload == "[DONE]" {
			return true
		}
		delta := ExtractDelta(dataPayload)
		if delta.Err != nil {
			*upstreamErr = delta.Err
			return false
		}
		if !delta.isEmpty() {
			onDelta(delta)
		}
		return true
	}
}

func (b *Bridge) CallQoderWithOpts(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, opts CallOpts, onDelta func(Delta)) error {
	// The client sent the external slug registered by the plugin (deepseek-v4-pro), first restore it to Qoder's upstream
	// internal model.key (dmodel). Upstream falls back and returns content for unknown keys without charging quota,
	// so failure at this step equals "request succeeded, dashboard usage is zero".
	originalModel := model
	model = b.resolveModel(agent, model)
	if model != originalModel {
		logger.Debug("mapModel %s/%s -> %s", agent, originalModel, model)
	}
	body := DeepCopyMap(b.templateBase)

	nid := NewUUID()
	body["request_id"] = nid
	body["chat_record_id"] = nid
	body["request_set_id"] = NewUUID()
	body["session_id"] = NewUUID()
	body["stream"] = true
	body["aliyun_user_type"] = b.sess.Identity.UserType

	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		mc["key"] = model
		if opts.IsReasoning {
			mc["is_reasoning"] = true
		}
	}

	if opts.MaxTokens > 0 {
		if params, ok := body["parameters"].(map[string]interface{}); ok {
			params["max_tokens"] = opts.MaxTokens
		}
	}

	// Extract summary prompt from last user message for chat_context and business.name
	prompt := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if mm, ok := messages[i].(map[string]interface{}); ok && mm["role"] == "user" {
			prompt = NormalizeMessageContent(mm)
			if prompt == "" {
				if contents, ok := mm["contents"].([]interface{}); ok {
					for _, block := range contents {
						if b, ok := block.(map[string]interface{}); ok {
							if t, ok := b["text"].(string); ok {
								prompt = t
								break
							}
						}
					}
				}
			}
			break
		}
	}

	if biz, ok := body["business"].(map[string]interface{}); ok {
		biz["id"] = NewUUID()
		biz["begin_at"] = UnixMs()
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	if cc, ok := body["chat_context"].(map[string]interface{}); ok {
		if txt, ok := cc["text"].(map[string]interface{}); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]interface{}); ok {
			if oc, ok := extra["originalContent"].(map[string]interface{}); ok {
				oc["text"] = prompt
			}
		}
	}

	body["messages"] = messages
	if tools != nil {
		body["tools"] = tools
	}

	mcSource := "system"
	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		if s, ok := mc["source"].(string); ok {
			mcSource = s
		}
	}

	qurl := qoderChatStreamURL(b.region)
	if b.chatStreamURL != "" {
		qurl = b.chatStreamURL
	}
	extra := map[string]string{
		"x-model-key":    model,
		"x-model-source": mcSource,
	}

	preview := prompt
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	logger.Info("callQoder model=%s prompt=%s", model, preview)
	logger.Debug("callQoder request body: %s", func() string { d, _ := json.Marshal(body); return string(d) }())

	// ---- SSE envelope retry gate (Step 3, referenced from hub should_retry_envelope /
	// aggregate_with_envelope_retry)------------------------------------------
	// Key pattern: The upstream HTTP 200 stream is established before returning 418/5xx in the envelope, which connection-layer retries cannot cover.
	// Gate rule: As long as "no non-empty delta has been sent to onDelta yet" and the error is transient
	// (envelope 418/5xx/provider_error, transport layer TLS EOF, etc.), restart upstream retry
	// transparently to the caller; if content has already been sent, throw directly to avoid duplicate content on the client.
	// Client parameter errors / content moderation / 401/429 are determined by IsTransientUpstream as
	// non-retriable, failing fast. Placing the gate here automatically covers the streaming and non-streaming paths
	// for all three chat/claude/codex protocols.
	// Note: Connection-phase errors are already retried inside openStreamLines; restarting here is
	// the second layer of defense (bounded: max TransientMaxRetries times), mainly covering in-stream envelope errors.
	var lastErr error
	// Queue waiting has a separate count: Upstream queue signals for free models (isQueued/serviceAvailable=false)
	// are not limited by transient retry counts, but are subject to the cap on the number of waits, and must wait for the upstream-suggested duration
	// (rather than 1s/2s transient backoff—waiting insufficiently equals not waiting at all).
	queueWaits := 0
	maxQueueWaits := QueueMaxWaits()
	for attempt := 0; attempt <= TransientMaxRetries+maxQueueWaits; attempt++ {
		if attempt > 0 {
			wait := RetryBackoff(attempt)
			if signal, queued := QueueSignalFromError(lastErr); queued {
				if queueWaits >= maxQueueWaits {
					return QueueSignalError(signal)
				}
				queueWaits++
				wait = queueWaitFor(signal)
				logger.Info("upstream queues model %s (%s); waiting %s before reopening (queue wait %d/%d)",
					signal.ModelKey, signal.QueueType, wait, queueWaits, maxQueueWaits)
			}
			if !core.SleepContext(ctx, wait) {
				return lastErr
			}
			logger.Info("reopen upstream after in-stream error (try %d/%d): %v",
				attempt+1, TransientMaxRetries+maxQueueWaits+1, lastErr)
		}
		emitted := false
		var upstreamErr error
		onDeltaWrapped := func(d Delta) {
			if !d.isEmpty() {
				emitted = true
			}
			onDelta(d)
		}
		streamErr := b.client.openStreamLines(ctx, qurl, body, extra,
			deltaDispatcher(onDeltaWrapped, &upstreamErr))
		if upstreamErr == nil && streamErr == nil {
			// Upstream closes stream normally but emits no valid delta/usage frames: align with hub
			// "empty upstream stream" explicit error (not included in the restart gate—
			// isRetryableStreamError naturally returns false for sentinel errors, no retry)
			if !emitted {
				return ErrEmptyStream
			}
			return nil
		}
		if upstreamErr != nil {
			lastErr = upstreamErr
		} else {
			lastErr = streamErr
		}
		if emitted || attempt >= TransientMaxRetries+maxQueueWaits || !isRetryableStreamError(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// isRetryableStreamError determines whether an in-stream/connection error warrants restarting upstream:
// Structured upstream errors are judged by transient classification; bare transport errors (TLS EOF, etc.) by transport layer;
// ctx cancellations, business errors, content moderation, etc., are strictly non-retriable.
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		// Queue signals are only worth retrying when we actually "wait": when queue_max_waits=0 (fail fast)
		// it must not degrade into 1s/2s transient retries (which would just hit upstream in vain without getting results).
		// Judgment goes via two paths: ErrType and signal; friendly error text may no longer contain queue markers.
		if ue.ErrType == ErrTypeModelBusy {
			return QueueMaxWaits() > 0
		}
		if _, queued := QueueSignalFromError(err); queued {
			return QueueMaxWaits() > 0
		}
		return IsTransientUpstream(ue.Status, ue.Detail)
	}
	return IsTransientTransport(err)
}

// When sensitiveFieldNames matches, its string sub-values will be desensitized (recursively processing arrays/objects)
var sensitiveFieldNames = map[string]bool{
	"content":      true,
	"text":         true,
	"system":       true,
	"input":        true,
	"instructions": true,
	"prompt":       true,
	"description":  true,
}

func redactValue(v interface{}, parentField string) {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, child := range x {
			if s, ok := child.(string); ok {
				if (sensitiveFieldNames[k] || sensitiveFieldNames[parentField]) && len(s) > 32 {
					x[k] = fmt.Sprintf("<redacted len=%d>", len(s))
				}
				continue
			}
			redactValue(child, k)
		}
	case []interface{}:
		for i, item := range x {
			if s, ok := item.(string); ok {
				if sensitiveFieldNames[parentField] && len(s) > 32 {
					x[i] = fmt.Sprintf("<redacted len=%d>", len(s))
				}
				continue
			}
			redactValue(item, parentField)
		}
	}
}

func StrVal(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func StrValDefault(m map[string]interface{}, key, def string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

// InferAgent heuristically infers the agent type based on the model name, used to select the correct
// mapping bucket for multi-agent shared endpoints like chat/completions. Keyword matches follow gemini > claude > default codex.
func InferAgent(model string) string {
	low := strings.ToLower(model)
	switch {
	case strings.Contains(low, "gemini"):
		return "gemini"
	case strings.Contains(low, "claude"), strings.Contains(low, "sonnet"), strings.Contains(low, "opus"), strings.Contains(low, "haiku"):
		return "claude"
	default:
		return "codex"
	}
}

// defaultModelMapping is the built-in static mapping table of the bridge.
// It uses the format "family keyword → Qoder model.key", relying on the bidirectional substring fuzzy matching of lookupMapping below,
// allowing a single "sonnet" to cover all variants like claude-sonnet-4-6 / claude-sonnet-4-20250514.
//
// Design principle: The default table only ensures requests land on valid SKUs without error.
// Valid upstream Qoder keys are returned by /v1/models and adjusted dynamically as upstream changes.
// GPT / Gemini have no dedicated SKUs and are uniformly mapped to the main performance tier — neither wasting the ultimate high price_factor,
// nor being rate-limited by lite; for tiered routing (e.g., gpt-5→ultimate / gpt-5-mini→efficient), please configure in the UI.
var defaultModelMapping = map[string]string{
	// Claude three tiers
	"opus":   "qmodel_38max",
	"sonnet": "gmodel",
	"haiku":  "qfmodel",
	// Non-Claude family fallback
	"gpt":    "dmodel",
	"gemini": "gmodel",
}

// resolveModel resolves the model name passed by the client into the upstream model_config.key.
//
// Resolution order:
//  1. Exact match in this instance's model index (external slug "deepseek-v4-pro", or compatible old upstream
//     key "dmodel");
//  2. MapModel handles scenarios where "the client sends another provider's model name" (claude-sonnet-4-6 → gmodel,
//     as well as case normalization for documentation display_name).
//
// When missed, MapModel returns ToLower as-is—upstream returns content as usual but without quota consumption,
// so a Debug is left here to facilitate troubleshooting "dashboard has no usage".
func (b *Bridge) resolveModel(agent, model string) string {
	if b.models != nil {
		if key, okKey := b.models.UpstreamKey(model); okKey {
			return key
		}
	}
	return MapModel(agent, model)
}

// Fallback rules for MapModel to map external model names to upstream keys:
//  1. defaultModelMapping exact hit
//  2. defaultModelMapping bidirectional substring fuzzy matching (sorted by source length descending, longest first)
//  3. ToLower fallback (e.g., client passes display_name case variations like "Performance")
//
// Only process cases where the index misses; models registered by this plugin itself go through Bridge.resolveModel
// for precise resolution via modelIndex first.
//
// agent is passed in by the handler entry and retained for future extensions differentiated by agent; currently not involved in mapping.
//
// Meaning of bidirectional substring: source contains model (short keyword matches long model name, e.g., "sonnet" → "claude-sonnet-4-6")
// or model contains source (long alias matches short model name, can also match in reverse).
func MapModel(agent, model string) string {
	if model == "" {
		return model
	}

	// Built-in default mapping fallback
	if mapped := lookupMapping(model, defaultModelMapping); mapped != "" {
		return mapped
	}

	// Case normalization fallback (client might pass Qoder keys directly like "Performance"/"dmodel")
	return strings.ToLower(model)
}

// lookupMapping executes within a single mapping table: exact match -> length descending + bidirectional substring fuzzy matching.
// Returns an empty string if missed.
func lookupMapping(model string, table map[string]string) string {
	if len(table) == 0 {
		return ""
	}
	// 1. Exact match
	if v, ok := table[model]; ok && v != "" {
		return v
	}
	// 2. Length descending, ensuring "claude-sonnet-4-6" matches before "sonnet"
	type kv struct{ src, dst string }
	pairs := make([]kv, 0, len(table))
	for k, v := range table {
		if k == "" || v == "" {
			continue
		}
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].src) > len(pairs[j].src) })
	mLow := strings.ToLower(model)
	for _, p := range pairs {
		sLow := strings.ToLower(p.src)
		if strings.Contains(mLow, sLow) || strings.Contains(sLow, mLow) {
			return p.dst
		}
	}
	return ""
}

// Upstream queuing/service not ready signal.
//
// Free models (such as Qwen3.8-Flash / qfmodel) are queued by upstream, returning HTTP 403 +
// payloads like the following (where message wraps two layers of JSON strings, and is escaped):
//
//	{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,
//	 \\\"modelKey\\\":\\\"qfmodel\\\",\\\"queueCount\\\":0,\\\"queueType\\\":\\\"p3\\\",
//	 \\\"retryAfterSeconds\\\":30,\\\"serviceAvailable\\\":false,\\\"waitTime\\\":30}\"}"}
//
// This is not a quota/credential error: retrying after a moment will succeed. The old implementation treated it as a 403 permission error,
// so free models could never be used (the host would also display insufficient_quota, misleading users into topping up).
type QueueSignal struct {
	Queued            bool
	ServiceAvailable  bool
	ModelKey          string
	QueueType         string
	QueueCount        int
	RetryAfterSeconds int
	WaitTimeSeconds   int
}

// WaitDuration returns the duration to wait as suggested by upstream (config-overridden values do not pass through here).
func (s QueueSignal) WaitDuration() time.Duration {
	if s.retryAfter() > 0 {
		return s.cappedWait()
	}
	return time.Duration(DefaultQueueWaitSeconds) * time.Second
}

// retryAfter is the wait seconds given by upstream (retryAfterSeconds takes priority, falling back to waitTime).
func (s QueueSignal) retryAfter() int {
	if s.RetryAfterSeconds > 0 {
		return s.RetryAfterSeconds
	}
	if s.WaitTimeSeconds > 0 {
		return s.WaitTimeSeconds
	}
	return 0
}

func (s QueueSignal) cappedWait() time.Duration {
	d := time.Duration(s.retryAfter()) * time.Second
	if d > MaxQueueWait {
		return MaxQueueWait
	}
	return d
}

const (
	// DefaultQueueWaitSeconds is the fallback when upstream doesn't suggest a wait duration (seconds).
	DefaultQueueWaitSeconds = 15
	// DefaultQueueMaxWaits is the default maximum number of queue waits allowed within a single request.
	DefaultQueueMaxWaits = 2
	// MaxQueueWait is the upper limit for a single queue wait, preventing upstream from giving an absurd number that hangs the request.
	MaxQueueWait = 90 * time.Second
)

// QueueMaxWaits returns the currently allowed number of queue waits.
func QueueMaxWaits() int { return DefaultQueueMaxWaits }

// queueWaitFor calculates how long to wait for this queueing: config override takes priority, otherwise upstream suggested value is used (subject to MaxQueueWait constraint).
func queueWaitFor(signal QueueSignal) time.Duration {
	if signal.retryAfter() > 0 {
		return signal.cappedWait()
	}
	return time.Duration(DefaultQueueWaitSeconds) * time.Second
}

// quoted matches potentially escaped JSON quotes: forms like \"isQueued\" are common in the body.
const quoted = `(?:\\?")?`

var (
	queueIntPattern    = regexp.MustCompile(quoted + `(?:retryAfterSeconds|waitTime)` + quoted + `\s*:\s*\\?"?\s*(\d+)`)
	queueStringPattern = regexp.MustCompile(quoted + `(?:modelKey|queueType)` + quoted + `\s*:\s*\\?"?\s*([A-Za-z0-9_.\-]+)`)
	queueBoolPattern   = regexp.MustCompile(quoted + `(?:isQueued|serviceAvailable)` + quoted + `\s*:\s*(true|false)`)
	queueQueuedPattern = regexp.MustCompile(quoted + `isQueued` + quoted + `\s*:\s*(true|false)`)
	queueAvailPattern  = regexp.MustCompile(quoted + `serviceAvailable` + quoted + `\s*:\s*(true|false)`)
)

// ParseQueueSignal identifies queuing signals from the upstream response body.
//
// The body may be multi-layered JSON strings (hosts/upstream are accustomed to wrapping error bodies an extra layer), so we unwrap layer by layer first,
// and then do a regex fallback scan—relying only on the existence of a few fields without assuming the overall structure.
func ParseQueueSignal(detail string) (QueueSignal, bool) {
	trimmed := strings.TrimSpace(detail)
	if trimmed == "" {
		return QueueSignal{}, false
	}
	if !strings.Contains(trimmed, "isQueued") && !strings.Contains(trimmed, "serviceAvailable") &&
		!strings.Contains(trimmed, "queueType") {
		return QueueSignal{}, false
	}

	signal := QueueSignal{ServiceAvailable: true}
	// seenBool records whether the queue boolean field was actually read from the body (to avoid misjudging unrelated errors as queueing).
	seenBool := false
	applyQueueFields := func(segment string) {
		// Layer-by-layer unwrap: if the current segment itself is JSON, take message/msg/detail and continue, while reading fields of this layer.
		for depth := 0; depth < 6 && segment != ""; depth++ {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(segment), &parsed) != nil {
				return
			}
			if queued, ok := parsed["isQueued"].(bool); ok {
				signal.Queued = queued
				seenBool = true
			}
			if available, ok := parsed["serviceAvailable"].(bool); ok {
				signal.ServiceAvailable = available
				seenBool = true
			}
			if key, ok := parsed["modelKey"].(string); ok && strings.TrimSpace(key) != "" {
				signal.ModelKey = strings.TrimSpace(key)
			}
			if queueType, ok := parsed["queueType"].(string); ok && strings.TrimSpace(queueType) != "" {
				signal.QueueType = strings.TrimSpace(queueType)
			}
			signal.RetryAfterSeconds = firstPositiveInt(signal.RetryAfterSeconds, parsed["retryAfterSeconds"])
			signal.WaitTimeSeconds = firstPositiveInt(signal.WaitTimeSeconds, parsed["waitTime"])
			signal.QueueCount = firstPositiveInt(signal.QueueCount, parsed["queueCount"])

			next := ""
			for _, key := range []string{"message", "msg", "detail", "error"} {
				switch value := parsed[key].(type) {
				case string:
					if strings.TrimSpace(value) != "" {
						next = strings.TrimSpace(value)
					}
				case map[string]interface{}:
					if encoded, errMarshal := json.Marshal(value); errMarshal == nil {
						next = string(encoded)
					}
				}
				if next != "" {
					break
				}
			}
			segment = next
		}
	}
	applyQueueFields(trimmed)

	// Regex fallback: the payload might be escaped to the point that json.Unmarshal cannot parse it (e.g., upstream concatenating strings directly).
	if signal.RetryAfterSeconds == 0 {
		if match := queueIntPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			signal.RetryAfterSeconds, _ = strconv.Atoi(match[1])
		}
	}
	if signal.ModelKey == "" {
		if match := queueStringPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			signal.ModelKey = match[1]
		}
	}
	// Boolean field fallback: the body might be escaped to the point that json.Unmarshal cannot parse it (cases where upstream concatenates strings).
	if match := queueQueuedPattern.FindStringSubmatch(trimmed); len(match) == 2 {
		if !signal.Queued {
			signal.Queued = match[1] == "true"
			seenBool = true
		}
	}
	if match := queueAvailPattern.FindStringSubmatch(trimmed); len(match) == 2 {
		if signal.ServiceAvailable {
			signal.ServiceAvailable = match[1] == "true"
			seenBool = true
		}
	}
	if !seenBool {
		// Not a single boolean field was read, and there are no queuing traces: not a queuing signal.
		if !queueBoolPattern.MatchString(trimmed) {
			return QueueSignal{}, false
		}
	}
	if signal.ServiceAvailable && !signal.Queued {
		return QueueSignal{}, false
	}
	return signal, true
}

func firstPositiveInt(current int, value interface{}) int {
	if current > 0 {
		return current
	}
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			return int(typed)
		}
	case json.Number:
		if parsed, errParse := typed.Int64(); errParse == nil && parsed > 0 {
			return int(parsed)
		}
	case string:
		if parsed, errParse := strconv.Atoi(strings.TrimSpace(typed)); errParse == nil && parsed > 0 {
			return parsed
		}
	}
	return current
}

// QueueSignalError is the error after the queue wait budget is exhausted: retry after waiting for a period of time,
// Irrelevant to quota/credentials.
func QueueSignalError(signal QueueSignal) *UpstreamError {
	detail := "Upstream queuing"
	if signal.ModelKey != "" {
		detail = "Upstream model " + signal.ModelKey + " is queuing"
	}
	if signal.QueueType != "" {
		detail += " (Queue " + signal.QueueType + ")"
	}
	detail += ", please wait and retry"
	return &UpstreamError{
		Status:  503,
		Detail:  detail,
		ErrType: ErrTypeModelBusy,
		Message: detail + " (Not a quota or credential issue; the plugin has waited as suggested by upstream, but it is still unavailable)",
		// Must include the signal itself: flags like isQueued are no longer present in the user-friendly text,
		// and the upper layer (isRetryableStreamError / executor) relies on this to determine "this is a queue, not a transient failure".
		Queue: &signal,
	}
}

// QueueSignalFromError extracts the queue signal from an error: prioritizing structured fields,
// and falling back to re-recognizing in the message/detail text (as errors may be wrapped).
func QueueSignalFromError(err error) (QueueSignal, bool) {
	if err == nil {
		return QueueSignal{}, false
	}
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		if upstream.Queue != nil {
			return *upstream.Queue, true
		}
		if signal, queued := ParseQueueSignal(upstream.Detail); queued {
			return signal, true
		}
	}
	return ParseQueueSignal(err.Error())
}

// Region identifies the Qoder site region
type Region string

const (
	RegionGlobal Region = "global"
	RegionCN     Region = "cn"
)

// Endpoints contains all API endpoints corresponding to a region
type Endpoints struct {
	DeviceLoginBase string // OAuth login page
	PollEndpoint    string // deviceToken/poll
	UserinfoBase    string // /api/v1/userinfo
	PlanEndpoint    string // /api/v2/user/plan
	QuotaEndpoint   string // /api/v2/quota/usage
	ChatStreamURL   string // SSE chat
	ModelListURL    string // model list
	JobTokenURL     string // PAT exchange
	// SashBase is the domain for check-in/campaign APIs (/sash/api/v1/...), domain only without path.
	// Must be selected by region: sending a Global token to a CN domain will result in 401 TOKEN_EXPIRE.
	SashBase string
}

var endpointsGlobal = Endpoints{
	DeviceLoginBase: "https://qoder.com/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.sh/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.sh/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.sh/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.sh/api/v2/quota/usage",
	ChatStreamURL:   "https://api1.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	ModelListURL:    "https://api2.qoder.sh/algo/api/v2/model/list?Encode=1",
	JobTokenURL:     "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1",
	SashBase:        "https://openapi.qoder.sh",
}

var endpointsCN = Endpoints{
	DeviceLoginBase: "https://qoder.com.cn/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.com.cn/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.com.cn/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.com.cn/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.com.cn/api/v2/quota/usage",
	ChatStreamURL:   "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	ModelListURL:    "https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1",
	JobTokenURL:     "https://gateway.qoder.com.cn/algo/api/v3/user/jobToken?Encode=1",
	SashBase:        "https://openapi.qoder.com.cn",
}

// GetEndpoints returns the endpoint configuration for a region, treating empty or unknown values as global
func GetEndpoints(region Region) Endpoints {
	if region == RegionCN {
		return endpointsCN
	}
	return endpointsGlobal
}

// NormalizeRegion normalizes a string to a valid Region value
func NormalizeRegion(s string) Region {
	if s == "cn" || s == "CN" {
		return RegionCN
	}
	return RegionGlobal
}
