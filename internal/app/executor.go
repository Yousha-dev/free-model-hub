package app

// This file implements two types of host capabilities: executor and quota:
//   - executor: non-streaming/streaming execution, token estimation, outbound HTTP declarations;
//   - quota: quota query and check-in style reset.
//
// Actual upstream protocol calls are handled by each provider's Execute/Quota implementations.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

const (
	// executorRPCTimeout is the internal limit for non-streaming execution (streaming has its own idle control).
	executorRPCTimeout = 10 * time.Minute
	// streamEmitTimeout is the limit for a single stream chunk emission.
	streamEmitTimeout = 30 * time.Second
)

// executorRPCRequest is aligned with the host's rpcExecutorRequest.
//
// Note: Embedded fields are transmitted by Go field names (without json tags), while stream_id / host_callback_id use lowercase keys.
type executorRPCRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// rpcExecutorStreamResponse is aligned with the host's rpcExecutorStreamResponse.
//
// This plugin follows "synchronous header return + asynchronous emit chunks", so only headers are returned.
type rpcExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

// preparedExecution aggregates the synchronous validation results of an execution.
type preparedExecution struct {
	rpc        executorRPCRequest
	vendor     core.Vendor
	credential *core.Credential
	model      string
	// modelAliased reports that the request hit an alias: the model name in the request body must be restored before outbound.
	modelAliased bool
}

// executeRequest converts the host's execution request into a provider-agnostic form.
func (p preparedExecution) executeRequest() *core.ExecuteRequest {
	payload := p.rpc.Payload
	if p.modelAliased {
		// Provider chat implementations often read the model from the request body (Qoder / Trae, etc.), so aliases must be
		// restored to official IDs here, otherwise the upstream will receive the alias.
		payload = rewritePayloadModel(payload, p.model)
	}
	return &core.ExecuteRequest{
		Model:    p.model,
		Payload:  payload,
		Headers:  p.rpc.Headers,
		ClientIP: core.ExtractClientIP(p.rpc.Headers),
		Stream:   p.rpc.Stream,
	}
}

// handleExecutorExecute executes a non-streaming chat.
func (a *App) handleExecutorExecute(request []byte) ([]byte, error) {
	prepared, errPrepare := a.prepareExecution(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	ctx, cancel := context.WithTimeout(
		hostbridge.WithCallbackID(context.Background(), prepared.rpc.HostCallbackID), executorRPCTimeout)
	defer cancel()

	response, errExecute := prepared.vendor.Execute(ctx, prepared.credential, prepared.executeRequest())
	if errExecute != nil {
		return nil, core.ToPluginError(errExecute)
	}
	return core.OKEnvelope(response)
}

// handleExecutorExecuteStream executes a streaming chat.
//
// Architecture: **Synchronous response header return + goroutine asynchronous chunk delivery**.
// The host first obtains headers to construct the downstream response, and then receives chunks via host.stream.emit,
// and receives the end signal via host.stream.close.
func (a *App) handleExecutorExecuteStream(request []byte) ([]byte, error) {
	prepared, errPrepare := a.prepareExecution(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	if strings.TrimSpace(prepared.rpc.StreamID) == "" {
		return nil, core.NewPluginError("invalid_request", "stream_id is required for streaming execution", http.StatusBadRequest)
	}

	// Headers must be returned synchronously: the host uses them to construct the downstream response.
	streamHeaders := http.Header{
		"Content-Type":  []string{"text/event-stream"},
		"Cache-Control": []string{"no-cache"},
	}

	streamCtx, cancelStream, finishStream := a.BeginPluginStream()
	go func() {
		defer finishStream()
		a.runStreamExecution(streamCtx, cancelStream, prepared)
	}()

	return core.OKEnvelope(rpcExecutorStreamResponse{Headers: streamHeaders})
}

// rewritePayloadModel rewrites the model field in the request body to the official ID.
//
// When parsing fails, return as-is: better not to rewrite than to let a chat session be interrupted due to local JSON parsing failure —
// the upstream's real error has more diagnostic value than a local parsing error (same convention as workbuddy.PrepareBody).
func rewritePayloadModel(payload []byte, model string) []byte {
	if len(payload) == 0 || strings.TrimSpace(model) == "" {
		return payload
	}
	var obj map[string]any
	if errUnmarshal := json.Unmarshal(payload, &obj); errUnmarshal != nil || obj == nil {
		return payload
	}
	obj["model"] = model
	encoded, errMarshal := json.Marshal(obj)
	if errMarshal != nil {
		return payload
	}
	return encoded
}

// runStreamExecution forwards upstream SSE to the host stream in the background.
func (a *App) runStreamExecution(streamCtx context.Context, cancelStream context.CancelFunc, prepared preparedExecution) {
	ctx := hostbridge.WithCallbackID(streamCtx, prepared.rpc.HostCallbackID)
	streamID := prepared.rpc.StreamID
	callbackID := prepared.rpc.HostCallbackID

	sink := &hostStreamSink{
		callbackID:   callbackID,
		streamID:     streamID,
		cancelStream: cancelStream,
	}
	errForward := prepared.vendor.ExecuteStream(ctx, prepared.credential, prepared.executeRequest(), sink)
	// Downstream active disconnection cancels the context; that is not an error and should not send an error frame to the client.
	if errForward != nil && streamCtx.Err() == nil {
		closeStream(callbackID, streamID, streamFailureMessage(errForward))
		return
	}
	closeStream(callbackID, streamID, "")
}

// hostStreamSink wraps the host's streaming delivery capability into a core.StreamSink.
//
// Providers are only responsible for producing chunk content, while delivery (host.stream.emit) and finalization are handled uniformly by the root layer —
// this logic is related to the host contract and should not be rewritten by each provider.
type hostStreamSink struct {
	callbackID   string
	streamID     string
	cancelStream context.CancelFunc
}

// Emit only delivers the raw data payload: the host's OpenAI handler is responsible for the data: prefix, blank lines, and [DONE].
func (s *hostStreamSink) Emit(payload []byte) error {
	frame := executorStreamPayload(payload)
	if len(frame) == 0 {
		return nil
	}
	if errEmit := emitStreamChunk(s.callbackID, s.streamID, frame); errEmit != nil {
		// Downstream disconnection: cancel upstream reading, silently finalize (this is not an error).
		s.cancelStream()
		return errEmit
	}
	return nil
}

// executorStreamPayload normalizes a single provider chunk into the raw payload accepted by the host executor.
// SSE control lines cannot be delivered as data, otherwise heartbeats will become unparseable data: : heartbeat.
func executorStreamPayload(payload []byte) []byte {
	payload = bytes.TrimSpace(payload)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) ||
		bytes.HasPrefix(payload, []byte(":")) ||
		bytes.HasPrefix(payload, []byte("event:")) ||
		bytes.HasPrefix(payload, []byte("id:")) ||
		bytes.HasPrefix(payload, []byte("retry:")) {
		return nil
	}
	return payload
}

// handleResponseNormalizeBefore only adds the SSE envelope required by the converter before cross-protocol streaming conversion.
// Same-format OpenAI responses are passed directly to the handler and must not be wrapped prematurely at the executor exit.
//
// The host calls this for every SSE line, passing the complete client request and upstream request (which can be several MBs).
// Therefore, ResponseTransformRequest is never fully decoded here; values are obtained via the single-pass linear scan
// of scanResponseTransform (see explanation in responsegate.go).
//
// Input parameters may be the host's read-only buffer: C ABI directly borrows host memory for this method (see root
// cgo.go's hostRequestBytes / isReadOnlyRequestMethod). Therefore, only read-only scanning is allowed here,
// Do not modify the request in place, nor save any fragments of it after the call returns.
//
// Returning an empty PayloadResponse indicates "unmodified" -- the host explicitly treats this as a no-op
// (callResponseNormalizer preserves the original body when len(Body)==0).
func (a *App) handleResponseNormalizeBefore(request []byte) ([]byte, error) {
	body, okChunk := normalizeResponseChunk(request)
	if !okChunk || !json.Valid(bytes.TrimSpace(body)) {
		return core.OKEnvelope(pluginapi.PayloadResponse{})
	}
	return core.OKEnvelope(pluginapi.PayloadResponse{Body: formatSSEChunk(body)})
}

// formatSSEChunk is only for pre-conversion adaptation; executor shards must not call it.
func formatSSEChunk(payload []byte) []byte {
	trimmed := bytes.TrimRight(payload, "\r\n ")
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) ||
		bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte(":")) {
		return append(trimmed, '\n', '\n')
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return []byte("data: [DONE]\n\n")
	}
	out := make([]byte, 0, len(trimmed)+8)
	out = append(out, []byte("data: ")...)
	out = append(out, trimmed...)
	out = append(out, '\n', '\n')
	return out
}

// Close ends the stream. Error frames are sent uniformly by the caller after getting the return value of ExecuteStream,
// so do not resend here -- closing the same stream twice causes the client to receive duplicate end events.
func (s *hostStreamSink) Close(errMsg string) error { return nil }

// prepareExecution completes synchronous validation: request decoding, credential parsing, model and domain resolution.
func (a *App) prepareExecution(request []byte) (preparedExecution, error) {
	var prepared preparedExecution
	if errDecode := decodeRequest(request, &prepared.rpc); errDecode != nil {
		return prepared, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}

	// This plugin only declares chat-completions. If the host sends another format, it means both sides declare inconsistently,
	// and explicit error reporting is easier to locate than silent handling under incorrect formats.
	if format := strings.TrimSpace(prepared.rpc.Format); format != "" && !isChatFormat(format) {
		return prepared, core.NewPluginError("unsupported_format",
			fmt.Sprintf("unsupported protocol format %q (this plugin only handles %s)", format, formatChatCompletions),
			http.StatusBadRequest)
	}
	if format := strings.TrimSpace(prepared.rpc.SourceFormat); format != "" && !isChatFormat(format) {
		return prepared, core.NewPluginError("unsupported_format",
			fmt.Sprintf("unsupported source format %q (this plugin only handles %s)", format, formatChatCompletions),
			http.StatusBadRequest)
	}

	ctx := hostbridge.WithCallbackID(context.Background(), prepared.rpc.HostCallbackID)
	credential, vendor, errResolve := a.resolveVendorCredential(ctx, prepared.rpc.HostCallbackID,
		prepared.rpc.StorageJSON, prepared.rpc.AuthID, prepared.rpc.AuthAttributes)
	if errResolve != nil {
		return prepared, errResolve
	}
	prepared.vendor = vendor
	prepared.credential = credential

	// The provider prefix carried by the model takes priority; when absent, use the bare name (the host merges same-name models via EqualFold).
	_, bareModel := a.registry.SplitModelID(prepared.rpc.Model)
	if strings.TrimSpace(bareModel) == "" {
		return prepared, core.NewPluginError("invalid_request", "model is required", http.StatusBadRequest)
	}

	// When the client requests an alias, restore it to the upstream official ID: both validation and outbound use the official ID,
	// so the restoration must occur before both.
	prepared.model = a.resolveOutboundModel(vendor.ID(), bareModel)
	prepared.modelAliased = prepared.model != bareModel

	// Check whether the model has been disabled by plugin configuration (the disable key is vendor:model, and cross-vendor settings do not affect each other).
	// Judge using the official ID: both the disable key displayed and written in the management page is the official ID, while the alias is merely an external name.
	if a.isModelDisabled(vendor.ID(), prepared.model) {
		return prepared, core.NewPluginError("model_disabled",
			fmt.Sprintf("model %q is disabled by plugin configuration", prepared.rpc.Model),
			http.StatusBadRequest)
	}

	// Directly reject when the region where the provider is located is configured as closed (instead of letting requests hit the wrong upstream domain).
	if !core.RealmEnabled(a.Config().EnabledRealms, vendor.Region()) {
		return prepared, core.NewPluginError("vendor_disabled",
			fmt.Sprintf("vendor %s is disabled by plugin configuration", vendor.ID()), http.StatusBadRequest)
	}
	return prepared, nil
}

// handleExecutorCountTokens estimates the token count.
//
// The upstream has no token counting interface, so this is a **conservative estimate**: better to overestimate than underestimate,
// because clients use it for context management -- underestimating causes clients to send oversized contexts that get rejected upstream.
// Explicitly mark estimated in the response to avoid being treated as an upstream precise value.
func (a *App) handleExecutorCountTokens(request []byte) ([]byte, error) {
	var rpc executorRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	estimate := core.EstimateTokens(append(append([]byte(nil), rpc.OriginalRequest...), rpc.Payload...))
	payload, errMarshal := json.Marshal(map[string]any{
		"total_tokens":  estimate,
		"input_tokens":  estimate,
		"output_tokens": 0,
		"estimated":     true,
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal token estimate: %w", errMarshal)
	}
	return core.OKEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{jsonContentType}},
	})
}

// handleExecutorHTTPRequest lets the host delegate an outbound HTTP request to the plugin for execution.
//
// This plugin does not need this capability (all outbounds are actively initiated by the plugin and routed through the host's HTTP bridge),
// but since the executor is declared, it must be implemented. Returning "unsupported" is more honest than returning a false success.
func (a *App) handleExecutorHTTPRequest(request []byte) ([]byte, error) {
	return nil, core.NewPluginError("unsupported_operation",
		"this plugin does not proxy executor HTTP requests; outbound calls go through the host HTTP bridge",
		http.StatusNotImplemented)
}

// emitStreamChunk pushes a payload segment to the host stream.
func emitStreamChunk(callbackID, streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("stream id is missing")
	}
	_, errCall := hostbridge.Call(callbackID, pluginabi.MethodHostStreamEmit, map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	if errCall != nil {
		logger.Debug("stream emit failed: %v", errCall)
	}
	return errCall
}

// closeStream closes the host stream; when failure is non-empty, the host converts it into an error frame.
func closeStream(callbackID, streamID, failure string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	payload := map[string]any{"stream_id": streamID}
	if failure != "" {
		payload["error"] = failure
	}
	if _, errCall := hostbridge.Call(callbackID, pluginabi.MethodHostStreamClose, payload); errCall != nil {
		logger.Debug("stream close failed: %v", errCall)
	}
}

// streamFailureMessage converts errors into messages visible to downstream.
//
// The upstream's raw error message has more diagnostic value than our rewritten one, so pass it through preferentially.
func streamFailureMessage(err error) string {
	if err == nil {
		return ""
	}
	var detail core.UpstreamDetail
	if errors.As(err, &detail) {
		if message := strings.TrimSpace(detail.UpstreamMessage()); message != "" {
			return message
		}
	}
	return err.Error()
}

// handleQuotaDescribe describes the quota capabilities of this plugin.
func (a *App) handleQuotaDescribe(request []byte) ([]byte, error) {
	return core.OKEnvelope(pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerKey},
		DisplayName:        pluginDisplayName,
		SupportsReset:      false,
	})
}

// handleQuotaFetch queries the quota of an account.
//
// Fetching data is delegated to the provider to which the credential belongs: each party's quota interface and normalization method are completely different
// (WorkBuddy uses point balance, Qoder uses plan usage buckets).
func (a *App) handleQuotaFetch(request []byte) ([]byte, error) {
	var rpc pluginapi.QuotaFetchRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), "")
	// The host's quota routing **does not send StorageJSON** (only AuthIndex/AuthID + attributes),
	// so it must be able to fetch back by index here, otherwise it will falsely report "credential missing".
	credential, vendor, errResolve := a.resolveVendorCredential(ctx, "", rpc.StorageJSON,
		core.FirstNonEmpty(rpc.AuthIndex, rpc.AuthID), rpc.Attributes)
	if errResolve != nil {
		return nil, errResolve
	}

	provider, okQuota := core.As[core.QuotaProvider](vendor)
	if !okQuota {
		// Providers without a quota API (keyless pools, OpenRouter, Cline...)
		// answer with an informational card instead of an error, so the host's
		// auth-file page does not show "Failed to load quota" for them.
		return core.OKEnvelope(pluginapi.QuotaFetchResponse{
			Subscription: &pluginapi.QuotaSubscription{
				Plan:     vendor.Name(),
				TierName: "No quota data: this provider does not publish usage",
			},
		})
	}
	response, errQuota := provider.Quota(ctx, credential)
	if errQuota != nil {
		return nil, core.ToPluginError(errQuota)
	}
	return core.OKEnvelope(response)
}

// handleQuotaReset reports that quotas cannot be reset from here: the reset
// used to run a reward check-in, which was removed. Quotas renew on each
// upstream's own schedule.
func (a *App) handleQuotaReset(_ []byte) ([]byte, error) {
	return core.OKEnvelope(pluginapi.QuotaResetResponse{
		Success: false,
		Message: "quota reset is not supported; quotas renew on the upstream schedule",
	})
}
