package minimaxcode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"

	"unicode/utf8"
)

var (
	contentKeys  = []string{"msg_content", "msgContent", "content", "text", "delta", "answer"}
	thinkingKeys = []string{
		"reasoning_content", "thinking_content", "reason_content",
		"think_content", "reasoning", "thinking",
	}
	// upstreamErrTextRe recognizes full messages starting with "digital error code:" (e.g. 50110:insufficient balance).
	// It is only used for complete message nodes, not streaming increments—increment split points are arbitrary, so anchoring the start would cause false positives.
	upstreamErrTextRe = regexp.MustCompile(`^\d{5,8}:\s*[A-Za-z]`)
)

// ChatRequest internal parsing structure.
type chatPayload struct {
	Model      string `json:"model"`
	Messages   []any  `json:"messages"`
	Stream     bool   `json:"stream"`
	Tools      []any  `json:"tools"`
	ToolChoice any    `json:"tool_choice"`
}

// toolsEnabledFor determines whether this request carries tools (deciding whether to enable the marking protocol).
func (cp *chatPayload) toolsEnabled() bool {
	return len(cp.Tools) > 0 && !isToolChoiceNone(cp.ToolChoice)
}

// frameAggregator aggregates the frame content of an entire upstream SSE stream.
//
// The upstream has three types of text-carrying frames, handled differently:
//   - agent_message_chunk: streaming increment, can be emitted downstream directly;
//   - agent_message: complete message node, where role=user is the upstream echo of user input
//     (must be discarded—otherwise the prompt would be output to the client as-is), and role=assistant
//     is a complete copy of the full reply, duplicating the chunk increment, used only when there are no chunks;
//   - other unknown frames: deep search fallback, lowest priority.
type frameAggregator struct {
	chunkText, chunkThinking strings.Builder
	msgText, msgThinking     strings.Builder
	miscText, miscThinking   strings.Builder
	finishReason             string
	err                      string
}

func (a *frameAggregator) add(payload map[string]any) {
	if a.err != "" {
		return
	}
	if errMsg := extractUpstreamError(payload); errMsg != "" {
		a.err = errMsg
		return
	}
	if fr := findDeepString(payload, "finish_reason", "finishReason"); fr != "" {
		a.finishReason = fr
	}

	chunkNode, hasChunk := payload["agent_message_chunk"].(map[string]any)
	msgNode, hasMsg := payload["agent_message"].(map[string]any)

	switch {
	case hasChunk:
		a.chunkText.WriteString(findDeepString(chunkNode, contentKeys...))
		a.chunkThinking.WriteString(findDeepString(chunkNode, thinkingKeys...))
	case hasMsg:
		if isUserRoleNode(msgNode) {
			// User message echoed by upstream: neither a reply nor an increment, discard directly.
			return
		}
		text := findDeepString(msgNode, contentKeys...)
		a.msgText.WriteString(text)
		a.msgThinking.WriteString(findDeepString(msgNode, thinkingKeys...))
		// Text starting with "error code:" on complete message nodes is treated as an upstream error.
		if errMsg := looksLikeUpstreamErrorText(a.msgText.String()); errMsg != "" {
			a.err = errMsg
		}
	default:
		text := findDeepStringSkipUser(payload, contentKeys...)
		a.miscText.WriteString(text)
		a.miscThinking.WriteString(findDeepStringSkipUser(payload, thinkingKeys...))
		if errMsg := looksLikeUpstreamErrorText(a.miscText.String()); errMsg != "" {
			a.err = errMsg
		}
	}
}

// chunkContent reports whether there is chunk increment text.
func (a *frameAggregator) hasChunkContent() bool {
	return a.chunkText.Len() > 0
}

// finalize retrieves the body and thinking content after the stream ends (chunks take priority, complete message fallback).
func (a *frameAggregator) finalize() (text, thinking string) {
	text = a.chunkText.String()
	if text == "" {
		text = a.msgText.String()
	}
	if text == "" {
		text = a.miscText.String()
	}
	thinking = a.chunkThinking.String()
	if thinking == "" {
		thinking = a.msgThinking.String()
	}
	if thinking == "" {
		thinking = a.miscThinking.String()
	}
	return text, thinking
}

// errorText returns structured or text-form upstream errors (if any).
func (a *frameAggregator) errorText() string {
	if a.err != "" {
		return a.err
	}
	return ""
}

// isUserRoleNode determines whether a node is marked as a user role (echo frame).
func isUserRoleNode(node map[string]any) bool {
	role, _ := node["role"].(string)
	return strings.EqualFold(strings.TrimSpace(role), "user")
}

// findDeepStringSkipUser deeply searches for strings, skipping role=user nodes (echo protection).
func findDeepStringSkipUser(data any, keys ...string) string {
	switch v := data.(type) {
	case map[string]any:
		if isUserRoleNode(v) {
			return ""
		}
		for _, k := range keys {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
		}
		for _, val := range v {
			if found := findDeepStringSkipUser(val, keys...); found != "" {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findDeepStringSkipUser(item, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}

// looksLikeUpstreamErrorText determines whether a complete message is upstream error text.
func looksLikeUpstreamErrorText(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || len(trimmed) > 300 {
		return ""
	}
	if upstreamErrTextRe.MatchString(trimmed) {
		return trimmed
	}
	return ""
}

// openAIToolCalls converts parsing results into OpenAI tool_calls arrays.
func openAIToolCalls(calls []ToolCall) []any {
	out := make([]any, 0, len(calls))
	for i, call := range calls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%s", RandomUUID()[:8])
		}
		out = append(out, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
			"index": i,
		})
	}
	return out
}

// normalizeFinishReason only allows values recognized by the client.
//
// Upstream may return non-standard values like "error" (error text is already intercepted in error scenarios,
// but this is a defensive fallback); passing unknown values through makes protocol converter behavior uncertain.
func normalizeFinishReason(reason string) string {
	switch reason {
	case "stop", "length":
		return reason
	default:
		return "stop"
	}
}

// Chat executes a non-streaming conversation.
func Chat(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string) ([]byte, error) {
	return chat(ctx, cred, req, baseURLOverride, nil)
}

// chat is the implementation of Chat; when workspace is nil, account-level directory caching is not used.
func chat(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string, workspace *workspaceCache) ([]byte, error) {
	var cp chatPayload
	if len(req.Payload) > 0 {
		_ = json.Unmarshal(req.Payload, &cp)
	}
	toolsEnabled := cp.toolsEnabled()

	respBody, modelName, promptTokens, errDo := doMessageRequest(ctx, cred, req, baseURLOverride, workspace)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = respBody.Close() }()

	agg := &frameAggregator{}

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var payload map[string]any
		if errJSON := json.Unmarshal([]byte(data), &payload); errJSON != nil {
			continue
		}

		agg.add(payload)
		if errMsg := agg.errorText(); errMsg != "" {
			return nil, Classify(0, errMsg)
		}
	}

	if errScan := scanner.Err(); errScan != nil && errScan != io.EOF {
		return nil, fmt.Errorf("read stream: %w", errScan)
	}

	if errMsg := agg.errorText(); errMsg != "" {
		return nil, Classify(0, errMsg)
	}

	contentStr, reasoningStr := agg.finalize()
	var calls []ToolCall
	if toolsEnabled {
		contentStr, calls = parseToolCalls(contentStr)
	}
	if contentStr == "" && reasoningStr == "" && len(calls) == 0 {
		return nil, fmt.Errorf("minimax upstream returned empty response")
	}

	finishReason := normalizeFinishReason(agg.finishReason)

	msgMap := map[string]any{
		"role":    "assistant",
		"content": contentStr,
	}
	if reasoningStr != "" {
		msgMap["reasoning_content"] = reasoningStr
	}
	if len(calls) > 0 {
		finishReason = "tool_calls"
		msgMap["tool_calls"] = openAIToolCalls(calls)
		if contentStr == "" {
			// OpenAI semantics: content is null only when there are tool calls, not an empty string.
			msgMap["content"] = nil
		}
	}

	completionTokens := EstimateTokens(contentStr + reasoningStr)

	out := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msgMap,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}

	return json.Marshal(out)
}

// ChatStream executes a streaming conversation, with chunks emitted via the onChunk callback.
func ChatStream(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string, onChunk func([]byte) error) error {
	return chatStream(ctx, cred, req, baseURLOverride, onChunk, nil)
}

// chatStream is the implementation of ChatStream; when workspace is nil, account-level directory caching is not used.
func chatStream(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string, onChunk func([]byte) error, workspace *workspaceCache) error {
	var cp chatPayload
	if len(req.Payload) > 0 {
		_ = json.Unmarshal(req.Payload, &cp)
	}
	toolsEnabled := cp.toolsEnabled()

	respBody, modelName, _, errDo := doMessageRequest(ctx, cred, req, baseURLOverride, workspace)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = respBody.Close() }()

	agg := &frameAggregator{}
	parser := &toolStreamParser{}

	chunkID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	emitDelta := func(delta map[string]any) error {
		chunk := map[string]any{
			"id":      chunkID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         delta,
					"finish_reason": nil,
				},
			},
		}
		chunkBytes, _ := json.Marshal(chunk)
		return onChunk(chunkBytes)
	}
	emitContent := func(text string) error {
		if text == "" {
			return nil
		}
		return emitDelta(map[string]any{"content": text})
	}

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var payload map[string]any
		if errJSON := json.Unmarshal([]byte(data), &payload); errJSON != nil {
			continue
		}

		// Structured errors, echo filtering, and error code text recognition are all completed within add.
		agg.add(payload)
		if errMsg := agg.errorText(); errMsg != "" {
			return Classify(0, errMsg)
		}

		// Chunk increments are emitted immediately; complete message node texts are selected by priority when the stream ends.
		if chunkNode, ok := payload["agent_message_chunk"].(map[string]any); ok {
			thinking := findDeepString(chunkNode, thinkingKeys...)
			if thinking != "" {
				if errEmit := emitDelta(map[string]any{"reasoning_content": thinking}); errEmit != nil {
					return errEmit
				}
			}
			text := findDeepString(chunkNode, contentKeys...)
			if text != "" {
				outText := text
				if toolsEnabled {
					outText = parser.push(text)
				}
				if errEmit := emitContent(outText); errEmit != nil {
					return errEmit
				}
			}
		}
	}

	if errScan := scanner.Err(); errScan != nil && errScan != io.EOF {
		return fmt.Errorf("read stream: %w", errScan)
	}
	if errMsg := agg.errorText(); errMsg != "" {
		return Classify(0, errMsg)
	}

	// When there are no chunk increments at all, the complete message is the reply body (error detection was already triggered within add).
	var calls []ToolCall
	if !agg.hasChunkContent() {
		finalText, _ := agg.finalize()
		if finalText != "" {
			if toolsEnabled {
				if errEmit := emitContent(parser.push(finalText)); errEmit != nil {
					return errEmit
				}
			} else if errEmit := emitContent(finalText); errEmit != nil {
				return errEmit
			}
		}
		// When chunks have no thinking increments, fall back to the thinking on the complete message.
		if agg.chunkThinking.Len() == 0 {
			if thinking := agg.msgThinking.String(); thinking != "" {
				if errEmit := emitDelta(map[string]any{"reasoning_content": thinking}); errEmit != nil {
					return errEmit
				}
			} else if thinking := agg.miscThinking.String(); thinking != "" {
				if errEmit := emitDelta(map[string]any{"reasoning_content": thinking}); errEmit != nil {
					return errEmit
				}
			}
		}
	}

	if toolsEnabled {
		rest, parsedCalls := parser.flush()
		if errEmit := emitContent(rest); errEmit != nil {
			return errEmit
		}
		calls = parsedCalls
		for i, call := range calls {
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("call_%s", RandomUUID()[:8])
			}
			// Emit the complete call at once: the host accumulates arguments by index,
			// sending across multiple frames will cause duplicate concatenation.
			errEmit := emitDelta(map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": i,
						"id":    id,
						"type":  "function",
						"function": map[string]any{
							"name":      call.Name,
							"arguments": call.Arguments,
						},
					},
				},
			})
			if errEmit != nil {
				return errEmit
			}
		}
	}

	// Final end frame
	finish := normalizeFinishReason(agg.finishReason)
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	finalChunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finish,
			},
		},
	}
	finalBytes, _ := json.Marshal(finalChunk)
	return onChunk(finalBytes)
}

func doMessageRequest(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string, workspace *workspaceCache) (io.ReadCloser, string, int, error) {
	var cp chatPayload
	if len(req.Payload) > 0 {
		_ = json.Unmarshal(req.Payload, &cp)
	}

	modelID := req.Model
	if modelID == "" {
		modelID = cp.Model
	}
	modelCfg, _ := findModel(modelID)
	// Workspace resolution: upstream may rename/deprecate models (built-in directory is fixed at compile time),
	// relying on the model_id listed in the account workspace /config, falling back to built-in values if not found.
	if upstreamID, wsVariant, ok := resolveWorkspaceModel(ctx, workspace, cred, baseURLOverride, modelID, modelCfg); ok {
		modelCfg.UpstreamModel = upstreamID
		modelCfg.Variant = wsVariant
	}

	var turns []*Turn
	for _, m := range cp.Messages {
		if t := ParseTurn(m); t != nil {
			turns = append(turns, t)
		}
	}

	prompt := BuildPromptWithTools(turns, cp.Tools, cp.ToolChoice)
	if prompt == "" {
		return nil, "", 0, fmt.Errorf("empty prompt")
	}
	promptTokens := EstimateTokens(prompt)

	sessionID, errSession := createSession(ctx, cred, baseURLOverride, workspace)
	if errSession != nil {
		return nil, "", 0, fmt.Errorf("create minimax session: %w", errSession)
	}

	bodyMap := map[string]any{
		"content":      prompt,
		"turn_id":      RandomUUID(),
		"worktreeMode": false,
	}
	if modelCfg.UpstreamModel != "" {
		sel := map[string]any{
			"model_id":    modelCfg.UpstreamModel,
			"provider_id": "minimax",
		}
		if modelCfg.Variant != "" {
			sel["variant"] = modelCfg.Variant
		}
		bodyMap["model"] = sel
	}

	bodyBytes, _ := json.Marshal(bodyMap)

	streamBase := StreamHostFor(cred.Region, baseURLOverride)
	path := fmt.Sprintf(EpMessage, sessionID)
	target := buildAgentTarget(cred, streamBase, path, http.MethodPost, string(bodyBytes), true)

	resp, errDo := doRequest(ctx, cred, target, streamBase, 120*time.Second)
	if errDo != nil {
		return nil, "", 0, fmt.Errorf("send message request: %w", errDo)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, "", 0, Classify(resp.StatusCode, string(raw))
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, "", 0, Classify(resp.StatusCode, string(raw))
	}

	return resp.Body, modelCfg.ID, promptTokens, nil
}

func extractUpstreamError(payload map[string]any) string {
	if errObj, ok := payload["error"].(map[string]any); ok {
		if msg := core.GetString(errObj, "message"); msg != "" {
			return msg
		}
	}
	if errStr, ok := payload["error"].(string); ok && errStr != "" {
		return errStr
	}
	for _, key := range []string{"base_resp", "statusInfo"} {
		if node, ok := payload[key].(map[string]any); ok {
			code := core.GetInt64(node, "status_code", "code")
			if code != 0 {
				msg := core.FirstNonEmpty(core.GetString(node, "status_msg"), core.GetString(node, "message"))
				return fmt.Sprintf("status %d: %s", code, msg)
			}
		}
	}
	for _, key := range []string{"error_msg", "error_message", "err_msg", "status_msg"} {
		if msg := core.GetString(payload, key); msg != "" && !strings.EqualFold(msg, "success") && !strings.EqualFold(msg, "ok") {
			return msg
		}
	}
	return ""
}

// Target is a signed MiniMax upstream request.
type Target struct {
	Method  string
	URL     string
	SignURL string
	UnixMS  int64
	Body    string
	Checkin bool
}

// AgentInfo describes an Agent under a MiniMax account.
type AgentInfo struct {
	ID            string `json:"name"`
	Role          string `json:"agent_role"`
	RootSessionID string `json:"root_session_id"`
}

// buildAgentQuery builds the query string for the agent family of interfaces.
func buildAgentQuery(cred *Credential, unixMS int64) string {
	cred.EnsureIdentifiers()
	width := cred.ScreenWidth
	if width == 0 {
		width = 1920
	}
	height := cred.ScreenHeight
	if height == 0 {
		height = 1080
	}

	if cred.AuthMode == "oauth" {
		pairs := [][2]string{
			{"device_platform", "web"},
			{"biz_id", "3"},
			{"app_id", "3001"},
			{"version_code", "22201"},
			{"unix", strconv.FormatInt(unixMS, 10)},
			{"timezone_offset", "28800"},
			{"is_desktop", "1"},
			{"desktop_version", "3.0.73"},
			{"sys_language", "en"},
			{"lang", "en"},
			{"uuid", cred.UUID},
			{"device_id", cred.DeviceID},
			{"os_name", "macOS"},
			{"browser_name", "Chrome"},
			{"device_memory", "16"},
			{"cpu_core_num", "8"},
			{"browser_language", "zh-CN"},
			{"browser_platform", "MacIntel"},
			{"user_id", cred.UserID},
			{"screen_width", strconv.Itoa(width)},
			{"screen_height", strconv.Itoa(height)},
			{"client", "desktop"},
			{"region", "en"},
		}
		var parts []string
		for _, p := range pairs {
			// Keep desktop_version even if empty
			if p[1] != "" || p[0] == "desktop_version" {
				parts = append(parts, p[0]+"="+EncodeURIComponent(p[1]))
			}
		}
		return strings.Join(parts, "&")
	}

	// Web JWT mode
	pairs := [][2]string{
		{"device_platform", "web"},
		{"biz_id", "3"},
		{"app_id", "3001"},
		{"version_code", "22201"},
		{"unix", strconv.FormatInt(unixMS, 10)},
		{"timezone_offset", "28800"},
		{"sys_language", "en"},
		{"lang", "en"},
		{"uuid", cred.UUID},
		{"device_id", cred.DeviceID},
		{"os_name", "Windows"},
		{"browser_name", "Chrome"},
		{"device_memory", "16"},
		{"cpu_core_num", "8"},
		{"browser_language", "zh-CN"},
		{"browser_platform", "Win32"},
		{"user_id", cred.UserID},
		{"screen_width", strconv.Itoa(width)},
		{"screen_height", strconv.Itoa(height)},
		{"token", cred.AccessToken},
		{"client", "web"},
		{"region", "en"},
	}
	var parts []string
	for _, p := range pairs {
		if p[1] != "" {
			parts = append(parts, p[0]+"="+EncodeURIComponent(p[1]))
		}
	}
	return strings.Join(parts, "&")
}

// buildSigninQuery builds the query string for check-in, quota, and account information endpoints.
func buildSigninQuery(cred *Credential, unixMS int64, forSignature bool) string {
	cred.EnsureIdentifiers()
	width := cred.ScreenWidth
	if width == 0 {
		width = 1920
	}
	height := cred.ScreenHeight
	if height == 0 {
		height = 1080
	}

	userID := cred.UserID
	if userID == "" {
		userID = "0"
	}

	tokenParam := ""
	if cred.AuthMode != "oauth" {
		tokenParam = cred.AccessToken
	}

	pairs := [][2]string{
		{"device_platform", "web"},
		{"biz_id", "3"},
		{"app_id", "3001"},
		{"version_code", "22201"},
		{"unix", strconv.FormatInt(unixMS, 10)},
		{"timezone_offset", "28800"},
		{"sys_language", "en"},
		{"lang", "en"},
		{"uuid", cred.UUID},
		{"device_id", cred.DeviceID},
		{"os_name", "Windows"},
		{"browser_name", "Chrome"},
		{"device_memory", "16"},
		{"cpu_core_num", "8"},
		{"browser_language", "en-US"},
		{"browser_platform", "Win32"},
		{"user_id", userID},
		{"op_ticket", "undefined"},
		{"screen_width", strconv.Itoa(width)},
		{"screen_height", strconv.Itoa(height)},
		{"token", tokenParam},
		{"client", "web"},
	}

	var parts []string
	for _, p := range pairs {
		if p[0] == "op_ticket" && !forSignature {
			continue
		}
		if p[1] == "" && p[0] != "op_ticket" {
			continue
		}
		parts = append(parts, p[0]+"="+FormEncode(p[1]))
	}
	return strings.Join(parts, "&")
}

// buildAgentTarget builds the signing target for Agent family endpoints.
func buildAgentTarget(cred *Credential, baseURL, path, method, body string, streamHost bool) Target {
	unixMS := time.Now().UnixMilli()
	query := buildAgentQuery(cred, unixMS)
	wire := baseURL + path
	if strings.Contains(wire, "?") {
		wire += "&" + query
	} else {
		wire += "?" + query
	}

	var signURL string
	if streamHost {
		signURL = wire
	} else {
		signURL = path
		if strings.Contains(signURL, "?") {
			signURL += "&" + query
		} else {
			signURL += "?" + query
		}
	}

	return Target{
		Method:  method,
		URL:     wire,
		SignURL: signURL,
		UnixMS:  unixMS,
		Body:    body,
		Checkin: false,
	}
}

// buildSigninTarget builds the signing target for Signin/Quota/UserInfo endpoints.
func buildSigninTarget(cred *Credential, baseURL, path, method, body string) Target {
	unixMS := time.Now().UnixMilli()
	signQuery := buildSigninQuery(cred, unixMS, true)
	wireQuery := buildSigninQuery(cred, unixMS, false)

	signURL := path + "?" + signQuery
	wireURL := baseURL + path + "?" + wireQuery

	return Target{
		Method:  method,
		URL:     wireURL,
		SignURL: signURL,
		UnixMS:  unixMS,
		Body:    body,
		Checkin: true,
	}
}

// buildHeaders assembles outbound HTTP request headers.
func buildHeaders(cred *Credential, target Target, origin string) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")

	unixSec := target.UnixMS / 1000
	h.Set("X-Timestamp", strconv.FormatInt(unixSec, 10))
	h.Set("X-Signature", XSignature(unixSec, target.Body))

	yyBody := target.Body
	if target.Checkin {
		yyBody = "{}"
		h.Set("Accept", "*/*")
	} else if cred.AuthMode == "oauth" {
		h.Set("Accept", "application/json, text/plain, */*")
	} else {
		h.Set("Accept", "text/event-stream, application/json, */*")
	}

	h.Set("YY", YYSignature(target.SignURL, yyBody, target.UnixMS))

	if cred.AuthMode == "oauth" {
		h.Set("User-Agent", DesktopUA)
		h.Set("Authorization", "Bearer "+cred.AccessToken)
	} else {
		h.Set("User-Agent", WebUA)
		if cred.AccessToken != "" {
			h.Set("Token", cred.AccessToken)
		}
		h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	}
	return h
}

// doRequest executes a signed HTTP request.
func doRequest(ctx context.Context, cred *Credential, target Target, origin string, timeout time.Duration) (*http.Response, error) {
	var bodyReader io.Reader
	if target.Body != "" {
		bodyReader = bytes.NewReader([]byte(target.Body))
	}
	req, errNew := http.NewRequestWithContext(ctx, target.Method, target.URL, bodyReader)
	if errNew != nil {
		return nil, fmt.Errorf("create request: %w", errNew)
	}
	req.Header = buildHeaders(cred, target, origin)
	return hostbridge.Client(ctx, timeout).Do(req)
}

// fetchUserInfo retrieves the current account's realUserID and display identifier.
func fetchUserInfo(ctx context.Context, cred *Credential, baseURLOverride string) (userID, name, email string, err error) {
	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildSigninTarget(cred, base, EpUserInfo, http.MethodGet, "")
	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return "", "", "", errDo
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", "", Classify(resp.StatusCode, string(body))
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return "", "", "", errDec
	}

	profile := raw
	if data, ok := raw["data"].(map[string]any); ok {
		profile = data
	}
	if ui, ok := profile["userInfo"].(map[string]any); ok {
		profile = ui
	}

	realUserID := core.GetString(profile, "realUserID", "real_user_id", "userID", "user_id")
	accName := core.GetString(profile, "name", "username")
	accEmail := core.GetString(profile, "email", "phone")

	if realUserID != "" {
		cred.UserID = realUserID
	}
	if accEmail != "" && cred.Email == "" {
		cred.Email = accEmail
	}
	if accName != "" && cred.Label == "" {
		cred.Label = accName
	}

	return realUserID, accName, accEmail, nil
}

// fetchAgents retrieves the list of agents under the account and picks a suitable AgentID for chat.
func fetchAgents(ctx context.Context, cred *Credential, baseURLOverride string) (string, error) {
	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildAgentTarget(cred, base, EpAgentList, http.MethodGet, "", false)
	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return "", errDo
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", Classify(resp.StatusCode, string(body))
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return "", errDec
	}

	var agents []AgentInfo
	profile := raw
	if data, ok := raw["data"].(map[string]any); ok {
		profile = data
	}
	if rawAgents, ok := profile["agents"].([]any); ok {
		for _, a := range rawAgents {
			if am, ok := a.(map[string]any); ok {
				agents = append(agents, AgentInfo{
					ID:            core.GetString(am, "name", "id"),
					Role:          core.GetString(am, "agent_role", "role"),
					RootSessionID: core.GetString(am, "root_session_id"),
				})
			}
		}
	}

	// Prefer mavis, second general, and finally any agent with an ID
	for _, a := range agents {
		if a.Role == "mavis" && a.ID != "" {
			return a.ID, nil
		}
	}
	for _, a := range agents {
		if a.Role == "general" && a.ID != "" {
			return a.ID, nil
		}
	}
	for _, a := range agents {
		if a.ID != "" {
			return a.ID, nil
		}
	}
	return "", nil
}

// prepare executes the Agent startup handshake (/config, /agent, /channel/connections).
//
// Step 1: Reuse workspace directory cache: do not repeat if this instance recently fetched the same /config
// request (including failure cooling, avoiding extra timeout costs per message during upstream jitter). When workspace is
// nil (one-time flows like login/check-in), fetch only once, and the result is not used for display.
func prepare(ctx context.Context, cred *Credential, baseURLOverride string, workspace *workspaceCache) error {
	base := AgentHostFor(cred.Region, baseURLOverride)

	// 1. /config endpoint
	_ = workspace.models(ctx, cred, baseURLOverride)

	// 2. /agent list parsing
	if cred.AgentID == "" {
		if agentID, err := fetchAgents(ctx, cred, baseURLOverride); err == nil && agentID != "" {
			cred.AgentID = agentID
		}
	}

	// 3. /channel/connections endpoint
	targetConn := buildAgentTarget(cred, base, EpConnections, http.MethodGet, "", false)
	if resp, err := doRequest(ctx, cred, targetConn, base, 20*time.Second); err == nil {
		_ = resp.Body.Close()
	}

	return nil
}

// createSession opens an independent chat session.
func createSession(ctx context.Context, cred *Credential, baseURLOverride string, workspace *workspaceCache) (string, error) {
	if cred.UserID == "" {
		_, _, _, _ = fetchUserInfo(ctx, cred, baseURLOverride)
	}
	if cred.AgentID == "" {
		_ = prepare(ctx, cred, baseURLOverride, workspace)
	}
	if cred.AgentID == "" {
		return "", fmt.Errorf("minimax agent_id is unknown and could not be discovered")
	}

	base := AgentHostFor(cred.Region, baseURLOverride)
	path := fmt.Sprintf(EpSession, cred.AgentID)
	bodyBytes, _ := json.Marshal(map[string]any{
		"agent_id":     cred.AgentID,
		"worktreeMode": false,
	})

	target := buildAgentTarget(cred, base, path, http.MethodPost, string(bodyBytes), false)
	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return "", errDo
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", Classify(resp.StatusCode, string(body))
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return "", fmt.Errorf("decode create session response: %w", errDec)
	}

	sessionID := findDeepString(raw, "session_id", "sessionId", "id", "sessionID")
	if sessionID == "" {
		rawBytes, _ := json.Marshal(raw)
		return "", fmt.Errorf("no session_id in response: %s", string(rawBytes))
	}

	return sessionID, nil
}

func findDeepString(data any, keys ...string) string {
	switch v := data.(type) {
	case map[string]any:
		for _, k := range keys {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
		}
		for _, val := range v {
			if found := findDeepString(val, keys...); found != "" {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findDeepString(item, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}

// This file parses vendor-specific configurations for MiniMax Code.

// isUnreserved determines whether a character is unreserved according to JavaScript encodeURIComponent.
// JS standard character set: A-Z a-z 0-9 - _ . ! ~ * ' ( )
func isUnreserved(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '-' || b == '_' || b == '.' ||
		b == '!' || b == '~' || b == '*' ||
		b == '\'' || b == '(' || b == ')'
}

// EncodeURIComponent simulates the behavior of JavaScript's encodeURIComponent.
// Encodes non-ASCII characters as uppercase %XX sequences using UTF-8 bytes.
func EncodeURIComponent(value string) string {
	var buf strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if isUnreserved(b) {
			buf.WriteByte(b)
		} else {
			fmt.Fprintf(&buf, "%%%02X", b)
		}
	}
	return buf.String()
}

// FormEncode simulates URLSearchParams form key-value encoding.
// Character set matches EncodeURIComponent, but spaces are escaped as +.
func FormEncode(value string) string {
	return strings.ReplaceAll(EncodeURIComponent(value), "%20", "+")
}

// MD5Hex calculates the lowercase 32-bit MD5 hash of a string.
func MD5Hex(value string) string {
	h := md5.Sum([]byte(value))
	return hex.EncodeToString(h[:])
}

// XSignature calculates the x-signature request header: MD5(unix_seconds + salt + body).
func XSignature(unixSeconds int64, body string) string {
	return MD5Hex(fmt.Sprintf("%d%s%s", unixSeconds, SignatureSalt, body))
}

// YYSignature calculates the yy request header: MD5(encodeURIComponent(sign_url) + "_" + body + MD5(ms) + "ooui").
func YYSignature(signURL, body string, unixMS int64) string {
	msMD5 := MD5Hex(strconv.FormatInt(unixMS, 10))
	input := EncodeURIComponent(signURL) + "_" + body + msMD5 + SignatureSuffix
	return MD5Hex(input)
}

// Region is the MiniMax Code service deployment region.
type Region string

const (
	RegionCN     Region = "cn"
	RegionGlobal Region = "global"

	VendorIDCN       = "minimaxcodecn"
	VendorNameCN     = "MiniMax Code CN"
	VendorIDGlobal   = "minimaxcodeglobal"
	VendorNameGlobal = "MiniMax Code Global"

	// CN
	AccountHostCN = "https://account.minimax.cn"
	AgentHostCN   = "https://agent.minimaxi.com"
	StreamHostCN  = "https://agent-stream.minimaxi.com"
	// Open platform domain: coding_plan/remains (Token Plan window) is hosted here,
	// separated from the signed account domain, and can be accessed with pure Bearer.
	PlatformHostCN = "https://www.minimaxi.com"

	// Global
	AccountHostGlobal  = "https://account.minimax.io"
	AgentHostGlobal    = "https://agent.minimax.io"
	StreamHostGlobal   = "https://agent-stream.minimax.io"
	PlatformHostGlobal = "https://platform.minimax.io"

	// OAuth2 device code configuration (aligned with desktop client)
	ClientID         = "mcode-public"
	OAuthScope       = "agent.default"
	OAuthAudience    = "agent-backend"
	DeviceGrantType  = "urn:ietf:params:oauth:grant-type:device_code"
	RefreshGrantType = "refresh_token"

	// Official signing protocol salt and suffix
	SignatureSalt   = "I*7Cf%WZ#S&%1RlZJ&C2"
	SignatureSuffix = "ooui"

	// Client UA fingerprint
	DesktopUA = "MiniMaxAgent Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) MiniMaxAgent/3.0.73 Chrome/148.0.7778.280 Safari/537.36"
	WebUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

	// Endpoint paths
	EpDeviceCode    = "/oauth2/device/code"
	EpToken         = "/oauth2/token"
	EpUserInfo      = "/v1/api/user/info"
	EpConfig        = "/minimax-cloud/api/v1/config"
	EpAgentList     = "/minimax-cloud/api/v1/agent"
	EpConnections   = "/minimax-cloud/api/v1/channel/connections"
	EpSession       = "/minimax-cloud/api/v1/agent/%s/session"
	EpMessage       = "/minimax-cloud/api/v1/session/%s/message"
	EpCheckinStatus = "/minimax-cloud/api/v1/signin/status"
	EpCheckinClaim  = "/minimax-cloud/api/v1/signin/claim"
	EpMembership    = "/matrix/api/v1/commerce/get_membership_info"
	// EpCodingPlanRemains is the 5-hour / 7-day window remaining quota for Token Plan (platform domain).
	EpCodingPlanRemains = "/v1/api/openplatform/coding_plan/remains"
)

// VendorIDFor returns the VendorID corresponding to a region.
func VendorIDFor(r Region) string {
	if r == RegionGlobal {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// VendorNameFor returns the display name corresponding to a region.
func VendorNameFor(r Region) string {
	if r == RegionGlobal {
		return VendorNameGlobal
	}
	return VendorNameCN
}

// AccountHostFor returns the account service base address.
func AccountHostFor(r Region) string {
	if r == RegionCN {
		return AccountHostCN
	}
	return AccountHostGlobal
}

// AgentHostFor returns the Agent service base address (overridable by configuration).
func AgentHostFor(r Region, override string) string {
	if trimmed := strings.TrimRight(strings.TrimSpace(override), "/"); trimmed != "" {
		return trimmed
	}
	if r == RegionCN {
		return AgentHostCN
	}
	return AgentHostGlobal
}

// PlatformHostFor returns the open platform domain for the region (used for Token Plan window queries).
//
// Not subject to <vendorid>_base_url override: agent mirrors do not necessarily host open platform APIs,
// applying the mirror address would only cause the window to be permanently missing.
func PlatformHostFor(r Region) string {
	if r == RegionCN {
		return PlatformHostCN
	}
	return PlatformHostGlobal
}

// StreamHostFor returns the streaming chat message base address.
func StreamHostFor(r Region, override string) string {
	base := AgentHostFor(r, override)
	// If it is agent.xxx, convert to agent-stream.xxx
	if strings.Contains(base, "://agent.") {
		return strings.Replace(base, "://agent.", "://agent-stream.", 1)
	}
	if r == RegionCN {
		return StreamHostCN
	}
	return StreamHostGlobal
}

// ==================== Prompt Assembly and Multi-turn Message Parsing ====================

const (
	systemTag    = "[System Instruction]"
	assistantTag = "Assistant: "
	userTag      = " User: "
	textOnlyNote = "See the image"
)

// Turn represents a parsed single-turn chat message.
type Turn struct {
	Role   string
	Text   string
	Images []string
	// ToolCalls are the tool calls of the assistant message (OpenAI format).
	ToolCalls []map[string]any
	// ToolCallID / ToolName are the correlation info for role=tool result messages.
	ToolCallID string
	ToolName   string
}

// ParseTurn parses a Turn from an OpenAI format message.
func ParseTurn(item any) *Turn {
	if item == nil {
		return nil
	}
	if s, ok := item.(string); ok {
		return &Turn{Role: "user", Text: s}
	}
	m, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	role, _ := m["role"].(string)
	role = strings.TrimSpace(role)
	if role != "system" && role != "assistant" && role != "user" && role != "tool" {
		role = "user"
	}

	var textParts []string
	var images []string

	if content, exists := m["content"]; exists && content != nil {
		switch c := content.(type) {
		case string:
			textParts = append(textParts, c)
		case []any:
			for _, part := range c {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				pType, _ := pm["type"].(string)
				switch pType {
				case "text":
					if t, ok := pm["text"].(string); ok {
						textParts = append(textParts, t)
					}
				case "image_url", "input_image":
					if iu, ok := pm["image_url"]; ok {
						if uMap, ok := iu.(map[string]any); ok {
							if u, ok := uMap["url"].(string); ok && u != "" {
								images = append(images, u)
							}
						} else if uStr, ok := iu.(string); ok && uStr != "" {
							images = append(images, uStr)
						}
					}
				}
			}
		}
	}

	// Some clients put images in top-level fields
	if rawImgs, ok := m["images"].([]any); ok {
		for _, img := range rawImgs {
			if s, ok := img.(string); ok && s != "" {
				images = append(images, s)
			}
		}
	}

	turn := &Turn{
		Role:   role,
		Text:   strings.TrimSpace(strings.Join(textParts, "")),
		Images: images,
	}

	if role == "assistant" {
		if calls, ok := m["tool_calls"].([]any); ok {
			for _, item := range calls {
				if call, ok := item.(map[string]any); ok {
					turn.ToolCalls = append(turn.ToolCalls, call)
				}
			}
		}
	}
	if role == "tool" {
		turn.ToolCallID, _ = m["tool_call_id"].(string)
		turn.ToolName, _ = m["name"].(string)
		if turn.ToolName == "" {
			turn.ToolName = core.GetString(m, "tool_name")
		}
	}

	return turn
}

// BuildPrompt flattens multi-turn OpenAI chat messages into a single content string required by the MiniMax Agent.
func BuildPrompt(turns []*Turn) string {
	return BuildPromptWithTools(turns, nil, nil)
}

// BuildPromptWithTools renders historical tool interactions and tool definitions into the prompt alongside flattening the conversation.
//
// When tools is nil/empty, it is completely identical to BuildPrompt (without the tool section).
func BuildPromptWithTools(turns []*Turn, tools []any, toolChoice any) string {
	if len(turns) == 0 {
		return ""
	}

	var parts []string
	userTurnCount := 0

	for _, turn := range turns {
		if turn == nil {
			continue
		}
		text := strings.TrimSpace(turn.Text)
		images := turn.Images
		var block string

		switch turn.Role {
		case "system":
			if text != "" {
				block = systemTag + " " + text
			}
		case "assistant":
			var chunks []string
			if text != "" {
				chunks = append(chunks, assistantTag+text)
			}
			for _, call := range turn.ToolCalls {
				if rendered := renderToolCallBlock(call); rendered != "" {
					chunks = append(chunks, rendered)
				}
			}
			block = strings.Join(chunks, "\n\n")
		case "tool":
			// Results with correlation IDs are rendered as tool result blocks (preserving the mapping to calls);
			// Orphaned results without IDs fall back to historical behavior, treated as normal user turns.
			if turn.ToolCallID != "" {
				block = renderToolResultBlock(turn.ToolCallID, turn.ToolName, text)
			} else if text != "" {
				userTurnCount++
				block = userTagFor(userTurnCount, len(turns)) + text
			}
		default: // "user" or other unknown roles
			userTurnCount++
			prefix := userTagFor(userTurnCount, len(turns))
			body := text
			if body == "" && len(images) > 0 {
				body = textOnlyNote
			}
			if body != "" {
				block = prefix + body
			}
		}

		if len(images) > 0 {
			var imgLines []string
			if block != "" {
				imgLines = append(imgLines, block)
			}
			for n, url := range images {
				imgLines = append(imgLines, fmt.Sprintf("![Ignored Image %d](%s)", n, url))
			}
			block = strings.Join(imgLines, "\n")
		}

		if block != "" {
			parts = append(parts, block)
		}
	}

	prompt := strings.Join(parts, "\n\n")
	prompt += renderToolsSection(tools, toolChoice)
	return prompt
}

func userTagFor(number, total int) string {
	if total == 1 {
		return ""
	}
	return userTag + strconv.Itoa(number) + ". "
}

// EstimateTokens estimates the number of text tokens. CJK characters count as 1, and other characters are about 1 token per 4 bytes.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	wide := 0
	for _, r := range text {
		if r > 0x2E80 {
			wide++
		}
	}
	totalChars := len([]rune(text))
	tokens := wide + (totalChars-wide)/4
	if tokens < 1 {
		return 1
	}
	return tokens
}

// This file implements the "text protocol tool bridge": the upstream agent endpoint has only a single content string
// and no native tools parameter, so Claude Code's tool calls must be carried via text markers:
//
//   - Request side: tool definitions, historical tool_calls, and tool results are all rendered into the flat prompt;
//   - Response side: parse marker blocks from the model output and restore them into OpenAI tool_calls.
//
// Markers use 64-character random delimiters instead of common XML tags: the probability of a collision in the body text is zero,
// and the streaming hold-back window is therefore only a few dozen bytes. Instructions teach one format,
// while parsing additionally supports ```tool_call fences—models have a natural preference for fences, so rather than correcting it, we accept it.
// Marker blocks in history themselves serve as few-shot samples, and the model will output the same format in the next turn.

const (
	toolCallStart = "<minimax-tool-call>"
	toolCallEnd   = "</minimax-tool-call>"
	toolFence     = "```tool_call"
	toolFenceEnd  = "```"
)

// ToolCall is the restored OpenAI-format tool call.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// renderToolsSection renders tool definitions and call instructions into the instruction section at the end of the prompt.
func renderToolsSection(tools []any, toolChoice any) string {
	if len(tools) == 0 || isToolChoiceNone(toolChoice) {
		return ""
	}

	defs := make([]map[string]any, 0, len(tools))
	for _, item := range tools {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		if fn == nil {
			fn = m
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		entry := map[string]any{"name": name}
		if desc, ok := fn["description"].(string); ok && desc != "" {
			entry["description"] = desc
		}
		if schema, ok := fn["parameters"]; ok && schema != nil {
			entry["parameters"] = schema
		}
		defs = append(defs, entry)
	}
	if len(defs) == 0 {
		return ""
	}
	defsJSON, _ := json.Marshal(defs)

	var b strings.Builder
	b.WriteString("\n\n[Available Tools]\n")
	b.WriteString("Here are the tool definitions you can call (JSON):\n")
	b.WriteString(string(defsJSON))
	b.WriteString("\n\n[Tool Call Format]\n")
	b.WriteString("When a tool needs to be called, output the following marker block (the marker content must be a single-line JSON, and arguments must comply with the tool's parameters schema).")
	b.WriteString("Multiple marker blocks can be output consecutively for parallel calls; brief explanatory text can be output outside the marker blocks. Stop generation after outputting the marker blocks and wait for the tool results.\n")
	b.WriteString(toolCallStart + "\n")
	b.WriteString(`{"name":"tool_name","arguments":{...}}` + "\n")
	b.WriteString(toolCallEnd + "\n")

	switch choice := normalizeToolChoice(toolChoice).(type) {
	case string:
		if choice == "required" {
			b.WriteString("This round of response must output at least one tool call marker block.\n")
		}
	case map[string]any:
		if name, ok := choice["name"].(string); ok && name != "" {
			b.WriteString("This round of response must call the tool " + name + ".\n")
		}
	}
	return b.String()
}

// isToolChoiceNone determines whether tool_choice is "none".
func isToolChoiceNone(toolChoice any) bool {
	s, ok := toolChoice.(string)
	return ok && strings.EqualFold(strings.TrimSpace(s), "none")
}

// normalizeToolChoice normalizes OpenAI tool_choice to a string or {name}.
func normalizeToolChoice(toolChoice any) any {
	switch choice := toolChoice.(type) {
	case string:
		return choice
	case map[string]any:
		if t, _ := choice["type"].(string); t == "function" {
			if fn, ok := choice["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					return map[string]any{"name": name}
				}
			}
		}
		if name, ok := choice["name"].(string); ok {
			return map[string]any{"name": name}
		}
	}
	return nil
}

// renderToolCallBlock renders a historical tool call into a markup block (few-shot: history serves as the formatting sample).
func renderToolCallBlock(call map[string]any) string {
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		return ""
	}
	name, _ := fn["name"].(string)
	if strings.TrimSpace(name) == "" {
		return ""
	}
	args, _ := fn["arguments"].(string)
	args = strings.TrimSpace(args)
	if args == "" {
		args = "{}"
	}
	var line strings.Builder
	line.WriteString(`{"name":`)
	nameJSON, _ := json.Marshal(name)
	line.Write(nameJSON)
	line.WriteString(`,"arguments":`)
	if json.Valid([]byte(args)) {
		line.WriteString(args)
	} else {
		// Keep malformed parameters as a string and leave them for the parser to receive as string arguments.
		escaped, _ := json.Marshal(args)
		line.Write(escaped)
	}
	line.WriteString("}")
	if id, _ := call["id"].(string); strings.TrimSpace(id) != "" {
		// Put id into an independent key at the end of the block to keep the name/arguments body consistent with the protocol example.
		encoded := line.String()
		encoded = strings.TrimSuffix(encoded, "}") + `,"id":` + mustJSONString(id) + "}"
		return toolCallStart + "\n" + encoded + "\n" + toolCallEnd
	}
	return toolCallStart + "\n" + line.String() + "\n" + toolCallEnd
}

// renderToolResultBlock renders the role=tool result into a history block.
func renderToolResultBlock(id, name, content string) string {
	var b strings.Builder
	b.WriteString("[Tool Result")
	if id != "" {
		b.WriteString(" id=" + id)
	}
	if name != "" {
		b.WriteString(" name=" + name)
	}
	b.WriteString("]\n")
	b.WriteString(content)
	return b.String()
}

func mustJSONString(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

// parseToolCalls restores tool calls from the model output text and returns the body stripped of markers.
func parseToolCalls(text string) (string, []ToolCall) {
	if text == "" {
		return text, nil
	}
	if !strings.Contains(text, toolCallStart) && !strings.Contains(text, toolFence) {
		// Fast path: when there are no markers, only detect the single special case of a "naked JSON tool object".
		if trimmed := strings.TrimSpace(text); looksLikeToolCallObject(trimmed) {
			if call, ok := decodeToolCallJSON([]byte(trimmed)); ok {
				return "", []ToolCall{call}
			}
		}
		return text, nil
	}

	var calls []ToolCall
	var b strings.Builder
	rest := text
	for {
		startIdx, startLen := indexToolCallStart(rest)
		if startIdx < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:startIdx])
		after := rest[startIdx+startLen:]
		endIdx := indexToolCallEnd(after)
		var blockBody string
		if endIdx >= 0 {
			blockBody = after[:endIdx]
			// Strip both forms of closing markers (opening and closing may not be paired, e.g., fenced open with naked close).
			rest = after[endIdx:]
			if strings.HasPrefix(rest, toolCallEnd) {
				rest = rest[len(toolCallEnd):]
			} else if strings.HasPrefix(rest, toolFenceEnd) {
				rest = rest[len(toolFenceEnd):]
			}
		} else {
			// Unclosed: try our best to parse the remaining part as the block body.
			blockBody = after
			rest = ""
		}
		if call, ok := decodeToolCallBlock(blockBody); ok {
			calls = append(calls, call)
		} else {
			// Blocks that fail to parse are kept as-is in the body without swallowing content.
			if startLen == len(toolFence) {
				b.WriteString(toolFence)
			} else {
				b.WriteString(toolCallStart)
			}
			b.WriteString(blockBody)
			if endIdx >= 0 {
				b.WriteString(toolCallEnd)
			}
		}
	}
	return b.String(), calls
}

// indexToolCallStart returns the position of the nearest open marker (bare markers take precedence over fences).
func indexToolCallStart(s string) (int, int) {
	bare := strings.Index(s, toolCallStart)
	fence := strings.Index(s, toolFence)
	switch {
	case bare >= 0 && (fence < 0 || bare <= fence):
		return bare, len(toolCallStart)
	case fence >= 0:
		return fence, len(toolFence)
	default:
		return -1, 0
	}
}

// indexToolCallEnd returns the position of the close marker; both forms of close markers are recognized.
func indexToolCallEnd(s string) int {
	idxBare := strings.Index(s, toolCallEnd)
	idxFence := strings.Index(s, toolFenceEnd)
	switch {
	case idxBare >= 0 && (idxFence < 0 || idxBare <= idxFence):
		return idxBare
	case idxFence >= 0:
		return idxFence
	default:
		return -1
	}
}

// looksLikeToolCallObject determines whether the text is precisely a tool call JSON object.
func looksLikeToolCallObject(trimmed string) bool {
	return strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") &&
		strings.Contains(trimmed, `"name"`)
}

// decodeToolCallBlock decodes a tool call from the marker block body.
func decodeToolCallBlock(body string) (ToolCall, bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ToolCall{}, false
	}
	// The block body may be wrapped by fences or explanatory text: slice from the first { to the last }.
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return ToolCall{}, false
	}
	return decodeToolCallJSON([]byte(trimmed[start : end+1]))
}

// decodeToolCallJSON parses the {"name":...,"arguments":...} format.
func decodeToolCallJSON(raw []byte) (ToolCall, bool) {
	var obj map[string]any
	if errUnmarshal := json.Unmarshal(raw, &obj); errUnmarshal != nil {
		return ToolCall{}, false
	}
	name, _ := obj["name"].(string)
	if strings.TrimSpace(name) == "" {
		return ToolCall{}, false
	}
	call := ToolCall{Name: name}
	call.ID, _ = obj["id"].(string)
	switch args := obj["arguments"].(type) {
	case string:
		call.Arguments = args
	case map[string]any:
		encoded, errMarshal := json.Marshal(args)
		if errMarshal != nil {
			return ToolCall{}, false
		}
		call.Arguments = string(encoded)
	default:
		call.Arguments = "{}"
	}
	if strings.TrimSpace(call.Arguments) == "" {
		call.Arguments = "{}"
	}
	return call, true
}

// maxToolMarkerSpan is the maximum length of an open marker, representing the minimum trailing bytes to hold back during streaming.
var maxToolMarkerSpan = core.MaxInt(len(toolCallStart), len(toolFence))

// toolStreamParser incrementally parses tool markers in streaming text.
//
// How it works: holds back a few trailing bytes (insufficient to determine if a marker appears), emits the safely
// determinable portion as text; once an open marker is seen, it enters capture mode, parses the tool call
// upon reaching the close marker, and strips the block from the text. flush delivers the remaining text at the end of the stream.
type toolStreamParser struct {
	hold      strings.Builder
	capturing bool
	capture   strings.Builder
	calls     []ToolCall
	// rejected records blocks returned to the body after parsing failure (used as a fallback in flush).
	pendingEmit strings.Builder
}

// push feeds a piece of text and returns the text increment that can be safely sent to the client.
func (p *toolStreamParser) push(delta string) string {
	p.hold.WriteString(delta)
	var out strings.Builder
	for {
		hold := p.hold.String()
		if !p.capturing {
			idx, markerLen := indexToolCallStart(hold)
			if idx >= 0 {
				pending := hold[:idx]
				out.WriteString(pending)
				p.hold.Reset()
				p.hold.WriteString(hold[idx+markerLen:])
				p.capturing = true
				p.capture.Reset()
				continue
			}
			// No marker: send the safe prefix, retaining the tail that may span chunks.
			safe := len(hold) - (maxToolMarkerSpan - 1)
			// The cut point must fall on a rune boundary, otherwise multi-byte characters will be truncated into invalid UTF-8.
			for safe > 0 && safe < len(hold) && !utf8.RuneStart(hold[safe]) {
				safe--
			}
			if safe > 0 {
				out.WriteString(hold[:safe])
				p.hold.Reset()
				p.hold.WriteString(hold[safe:])
			}
			return out.String()
		}
		// Capture mode: look for the closing marker. The marker may span across the boundary of capture and hold
		// (the first half of the opening marker falls into capture, and the second half of the closing marker arrives later), so search on the concatenated body.
		combined := p.capture.String() + hold
		endIdx := indexToolCallEnd(combined)
		if endIdx < 0 {
			// Defense: abandon parsing when the capture body expands abnormally, and fall back the entire thing to the body.
			if len(combined) > 1<<20 {
				p.capturing = false
				p.pendingEmit.WriteString(toolCallStart)
				p.pendingEmit.WriteString(combined)
				p.hold.Reset()
				p.capture.Reset()
				continue
			}
			p.capture.Reset()
			p.capture.WriteString(combined)
			p.hold.Reset()
			return out.String()
		}
		blockBody := combined[:endIdx]
		rest := combined[endIdx:]
		// Strip the closing marker according to the opening marker format.
		// Since the opening marker was already consumed upon entering capture, either closing marker is possible here.
		if strings.HasPrefix(rest, toolCallEnd) {
			rest = rest[len(toolCallEnd):]
		} else if strings.HasPrefix(rest, toolFenceEnd) {
			rest = rest[len(toolFenceEnd):]
		}
		p.capturing = false
		p.capture.Reset()
		p.hold.Reset()
		p.hold.WriteString(rest)
		if call, ok := decodeToolCallBlock(blockBody); ok {
			p.calls = append(p.calls, call)
		} else {
			// Parsing failed: return the block to the body (sent to the client as plain text).
			p.pendingEmit.WriteString(toolCallStart)
			p.pendingEmit.WriteString(blockBody)
			p.pendingEmit.WriteString(toolCallEnd)
		}
	}
}

// flush delivers the remaining text and restored tool calls at the end of the stream.
func (p *toolStreamParser) flush() (string, []ToolCall) {
	var out strings.Builder
	out.WriteString(p.pendingEmit.String())
	p.pendingEmit.Reset()
	rest := p.hold.String()
	if p.capturing {
		// Unclosed block: parse it as a call if possible, otherwise return it to the body.
		blockBody := p.capture.String() + rest
		if call, ok := decodeToolCallBlock(blockBody); ok {
			p.calls = append(p.calls, call)
		} else {
			out.WriteString(toolCallStart)
			out.WriteString(blockBody)
		}
	} else if rest != "" {
		// Wrap with another layer if the end of the body happens to be a bare tool object.
		if trimmed := strings.TrimSpace(rest); looksLikeToolCallObject(trimmed) && len(p.calls) == 0 && strings.TrimSpace(out.String()) == "" {
			if call, ok := decodeToolCallJSON([]byte(trimmed)); ok {
				p.calls = append(p.calls, call)
			} else {
				out.WriteString(rest)
			}
		} else {
			out.WriteString(rest)
		}
	}
	p.hold.Reset()
	p.capture.Reset()
	p.capturing = false
	return out.String(), p.calls
}
