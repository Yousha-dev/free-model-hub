package cline

import (
	"bytes"
	"context"
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

// This file implements Cline's chat-completions forwarding.
//
// Cline's upstream is standard OpenAI chat-completions, request body **does not need to be rewritten**—
// The only things that need to be changed are the request headers (auth + client identification), and adding the upstream-required
// session_id and reasoning_effort fields.
//
// These two fields are hard requirements from upstream (empirically missing ones cause rejections), so they are injected before forwarding;
// Do not overwrite if the client has already provided values.

// chatHTTPTimeout is the upper limit for non-streaming conversations.
const chatHTTPTimeout = 10 * time.Minute

// ChatRequest is the input for a conversation forwarding.
type ChatRequest struct {
	// BearerToken is the token that can be placed directly into Authorization (including the workos: prefix).
	BearerToken string
	// Body is the chat-completions request body translated by the host.
	Body []byte
	// Stream reports whether it is streaming.
	Stream bool
}

// Chat executes a non-streaming conversation and returns the complete upstream response body.
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	body, errPrepare := prepareBody(req.Body)
	if errPrepare != nil {
		return nil, errPrepare
	}
	resp, errDo := doChat(ctx, baseURL, req, body, chatHTTPTimeout)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	payload, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(payload))
	}
	return unwrapData(payload), nil
}

// unwrapData returns the chat completion inside Cline's {"data": {...}}
// envelope; other bodies are returned unchanged.
func unwrapData(payload []byte) []byte {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(payload, &envelope); errUnmarshal != nil || len(envelope.Data) == 0 {
		return payload
	}
	var inner struct {
		Choices json.RawMessage `json:"choices"`
	}
	if errInner := json.Unmarshal(envelope.Data, &inner); errInner != nil || len(inner.Choices) == 0 {
		return payload
	}
	return envelope.Data
}

// ChatStream executes a streaming conversation and returns the upstream response body (the caller is responsible for closing it).
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	body, errPrepare := prepareBody(req.Body)
	if errPrepare != nil {
		return nil, errPrepare
	}
	// Timeout is 0: the total duration of a streaming response is unpredictable, and a one-off timeout would truncate long answers.
	resp, errDo := doChat(ctx, baseURL, req, body, 0)
	if errDo != nil {
		return nil, errDo
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, string(payload))
	}
	return resp, nil
}

// doChat initiates a conversation request.
func doChat(ctx context.Context, baseURL string, req ChatRequest, body []byte, timeout time.Duration) (*http.Response, error) {
	endpoint := BaseURL(baseURL) + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.BearerToken, NewSessionID())

	var client *http.Client
	if timeout > 0 {
		client = hostbridge.Client(ctx, timeout)
	} else {
		client = hostbridge.StreamClient(ctx, 0)
	}
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// prepareBody injects the fields required by upstream.
//
// Inject two fields:
//   - session_id: upstream uses it for session affinity (missing it will be treated as a new session, affecting cache hit rates);
//   - reasoning_effort: upstream has requirements for the default reasoning tier, and missing it causes some models to reject the request.
//
// **Do not overwrite client-provided values**: respect them if explicitly specified by the user, the plugin only supplements defaults.
func prepareBody(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		// If it cannot be parsed, forward as-is: upstream will provide a more accurate error than we can.
		return raw, nil
	}
	changed := false
	if _, okSession := payload["session_id"]; !okSession {
		payload["session_id"] = NewSessionID()
		changed = true
	}
	if _, okEffort := payload["reasoning_effort"]; !okEffort {
		payload["reasoning_effort"] = "high"
		changed = true
	}
	if !changed {
		return raw, nil
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return raw, nil
	}
	return encoded, nil
}

// This file defines Cline's upstream endpoints and request headers.
//
// Endpoints and request header formats are ported from measured values in cline2api (auth.go and admin.go):
// Cline's upstream is standard OpenAI chat-completions, but with a full set of client identification headers;
// missing them will cause it to be recognized as an unofficial client.

const (
	// DefaultBaseURL is Cline's upstream base address.
	DefaultBaseURL = "https://api.cline.bot/api/v1"
	// ChatPath is the path for chat-completions.
	ChatPath = "/chat/completions"
	// ModelsPath is the path for the recommended model list (**no auth required**).
	ModelsPath = "/ai/cline/recommended-models"
	// RefreshPath is the path for token refresh.
	RefreshPath = "/auth/refresh"
	// RegisterPath is the path for exchanging a WorkOS token for a Cline token.
	RegisterPath = "/auth/register"

	// DefaultClientVersion is the disguised Cline client version.
	DefaultClientVersion = "3.0.47"
	// DefaultCoreVersion is the disguised Cline core version.
	DefaultCoreVersion = "0.0.66"
)

// BaseURL returns the upstream base address (no trailing slash).
//
// Allows configuration overrides: no need to change code when upstream changes domains or when users want to route through a self-built gateway.
func BaseURL(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return DefaultBaseURL
	}
	return trimmed
}

// UserAgent returns the UA disguised as the official Cline client.
func UserAgent() string {
	return "Cline/" + DefaultClientVersion
}

// ApplyChatHeaders writes all headers required for chat-completions requests.
//
// This complete set of X-CLIENT-* / X-PLATFORM headers, other than authentication, serves as a
// spoofing layer to "look more like an official client". Empirically, upstream only strictly requires Authorization and Content-Type, but lacking these headers
// makes it more susceptible to risk control flags, so they are completed to match the official client's format.
func ApplyChatHeaders(req *http.Request, bearerToken, sessionID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("HTTP-Referer", "https://cline.bot")
	req.Header.Set("X-Title", "Cline")
	req.Header.Set("X-Task-ID", sessionID)
	req.Header.Set("X-IS-MULTIROOT", "false")
	req.Header.Set("X-CLIENT-TYPE", "cline-cli")
	req.Header.Set("X-CLIENT-VERSION", DefaultClientVersion)
	req.Header.Set("X-PLATFORM", "terminal")
	req.Header.Set("X-PLATFORM-VERSION", DefaultClientVersion)
	req.Header.Set("X-CORE-VERSION", DefaultCoreVersion)
}

// ApplyJSONHeaders writes headers for regular JSON requests (refresh, registration).
//
// Login and refresh chains **do not have** client spoofing headers: tests show upstream /auth/* only recognizes Content-Type,
// and extra spoofing headers actually make behavior inconsistent with the official client.
func ApplyJSONHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
}

// ApplyFormHeaders writes headers for form requests (WorkOS device code flow).
func ApplyFormHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
}

// NewSessionID generates a session ID (upstream requires sess_<millisecond timestamp> format).
func NewSessionID() string {
	return "sess_" + strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// This file parses Cline's provider-specific configuration.
