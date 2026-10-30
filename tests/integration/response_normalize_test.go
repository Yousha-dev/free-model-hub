package tests

// This file implements response.normalize_before (the global response normalization hook registered by the plugin).
//
// The host calls this hook for every SSE line, passing the complete client request and complete
// upstream request (which can be several MBs, base64 encoded) in the RPC arguments. The CPU hotspot from issue #9 comes from here:
// Full payload deserialization would decode both large request bodies entirely, whereas the plugin only uses Stream / FromFormat /
// ToFormat / Body. Therefore, the focus of this file is not just "cross-protocol needs data: prefix addition",
// but also "overhead must not grow with the request body size".
//
// Background: Adding the data: prefix is the fix for issue #5 / #8 (the Claude translator requires this prefix,
// and drops the entire frame if missing), which must not be regressed for performance optimization.

import (
	"encoding/json"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/app"
)

// callNormalizeBefore invokes response.normalize_before and returns the Body provided by the plugin.
func callNormalizeBefore(t *testing.T, request pluginapi.ResponseTransformRequest) []byte {
	t.Helper()
	result := callMethod(t, pluginabi.MethodResponseNormalizeBefore, request)
	var response pluginapi.PayloadResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode normalize response: %v (raw=%s)", errUnmarshal, result)
	}
	return response.Body
}

// TestResponseNormalizeBeforeFramesCrossProtocol verifies that cross-protocol streaming chunks are padded with the SSE envelope.
//
// Test all three target formats: the Claude translator strictly requires the data: prefix, while the Gemini and
// openai-responses translators tolerate it—uniformly adding the prefix is correct for all three.
func TestResponseNormalizeBeforeFramesCrossProtocol(t *testing.T) {
	setupTestPlugin(t)

	for _, toFormat := range []string{"claude", "gemini", "openai-response"} {
		t.Run(toFormat, func(t *testing.T) {
			body := callNormalizeBefore(t, pluginapi.ResponseTransformRequest{
				FromFormat: "openai",
				ToFormat:   toFormat,
				Model:      "glm-5.2",
				Stream:     true,
				Body:       []byte(`{"id":"c1","choices":[]}`),
			})
			want := "data: {\"id\":\"c1\",\"choices\":[]}\n\n"
			if string(body) != want {
				t.Fatalf("body = %q, want %q", body, want)
			}
		})
	}
}

// TestResponseNormalizeBeforePassesThrough verifies that calls not requiring normalization return an empty Body.
//
// An empty Body is an explicit no-op on the host side (preserving the original chunk), thus saving a clone
// and preventing a double data: data: prefix in same-format paths.
func TestResponseNormalizeBeforePassesThrough(t *testing.T) {
	setupTestPlugin(t)

	cases := []struct {
		name    string
		request pluginapi.ResponseTransformRequest
	}{
		{
			name: "同格式 openai 不做任何处理",
			request: pluginapi.ResponseTransformRequest{
				FromFormat: "openai", ToFormat: "openai", Stream: true,
				Body: []byte(`{"id":"c1"}`),
			},
		},
		{
			name: "非流式请求不需要信封",
			request: pluginapi.ResponseTransformRequest{
				FromFormat: "openai", ToFormat: "claude", Stream: false,
				Body: []byte(`{"id":"c1"}`),
			},
		},
		{
			name: "源格式不是 chat-completions",
			request: pluginapi.ResponseTransformRequest{
				FromFormat: "gemini", ToFormat: "claude", Stream: true,
				Body: []byte(`{"id":"c1"}`),
			},
		},
		{
			name: "目标格式为空",
			request: pluginapi.ResponseTransformRequest{
				FromFormat: "openai", ToFormat: "", Stream: true,
				Body: []byte(`{"id":"c1"}`),
			},
		},
		{
			name: "已带 data: 前缀的分片不再包装",
			request: pluginapi.ResponseTransformRequest{
				FromFormat: "openai", ToFormat: "claude", Stream: true,
				Body: []byte("data: {\"id\":\"c1\"}"),
			},
		},
		{
			name: "[DONE] 不是合法 JSON 载荷",
			request: pluginapi.ResponseTransformRequest{
				FromFormat: "openai", ToFormat: "claude", Stream: true,
				Body: []byte("[DONE]"),
			},
		},
		{
			name: "空 Body",
			request: pluginapi.ResponseTransformRequest{
				FromFormat: "openai", ToFormat: "claude", Stream: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if body := callNormalizeBefore(t, tc.request); len(body) != 0 {
				t.Fatalf("body = %q, want empty (no-op)", body)
			}
		})
	}
}

// TestResponseNormalizeBeforeToleratesUnknownFields verifies that new host fields do not affect determination.
//
// The parser only recognizes known keys in the top-level object, and all other keys (including wrapper fields the host might add in the future)
// are skipped entirely rather than being used to determine "no processing needed".
func TestResponseNormalizeBeforeToleratesUnknownFields(t *testing.T) {
	setupTestPlugin(t)

	raw := []byte(`{"SchemaVersion":6,"HostCallbackID":"cb-1","FromFormat":"openai",` +
		`"Metadata":{"nested":[1,2,{"deep":"a\"b"}]},"ToFormat":"claude",` +
		`"Model":"glm-5.2","Stream":true,"OriginalRequest":"YWJj","Body":"eyJpZCI6ImMxIn0="}`)

	result := callMethod(t, pluginabi.MethodResponseNormalizeBefore, json.RawMessage(raw))
	var response pluginapi.PayloadResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode normalize response: %v", errUnmarshal)
	}
	if got, want := string(response.Body), "data: {\"id\":\"c1\"}\n\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestResponseNormalizeBeforeIgnoresLargeRequestBodies is a regression guard for issue #9.
//
// The host includes the complete client request and upstream request per chunk. Regardless of how large these fields are, they must not
// appear in the plugin's decoding path: allocation volume must be independent of their size.
func TestResponseNormalizeBeforeIgnoresLargeRequestBodies(t *testing.T) {
	setupTestPlugin(t)

	request := func(extraBytes int) pluginapi.ResponseTransformRequest {
		return pluginapi.ResponseTransformRequest{
			FromFormat:        "openai",
			ToFormat:          "claude",
			Model:             "glm-5.2",
			Stream:            true,
			OriginalRequest:   make([]byte, extraBytes),
			TranslatedRequest: make([]byte, extraBytes),
			Body:              []byte(`{"id":"c1","choices":[]}`),
		}
	}

	marshal := func(size int) []byte {
		raw, errMarshal := json.Marshal(request(size))
		if errMarshal != nil {
			t.Fatalf("marshal request: %v", errMarshal)
		}
		return raw
	}
	small := marshal(1 << 10) // 1 KiB
	large := marshal(4 << 20) // 4 MiB
	if len(small) >= len(large) {
		t.Fatalf("large request must be bigger: small=%d large=%d", len(small), len(large))
	}

	// Both request bodies are zero bytes, resulting in repeating "AAAA…" after base64 encoding: hitting the scanner's
	// string skip path rather than being mistakenly treated as a field value.
	allocated := func(raw []byte) float64 {
		return testing.AllocsPerRun(20, func() {
			if _, errHandle := testApp.HandleMethod(pluginabi.MethodResponseNormalizeBefore, raw); errHandle != nil {
				t.Fatalf("normalize: %v", errHandle)
			}
		})
	}
	smallAllocs, largeAllocs := allocated(small), allocated(large)
	if largeAllocs > smallAllocs {
		t.Fatalf("allocations must not grow with request bodies: 1KiB=%.1f 4MiB=%.1f", smallAllocs, largeAllocs)
	}

	// The result itself remains correct: large request bodies do not affect envelope completion.
	result := callMethod(t, pluginabi.MethodResponseNormalizeBefore, json.RawMessage(large))
	var response pluginapi.PayloadResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode normalize response: %v", errUnmarshal)
	}
	if got, want := string(response.Body), "data: {\"id\":\"c1\",\"choices\":[]}\n\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestResponseNormalizeBeforeCompatWithExecutorChunks chains the executor output and
// cross-protocol conversion stages: the executor delivers raw JSON (fix for #5/#8), and normalize adds a
// data: prefix—resulting in a single prefix layer in the end.
func TestResponseNormalizeBeforeCompatWithExecutorChunks(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			return fakeUpstreamResponse{Status: 200, Body: "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A\"}}]}\n\ndata: [DONE]\n\n"}
		}
		if strings.Contains(url, "/activity/growth/heatmap") {
			return fakeUpstreamResponse{Status: 200, Body: `{"code":0,"data":{"cells":[]}}`}
		}
		if strings.Contains(url, "/v2/report") {
			return fakeUpstreamResponse{Status: 200, Body: `{"code":0,"data":{}}`}
		}
		return fakeUpstreamResponse{Status: 404, Body: `{"code":404}`}
	})

	callMethod(t, pluginabi.MethodExecutorExecuteStream, app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Format:  app.FormatChatCompletions,
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		},
		StreamID: "s1",
	})
	if !waitFor(t, 3e9, func() bool { return len(host.emittedChunks()) > 0 }) {
		t.Fatal("executor emitted no chunks")
	}

	for _, chunk := range host.emittedChunks() {
		body := callNormalizeBefore(t, pluginapi.ResponseTransformRequest{
			FromFormat: "openai", ToFormat: "claude", Stream: true, Body: []byte(chunk),
		})
		if strings.HasPrefix(chunk, "data:") {
			t.Fatalf("executor chunk must be bare JSON, got %q", chunk)
		}
		if !strings.HasPrefix(string(body), "data: ") || !strings.HasSuffix(string(body), "\n\n") {
			t.Fatalf("normalized chunk must carry exactly one data: envelope, got %q", body)
		}
		if strings.HasPrefix(string(body), "data: data:") {
			t.Fatalf("double data: prefix regressed (#5/#8): %q", body)
		}
	}
}
