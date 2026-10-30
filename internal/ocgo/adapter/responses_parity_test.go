// Package adapters hosts cross-route tests that compare the synthesized
// wire bytes of sibling protocol adapters against each other (FR-006).
package adapters

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"free-model-hub/internal/ocgo/adapter/chatcompletions"
	"free-model-hub/internal/ocgo/adapter/messages"
	"free-model-hub/internal/ocgo/adapter/responses"
	"free-model-hub/internal/ocgo/errclass"
)

type feeder interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
}

// F5 route parity: one logical turn — a single user message whose content
// is "" — must receive the IDENTICAL empty-input policy on every
// cross-route normalizer: DROP it. Divergence pin: the Responses target
// used to emit a live empty input_text part while the Messages and Chat
// Completions targets dropped the whole message.
func TestEmptyStringContentParityAcrossTargets(t *testing.T) {
	ccBody := []byte(`{"model":"x","messages":[{"role":"user","content":""}]}`)
	respBody := []byte(`{"model":"x","input":[{"type":"message","role":"user","content":""}]}`)

	checkDropped := func(t *testing.T, out []byte, field string) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("output not json: %v", err)
		}
		switch v := m[field].(type) {
		case nil:
		case []any:
			if len(v) != 0 {
				t.Fatalf("empty-content message must be dropped uniformly, %s = %v", field, v)
			}
		default:
			t.Fatalf("%s has unexpected shape %T", field, v)
		}
	}

	t.Run("responses target", func(t *testing.T) {
		out, eErr := responses.BuildRequest("m", "openai", ccBody, nil)
		if eErr != nil {
			t.Fatalf("unexpected error: %v", eErr)
		}
		checkDropped(t, out, "input")
	})
	t.Run("messages target", func(t *testing.T) {
		out, eErr := messages.BuildRequest("m", "openai", ccBody, nil)
		if eErr != nil {
			t.Fatalf("unexpected error: %v", eErr)
		}
		checkDropped(t, out, "messages")
	})
	t.Run("chatcompletions target", func(t *testing.T) {
		out, eErr := chatcompletions.BuildRequest("m", "openai-response", respBody, nil)
		if eErr != nil {
			t.Fatalf("unexpected error: %v", eErr)
		}
		checkDropped(t, out, "messages")
	})
}

// FR-006 route parity: equivalent upstream streams — Anthropic Messages
// and Chat Completions — must synthesize byte-equal openai-response event
// sequences through the shared ResponsesEventEmitter kernel. Only
// created_at (render-time fallback, never observed upstream) is
// normalized before the byte comparison.
func TestResponsesSynthesisRouteParity(t *testing.T) {
	claudeUp := []string{
		"event: message_start\n" + `data: {"type":"message_start","message":{"id":"resp_1","model":"m1","usage":{"input_tokens":2}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"index":0,"content_block":{"type":"text"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"index":0,"delta":{"type":"text_delta","text":"He"}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"index":1,"content_block":{"type":"tool_use","id":"call_1","name":"f"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}
	ccUp := []string{
		"data: {\"id\":\"resp_1\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"He\"}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n",
		"data: [DONE]\n",
	}

	run := func(t *testing.T, f feeder, chunks []string) string {
		t.Helper()
		var b strings.Builder
		for _, c := range chunks {
			evs, done, eErr := f.Feed([]byte(c))
			if eErr != nil {
				t.Fatalf("feed %q: %v", c, eErr)
			}
			for _, ev := range evs {
				b.Write(ev)
			}
			if done {
				break
			}
		}
		return b.String()
	}

	fromClaude := run(t, messages.NewStreamConverter("openai-response"), claudeUp)
	fromCC := run(t, chatcompletions.NewStreamConverter("openai-response"), ccUp)

	norm := func(s string) string {
		return regexp.MustCompile(`"created_at":\d+`).ReplaceAllString(s, `"created_at":0`)
	}
	a, b := norm(fromClaude), norm(fromCC)
	if a != b {
		t.Fatalf("routes diverge:\nclaude-source: %s\ncc-source:     %s", a, b)
	}
	for _, want := range []string{
		`response.created`, `response.output_item.added`, `response.output_text.delta`,
		`response.function_call_arguments.delta`, `response.function_call_arguments.done`,
		`response.output_item.done`, `"model":"m1"`, `"status":"completed"`,
		`"total_tokens":5`,
	} {
		if !strings.Contains(a, want) {
			t.Errorf("synthesized stream missing %s", want)
		}
	}
	if strings.Count(a, "data: ") != 10 {
		t.Errorf("event count = %d, want 10 (created, 2 added, text delta, 2 args deltas, args done, 2 item dones, completed)",
			strings.Count(a, "data: "))
	}
}

// orderedEvent is one parsed synthesized SSE frame in emission order.
type orderedEvent struct {
	name string
	data map[string]any
}

// feedOrderedEvents feeds chunks and returns the synthesized frames in order.
func feedOrderedEvents(t *testing.T, f feeder, chunks []string) []orderedEvent {
	t.Helper()
	var raw strings.Builder
	for _, c := range chunks {
		evs, done, eErr := f.Feed([]byte(c))
		if eErr != nil {
			t.Fatalf("feed %q: %v", c, eErr)
		}
		for _, ev := range evs {
			raw.Write(ev)
		}
		if done {
			break
		}
	}
	var out []orderedEvent
	for _, block := range strings.Split(raw.String(), "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("bad frame payload %q: %v", data, err)
		}
		out = append(out, orderedEvent{name: name, data: m})
	}
	return out
}

// Regression pin: a synthesized Responses stream must close the tool-call
// lifecycle — response.function_call_arguments.done plus
// response.output_item.done per announced function_call — BEFORE the
// terminal response.completed. Without the item-done frame a client cannot
// know the arguments are complete and must reject the whole turn (Pi
// reported an unfinished tool call for DeepSeek routes served by the Chat
// Completions adapter).
func TestResponsesToolLifecycleClosureParity(t *testing.T) {
	claudeUp := []string{
		"event: message_start\n" + `data: {"type":"message_start","message":{"id":"resp_1","model":"m1","usage":{"input_tokens":2}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"index":0,"content_block":{"type":"text"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"index":0,"delta":{"type":"text_delta","text":"He"}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"index":1,"content_block":{"type":"tool_use","id":"call_1","name":"f"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}
	ccUp := []string{
		"data: {\"id\":\"resp_1\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"He\"}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n",
		"data: [DONE]\n",
	}
	cases := []struct {
		name   string
		f      feeder
		chunks []string
	}{
		{"claude-source", messages.NewStreamConverter("openai-response"), claudeUp},
		{"chat-completions-source", chatcompletions.NewStreamConverter("openai-response"), ccUp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := feedOrderedEvents(t, tc.f, tc.chunks)
			announced := map[int]string{}
			closedAt := map[int]int{}
			argsDoneAt := map[int]int{}
			argsDoneValue := map[int]string{}
			completedAt := -1
			for i, ev := range events {
				switch ev.name {
				case "response.output_item.added":
					item, _ := ev.data["item"].(map[string]any)
					announced[int(ev.data["output_index"].(float64))], _ = item["type"].(string)
				case "response.function_call_arguments.done":
					idx := int(ev.data["output_index"].(float64))
					argsDoneAt[idx] = i
					argsDoneValue[idx], _ = ev.data["arguments"].(string)
				case "response.output_item.done":
					closedAt[int(ev.data["output_index"].(float64))] = i
				case "response.completed":
					if completedAt >= 0 {
						t.Fatalf("duplicate response.completed at %d and %d", completedAt, i)
					}
					completedAt = i
				}
			}
			if completedAt < 0 {
				t.Fatal("missing response.completed")
			}
			if completedAt != len(events)-1 {
				t.Fatalf("response.completed must terminate the stream, got event %d of %d", completedAt, len(events))
			}
			if len(announced) == 0 {
				t.Fatal("no output items announced")
			}
			for index, kind := range announced {
				closeAt, closed := closedAt[index]
				if !closed {
					t.Errorf("item %d (%s) never closed by output_item.done before completion", index, kind)
					continue
				}
				if kind != "function_call" {
					continue
				}
				argsAt, ok := argsDoneAt[index]
				if !ok || argsAt > closeAt {
					t.Errorf("function_call %d missing function_call_arguments.done before output_item.done", index)
					continue
				}
				if want := `{"a":1}`; argsDoneValue[index] != want {
					t.Errorf("function_call_arguments.done arguments = %q, want %q", argsDoneValue[index], want)
				}
			}
		})
	}
}
