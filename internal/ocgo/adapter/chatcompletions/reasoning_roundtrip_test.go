package chatcompletions

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// OpenCode can stream reasoning rather than reasoning_content for the same
// DeepSeek model. Exercise the response -> Responses history -> Chat request
// seam: both the assistant text and its parallel tool calls need the replay.
func TestReasoningFieldRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		want   string
		absent bool
	}{
		{"canonical", `"reasoning_content":"inspect both files"`, "inspect both files", false},
		{"alternate", `"reasoning":"inspect both files"`, "inspect both files", false},
		{"canonical-wins", `"reasoning_content":"canonical","reasoning":"alternate"`, "canonical", false},
		{"empty-canonical", `"reasoning_content":"","reasoning":"alternate"`, "alternate", false},
		{"null-canonical", `"reasoning_content":null,"reasoning":"alternate"`, "alternate", false},
		{"whitespace", `"reasoning":"  plan\n"`, "  plan\n", false},
		{"canonical-only-empty", `"reasoning_content":""`, "", false},
		{"alternate-only-empty", `"reasoning":""`, "", false},
		{"null", `"reasoning_content":null,"reasoning":null`, "", true},
		{"canonical-alternate-empty", `"reasoning_content":"plan","reasoning":""`, "plan", false},
		{"empty", `"reasoning_content":"","reasoning":""`, "", false},
		{"absent", `"role":"assistant"`, "", true},
	}
	for _, tc := range cases {
		for _, streaming := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				output := reasoningRoundTripOutput(t, tc.fields, tc.want, streaming)
				reasoningItems := 0
				for i, raw := range output {
					item := raw.(map[string]any)
					if item["type"] != "reasoning" {
						continue
					}
					reasoningItems++
					if i != 0 {
						t.Fatal("reasoning must precede text and tool calls")
					}
					parts := item["content"].([]any)
					if len(parts) != 1 || parts[0].(map[string]any)["text"] != tc.want {
						t.Fatalf("reasoning item content = %v, want %q", parts, tc.want)
					}
				}
				wantReasoningItems := 0
				if !tc.absent {
					wantReasoningItems = 1
				}
				if reasoningItems != wantReasoningItems {
					t.Fatalf("reasoning items = %d, want %d", reasoningItems, wantReasoningItems)
				}

				input := []any{map[string]any{"role": "user", "content": "echo test"}}
				input = append(input, output...)
				for _, id := range []string{"call_a", "call_b"} {
					input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": "ok"})
				}
				body, err := json.Marshal(map[string]any{"input": input})
				if err != nil {
					t.Fatal(err)
				}
				upstream, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
				if eErr != nil {
					t.Fatal(eErr)
				}
				var request struct {
					Messages []struct {
						Role             string
						ReasoningContent *string               `json:"reasoning_content"`
						ToolCalls        []struct{ ID string } `json:"tool_calls"`
					}
				}
				if err := json.Unmarshal(upstream, &request); err != nil {
					t.Fatal(err)
				}
				assistants := 0
				var toolIDs []string
				for _, message := range request.Messages {
					if message.Role != "assistant" {
						if message.ReasoningContent != nil {
							t.Fatalf("reasoning leaked to %s", message.Role)
						}
						continue
					}
					assistants++
					if tc.absent {
						if message.ReasoningContent != nil {
							t.Fatal("empty reasoning must not invent a replay field")
						}
					} else if message.ReasoningContent == nil || *message.ReasoningContent != tc.want {
						t.Fatalf("assistant reasoning_content = %v, want %q", message.ReasoningContent, tc.want)
					}
					for _, call := range message.ToolCalls {
						toolIDs = append(toolIDs, call.ID)
					}
				}
				if assistants != 2 || strings.Join(toolIDs, ",") != "call_a,call_b" {
					t.Fatalf("split tool turn changed: assistants=%d tool IDs=%v", assistants, toolIDs)
				}
			})
		}
	}
}

func reasoningRoundTripOutput(t *testing.T, fields, want string, streaming bool) []any {
	t.Helper()
	if streaming {
		sc := NewStreamConverter("openai-response")
		events := feedAll(t, sc,
			`data: {"id":"r1","model":"deepseek-v4.1-flash","choices":[{"delta":{"role":"assistant"}}]}`,
			fmt.Sprintf(`data: {"choices":[{"delta":{%s}}]}`, fields),
			`data: {"choices":[{"delta":{"content":"I will check now."}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"echo","arguments":"{}"}},{"index":1,"id":"call_b","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			`data: {"choices":[]}`)
		var output []any
		var deltaText strings.Builder
		for _, event := range events {
			if event.Name == "response.reasoning_text.delta" {
				deltaText.WriteString(event.Data["delta"].(string))
			}
			if event.Name == "response.completed" {
				output = event.Data["response"].(map[string]any)["output"].([]any)
			}
		}
		if got := deltaText.String(); got != want {
			t.Fatalf("streamed reasoning = %q, want %q", got, want)
		}
		if output == nil {
			t.Fatal("missing completed output")
		}
		return output
	}
	body := fmt.Sprintf(`{"id":"r1","model":"deepseek-v4.1-flash","choices":[{"message":{%s,"content":"I will check now.","tool_calls":[{"id":"call_a","type":"function","function":{"name":"echo","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, fields)
	converted, eErr := ConvertNonStreamResponse("openai-response", 200, []byte(body))
	if eErr != nil {
		t.Fatal(eErr)
	}
	var response struct{ Output []any }
	if err := json.Unmarshal(converted, &response); err != nil {
		t.Fatal(err)
	}
	return response.Output
}

func TestReasoningFieldNativePassthrough(t *testing.T) {
	body := `{"id":"r1","choices":[{"message":{"reasoning":"plan","content":"hi"}}]}`
	converted, eErr := ConvertNonStreamResponse("openai", 200, []byte(body))
	if eErr != nil || string(converted) != body {
		t.Fatalf("native non-stream response changed: %s, %v", converted, eErr)
	}
	chunk := `{"choices":[{"delta":{"reasoning":"plan"}}]}`
	events, _, eErr := NewStreamConverter("openai").Feed([]byte("data: " + chunk + "\n\n"))
	if eErr != nil || len(events) != 1 || string(events[0]) != chunk {
		t.Fatalf("native stream payload changed: %q, %v", events, eErr)
	}
}

func TestReasoningFieldStreamFragments(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	events := feedAll(t, sc,
		`data: {"id":"r1","choices":[{"delta":{"reasoning":"think "}}]}`,
		`data: {"choices":[{"delta":{"reasoning":"hard"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)
	var streamed strings.Builder
	var doneText string
	for _, event := range events {
		switch event.Name {
		case "response.reasoning_text.delta":
			streamed.WriteString(event.Data["delta"].(string))
		case "response.reasoning_text.done":
			doneText = event.Data["text"].(string)
		}
	}
	if streamed.String() != "think hard" || doneText != "think hard" {
		t.Fatalf("reasoning fragments lost: streamed=%q done=%q", streamed.String(), doneText)
	}
}

func TestReasoningPresenceStreamLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fragments []string
		want      string
		present   bool
		deltas    int
	}{
		{"empty-null-absent", []string{`"reasoning_content":""`, `"reasoning_content":null`, `"role":"assistant"`}, "", true, 1},
		{"repeated-empty", []string{`"reasoning":""`, `"reasoning_content":""`}, "", true, 2},
		{"empty-then-text", []string{`"reasoning":""`, `"reasoning":null`, `"reasoning":" plan"`, `"reasoning_content":" later"`}, " plan later", true, 3},
		{"text-null-empty", []string{`"reasoning_content":"plan"`, `"reasoning":null`, `"reasoning":""`}, "plan", true, 2},
		{"absent-null", []string{`"role":"assistant"`, `"reasoning":null`, `"reasoning_content":null`}, "", false, 0},
	} {
		for _, end := range []string{"done", "flush", "usage"} {
			t.Run(tc.name+"/"+end, func(t *testing.T) {
				sc := NewStreamConverter("openai-response")
				events := feedAll(t, sc, `data: {"id":"r1","choices":[{"delta":{}}]}`)
				for _, f := range tc.fragments {
					events = append(events, feedAll(t, sc, `data: {"choices":[{"delta":{`+f+`}}]}`)...)
				}
				events = append(events, feedAll(t, sc, `data: {"choices":[{"delta":{"content":"comment","tool_calls":[{"index":0,"id":"a","function":{"name":"echo","arguments":"{}"}},{"index":1,"id":"b","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)...)
				switch end {
				case "done":
					events = append(events, feedAll(t, sc, `data: [DONE]`)...)
				case "usage":
					events = append(events, feedAll(t, sc, `data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`, `data: [DONE]`)...)
				case "flush":
					events = append(events, parseEvents(t, sc.Flush())...)
				}
				if len(sc.Flush()) != 0 || len(sc.Flush()) != 0 {
					t.Fatal("duplicate flush")
				}
				counts := map[string]int{}
				added := map[int]map[string]any{}
				var completedItems []any
				closed := 0
				var output []any
				var deltas strings.Builder
				for _, e := range events {
					counts[e.Name]++
					switch e.Name {
					case "response.output_item.added":
						idx := int(e.Data["output_index"].(float64))
						if idx != len(added) {
							t.Fatal(e)
						}
						added[idx] = e.Data["item"].(map[string]any)
					case "response.reasoning_text.delta", "response.reasoning_text.done":
						if e.Data["item_id"] != "r1-rs" || e.Data["output_index"] != float64(0) {
							t.Fatal(e)
						}
						if e.Name == "response.reasoning_text.delta" {
							deltas.WriteString(e.Data["delta"].(string))
						} else if e.Data["text"] != tc.want {
							t.Fatal(e)
						}
					case "response.output_item.done":
						idx := int(e.Data["output_index"].(float64))
						item := e.Data["item"].(map[string]any)
						if idx != closed || item["type"] != added[idx]["type"] || item["id"] != added[idx]["id"] || item["call_id"] != added[idx]["call_id"] {
							t.Fatal(e)
						}
						completedItems = append(completedItems, item)
						closed++
					case "response.completed":
						resp := e.Data["response"].(map[string]any)
						output = resp["output"].([]any)
						if !reflect.DeepEqual(output, completedItems) {
							t.Fatalf("completed output differs from item-done payloads: %v / %v", output, completedItems)
						}
						if end == "usage" && resp["usage"].(map[string]any)["input_tokens"] != float64(7) {
							t.Fatal(e)
						}
					}
				}
				wantItems := 3
				wantReason := 0
				if tc.present {
					wantItems++
					wantReason = 1
					part := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)
					if text, ok := part["text"]; !ok || text != tc.want {
						t.Fatal(part)
					}
				}
				if counts["response.created"] != 1 || counts["response.output_item.added"] != wantItems || counts["response.function_call_arguments.done"] != 2 || counts["response.completed"] != 1 || closed != wantItems || counts["response.reasoning_text.done"] != wantReason || counts["response.reasoning_text.delta"] != tc.deltas || deltas.String() != tc.want {
					t.Fatal(counts, deltas.String())
				}
				b, _ := json.Marshal(map[string]any{"input": output})
				m := mustBuildModel(t, "deepseek-v4.1-flash", "openai-response", string(b), nil)
				for _, raw := range m["messages"].([]any) {
					got, ok := reasoningOf(raw.(map[string]any))
					if got != tc.want || ok != tc.present {
						t.Fatal(m)
					}
				}
			})
		}
	}
}

func TestReasoningPresenceNativePassthrough(t *testing.T) {
	for _, fields := range []string{`"reasoning":""`, `"reasoning_content":""`, `"reasoning":null,"reasoning_content":null`, `"reasoning":"plan","reasoning_content":"canonical"`} {
		body := `{"choices":[{"message":{` + fields + `}}]}`
		got, e := ConvertNonStreamResponse("openai", 200, []byte(body))
		if e != nil || string(got) != body {
			t.Fatal(string(got), e)
		}
		chunk := `{"choices":[{"delta":{` + fields + `}}]}`
		events, _, e := NewStreamConverter("openai").Feed([]byte("data: " + chunk + "\n"))
		if e != nil || len(events) != 1 || string(events[0]) != chunk {
			t.Fatal(events, e)
		}
		req := `{"model":"deepseek-v4.1-flash","messages":[{"role":"assistant",` + fields + `}]}`
		got, e = BuildRequest("deepseek-v4.1-flash", "openai", []byte(req), nil)
		if e != nil || string(got) != req {
			t.Fatal(string(got), e)
		}
	}
}

func TestReasoningPresenceRejectsNonStringUpstream(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		for _, value := range []string{`7`, `[]`, `{}`} {
			fields := fmt.Sprintf(`"%s":%s`, field, value)
			if _, e := ConvertNonStreamResponse("openai-response", 200, []byte(`{"choices":[{"message":{`+fields+`}}]}`)); e == nil {
				t.Fatal("non-string message accepted", fields)
			}
			if _, _, e := NewStreamConverter("openai-response").Feed([]byte(`data: {"choices":[{"delta":{` + fields + `}}]}` + "\n")); e == nil {
				t.Fatal("non-string delta accepted", fields)
			}
		}
	}
}
