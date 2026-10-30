package testhost

// Package testhost provides an in-memory implementation of host callbacks (Fake Host) and upstream stubs.
//
// Purpose: allow root and vendor tests to share the same set of host RPC behaviors, instead of each package
// writing its own stub that "only implements the methods it needs". It implements all common methods of host.*:
// outbound HTTP, streaming read/close, credential list/read, and stream chunk delivery.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/internal/core"
)

// UpstreamResponse is the fake host's response to an outbound request.
type UpstreamResponse struct {
	Status int
	Header map[string][]string
	Body   string
}

// Stream is a simulated streaming response.
type Stream struct {
	chunks   []string
	position int
	closed   bool
}

// Host is an in-memory implementation of host callbacks (implementing the signature of hostbridge.Caller).
type Host struct {
	mu sync.Mutex

	// calls records the names of the invoked host methods (for assertions).
	calls []string
	// authFiles is the return content of host.auth.list.
	authFiles []core.HostAuth
	// authJSON maps auth_index to credential JSON.
	authJSON map[string]string
	// authPaths maps auth_index to the credential file path returned by the host (the path for host.auth.get).
	// The path of a real host is not guaranteed to be absolute: hosts started with a relative auth-dir will return relative paths.
	authPaths map[string]string
	// upstream is the response function for outbound requests (matched by method + url).
	upstream func(method, url, body string) UpstreamResponse
	// upstreamCalls records the number of outbound requests (for singleflight-style assertions).
	upstreamCalls int

	streams    map[string]*Stream
	nextStream int

	// Records of stream forwarding.
	emits     []string
	streamErr []string
	closed    []string
}

// New constructs a Fake Host.
func New() *Host {
	return &Host{
		authJSON:  map[string]string{},
		authPaths: map[string]string{},
		streams:   map[string]*Stream{},
		upstream: func(string, string, string) UpstreamResponse {
			return UpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404,"msg":"not stubbed"}`}
		},
	}
}

// Call implements hostbridge.Caller.
func (h *Host) Call(callbackID, method string, payload any) (json.RawMessage, error) {
	h.mu.Lock()
	h.calls = append(h.calls, method)
	h.mu.Unlock()

	switch method {
	case pluginabi.MethodHostHTTPDo:
		return h.handleHTTPDo(payload)
	case pluginabi.MethodHostHTTPDoStream:
		return h.handleHTTPDoStream(payload)
	case pluginabi.MethodHostHTTPStreamRead:
		return h.handleStreamRead(payload)
	case pluginabi.MethodHostHTTPStreamClose:
		return h.handleStreamClose(payload)
	case pluginabi.MethodHostAuthList:
		return marshalJSON(map[string]any{"auths": h.SnapshotAuthFiles()})
	case pluginabi.MethodHostAuthGet:
		return h.handleAuthGet(payload)
	case pluginabi.MethodHostAuthSave:
		return h.handleAuthSave(payload)
	case pluginabi.MethodHostStreamEmit:
		return h.handleStreamEmit(payload)
	case pluginabi.MethodHostStreamClose:
		return h.handleStreamClosePayload(payload)
	}
	return nil, fmt.Errorf("fake host does not implement %s", method)
}

// SnapshotAuthFiles returns the currently registered credentials (as a copy).
func (h *Host) SnapshotAuthFiles() []core.HostAuth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]core.HostAuth, len(h.authFiles))
	copy(out, h.authFiles)
	return out
}

// AddAccount registers a fake account.
func (h *Host) AddAccount(authIndex, name, credentialJSON string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authJSON[authIndex] = credentialJSON
	h.authFiles = append(h.authFiles, core.HostAuth{
		ID: name, AuthIndex: authIndex, Name: name, Provider: core.ProviderKey, Label: name,
	})
}

// SetAccountCredential overwrites the credential JSON for a specific account while keeping the account identity (filename) unchanged.
//
// Used to simulate the host rewriting the same credential file after token refresh / re-login: content changes, but identity remains.
func (h *Host) SetAccountCredential(authIndex, credentialJSON string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authJSON[authIndex] = credentialJSON
}

// SetAuthFilePath makes host.auth.get return the physical file path of the credential.
//
// The path of a real host is not guaranteed to be absolute: hosts started with a relative auth-dir (EasyCLIProxyAPI
// defaults to ../oauth) will return the relative path obtained from WalkDir as-is. It is passed through as-is here,
// so that deletion and other path-dependent features can reproduce this behavior.
func (h *Host) SetAuthFilePath(authIndex, path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authPaths[authIndex] = path
}

// MarkUnavailable simulates the host routing layer marking a credential as "temporarily unavailable" (cooling down
// after upstream rate limiting/timeout). This differs from explicit user disablement: the credential should still participate in model probing and quota queries.
func (h *Host) MarkUnavailable(name string, unavailable bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for index := range h.authFiles {
		if h.authFiles[index].Name == name || h.authFiles[index].ID == name {
			h.authFiles[index].Unavailable = unavailable
		}
	}
}

// MarkDisabled simulates the user explicitly disabling a credential.
func (h *Host) MarkDisabled(name string, disabled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for index := range h.authFiles {
		if h.authFiles[index].Name == name || h.authFiles[index].ID == name {
			h.authFiles[index].Disabled = disabled
		}
	}
}

// SetUpstream sets the response for outbound requests.
func (h *Host) SetUpstream(fn func(method, url, body string) UpstreamResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.upstream = fn
}

// UpstreamCalls returns the number of outbound requests that have occurred.
func (h *Host) UpstreamCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.upstreamCalls
}

// doUpstream counts an outbound request and retrieves its response.
func (h *Host) doUpstream(method, url, body string) UpstreamResponse {
	h.mu.Lock()
	h.upstreamCalls++
	fn := h.upstream
	h.mu.Unlock()
	return fn(method, url, body)
}

// EmittedChunks returns the stream chunks that have been delivered.
func (h *Host) EmittedChunks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.emits...)
}

// StreamErrors returns the errors reported when streams were closed.
func (h *Host) StreamErrors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.streamErr...)
}

// ClosedStreams returns the list of closed stream IDs.
func (h *Host) ClosedStreams() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.closed...)
}

func (h *Host) handleHTTPDo(payload any) (json.RawMessage, error) {
	req := decodeRequest(payload)
	response := h.doUpstream(req.method, req.url, string(req.body))
	// Shaped like a real host: host.http.do returns a pluginapi.HTTPResponse **without json tags**.
	return marshalJSON(map[string]any{
		"StatusCode": response.Status,
		"Headers":    response.Header,
		"Body":       []byte(response.Body),
	})
}

func (h *Host) handleHTTPDoStream(payload any) (json.RawMessage, error) {
	req := decodeRequest(payload)
	response := h.doUpstream(req.method, req.url, string(req.body))

	h.mu.Lock()
	h.nextStream++
	streamID := fmt.Sprintf("stream-%d", h.nextStream)
	h.streams[streamID] = &Stream{chunks: splitChunks(response.Body)}
	h.mu.Unlock()

	return marshalJSON(map[string]any{
		"status_code": response.Status,
		"headers":     response.Header,
		"stream_id":   streamID,
	})
}

func (h *Host) handleStreamRead(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	stream, okStream := h.streams[req.StreamID]
	if !okStream {
		h.mu.Unlock()
		return marshalJSON(map[string]any{"done": true})
	}
	if stream.position >= len(stream.chunks) {
		delete(h.streams, req.StreamID)
		h.mu.Unlock()
		return marshalJSON(map[string]any{"done": true})
	}
	chunk := stream.chunks[stream.position]
	stream.position++
	h.mu.Unlock()
	return marshalJSON(map[string]any{"payload": []byte(chunk)})
}

func (h *Host) handleStreamClose(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	if stream, okStream := h.streams[req.StreamID]; okStream {
		stream.closed = true
	}
	delete(h.streams, req.StreamID)
	h.mu.Unlock()
	return marshalJSON(map[string]any{})
}

func (h *Host) handleAuthGet(payload any) (json.RawMessage, error) {
	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	raw, okRaw := h.authJSON[req.AuthIndex]
	path := h.authPaths[req.AuthIndex]
	h.mu.Unlock()
	if !okRaw {
		return json.RawMessage(`{"auth_index":"` + req.AuthIndex + `","json":null}`), nil
	}
	// A real host's HostAuthGetResponse.JSON is json.RawMessage: inlined as-is,
	// cannot go through []byte serialization (which would turn into base64 and fail to decode on the plugin side).
	response := struct {
		AuthIndex string          `json:"auth_index"`
		Path      string          `json:"path,omitempty"`
		JSON      json.RawMessage `json:"json"`
	}{AuthIndex: req.AuthIndex, Path: path, JSON: json.RawMessage(raw)}
	return marshalJSON(response)
}

// handleAuthSave simulates host.auth.save: writes the credential into the host's in-memory registry.
//
// The fake host has no disk auth directory, so it only updates authJSON and synchronizes the disabled
// state of authFiles according to the disabled field in the JSON — the end-to-end behavior of enabling/disabling accounts is observable through this step.
func (h *Host) handleAuthSave(payload any) (json.RawMessage, error) {
	var req struct {
		Name string          `json:"name"`
		JSON json.RawMessage `json:"json"`
	}
	if errUnmarshal := json.Unmarshal(mustMarshal(payload), &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(req.JSON, &fields); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	var disabled bool
	_ = json.Unmarshal(fields["disabled"], &disabled)

	h.mu.Lock()
	for index := range h.authFiles {
		if h.authFiles[index].Name != req.Name && h.authFiles[index].ID != req.Name {
			continue
		}
		h.authFiles[index].Disabled = disabled
		h.authJSON[h.authFiles[index].AuthIndex] = string(req.JSON)
	}
	path := h.authPaths[resolvedAuthIndex(h, req.Name)]
	h.mu.Unlock()
	return marshalJSON(map[string]any{"name": req.Name, "path": path})
}

func resolvedAuthIndex(h *Host, name string) string {
	for _, entry := range h.authFiles {
		if entry.Name == name || entry.ID == name {
			return entry.AuthIndex
		}
	}
	return ""
}

func (h *Host) handleStreamEmit(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	h.emits = append(h.emits, string(req.Payload))
	h.mu.Unlock()
	return marshalJSON(map[string]any{})
}

func (h *Host) handleStreamClosePayload(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	h.closed = append(h.closed, req.StreamID)
	if req.Error != "" {
		h.streamErr = append(h.streamErr, req.Error)
	}
	h.mu.Unlock()
	return marshalJSON(map[string]any{})
}

type request struct {
	method string
	url    string
	body   []byte
}

func decodeRequest(payload any) request {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(mustMarshal(payload), &fields)

	out := request{}
	if raw, okRaw := fields["method"]; okRaw {
		_ = json.Unmarshal(raw, &out.method)
	}
	if raw, okRaw := fields["url"]; okRaw {
		_ = json.Unmarshal(raw, &out.url)
	}
	if raw, okRaw := fields["body"]; okRaw {
		_ = json.Unmarshal(raw, &out.body)
	}
	return out
}

func mustMarshal(value any) []byte {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}

func marshalJSON(value any) (json.RawMessage, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return raw, nil
}

// splitChunks splits the response body into small pieces to simulate the host's real 32KB chunked reading path.
//
// Deliberately avoid feeding the entire body at once: that would mask bugs related to chunk boundaries.
func splitChunks(body string) []string {
	if body == "" {
		return nil
	}
	const size = 17
	chunks := make([]string, 0, len(body)/size+1)
	for len(body) > 0 {
		take := size
		if take > len(body) {
			take = len(body)
		}
		chunks = append(chunks, body[:take])
		body = body[take:]
	}
	return chunks
}
