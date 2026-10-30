package hostbridge

// This file tests the contract compatibility of the host HTTP bridge.
//
// The focus is on host.http.do response key names: the host puts pluginapi.HTTPResponse without JSON tags
// directly into the envelope, and production key names are Go field names. Recognizing only one key name silently changes the status code to 0,
// resulting in all non-streaming upstream requests being judged as failures, while the error body contains the real response,
// making it look like "credential rejected"—which is extremely difficult to troubleshoot. Therefore, both formats must be decodable.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestHostHTTPResponseAcceptsGoFieldNames verifies the key name format of the host's main path.
func TestHostHTTPResponseAcceptsGoFieldNames(t *testing.T) {
	raw := []byte(`{"StatusCode":200,"Headers":{"Content-Type":["application/json"]},"Body":"aGVsbG8="}`)
	var response rpcHostHTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if !response.statusSeen {
		t.Fatal("StatusCode present but reported missing")
	}
	if response.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", response.StatusCode)
	}
	if string(response.Body) != "hello" {
		t.Fatalf("Body = %q, want hello", response.Body)
	}
}

// TestHostHTTPResponseAcceptsSnakeCase verifies that the intermediate format with tags can also be decoded.
func TestHostHTTPResponseAcceptsSnakeCase(t *testing.T) {
	raw := []byte(`{"status_code":429,"headers":{"Retry-After":["30"]},"body":"cmF0ZQ=="}`)
	var response rpcHostHTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if response.StatusCode != 429 {
		t.Fatalf("StatusCode = %d, want 429", response.StatusCode)
	}
	if response.Headers.Get("Retry-After") != "30" {
		t.Fatalf("headers not decoded: %v", response.Headers)
	}
}

// TestHostHTTPResponseFlagsMissingStatus verifies that missing status codes are explicitly flagged.
//
// It cannot be silently treated as 0: that would cause upstream 200 responses to be judged as failures.
func TestHostHTTPResponseFlagsMissingStatus(t *testing.T) {
	var response rpcHostHTTPResponse
	if errUnmarshal := json.Unmarshal([]byte(`{"headers":{},"body":""}`), &response); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if response.statusSeen {
		t.Fatal("absent status field must be flagged so the caller can report an ABI mismatch")
	}
}

// TestHostHTTPStreamResponseUsesSnakeCase verifies the key names for streaming responses (this struct has tags).
func TestHostHTTPStreamResponseUsesSnakeCase(t *testing.T) {
	raw := []byte(`{"status_code":200,"stream_id":"s1","headers":{"X-A":["b"]}}`)
	var response rpcHostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if response.StatusCode != 200 || response.StreamID != "s1" {
		t.Fatalf("stream response = %+v", response)
	}
}

func TestStreamBodyStopsAfterTooManyEmptyChunks(t *testing.T) {
	calls := 0
	Configure(func(_, method string, _ any) (json.RawMessage, error) {
		if method != "host.http.stream_read" {
			return json.RawMessage(`{"done":true}`), nil
		}
		calls++
		return json.RawMessage(`{"payload":null}`), nil
	})
	t.Cleanup(func() { Configure(nil) })

	body := &streamBody{ctx: context.Background(), streamID: "s1"}
	_, errRead := body.Read(make([]byte, 1))
	if !errors.Is(errRead, errTooManyEmptyReads) {
		t.Fatalf("Read error = %v, want %v", errRead, errTooManyEmptyReads)
	}
	if calls != maxConsecutiveEmptyReads {
		t.Fatalf("stream_read calls = %d, want %d", calls, maxConsecutiveEmptyReads)
	}
}

func TestStreamBodyEmptyChunksHonorContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	Configure(func(_, method string, _ any) (json.RawMessage, error) {
		if method != "host.http.stream_read" {
			return json.RawMessage(`{"done":true}`), nil
		}
		calls++
		if calls == 2 {
			cancel()
		}
		return json.RawMessage(`{"payload":null}`), nil
	})
	t.Cleanup(func() { Configure(nil); cancel() })

	body := &streamBody{ctx: ctx, streamID: "s1"}
	_, errRead := body.readChunk()
	if !errors.Is(errRead, context.Canceled) {
		t.Fatalf("readChunk error = %v, want context.Canceled", errRead)
	}
	if calls != 2 {
		t.Fatalf("stream_read calls = %d, want 2", calls)
	}
}

// TestWithCallbackID verifies the binding and reading of callback IDs.
func TestWithCallbackID(t *testing.T) {
	if got := CallbackID(WithCallbackID(nil, "cb-1")); got != "cb-1" {
		t.Fatalf("callback id = %q, want cb-1", got)
	}
	// Empty ID is not written to context (maintaining zero-value semantics).
	if got := CallbackID(WithCallbackID(nil, "   ")); got != "" {
		t.Fatalf("blank callback id must not be stored, got %q", got)
	}
}
