// Package adapters hosts cross-route tests that compare the synthesized
// wire bytes of sibling protocol adapters against each other (FR-006).
package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"free-model-hub/internal/ocgo/adapter/chatcompletions"
	"free-model-hub/internal/ocgo/adapter/messages"
	"free-model-hub/internal/ocgo/adapter/responses"
	"free-model-hub/internal/ocgo/errclass"
)

// sourceRequestFixture is one pinned conversion input. Route names the target
// adapter, Format the inbound source protocol.
type sourceRequestFixture struct {
	Name   string
	Route  string // "chat" | "messages" | "responses"
	Format string
	Model  string
	Body   string
}

// buildFixture runs one fixture through its target adapter's public
// BuildRequest entry point.
func buildFixture(t *testing.T, f sourceRequestFixture) ([]byte, *errclass.Error) {
	t.Helper()
	switch f.Route {
	case "chat":
		return chatcompletions.BuildRequest(f.Model, f.Format, []byte(f.Body), nil)
	case "messages":
		return messages.BuildRequest(f.Model, f.Format, []byte(f.Body), nil)
	case "responses":
		return responses.BuildRequest(f.Model, f.Format, []byte(f.Body), nil)
	default:
		t.Fatalf("fixture %q has unknown route %q", f.Name, f.Route)
		return nil, nil
	}
}

// compatFixtures pins the six cross-format conversion legs, the three native
// passthrough controls, and the edge cases called out by the plan: missing/
// null/empty content, malformed input, several simultaneously-invalid fields
// (error precedence), unknown native fields and native input shapes the typed
// conversion cannot model, large integers, raw tool arguments, unchanged-body
// passthrough, tool-call-id marking, and reasoning replay around tool calls.
func compatFixtures() []sourceRequestFixture {
	return []sourceRequestFixture{
		// ---- Target: /v1/chat/completions ----
		{
			Name: "chat/claude/full", Route: "chat", Format: "claude", Model: "deepseek-v4-pro",
			Body: `{"model":"client","system":"be terse","max_tokens":64,"stop_sequences":["STOP"],` +
				`"thinking":{"type":"enabled","budget_tokens":4096},` +
				`"tool_choice":{"type":"tool","name":"f"},` +
				`"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{}}}],` +
				`"messages":[` +
				`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"url","url":"https://img"}},{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"res"}],"is_error":true}]},` +
				`{"role":"assistant","content":[{"type":"text","text":"ans"},{"type":"thinking","thinking":"chain"},{"type":"tool_use","id":"c2","name":"f","input":{"a":1}}]}` +
				`]}`,
		},
		{Name: "chat/claude/empty-content", Route: "chat", Format: "claude", Model: "glm-5.3", Body: `{"messages":[{"role":"user","content":""}]}`},
		{Name: "chat/claude/null-content", Route: "chat", Format: "claude", Model: "glm-5.3", Body: `{"messages":[{"role":"user","content":null},{"role":"assistant","content":"hi"}]}`},
		{Name: "chat/claude/malformed", Route: "chat", Format: "claude", Model: "glm-5.3", Body: `{"messages":`},
		{Name: "chat/claude/tool-result-image", Route: "chat", Format: "claude", Model: "glm-5.3", Body: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"image","source":{"type":"url","url":"https://img"}}]}]}]}`},
		{Name: "chat/claude/unknown-role", Route: "chat", Format: "claude", Model: "glm-5.3", Body: `{"messages":[{"role":"robot","content":"x"}]}`},
		{Name: "chat/claude/multi-invalid", Route: "chat", Format: "claude", Model: "glm-5.3", Body: `{"tool_choice":42,"messages":[{"role":"user","content":[{"type":"audio"}]}]}`},

		{
			Name: "chat/responses/full", Route: "chat", Format: "openai-response", Model: "deepseek-v4-pro",
			Body: `{"model":"client","instructions":"be brief","max_output_tokens":128,"parallel_tool_calls":false,` +
				`"reasoning":{"effort":"high"},"tool_choice":{"type":"function","name":"f"},` +
				`"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],` +
				`"input":[` +
				`{"type":"message","role":"system","content":[{"type":"input_text","text":"sys"}]},` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"https://img"}]},` +
				`{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}],"content":[{"type":"reasoning_text","text":"why"}]},` +
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ans"}]},` +
				`{"type":"function_call","call_id":"call_a","name":"f","arguments":"{\"a\":1}"},` +
				`{"type":"function_call_output","call_id":"call_a","output":"done"}` +
				`]}`,
		},
		{
			Name: "chat/responses/reasoning-merge", Route: "chat", Format: "openai-response", Model: "deepseek-v4-pro",
			Body: `{"input":[` +
				`{"type":"message","role":"user","content":"go"},` +
				`{"type":"reasoning","summary":[{"type":"summary_text","text":"r1"}]},` +
				`{"type":"message","role":"assistant","content":"first"},` +
				`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
				`{"type":"function_call","call_id":"c2","name":"f","arguments":"{}"},` +
				`{"type":"function_call_output","call_id":"c1","output":"ok"}` +
				`]}`,
		},
		{Name: "chat/responses/empty-input", Route: "chat", Format: "openai-response", Model: "glm-5.3", Body: `{"input":""}`},
		{Name: "chat/responses/null-input", Route: "chat", Format: "openai-response", Model: "glm-5.3", Body: `{"input":null}`},
		{Name: "chat/responses/malformed", Route: "chat", Format: "openai-response", Model: "glm-5.3", Body: `{`},
		{Name: "chat/responses/unknown-item", Route: "chat", Format: "openai-response", Model: "glm-5.3", Body: `{"input":[{"type":"web_search_call"}]}`},
		{Name: "chat/responses/multi-invalid", Route: "chat", Format: "openai-response", Model: "glm-5.3", Body: `{"tool_choice":42,"input":[{"type":"message","role":"user","content":[{"type":"audio"}]}]}`},

		{Name: "chat/openai/native-unchanged", Route: "chat", Format: "openai", Model: "glm-5.3", Body: `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`},
		{Name: "chat/openai/native-large-int", Route: "chat", Format: "openai", Model: "glm-5.3", Body: `{"model":"client","big":9007199254740993,"max_tokens":123456789012345678,"messages":[{"role":"user","content":"hi"}]}`},
		{Name: "chat/openai/native-developer-role", Route: "chat", Format: "openai", Model: "glm-5.3", Body: `{"model":"glm-5.3","messages":[{"role":"developer","content":"d"},{"role":"user","content":"hi"}]}`},
		{Name: "chat/openai/native-malformed", Route: "chat", Format: "openai", Model: "glm-5.3", Body: `{"messages":`},
		{Name: "chat/openai/native-null", Route: "chat", Format: "openai", Model: "glm-5.3", Body: `null`},
		{Name: "chat/grpc/unsupported", Route: "chat", Format: "grpc", Model: "glm-5.3", Body: `{}`},

		// ---- Target: /v1/responses ----
		{
			Name: "responses/openai/full", Route: "responses", Format: "openai", Model: "gpt-5.6-luna",
			Body: `{"model":"client","max_tokens":256,"reasoning_effort":"medium","parallel_tool_calls":true,"stop":["x"],` +
				`"tool_choice":{"type":"function","function":{"name":"f"}},` +
				`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],` +
				`"messages":[` +
				`{"role":"system","content":"sys"},` +
				`{"role":"developer","content":[{"type":"text","text":"dev"}]},` +
				`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://img"}}]},` +
				`{"role":"assistant","content":"ans","tool_calls":[{"id":"call_a","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},` +
				`{"role":"tool","tool_call_id":"call_a","content":"done"}` +
				`]}`,
		},
		{Name: "responses/openai/large-int", Route: "responses", Format: "openai", Model: "gpt-5.6-luna", Body: `{"max_completion_tokens":9007199254740993,"messages":[{"role":"user","content":"hi"}]}`},
		{Name: "responses/openai/empty-content", Route: "responses", Format: "openai", Model: "gpt-5.6-luna", Body: `{"messages":[{"role":"user","content":""},{"role":"assistant","content":null}]}`},
		{Name: "responses/openai/malformed", Route: "responses", Format: "openai", Model: "gpt-5.6-luna", Body: `{"messages":`},
		{Name: "responses/openai/multi-invalid", Route: "responses", Format: "openai", Model: "gpt-5.6-luna", Body: `{"tool_choice":42,"messages":[{"role":"robot","content":"x"}]}`},
		{Name: "responses/openai/unknown-role", Route: "responses", Format: "openai", Model: "gpt-5.6-luna", Body: `{"messages":[{"role":"robot","content":"x"}]}`},

		{
			Name: "responses/claude/full", Route: "responses", Format: "claude", Model: "gpt-5.6-luna",
			Body: `{"model":"client","system":"sys","max_tokens":32,"stop_sequences":["STOP"],` +
				`"thinking":{"type":"enabled","budget_tokens":2048},` +
				`"tools":[{"name":"f","input_schema":{"type":"object"}}],` +
				`"messages":[` +
				`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]},` +
				`{"role":"assistant","content":[{"type":"text","text":"ans"},{"type":"tool_use","id":"c1","name":"f","input":{"a":1}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}` +
				`]}`,
		},
		{Name: "responses/claude/empty", Route: "responses", Format: "claude", Model: "gpt-5.6-luna", Body: `{"messages":[{"role":"user","content":""}]}`},
		{Name: "responses/claude/malformed", Route: "responses", Format: "claude", Model: "gpt-5.6-luna", Body: `{"messages":`},
		{Name: "responses/claude/multi-invalid", Route: "responses", Format: "claude", Model: "gpt-5.6-luna", Body: `{"tool_choice":42,"messages":[{"role":"robot","content":"x"}]}`},

		{Name: "responses/openai-response/native-unchanged", Route: "responses", Format: "openai-response", Model: "gpt-5.6-luna", Body: `{"model":"gpt-5.6-luna","input":"hi"}`},
		{Name: "responses/openai-response/native-large-int", Route: "responses", Format: "openai-response", Model: "gpt-5.6-luna", Body: `{"model":"client","big":9007199254740993,"input":"hi"}`},
		{Name: "responses/openai-response/native-unknown-input", Route: "responses", Format: "openai-response", Model: "gpt-5.6-luna", Body: `{"model":"gpt-5.6-luna","input":["raw-string-entry"]}`},
		{Name: "responses/openai-response/native-tool-call-marks", Route: "responses", Format: "openai-response", Model: "gpt-5.6-luna", Body: `{"model":"gpt-5.6-luna","input":[{"type":"function_call","call_id":"call_a","name":"f","arguments":"{}"}]}`},
		{Name: "responses/openai-response/native-malformed", Route: "responses", Format: "openai-response", Model: "gpt-5.6-luna", Body: `{"input":`},
		{Name: "responses/grpc/unsupported", Route: "responses", Format: "grpc", Model: "gpt-5.6-luna", Body: `{}`},

		// ---- Target: /v1/messages ----
		{
			Name: "messages/openai/full", Route: "messages", Format: "openai", Model: "minimax-m3",
			Body: `{"model":"client","max_completion_tokens":64,"stop":"END","reasoning_effort":"low","parallel_tool_calls":false,` +
				`"tool_choice":"any","tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],` +
				`"messages":[` +
				`{"role":"system","content":"sys"},` +
				`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://img"}}]},` +
				`{"role":"assistant","content":"ans","tool_calls":[{"id":"call_a","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},` +
				`{"role":"tool","tool_call_id":"call_a","content":"done"}` +
				`]}`,
		},
		{Name: "messages/openai/raw-tool-args", Route: "messages", Format: "openai", Model: "minimax-m3", Body: `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"  "}},{"id":"c2","type":"function","function":{"name":"g","arguments":"[1,2]"}}]}]}`},
		{Name: "messages/openai/multi-invalid", Route: "messages", Format: "openai", Model: "minimax-m3", Body: `{"stop":42,"messages":[{"role":"robot","content":"x"}]}`},
		{Name: "messages/openai/malformed", Route: "messages", Format: "openai", Model: "minimax-m3", Body: `{"messages":`},
		{Name: "messages/openai/empty-content", Route: "messages", Format: "openai", Model: "minimax-m3", Body: `{"messages":[{"role":"user","content":""}]}`},

		{
			Name: "messages/responses/full", Route: "messages", Format: "openai-response", Model: "qwen3.7-max",
			Body: `{"instructions":"sys","max_output_tokens":48,"reasoning":{"effort":"high"},"parallel_tool_calls":false,` +
				`"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],` +
				`"input":[` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ans"}]},` +
				`{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]},` +
				`{"type":"function_call","call_id":"call_a","name":"f","arguments":"{\"a\":1}"},` +
				`{"type":"function_call_output","call_id":"call_a","output":"done"}` +
				`]}`,
		},
		{Name: "messages/responses/multi-invalid", Route: "messages", Format: "openai-response", Model: "qwen3.7-max", Body: `{"tool_choice":42,"input":[{"type":"web_search_call"}]}`},
		{Name: "messages/responses/malformed", Route: "messages", Format: "openai-response", Model: "qwen3.7-max", Body: `{"input":`},
		{Name: "messages/responses/none-tool-choice", Route: "messages", Format: "openai-response", Model: "qwen3.7-max", Body: `{"tool_choice":"none","input":"hi"}`},

		{Name: "messages/claude/native-unchanged", Route: "messages", Format: "claude", Model: "minimax-m3", Body: `{"model":"minimax-m3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{Name: "messages/claude/native-large-int", Route: "messages", Format: "claude", Model: "minimax-m3", Body: `{"model":"client","big":9007199254740993,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{Name: "messages/claude/native-malformed", Route: "messages", Format: "claude", Model: "minimax-m3", Body: `{"messages":`},
		{Name: "messages/grpc/unsupported", Route: "messages", Format: "grpc", Model: "minimax-m3", Body: `{}`},
	}
}

// compatGolden is the pinned result of one fixture: either the exact output
// bytes or the classified error (class + message).
type compatGolden struct {
	Output string `json:"output,omitempty"`
	Error  *struct {
		Class   string `json:"class"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func compatGoldenPath() string {
	return filepath.Join("testdata", "source_request_compat.json")
}

func loadCompatGoldens(t *testing.T) map[string]compatGolden {
	t.Helper()
	raw, err := os.ReadFile(compatGoldenPath())
	if err != nil {
		t.Fatalf("read goldens: %v", err)
	}
	goldens := map[string]compatGolden{}
	if err := json.Unmarshal(raw, &goldens); err != nil {
		t.Fatalf("decode goldens: %v", err)
	}
	return goldens
}

// TestSourceRequestCompatPinsConversionOutputs is the behavior-preservation
// net for the lazy SourceRequest refactor: every leg's exact output bytes and
// error class+message are pinned against testdata captured from the
// pre-refactor adapters. It must pass unchanged before and after the change.
func TestSourceRequestCompatPinsConversionOutputs(t *testing.T) {
	goldens := loadCompatGoldens(t)
	for _, f := range compatFixtures() {
		t.Run(f.Name, func(t *testing.T) {
			golden, ok := goldens[f.Name]
			if !ok {
				t.Fatalf("no golden for fixture %q; regenerate with UPDATE_COMPAT_GOLDENS=1", f.Name)
			}
			out, eErr := buildFixture(t, f)
			if golden.Error != nil {
				if eErr == nil {
					t.Fatalf("expected error %s, got output %s", golden.Error.Class, out)
				}
				if string(eErr.Class) != golden.Error.Class || eErr.Message != golden.Error.Message {
					t.Fatalf("error = %s: %q, want %s: %q", eErr.Class, eErr.Message, golden.Error.Class, golden.Error.Message)
				}
				return
			}
			if eErr != nil {
				t.Fatalf("unexpected error: %v", eErr)
			}
			if string(out) != golden.Output {
				t.Fatalf("output changed:\n got: %s\nwant: %s", out, golden.Output)
			}
		})
	}
}

// TestGenerateCompatGoldens rewrites the pinned file from the current
// adapters. Run it only to recapture after an intentional behavior change:
//
//	UPDATE_COMPAT_GOLDENS=1 go test ./internal/adapter/ -run TestGenerateCompatGoldens
func TestGenerateCompatGoldens(t *testing.T) {
	if os.Getenv("UPDATE_COMPAT_GOLDENS") == "" {
		t.Skip("set UPDATE_COMPAT_GOLDENS=1 to rewrite testdata/source_request_compat.json")
	}
	goldens := map[string]compatGolden{}
	for _, f := range compatFixtures() {
		out, eErr := buildFixture(t, f)
		if eErr != nil {
			goldens[f.Name] = compatGolden{Error: &struct {
				Class   string `json:"class"`
				Message string `json:"message"`
			}{Class: string(eErr.Class), Message: eErr.Message}}
			continue
		}
		goldens[f.Name] = compatGolden{Output: string(out)}
	}
	raw, err := json.MarshalIndent(goldens, "", "  ")
	if err != nil {
		t.Fatalf("marshal goldens: %v", err)
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(compatGoldenPath()), 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	if err := os.WriteFile(compatGoldenPath(), raw, 0o644); err != nil {
		t.Fatalf("write goldens: %v", err)
	}
}
