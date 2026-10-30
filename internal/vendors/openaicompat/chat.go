package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// ---- execution ----

// Execute runs one non-streaming chat completion.
func (a *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := a.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	httpReq, errReq := a.chatRequest(ctx, native, req, false)
	if errReq != nil {
		return nil, errReq
	}
	resp, errDo := hostbridge.Client(ctx, chatTimeout).Do(httpReq)
	if errDo != nil {
		return nil, core.ToPluginError(&core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient})
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if errRead != nil {
		return nil, core.ToPluginError(&core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, core.ToPluginError(classify(resp.StatusCode, body))
	}
	return &pluginapi.ExecutorResponse{
		Payload: body,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream runs one streaming chat completion and forwards data frames.
func (a *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := a.Native(cred)
	if errNative != nil {
		return errNative
	}
	httpReq, errReq := a.chatRequest(ctx, native, req, true)
	if errReq != nil {
		return errReq
	}
	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return core.ToPluginError(&core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient})
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return core.ToPluginError(classify(resp.StatusCode, body))
	}
	// Upstream keep-alive comments (": KILO PROCESSING", ": OPENROUTER
	// PROCESSING") and event lines are not JSON chunks; drop them.
	return hostbridge.ForwardRawSSE(resp.Body, func(payload []byte) error {
		trimmed := bytes.TrimSpace(payload)
		if len(trimmed) == 0 || trimmed[0] == ':' || bytes.HasPrefix(trimmed, []byte("event:")) {
			return nil
		}
		return sink.Emit(payload)
	})
}

func (a *adapter) chatRequest(ctx context.Context, native *Credential, req *core.ExecuteRequest, stream bool) (*http.Request, error) {
	body, errBody := rewriteChatBody(req.Payload, req.Model, a.UpstreamModelID, stream)
	if errBody != nil {
		return nil, core.NewPluginError("invalid_request", errBody.Error(), http.StatusBadRequest)
	}
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.provider.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if native.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+native.APIKey)
	}
	a.applyAppHeaders(httpReq)
	return httpReq, nil
}

// rewriteChatBody points the request at the upstream model ID and pins the
// stream flag to the execution mode the host chose.
func rewriteChatBody(payload []byte, model string, upstream func(string) string, stream bool) ([]byte, error) {
	var body map[string]any
	if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil {
		return nil, fmt.Errorf("chat payload is not a JSON object: %w", errUnmarshal)
	}
	if body == nil {
		return nil, fmt.Errorf("chat payload is empty")
	}
	if strings.TrimSpace(model) == "" {
		model, _ = body["model"].(string)
	}
	body["model"] = upstream(model)
	body["stream"] = stream
	if !stream {
		delete(body, "stream_options")
	}
	return json.Marshal(body)
}
