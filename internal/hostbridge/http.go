// Package httpx routes outbound HTTP requests from the plugin to the CPA host's HTTP bridge.
//
// Why it must go through the host bridge instead of making direct requests:
//   - The host's global proxies, account-level proxies, outbound request logs, and API statistics all hook into this chain;
//   - Direct net/http outbound calls from the plugin bypass all of the above, resulting in "proxy settings having no effect" and "requests not appearing in logs".
//
// This package provides two *http.Client instances:
//   - Client: Uses host.http.do to retrieve complete responses in a single call (for short requests like model lists, quotas, and check-ins);
//   - StreamClient: Uses host.http.do_stream, where response bodies are pulled on demand from host.http.stream_read (for SSE chats).
//
// Dependency direction on the internal package: This package does not know the host exists; host-invoked capabilities are injected by package main
// in init via Configure (preventing internal from reverse-importing main).
package hostbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"free-model-hub/cpasdk/pluginabi"
)

// Empty chunk protection: Back off when the host temporarily has no data; terminate promptly if the host remains abnormal, avoiding permanent spinning.
const (
	emptyReadBackoff         = time.Millisecond
	emptyReadBackoffMax      = 16 * time.Millisecond
	maxConsecutiveEmptyReads = 32
)

// Client returns a client routed outbound via the host's host.http.do.
func Client(ctx context.Context, timeout time.Duration) *http.Client {
	return &http.Client{Transport: &transport{ctx: ctx}, Timeout: timeout}
}

// StreamClient returns a client routed outbound via the host's host.http.do_stream (used for SSE).
//
// Timeout is set to 0: The total duration of a streaming response is unpredictable; time-to-first-byte and idle timeouts are managed separately by upstream
// ResponseHeaderTimeout and idle monitoring respectively (see idle handling in the WorkBuddy package).
func StreamClient(ctx context.Context, timeout time.Duration) *http.Client {
	return &http.Client{Transport: &transport{ctx: ctx, stream: true}, Timeout: timeout}
}

type transport struct {
	ctx    context.Context
	stream bool
}

// RoundTrip implements http.RoundTripper: converts requests into host HTTP bridge calls.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if t.ctx != nil {
		ctx = t.ctx
	}
	body, errBody := readRequestBody(req)
	if errBody != nil {
		return nil, errBody
	}

	payload := rpcHostHTTPRequest{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: req.Header,
		Body:    body,
	}
	method := pluginabi.MethodHostHTTPDo
	if t.stream {
		method = pluginabi.MethodHostHTTPDoStream
	}
	raw, errCall := CallCtx(ctx, method, payload)
	if errCall != nil {
		return nil, errCall
	}

	if !t.stream {
		var rpc rpcHostHTTPResponse
		if errUnmarshal := json.Unmarshal(raw, &rpc); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host http response: %w", errUnmarshal)
		}
		if !rpc.statusSeen {
			// Cannot silently default to 0: That would cause upstream 200 responses to be judged as failures, while the error body contains the real response,
			// Looks like "credential rejected", which is extremely hard to troubleshoot. Better to directly report protocol mismatch.
			return nil, fmt.Errorf("host http response has no status field (host ABI mismatch): %s", truncateForError(raw))
		}
		return buildResponse(req, rpc.StatusCode, rpc.Headers, io.NopCloser(bytes.NewReader(rpc.Body))), nil
	}

	var rpc rpcHostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &rpc); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream response: %w", errUnmarshal)
	}
	if rpc.StatusCode == 0 {
		return nil, fmt.Errorf("host http stream response has no status field (host ABI mismatch): %s", truncateForError(raw))
	}
	streamBody := &streamBody{
		ctx:      ctx,
		streamID: rpc.StreamID,
	}
	return buildResponse(req, rpc.StatusCode, rpc.Headers, streamBody), nil
}

func readRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, errRead := io.ReadAll(req.Body)
	if errClose := req.Body.Close(); errClose != nil && errRead == nil {
		return nil, fmt.Errorf("close request body: %w", errClose)
	}
	if errRead != nil {
		return nil, fmt.Errorf("read request body: %w", errRead)
	}
	return body, nil
}

func buildResponse(req *http.Request, status int, header http.Header, body io.ReadCloser) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          body,
		ContentLength: -1,
		Request:       req,
	}
}

// rpcHostHTTPRequest is the request body for host.http.do / do_stream.
//
// Fill only flat fields (method/url/headers/body): The host accepts both flat and nested forms,
// and the flat form is simpler and less prone to missing fields.
type rpcHostHTTPRequest struct {
	Method  string      `json:"method,omitempty"`
	URL     string      `json:"url,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

// rpcHostHTTPResponse is the response body for host.http.do.
//
// Crucial: The host's host.http.do drops pluginapi.HTTPResponse directly into the envelope, and this struct
// **has no json tags**, so online key names are Go field names (StatusCode / Headers / Body).
// However, a few places in the host use intermediate structs with tags (status_code / headers / body).
// If only one is recognized, the status code would silently become 0, resulting in all non-streaming upstream requests being judged as failures.
// Therefore, UnmarshalJSON is hand-written here to accept both forms.
type rpcHostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	// statusSeen tracks whether the status code was actually unmarshaled.
	// Using "seen" rather than "missing" ensures zero-value semantics are correct: the zero value of the struct means "not seen".
	statusSeen bool
}

// UnmarshalJSON is compatible with both host serialization forms (Go field names / snake_case).
func (r *rpcHostHTTPResponse) UnmarshalJSON(data []byte) error {
	if r == nil {
		return errors.New("nil host http response")
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &fields); errUnmarshal != nil {
		return errUnmarshal
	}
	for key, value := range fields {
		switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
		case "statuscode":
			r.statusSeen = true
			if errDecode := json.Unmarshal(value, &r.StatusCode); errDecode != nil {
				return fmt.Errorf("decode host http status: %w", errDecode)
			}
		case "headers":
			if errDecode := json.Unmarshal(value, &r.Headers); errDecode != nil {
				return fmt.Errorf("decode host http headers: %w", errDecode)
			}
		case "body":
			if errDecode := json.Unmarshal(value, &r.Body); errDecode != nil {
				return fmt.Errorf("decode host http body: %w", errDecode)
			}
		}
	}
	return nil
}

// rpcHostHTTPStreamResponse is the response body for host.http.do_stream.
// This struct **has** json tags on the host side, with snake_case key names.
type rpcHostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type rpcHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

// streamBody implements io.ReadCloser, pulling upstream chunks on demand from host.http.stream_read.
//
// Agreement with the host: The host returns done=true to indicate stream completion; a non-nil error indicates a stream error.
// This type is responsible for translating these two into io.EOF and errors.
type streamBody struct {
	ctx      context.Context
	streamID string

	mu       sync.Mutex
	closed   bool
	closeErr error
	pending  []byte
}

// Read reads data from the upstream stream. Each call requests one chunk from the host.
func (b *streamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		b.mu.Unlock()
		return n, nil
	}
	if b.closed {
		errClosed := b.closeErr
		b.mu.Unlock()
		if errClosed != nil {
			return 0, errClosed
		}
		return 0, io.EOF
	}
	b.mu.Unlock()

	chunk, errRead := b.readChunk()
	if errRead != nil {
		// Read failure means termination: mark as closed and close the host-side stream to prevent leaks.
		_ = b.Close()
		return 0, errRead
	}
	if len(chunk) == 0 {
		return 0, nil
	}
	n := copy(p, chunk)
	if n < len(chunk) {
		b.mu.Lock()
		b.pending = append(b.pending, chunk[n:]...)
		b.mu.Unlock()
	}
	return n, nil
}

// errTooManyEmptyReads indicates the host continuously returned empty chunks, and the stream has been proactively terminated.
var errTooManyEmptyReads = errors.New("host http stream returned too many empty chunks")

// readChunk requests a chunk from the host.
//
// Empty chunks are not end signals: the host bridge returns payload=nil when its buffer is temporarily empty. The handling
// is to back off, retry, and set an upper limit on consecutive empty chunks in a single call—preventing permanent spinning when the bridge is abnormal.
// Every Read is a new readChunk, so counters and backoff naturally start from the beginning.
func (b *streamBody) readChunk() ([]byte, error) {
	if strings.TrimSpace(b.streamID) == "" {
		return nil, errors.New("host http stream id is missing")
	}
	emptyReads := 0
	backoff := emptyReadBackoff
	for {
		select {
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		default:
		}
		raw, errCall := CallCtx(b.ctx, pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{StreamID: b.streamID})
		if errCall != nil {
			return nil, fmt.Errorf("host http stream read: %w", errCall)
		}
		var chunk rpcHostHTTPStreamReadResponse
		if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host http stream chunk: %w", errUnmarshal)
		}
		if chunk.Error != "" {
			return nil, errors.New(chunk.Error)
		}
		if chunk.Done {
			return nil, io.EOF
		}
		if len(chunk.Payload) > 0 {
			return chunk.Payload, nil
		}
		emptyReads++
		if emptyReads >= maxConsecutiveEmptyReads {
			return nil, errTooManyEmptyReads
		}
		// Empty chunk: exponential backoff yields the CPU until data arrives, stream ends, or context is cancelled.
		select {
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < emptyReadBackoffMax {
			backoff *= 2
			if backoff > emptyReadBackoffMax {
				backoff = emptyReadBackoffMax
			}
		}
	}
}

// Close closes the upstream stream on the host side. Idempotent: host.http.stream_close is only truly called once.
func (b *streamBody) Close() error {
	b.mu.Lock()
	if b.closed {
		errClosed := b.closeErr
		b.mu.Unlock()
		return errClosed
	}
	b.closed = true
	b.mu.Unlock()

	if strings.TrimSpace(b.streamID) == "" {
		return nil
	}
	_, errCall := CallCtx(b.ctx, pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{StreamID: b.streamID})
	if errCall != nil {
		errWrapped := fmt.Errorf("host http stream close: %w", errCall)
		b.mu.Lock()
		b.closeErr = errWrapped
		b.mu.Unlock()
		return errWrapped
	}
	return nil
}

// truncateForError truncates the response body for error messages to avoid stuffing a whole page of HTML into the logs.
func truncateForError(raw []byte) string {
	const limit = 256
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "..."
}
