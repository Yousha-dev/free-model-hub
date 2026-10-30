package tests

// This file uses a fake host to drive the complete Chat / ChatStream pipeline:
// constructing upstream SSE frames (echo, delta, full message, error), and asserting that the downstream
// receives the OpenAI response format. No real network requests are made, so regular test pipelines can run.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/vendors/minimaxcode"
	"free-model-hub/tests/testhost"
)

const (
	toolCallStart = "<minimax-tool-call>"
	toolCallEnd   = "</minimax-tool-call>"
)

// installFakeUpstream installs a fake host: session creation returns a fixed ID,
// and the message call returns the given SSE text.
func installFakeUpstream(t *testing.T, sseBody string) {
	t.Helper()
	host := testhost.New()
	host.SetUpstream(func(method, url, body string) testhost.UpstreamResponse {
		switch {
		case strings.Contains(url, "/message"):
			return testhost.UpstreamResponse{
				Status: 200,
				Header: map[string][]string{"Content-Type": {"text/event-stream"}},
				Body:   sseBody,
			}
		case strings.Contains(url, "/session"):
			return testhost.UpstreamResponse{
				Status: 200,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   `{"data":{"session_id":"sess-1"}}`,
			}
		}
		return testhost.UpstreamResponse{Status: 404, Body: `{"code":404,"msg":"not stubbed"}`}
	})
	hostbridge.Configure(host.Call)
	t.Cleanup(func() {
		hostbridge.Configure(nil)
	})
}

func fakeLiveCredential() *minimaxcode.Credential {
	return &minimaxcode.Credential{
		Region:      minimaxcode.RegionCN,
		AccessToken: "test-token",
		UserID:      "123",
		AgentID:     "agent-1",
		AuthMode:    "token",
	}
}

func sseFrame(v any) string {
	encoded, _ := json.Marshal(v)
	return "data: " + string(encoded) + "\n\n"
}

// buildToolConversationSSE constructs an upstream stream with tool calls:
// echo frame -> delta frame (tag block split apart) -> full message frame (content duplicated with delta).
func buildToolConversationSSE() string {
	var b strings.Builder
	b.WriteString(sseFrame(map[string]any{"type": 10}))
	b.WriteString(sseFrame(map[string]any{
		"type": "query_collapse_view",
		"query_collapse_view": map[string]any{
			"current_turn_id": "t1", "force_expanded": false,
		},
	}))
	// Echo frame: role=user, content is the original prompt.
	b.WriteString(sseFrame(map[string]any{
		"type": 2,
		"agent_message": map[string]any{
			"msg_id": "m1", "role": "user", "msg_type": 1,
			"msg_content": "[系统指令] secret system prompt\n\n 用户：1. 查天气",
			"source":      "api",
		},
	}))
	assistantText := "我来查询天气。\n" + toolCallStart + "\n" +
		`{"name":"get_weather","arguments":{"city":"北京"}}` + "\n" + toolCallEnd
	// Deltas are split at arbitrary boundaries, covering tags spanning across chunks.
	runes := []rune(assistantText)
	cutPoints := []int{5, 20, 33, len(runes)} // 刻意切在标记中间
	prev := 0
	for _, cut := range cutPoints {
		b.WriteString(sseFrame(map[string]any{
			"type": 6,
			"agent_message_chunk": map[string]any{
				"msg_id": "m2", "role": "assistant", "chunk_index": 0,
				"msg_content": string(runes[prev:cut]),
			},
		}))
		prev = cut
	}
	// Full message frame: full text duplicated with delta (should be suppressed by delta priority).
	b.WriteString(sseFrame(map[string]any{
		"type": 2,
		"agent_message": map[string]any{
			"msg_id": "m2", "role": "assistant", "msg_type": 1,
			"msg_content": assistantText,
		},
	}))
	b.WriteString(sseFrame(map[string]any{
		"type": "query_collapse_view",
		"query_collapse_view": map[string]any{
			"current_turn_id": "t1", "force_expanded": true,
		},
	}))
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func toolChatPayload(t *testing.T) []byte {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{
		"model": "minimax-m3.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "secret system prompt"},
			map[string]any{"role": "user", "content": "查天气"},
		},
		"stream": true,
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "查天气",
				"parameters":  map[string]any{"type": "object"},
			},
		}},
	})
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	return payload
}

func TestChatEndToEndToolCall(t *testing.T) {
	installFakeUpstream(t, buildToolConversationSSE())
	resp, errChat := minimaxcode.Chat(context.Background(), fakeLiveCredential(),
		&core.ExecuteRequest{Payload: toolChatPayload(t)}, "")
	if errChat != nil {
		t.Fatalf("Chat: %v", errChat)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal(resp, &decoded); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	if len(decoded.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(decoded.Choices))
	}
	choice := decoded.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason want tool_calls, got %q", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("want 1 tool_call, got %d", len(choice.Message.ToolCalls))
	}
	call := choice.Message.ToolCalls[0]
	if call.Function.Name != "get_weather" {
		t.Fatalf("tool name: %q", call.Function.Name)
	}
	if !strings.Contains(call.Function.Arguments, "北京") {
		t.Fatalf("arguments: %q", call.Function.Arguments)
	}
	if call.ID == "" {
		t.Fatalf("tool call id must be generated")
	}
	if choice.Message.Content != nil {
		if strings.Contains(*choice.Message.Content, "secret system prompt") {
			t.Fatalf("echo leaked into content: %q", *choice.Message.Content)
		}
		if strings.Contains(*choice.Message.Content, toolCallStart) {
			t.Fatalf("marker leaked into content: %q", *choice.Message.Content)
		}
	}
}

func TestChatStreamEndToEndToolCall(t *testing.T) {
	installFakeUpstream(t, buildToolConversationSSE())
	var deltas []map[string]any
	errStream := minimaxcode.ChatStream(context.Background(), fakeLiveCredential(),
		&core.ExecuteRequest{Payload: toolChatPayload(t)}, "",
		func(chunk []byte) error {
			var frame struct {
				Choices []struct {
					Delta        map[string]any `json:"delta"`
					FinishReason *string        `json:"finish_reason"`
				} `json:"choices"`
			}
			if errUnmarshal := json.Unmarshal(chunk, &frame); errUnmarshal != nil {
				return errUnmarshal
			}
			if len(frame.Choices) > 0 {
				deltas = append(deltas, map[string]any{
					"delta":  frame.Choices[0].Delta,
					"finish": frame.Choices[0].FinishReason,
				})
			}
			return nil
		})
	if errStream != nil {
		t.Fatalf("ChatStream: %v", errStream)
	}

	var content strings.Builder
	var toolCalls []any
	finish := ""
	for _, d := range deltas {
		delta, _ := d["delta"].(map[string]any)
		if text, ok := delta["content"].(string); ok {
			content.WriteString(text)
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			toolCalls = append(toolCalls, calls...)
		}
		if fr, _ := d["finish"].(*string); fr != nil {
			finish = *fr
		}
	}

	gotContent := content.String()
	if strings.Contains(gotContent, "secret system prompt") {
		t.Fatalf("echo leaked into stream content: %q", gotContent)
	}
	if strings.Contains(gotContent, toolCallStart) || strings.Contains(gotContent, "tool_call") {
		t.Fatalf("marker leaked into stream content: %q", gotContent)
	}
	if !strings.HasPrefix(gotContent, "我来查询天气") {
		t.Fatalf("content missing prefix: %q", gotContent)
	}
	if strings.Count(gotContent, "我来查询天气") != 1 {
		t.Fatalf("content must not be duplicated by full-message frame: %q", gotContent)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("want 1 tool call delta, got %d", len(toolCalls))
	}
	call, _ := toolCalls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn == nil || fn["name"] != "get_weather" {
		t.Fatalf("tool call delta: %+v", toolCalls[0])
	}
	if finish != "tool_calls" {
		t.Fatalf("final finish_reason: %q", finish)
	}
}

func TestChatEndToEndBalanceError(t *testing.T) {
	var b strings.Builder
	b.WriteString(sseFrame(map[string]any{
		"type": 2,
		"agent_message": map[string]any{
			"msg_id": "m1", "role": "user", "msg_type": 1,
			"msg_content": "[系统指令] secret system prompt", "source": "api",
		},
	}))
	b.WriteString(sseFrame(map[string]any{
		"type": 6,
		"agent_message_chunk": map[string]any{
			"msg_id": "m2", "role": "assistant", "finish": true, "finish_reason": "error",
		},
	}))
	b.WriteString(sseFrame(map[string]any{
		"type": 2,
		"agent_message": map[string]any{
			"msg_id": "m2", "role": "assistant", "msg_type": 1,
			"msg_content": "50110:insufficient balance (1008)", "finish_reason": "error",
		},
	}))
	b.WriteString("data: [DONE]\n\n")
	installFakeUpstream(t, b.String())

	_, errChat := minimaxcode.Chat(context.Background(), fakeLiveCredential(),
		&core.ExecuteRequest{Payload: toolChatPayload(t)}, "")
	if errChat == nil {
		t.Fatalf("want error, got nil")
	}
	if !strings.Contains(errChat.Error(), "insufficient balance") {
		t.Fatalf("error must carry upstream message: %v", errChat)
	}
	if strings.Contains(errChat.Error(), "secret system prompt") {
		t.Fatalf("echo must not leak into error: %v", errChat)
	}
}

func TestChatStreamEndToEndPlainMessageFallback(t *testing.T) {
	// No chunk deltas, only full message (role=assistant) and echo - body is taken from the full message at once.
	var b strings.Builder
	b.WriteString(sseFrame(map[string]any{
		"type": 2,
		"agent_message": map[string]any{
			"msg_id": "m1", "role": "user", "msg_type": 1,
			"msg_content": "这是被回显的用户输入", "source": "api",
		},
	}))
	b.WriteString(sseFrame(map[string]any{
		"type": 2,
		"agent_message": map[string]any{
			"msg_id": "m2", "role": "assistant", "msg_type": 1,
			"msg_content": "这是模型的最终回答",
		},
	}))
	b.WriteString("data: [DONE]\n\n")
	installFakeUpstream(t, b.String())

	payload, _ := json.Marshal(map[string]any{
		"model":    "minimax-m3.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	var content strings.Builder
	errStream := minimaxcode.ChatStream(context.Background(), fakeLiveCredential(),
		&core.ExecuteRequest{Payload: payload}, "",
		func(chunk []byte) error {
			var frame struct {
				Choices []struct {
					Delta map[string]any `json:"delta"`
				} `json:"choices"`
			}
			if errUnmarshal := json.Unmarshal(chunk, &frame); errUnmarshal != nil {
				return errUnmarshal
			}
			if len(frame.Choices) > 0 {
				if text, ok := frame.Choices[0].Delta["content"].(string); ok {
					content.WriteString(text)
				}
			}
			return nil
		})
	if errStream != nil {
		t.Fatalf("ChatStream: %v", errStream)
	}
	got := content.String()
	if got != "这是模型的最终回答" {
		t.Fatalf("content want final answer only, got %q", got)
	}
}
