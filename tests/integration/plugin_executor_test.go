package tests

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/app"
	"free-model-hub/internal/vendors/workbuddy"
)

// TestExecutorNonStreamingAggregatesUpstream verifies the complete chain of non-streaming execution.
func TestExecutorNonStreamingAggregatesUpstream(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			// Upstream forced streaming: non-streaming is aggregated by the plugin side.
			return fakeUpstreamResponse{Status: http.StatusOK, Body: strings.Join([]string{
				`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}`,
				"",
				`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"，世界"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
				"",
				"data: [DONE]",
				"",
			}, "\n")}
		}
		// Global daily active reward: heatmap not lit -> report beacon -> reading back as 0 is still considered a success.
		if strings.Contains(url, "/activity/growth/heatmap") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"cells":[{"date":"2026-10-18","score":0}]}}`,
			}
		}
		if strings.Contains(url, "/v2/report") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	result := callMethod(t, pluginabi.MethodExecutorExecute, app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Format:  app.FormatChatCompletions,
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		},
	})
	var response pluginapi.ExecutorResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode executor response: %v", errUnmarshal)
	}

	var aggregated struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(response.Payload, &aggregated); errUnmarshal != nil {
		t.Fatalf("decode aggregated payload: %v (raw=%s)", errUnmarshal, response.Payload)
	}
	if aggregated.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", aggregated.Object)
	}
	if len(aggregated.Choices) != 1 || aggregated.Choices[0].Message.Content != "你好，世界" {
		t.Fatalf("content not aggregated as expected: %+v", aggregated.Choices)
	}
	if aggregated.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop (default when upstream omits it)", aggregated.Choices[0].FinishReason)
	}
	// When total_tokens is missing, it should be filled by prompt + completion.
	if aggregated.Usage.TotalTokens != 7 {
		t.Fatalf("total_tokens = %d, want 7 (derived)", aggregated.Usage.TotalTokens)
	}
}

// TestExecutorStreamEmitsNormalizedFrames verifies that stream forwarding normalizes frames before delivery.
func TestExecutorStreamEmitsNormalizedFrames(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			return fakeUpstreamResponse{Status: http.StatusOK, Body: strings.Join([]string{
				`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"A"}}],"session_id":"leak-me"}`,
				"",
				`data: {"choices":[{"index":0,"delta":{"content":"B"}}]}`,
				"",
				"data: [DONE]",
				"",
			}, "\n")}
		}
		// Global daily active reward: heatmap not lit -> report beacon -> reading back as 0 is still considered a success.
		if strings.Contains(url, "/activity/growth/heatmap") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"cells":[{"date":"2026-10-18","score":0}]}}`,
			}
		}
		if strings.Contains(url, "/v2/report") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	result := callMethod(t, pluginabi.MethodExecutorExecuteStream, app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Format:  app.FormatChatCompletions,
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		},
		StreamID: "s1",
	})
	var response app.ExecutorStreamResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode stream response: %v", errUnmarshal)
	}
	if response.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream content-type = %q", response.Headers.Get("Content-Type"))
	}

	if !waitFor(t, 3e9, func() bool { return len(host.emittedChunks()) >= 2 }) {
		t.Fatalf("expected at least 2 emitted chunks, got %v", host.emittedChunks())
	}
	chunks := host.emittedChunks()
	for _, chunk := range chunks {
		if strings.Contains(chunk, "session_id") {
			t.Fatalf("upstream private field leaked to downstream: %s", chunk)
		}
		if !json.Valid([]byte(chunk)) {
			// The host is responsible for the SSE envelope, while the executor only delivers raw JSON.
			t.Fatalf("chat-completions chunk must be bare JSON, got %q", chunk)
		}
	}
	// The second frame lacks an id, which should be inherited as the first frame's id.
	if !strings.Contains(chunks[1], `"id":"c1"`) {
		t.Fatalf("missing id must be backfilled from the first frame: %s", chunks[1])
	}
	// The stream must end with a close.
	if !waitFor(t, 3e9, func() bool { return len(host.closedStreams()) == 1 }) {
		t.Fatalf("expected exactly one stream close, got %d", len(host.closedStreams()))
	}
	if len(host.streamErrors()) != 0 {
		t.Fatalf("successful stream must not report an error: %v", host.streamErrors())
	}
}

// TestExecutorStreamReportsUpstreamError verifies that upstream errors are reported as stream failures.
func TestExecutorStreamReportsUpstreamError(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			return fakeUpstreamResponse{
				Status: http.StatusTooManyRequests,
				Body:   `{"code":6004,"msg":"The model provider is rate-limiting requests."}`,
			}
		}
		// Global daily active reward: heatmap not lit -> report beacon -> reading back as 0 is still considered a success.
		if strings.Contains(url, "/activity/growth/heatmap") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"cells":[{"date":"2026-10-18","score":0}]}}`,
			}
		}
		if strings.Contains(url, "/v2/report") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	callMethod(t, pluginabi.MethodExecutorExecuteStream, app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[]}`),
		},
		StreamID: "s1",
	})

	if !waitFor(t, 3e9, func() bool { return len(host.streamErrors()) > 0 }) {
		t.Fatal("upstream 429 must surface as a stream failure")
	}
	if !strings.Contains(host.streamErrors()[0], "rate-limiting") {
		t.Fatalf("stream error should carry the upstream message verbatim: %v", host.streamErrors())
	}
}

// TestExecutorRejectsUnsupportedFormat verifies explicit error reporting on format declaration mismatch.
func TestExecutorRejectsUnsupportedFormat(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, errHandle := testApp.HandleMethod(pluginabi.MethodExecutorExecute, mustMarshal(app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "1", Model: "cn:x", Format: "gemini"},
	}))
	if errHandle == nil {
		t.Fatalf("unsupported format must produce an error envelope, got %s", raw)
	}

	_ = workbuddy.RegionCN
	_ = http.StatusOK
}

// TestFormatSSEChunk verifies that streaming chunk formatting ensures compliance with the SSE standard specification (starting with data: and ending with \n\n).
func TestFormatSSEChunk(t *testing.T) {
	// 1. Raw JSON chunks automatically complete data: and \n\n
	out := app.FormatSSEChunk([]byte(`{"id":"1"}`))
	if string(out) != "data: {\"id\":\"1\"}\n\n" {
		t.Errorf("unexpected format: %q", string(out))
	}

	// 2. Chunks with an existing data: prefix ensure \n\n at the end
	out = app.FormatSSEChunk([]byte("data: {\"id\":\"2\"}\n\n"))
	if string(out) != "data: {\"id\":\"2\"}\n\n" {
		t.Errorf("unexpected format: %q", string(out))
	}

	// 3. Raw [DONE] markers are completed as data: [DONE]\n\n
	out = app.FormatSSEChunk([]byte("[DONE]"))
	if string(out) != "data: [DONE]\n\n" {
		t.Errorf("unexpected format: %q", string(out))
	}

	// 4. Empty chunks return nil
	if out = app.FormatSSEChunk(nil); out != nil {
		t.Errorf("expected nil for empty payload, got %q", string(out))
	}
	if out = app.FormatSSEChunk([]byte("   \n\r  ")); out != nil {
		t.Errorf("expected nil for whitespace payload, got %q", string(out))
	}
}
