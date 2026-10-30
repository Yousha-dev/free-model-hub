package raccoon

// This file defines the upstream endpoints and request headers for Raccoon, and implements chat-completions forwarding.
//
// Protocol basis: aimod-cc/Agent2API (v1.1.0 desktop client packet capture and actual tests), Issue #24 specified as implementation reference.
//
// Upstream is OpenAI-shaped: request body forwarded as-is (model is guaranteed by the adapter to be the directory-standard
// id), returning OpenAI JSON / SSE, which the plugin proxies as-is.

import (
	"bytes"
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

// Upstream base address (can be overridden by configuration).
const (
	DefaultLLMBase    = "https://xiaohuanxiong.com/api/web/llm/v2"
	DefaultAuthBase   = "https://xiaohuanxiong.com/api/web/auth/v1"
	DefaultPointsBase = "https://xiaohuanxiong.com/api/web/points/v1"
	DefaultOrigin     = "https://xiaohuanxiong.com"
)

// Business paths (mounted under respective domains).
const (
	ChatPath      = "/chat/completions"
	CatalogPath   = "/model_catalog"
	RefreshPath   = "/refresh"
	LoginCodePath = "/login_with_authorization_code"
	BalancePath   = "/balance"
	UserInfoPath  = "/user_info"
	AuthorizePath = "/code/authorize"
	// DefaultConfigName is the upstream default chat model (v1.1.0 packet capture: directory default_model).
	DefaultConfigName = "raccoon-8c4485"
	// DefaultClientVersion is the built-in value for the outbound x-client-version header (verified via packet capture).
	DefaultClientVersion = "v1.1.0"
)

// chatHTTPTimeout is the upper limit for non-streaming chat.
const chatHTTPTimeout = 10 * time.Minute

// BaseURL normalizes the base address: override takes priority (no trailing slash), otherwise uses the default.
func BaseURL(override, fallback string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed != "" {
		return trimmed
	}
	return fallback
}

// ApplyClientHeaders writes the Raccoon desktop client identification headers (verified via packet capture: upstream performs
// risk control based on client channels, so include headers closest to real traffic).
func ApplyClientHeaders(req *http.Request) {
	req.Header.Set("x-client-name", "@raccoon/office-desktop")
	req.Header.Set("x-client-platform", "desktop-windows-x64")
	req.Header.Set("x-client-version", DefaultClientVersion)
	req.Header.Set("x-client-channel", "official")
}

// ApplyChatHeaders writes all headers required for chat requests.
func ApplyChatHeaders(req *http.Request, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept-Encoding", "identity")
	ApplyClientHeaders(req)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
}

// ApplyAPIHeaders writes headers for auth / credit domain JSON requests (Accept + client identification + Bearer).
func ApplyAPIHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	ApplyClientHeaders(req)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
}

// ChatRequest is the input for a chat forwarding operation.
type ChatRequest struct {
	// Token is the outbound credential (JWT access token).
	Token string
	// Body is the host-translated chat-completions request body.
	Body []byte
}

// Chat executes a non-streaming chat and returns the complete upstream response body.
func Chat(ctx context.Context, llmBase string, req ChatRequest) ([]byte, error) {
	resp, errDo := doChat(ctx, llmBase, req)
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
	// Business envelope ({"code","message"}): code != 0 is handled as a business error.
	if code, message, ok := envelopeError(body); ok {
		return nil, ClassifyBusiness(resp.StatusCode, code, message)
	}
	return body, nil
}

// ChatStream executes a streaming chat and returns the upstream response body (caller is responsible for closing).
func ChatStream(ctx context.Context, llmBase string, req ChatRequest) (*http.Response, error) {
	endpoint := llmBase + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat stream request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token)

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, string(payload))
	}
	return resp, nil
}

// doChat initiates a non-streaming chat request.
func doChat(ctx context.Context, llmBase string, req ChatRequest) (*http.Response, error) {
	endpoint := llmBase + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token)

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// envelopeError checks the business envelope {"code","message"}: returns (code, message, true) when code exists and is non-zero.
func envelopeError(body []byte) (int64, string, bool) {
	var probe struct {
		Code    *int64 `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, "", false
	}
	if probe.Code == nil || *probe.Code == 0 {
		return 0, "", false
	}
	message := probe.Message
	if message == "" {
		message = probe.Msg
	}
	return *probe.Code, message, true
}
