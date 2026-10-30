package opencodego

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/ocgo/adapter/chatcompletions"
	"free-model-hub/internal/ocgo/adapter/messages"
	"free-model-hub/internal/ocgo/adapter/responses"
	"free-model-hub/internal/ocgo/catalog"
	"free-model-hub/internal/ocgo/errclass"
)

// sourceFormat is the translators' name for the chat-completions protocol
// the host hands this plugin.
const sourceFormat = "openai"

// Execute runs one non-streaming request on the model's upstream route.
func (a *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	httpReq, rec, errReq := a.upstreamRequest(ctx, cred, req, false)
	if errReq != nil {
		return nil, errReq
	}
	resp, errDo := hostbridge.Client(ctx, 10*time.Minute).Do(httpReq)
	if errDo != nil {
		return nil, toFailure(errclass.FromNetwork(errDo))
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if errRead != nil {
		return nil, toFailure(errclass.FromNetwork(errRead))
	}
	converted, eErr := convertNonStream(rec.Protocol, resp.StatusCode, body)
	if eErr != nil {
		return nil, toFailure(eErr)
	}
	return &pluginapi.ExecutorResponse{
		Payload: converted,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream runs one streaming request and emits chat-completions chunks.
func (a *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	httpReq, rec, errReq := a.upstreamRequest(ctx, cred, req, true)
	if errReq != nil {
		return errReq
	}
	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return toFailure(errclass.FromNetwork(errDo))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return toFailure(errclass.FromStatus(resp.StatusCode, core.Truncate(string(body), 300)))
	}
	conv := newStreamConverter(rec.Protocol)
	buf := make([]byte, 32<<10)
	for {
		n, errRead := resp.Body.Read(buf)
		if n > 0 {
			events, done, eErr := conv.Feed(buf[:n])
			if eErr != nil {
				return toFailure(eErr)
			}
			if errEmit := emitEvents(sink, events); errEmit != nil {
				return errEmit
			}
			if done {
				return nil
			}
		}
		if errRead == io.EOF {
			if flusher, ok := conv.(interface{ Flush() [][]byte }); ok {
				return emitEvents(sink, flusher.Flush())
			}
			return nil
		}
		if errRead != nil {
			return toFailure(errclass.PostFirstByteNetwork(errRead.Error()))
		}
	}
}

func (a *adapter) upstreamRequest(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, stream bool) (*http.Request, catalog.ModelRecord, error) {
	native, errNative := a.Native(cred)
	if errNative != nil {
		return nil, catalog.ModelRecord{}, errNative
	}
	model := req.Model
	if strings.TrimSpace(model) == "" {
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(req.Payload, &probe)
		model = probe.Model
	}
	rec, cfg, ok := a.lookup(model)
	if !ok {
		if errRefresh := a.refresh(ctx, native.APIKey); errRefresh == nil {
			rec, cfg, ok = a.lookup(model)
		}
	}
	if !ok {
		return nil, rec, core.NewPluginError("model_not_found", "unknown OpenCode Go model "+model, http.StatusNotFound)
	}
	body, eErr := buildRequest(rec, req.Payload, stream)
	if eErr != nil {
		return nil, rec, toFailure(eErr)
	}
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, catalog.JoinUpstreamURL(cfg.BaseURL, rec.EndpointPath), bytes.NewReader(body))
	if errReq != nil {
		return nil, rec, errReq
	}
	if rec.Protocol == catalog.RouteMessages {
		httpReq.Header = messages.AuthHeaders(native.APIKey)
	} else {
		httpReq.Header = chatcompletions.AuthHeaders(native.APIKey)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-opencode-session", sessionID(req.Payload))
	return httpReq, rec, nil
}

// buildRequest translates the chat-completions payload for the model's route
// and pins the stream flag to the execution mode the host chose.
func buildRequest(rec catalog.ModelRecord, payload []byte, stream bool) ([]byte, *errclass.Error) {
	var body map[string]any
	if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil || body == nil {
		return nil, errclass.Translation("chat payload is not a JSON object")
	}
	body["stream"] = stream
	if !stream {
		delete(body, "stream_options")
	}
	normalized, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, errclass.Translation("chat payload could not be encoded")
	}
	switch rec.Protocol {
	case catalog.RouteMessages:
		return messages.BuildRequest(rec.UpstreamID, sourceFormat, normalized, rec.Thinking)
	case catalog.RouteResponses:
		return responses.BuildRequest(rec.UpstreamID, sourceFormat, normalized, rec.Thinking)
	default:
		return chatcompletions.BuildRequest(rec.UpstreamID, sourceFormat, normalized, rec.Thinking)
	}
}

func convertNonStream(route catalog.Route, status int, body []byte) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteMessages:
		return messages.ConvertNonStreamResponse(sourceFormat, status, body)
	case catalog.RouteResponses:
		return responses.ConvertNonStreamResponse(sourceFormat, status, body)
	default:
		return chatcompletions.ConvertNonStreamResponse(sourceFormat, status, body)
	}
}

type streamConverter interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
}

func newStreamConverter(route catalog.Route) streamConverter {
	switch route {
	case catalog.RouteMessages:
		return messages.NewStreamConverter(sourceFormat)
	case catalog.RouteResponses:
		return responses.NewStreamConverter(sourceFormat)
	default:
		return chatcompletions.NewStreamConverter(sourceFormat)
	}
}

// emitEvents unwraps SSE-framed converter output into bare JSON payloads,
// the shape the host expects from plugin stream chunks.
func emitEvents(sink core.StreamSink, events [][]byte) error {
	for _, event := range events {
		for _, line := range bytes.Split(event, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(payload) == 0 || string(payload) == "[DONE]" {
				continue
			}
			if errEmit := sink.Emit(payload); errEmit != nil {
				return errEmit
			}
		}
	}
	return nil
}

// sessionID keys upstream prompt caching on the conversation's opening
// messages, so later turns of one conversation share a session.
func sessionID(payload []byte) string {
	var body struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(payload, &body)
	hash := sha256.New()
	for index, message := range body.Messages {
		if index >= 2 {
			break
		}
		hash.Write(message)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
