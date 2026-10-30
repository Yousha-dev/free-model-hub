package autoclaw

// This file implements AutoClaw's protocol endpoints, request signing/headers, and chat-completions forwarding.
//
// Protocol basis: Agent2API (docs/reverse-notes.md, 2026-09 static reverse-engineering conclusions for Web frontend bundle and
// desktop 1.18.5), Issue #24 designated as the implementation reference for this provider.
//
// Channel: /autoclaw-proxy/proxy/autoclaw/v1/chat/completions under the region base URL
// (Agent2API's chat_proxy channel, model declared via X-Request-Model header).
// Upstream returns OpenAI-compatible JSON / SSE, which the plugin passes through as-is.
//
// Note: Agent2API also has a "sandbox relay" channel (wss device online + agent/send +
// /api/events), relying on per-account cloud sandbox and WebSocket persistent connections.
// This plugin chooses the proxy channel under the form of "credentials residing in host process, outbound per request";
// the persistent session state required by the relay channel is inconsistent with the plugin model and is not implemented for now (sandbox field retained in credentials for future expansion).

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// Regional default base addresses (userapi REST base, cn/global dual deployment).
const (
	DefaultBaseURLCN     = "https://autoglm-acceleration-api.zhipuai.cn"
	DefaultBaseURLGlobal = "https://autoglm-api.autoglm.ai"
)

// Business path (mounted under region base URL).
const (
	ChatPath         = "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	ModelsConfigPath = "/autoclaw-proxy/proxy/autoclaw-model-config"
	RefreshPath      = "/userapi/v1/refresh"
	WalletsPath      = "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
)

// Signing and client constants (reversed from Web frontend bundle).
const (
	authAppID     = "100003"
	authSecret    = "38d2391985e2369a5fb8227d8e6cd5e5"
	clientVer     = "1.18.5"
	errorCodeDead = 410000 // Not logged in / Session expired
)

// chatHTTPTimeout is the upper limit for non-streaming conversations.
const chatHTTPTimeout = 10 * time.Minute

// DefaultBaseURLFor returns the default base address for the region.
func DefaultBaseURLFor(region Region) string {
	if region.IsGlobal() {
		return DefaultBaseURLGlobal
	}
	return DefaultBaseURLCN
}

// BaseURL returns the upstream base address (no trailing slash): override takes precedence, otherwise regional default.
func BaseURL(override string, region Region) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed != "" {
		return trimmed
	}
	return DefaultBaseURLFor(region)
}

// AuthSign computes the request signature: MD5("<appid>&<ts>&<secret>").
func AuthSign(ts string) string {
	sum := md5.Sum([]byte(authAppID + "&" + ts + "&" + authSecret))
	return hex.EncodeToString(sum[:])
}

// bearer normalizes token: idempotent if Bearer prefix is already present.
func bearer(token string) string {
	trimmed := strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(trimmed), "bearer ") {
		return trimmed
	}
	return "Bearer " + trimmed
}

// ApplyUserAPIHeaders writes all headers required for userapi REST requests (business envelope interface).
func ApplyUserAPIHeaders(req *http.Request, token string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Version", clientVer)
	req.Header.Set("X-Tm", "web")
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Client-Type", "web")
	req.Header.Set("X-Channel", "official")
	req.Header.Set("X-Auth-Appid", authAppID)
	req.Header.Set("X-Auth-TimeStamp", ts)
	req.Header.Set("X-Auth-Sign", AuthSign(ts))
	req.Header.Set("X-Trace-Id", core.RandomHex(16))
	req.Header.Set("X-Lang", "en")
	// Cannot be left empty: Go's default "Go-http-client/1.1" is blocked by Alibaba Cloud WAF with 405 HTML
	// (empirically, changing only the UA recovers the request; that's where OAuth's 405 comes from).
	req.Header.Set("User-Agent", "AutoClaw/"+clientVer)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", bearer(token))
	}
}

// ApplyOAuthHeaders writes the request headers for Global OAuth two-hop (oauth-url / oauth-login).
//
// Difference from ApplyUserAPIHeaders (do not merge): OAuth goes through the official **desktop** flow,
// and the client fingerprint is pc / zcode / linux / zh-CN (Agent2API signed_auth_headers same caliber).
// User-Agent especially cannot be left empty: the Alibaba Cloud verification code token generated in the browser is forwarded as-is by the server,
// while Go's default UA is judged as a robot by Alibaba Cloud WAF and returns 405 HTML (the phenomenon is "Google passes,
// Zai always fails" — WAF rules hit by path, not endpoint shape issue).
func ApplyOAuthHeaders(req *http.Request, token string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "AutoClaw/"+clientVer)
	req.Header.Set("X-Version", clientVer)
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Client-Type", "pc")
	req.Header.Set("X-Harness-Type", "zcode")
	// Consistent with reference implementation: non-Windows builds send linux. Neither WAF nor the business layer distinguishes
	// the desktop system where the browser resides based on this, changing to mac yields no benefits (agent2api also sends linux on macOS).
	req.Header.Set("X-Tm", "linux")
	req.Header.Set("X-Lang", "zh-CN")
	req.Header.Set("X-Channel", "official")
	req.Header.Set("X-Auth-Appid", authAppID)
	req.Header.Set("X-Auth-TimeStamp", ts)
	req.Header.Set("X-Auth-Sign", AuthSign(ts))
	req.Header.Set("X-Trace-Id", core.RandomHex(16))
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", bearer(token))
	}
}

// ApplyChatHeaders writes all headers required for proxy chat requests.
//
// Difference from userapi headers: follows desktop form (X-Tm: mac / X-Client-Type: pc),
// model is declared via dual channels X-Request-Model and X-Authorization.
func ApplyChatHeaders(req *http.Request, token, model string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "AutoClaw/"+clientVer)
	req.Header.Set("X-Tm", "mac")
	req.Header.Set("X-Version", clientVer)
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Channel", "official")
	req.Header.Set("X-Lang", "zh-CN")
	req.Header.Set("X-Client-Type", "pc")
	req.Header.Set("X-Trace-Id", core.RandomHex(16))
	req.Header.Set("x_trace_id", "free-model-hub")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", bearer(token))
		req.Header.Set("X-Authorization", bearer(token))
	}
	if strings.TrimSpace(model) != "" {
		req.Header.Set("X-Request-Model", model)
	}
}

// ApplyModelsHeaders writes headers for model configuration requests (userapi envelope interface).
func ApplyModelsHeaders(req *http.Request, token string) {
	ApplyUserAPIHeaders(req, token)
	req.Header.Set("Accept", "application/json")
}

// DeadCode reports whether the business code is "session expired" (410000).
func DeadCode(code int64) bool { return code == errorCodeDead }

// ChatRequest is the input for a conversation forwarding.
type ChatRequest struct {
	// Token is the outbound credential (access token).
	Token string
	// Body is the chat-completions request body translated by the host.
	Body []byte
	// Model is the bare model name with the prefix stripped.
	Model string
}

// Chat executes a non-streaming conversation, returning the complete upstream response body.
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
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
		return nil, Classify(resp.StatusCode, 0, string(body))
	}
	// Business envelope form ({"code","msg","data"}): treat non-zero business codes as errors.
	if code, msg, ok := envelopeError(body); ok {
		return nil, Classify(resp.StatusCode, code, msg)
	}
	return body, nil
}

// ChatStream executes a streaming chat, returning the upstream response body (caller is responsible for closing).
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := baseURL + ChatPath
	body := prepareBody(req.Body, req.Model)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat stream request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token, req.Model)

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, 0, string(payload))
	}
	return resp, nil
}

// doChat initiates a non-streaming chat request.
func doChat(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := baseURL + ChatPath
	body := prepareBody(req.Body, req.Model)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token, req.Model)

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// prepareBody is a hook before the request body is sent: rewrites model to the upstream routing name, removes the user field
// (consistent with Agent2API's _prepare_proxy_body; the user field will be rejected by upstream), and adds two
// upstream pre-validation adaptations (system whitelist prefix and max_tokens lower bound, see normalizeSystemPrompt /
// normalizeMaxTokens).
func prepareBody(body []byte, model string) []byte {
	if len(body) == 0 {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if strings.TrimSpace(model) != "" {
		payload["model"] = model
	}
	delete(payload, "user")
	normalizeSystemPrompt(payload)
	normalizeMaxTokens(payload)
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return encoded
}

// ---- Upstream pre-validation adaptation (Issue #41) ----
//
// Upstream added two pre-validation gates to proxy chat in an AND relationship, triggering either results in a 406 empty response body
// (measured criteria see Agent2API's providers/autoclaw/prompt.rs and adapter.rs):
//
//  1. System whitelist (since 2022-09-22): the first message must be system, and the body must start with identityLine
//     and contain a "## Tooling" section, and no system/developer body may contain foreign harness
//     identity sentences (Claude Code / Codex / ZCode / DeepSeek Harness).
//  2. max_tokens lower bound (since 2026-09-29): small positive integers (30~8192) are strictly rejected; missing or
//     ≥ maxTokensFloor are allowed.
//
// Ordinary OpenAI clients (where the system is a neutral sentence or missing, and max_tokens is typically 1024/4096) hit both
// rules, so they must be normalized before outbound rather than making users modify their clients.

// identityLine is the identity sentence required by the upstream gate to "start with it" (idempotent determination recognizes only this one).
const identityLine = "You are a personal assistant running inside OpenClaw."

// identityPrefix is the outbound identity prefix: identity sentence + "## Tooling" section, taken verbatim from the official client's
// system prompt's first two lines (trailing newline matches Agent2API, body starts a new paragraph).
const identityPrefix = identityLine + "\n\n" +
	"## Tooling\n" +
	"Available tools are policy-filtered. Names are case-sensitive; call exactly as listed.\n"

// maxTokensFloor is the empirically tested safe lower bound: taking the latter between 8192 (rejected) and 10000 (passed).
const maxTokensFloor = 10000

// foreignIdentities are foreign identity sentences intercepted literally by upstream → neutral phrasing (longer ones first: shorter strings
// hitting first would fragment the long ones). The replacement string deliberately avoids product names—the gate blocks the product names themselves.
var foreignIdentities = [][2]string{
	{"You are ZCode, an interactive coding agent", "You are an interactive coding agent"},
	{"You are a coding agent running in the Codex CLI tool", "You are a coding agent running in a terminal CLI tool"},
	{"You are a coding agent running in the Codex CLI", "You are a coding agent running in a terminal CLI"},
	{"You are running as a coding agent in the Codex CLI", "You are running as a coding agent in a terminal CLI"},
	{"You are Claude Code", "You are a coding assistant"},
	{"You are ZCode", "You are an interactive coding agent"},
	{"You are an AI agent powered by DeepSeek Harness", "You are an AI agent powered by a local coding harness"},
	{"You are Codex", "You are a coding agent"},
}

// normalizeSystemPrompt makes the outbound system prompt comply with the upstream whitelist: first rewrites all
// foreign identity sentences in system/developer bodies, then ensures the first message is system and the body starts with the identity sentence
// (if it already starts with the identity sentence, it only rewrites without prepending; repeated outbound calls won't cause duplicate concatenation). If the first is not
// system, a system message with only the prefix is inserted at the head—without reordering, deleting other messages, or altering
// user/assistant/tool content (upstream gates only inspect system/developer).
func normalizeSystemPrompt(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if okMessage && isSystemRole(message) {
			rewriteIdentities(message)
		}
	}
	if len(messages) > 0 {
		if first, okFirst := messages[0].(map[string]any); okFirst && isSystemRole(first) {
			prependIdentity(first)
			return
		}
	}
	payload["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": identityPrefix,
	}}, messages...)
}

// isSystemRole reports whether the message role is system/developer.
func isSystemRole(message map[string]any) bool {
	role, okRole := message["role"].(string)
	if !okRole {
		return false
	}
	trimmed := strings.TrimSpace(role)
	return strings.EqualFold(trimmed, "system") || strings.EqualFold(trimmed, "developer")
}

// rewriteIdentities replaces foreign identity sentences in a system message body with neutral phrasing.
//
// The body has two payload forms: string, or text part array; other forms have no text to modify and are left to
// prependIdentity for unified handling.
func rewriteIdentities(message map[string]any) {
	switch content := message["content"].(type) {
	case string:
		message["content"] = replaceForeignIdentities(content)
	case []any:
		for _, item := range content {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			if text, okText := part["text"].(string); okText {
				part["text"] = replaceForeignIdentities(text)
			}
		}
	}
}

// prependIdentity prepends the identity prefix to the body of a system message.
//
// Three forms of content: string is directly concatenated; array is concatenated into the first text part (if none exists,
// one is inserted at the head); others (missing / null / object) are replaced by the prefix itself—such content as a system
// prompt is fundamentally invalid and sending it as-is will inevitably result in 406.
func prependIdentity(message map[string]any) {
	switch content := message["content"].(type) {
	case string:
		if !strings.HasPrefix(content, identityLine) {
			message["content"] = joinIdentity(content)
		}
	case []any:
		for _, item := range content {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			text, okText := part["text"].(string)
			if !okText {
				continue
			}
			if !strings.HasPrefix(text, identityLine) {
				part["text"] = joinIdentity(text)
			}
			return
		}
		message["content"] = append([]any{map[string]any{"type": "text", "text": identityPrefix}}, content...)
	default:
		message["content"] = identityPrefix
	}
}

// joinIdentity returns prefix + an empty line + original text (no empty line added if original text is empty).
func joinIdentity(original string) string {
	if original == "" {
		return identityPrefix
	}
	return identityPrefix + "\n" + original
}

// replaceForeignIdentities applies foreignIdentities one by one (full replacement, ignoring ASCII case).
func replaceForeignIdentities(text string) string {
	for _, rule := range foreignIdentities {
		text = replaceAllFold(text, rule[0], rule[1])
	}
	return text
}

// replaceAllFold fully replaces from with to in text (ignoring ASCII case).
func replaceAllFold(text, from, to string) string {
	index := indexFold(text, from)
	if index < 0 {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for index >= 0 {
		out.WriteString(text[:index])
		out.WriteString(to)
		text = text[index+len(from):]
		index = indexFold(text, from)
	}
	out.WriteString(text)
	return out.String()
}

// indexFold returns the first byte index of from in text (ignoring ASCII case), returning -1 if not found.
//
// Byte-level sliding window with strings.EqualFold: since from is all ASCII, non-ASCII bytes >= 0x80 cannot fold
// to ASCII, the matching range can only consist of ASCII bytes, and the slice is guaranteed to be at a character boundary.
func indexFold(text, from string) int {
	if from == "" {
		return -1
	}
	for start := 0; start+len(from) <= len(text); start++ {
		if strings.EqualFold(text[start:start+len(from)], from) {
			return start
		}
	}
	return -1
}

// normalizeMaxTokens raises small positive integer max_tokens in the outbound body that would be rejected by the upstream to the lower limit.
//
// Only normalize forms tested to be rejected (presence + positive integer + < maxTokensFloor): not injected by default (upstream
// defaults to 200, injecting out of nowhere changes behavior), while other forms (<=0 / non-integer / >= lower limit / non-numeric)
// are passed as-is to the upstream to return an error. max_completion_tokens does not participate in this validation and is left untouched.
func normalizeMaxTokens(payload map[string]any) {
	value, okValue := payload["max_tokens"].(float64)
	if !okValue || value <= 0 || value >= maxTokensFloor || value != float64(int64(value)) {
		return
	}
	payload["max_tokens"] = float64(maxTokensFloor)
}

// envelopeError checks the business envelope {"code","msg"}: returns (code, msg, true) when code exists and is non-zero.
func envelopeError(body []byte) (int64, string, bool) {
	var probe struct {
		Code    *int64 `json:"code"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, "", false
	}
	if probe.Code == nil || *probe.Code == 0 {
		return 0, "", false
	}
	msg := probe.Msg
	if msg == "" {
		msg = probe.Message
	}
	return *probe.Code, msg, true
}
