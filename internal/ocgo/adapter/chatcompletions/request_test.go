package chatcompletions

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/ocgo/adapter/shared"
	"free-model-hub/internal/ocgo/errclass"
)

func decodeOut(t *testing.T, out []byte, eErr *errclass.Error) map[string]any {
	t.Helper()
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	return m
}

func mustBuild(t *testing.T, sourceFormat, body string, ts *pluginapi.ThinkingSupport) map[string]any {
	t.Helper()
	out, eErr := BuildRequest("m", sourceFormat, []byte(body), ts)
	return decodeOut(t, out, eErr)
}

// mustBuildModel pins the upstream model when the assertion depends on the
// upstream family (reasoning replay is family-gated).
func mustBuildModel(t *testing.T, model, sourceFormat, body string, ts *pluginapi.ThinkingSupport) map[string]any {
	t.Helper()
	out, eErr := BuildRequest(model, sourceFormat, []byte(body), ts)
	return decodeOut(t, out, eErr)
}

func TestAuthHeaders(t *testing.T) {
	h := AuthHeaders("sk-test")
	if got := h.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildRequestUnsupportedFormat(t *testing.T) {
	_, eErr := BuildRequest("m", "grpc", []byte(`{}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("want ClassUnsupported, got %+v", eErr)
	}
}

func TestBuildRequestOpenAI(t *testing.T) {
	body := `{"model":"pub/glm","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":32}`
	m := mustBuild(t, "openai", body, nil)
	if m["model"] != "m" || m["stream"] != true || m["max_tokens"] != float64(32) {
		t.Fatalf("passthrough broken: %v", m)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages changed: %v", msgs)
	}
	if _, eErr := BuildRequest("m", "openai", []byte(`{`), nil); eErr == nil || eErr.Class != errclass.ClassTranslation {
		t.Fatalf("want ClassTranslation, got %+v", eErr)
	}
}

func TestBuildRequest_OpenAISanitizeThinking(t *testing.T) {
	t.Run("untyped thinking metadata stripped", func(t *testing.T) {
		body := `{"model":"other","thinking":{"levels":["low","high","max"]},"messages":[{"role":"user","content":"hi"}],"stream":true}`
		out, eErr := BuildRequest("m", "openai", []byte(body), nil)
		m := decodeOut(t, out, eErr)
		if _, ok := m["thinking"]; ok {
			t.Fatalf("thinking should be stripped: %v", m)
		}
		if m["model"] != "m" || m["stream"] != true {
			t.Fatalf("expected fields missing or wrong: %v", m)
		}
		msgs, ok := m["messages"].([]any)
		if !ok || len(msgs) != 1 {
			t.Fatalf("messages altered: %v", m["messages"])
		}

		bodySameModel := `{"model":"m","thinking":{"levels":["low","high","max"]},"stream":true}`
		outSame, eErrSame := BuildRequest("m", "openai", []byte(bodySameModel), nil)
		mSame := decodeOut(t, outSame, eErrSame)
		if _, ok := mSame["thinking"]; ok {
			t.Fatalf("thinking should be stripped even if model matches: %v", mSame)
		}
		if mSame["model"] != "m" || mSame["stream"] != true {
			t.Fatalf("fields corrupted: %v", mSame)
		}
	})

	t.Run("non-object or malformed thinking stripped", func(t *testing.T) {
		cases := []struct {
			name     string
			thinking string
		}{
			{"string fast", `"fast"`},
			{"number 123", `123`},
			{"array low", `["low"]`},
			{"boolean true", `true`},
			{"null value", `null`},
			{"type is number", `{"type": 123}`},
			{"type is null", `{"type": null}`},
			{"type is array", `{"type": ["enabled"]}`},
			{"type is empty string", `{"type": ""}`},
			{"type is whitespace", `{"type": "   "}`},
			{"empty object", `{}`},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"other","thinking":%s,"stream":true}`, tc.thinking)
				out, eErr := BuildRequest("m", "openai", []byte(body), nil)
				m := decodeOut(t, out, eErr)
				if _, ok := m["thinking"]; ok {
					t.Fatalf("%s: thinking should be stripped, got %v", tc.name, m["thinking"])
				}
				if m["model"] != "m" || m["stream"] != true {
					t.Fatalf("%s: fields altered: %v", tc.name, m)
				}
			})
		}
	})

	t.Run("valid thinking object preserved", func(t *testing.T) {
		cases := []struct {
			name     string
			model    string
			thinking string
		}{
			{"enabled with budget model rewrite", "old-model", `{"type":"enabled","budget_tokens":1024}`},
			{"enabled with budget same model", "m", `{"type":"enabled","budget_tokens":1024}`},
			{"disabled same model", "m", `{"type":"disabled"}`},
			{"disabled model rewrite", "old-model", `{"type":"disabled"}`},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				body := fmt.Sprintf(`{"model":%q,"thinking":%s,"stream":true}`, tc.model, tc.thinking)
				out, eErr := BuildRequest("m", "openai", []byte(body), nil)
				if eErr != nil {
					t.Fatalf("unexpected error: %v", eErr)
				}
				if tc.model == "m" && string(out) != body {
					t.Fatalf("expected raw body passthrough, got: %s", string(out))
				}
				m := decodeOut(t, out, eErr)
				if m["model"] != "m" {
					t.Fatalf("model = %v, want m", m["model"])
				}
				th, ok := m["thinking"].(map[string]any)
				if !ok {
					t.Fatalf("thinking missing or not object: %v", m["thinking"])
				}
				if strings.Contains(tc.name, "enabled") {
					if th["type"] != "enabled" || th["budget_tokens"] != float64(1024) {
						t.Fatalf("thinking object content corrupted: %v", th)
					}
				} else {
					if th["type"] != "disabled" {
						t.Fatalf("thinking object content corrupted: %v", th)
					}
				}
			})
		}
	})

	t.Run("reasoning_effort preserved without thinking", func(t *testing.T) {
		body := `{"model":"old-model","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`
		out, eErr := BuildRequest("m", "openai", []byte(body), nil)
		m := decodeOut(t, out, eErr)
		if m["model"] != "m" {
			t.Fatalf("model = %v, want m", m["model"])
		}
		if m["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %v, want high", m["reasoning_effort"])
		}
		if _, ok := m["thinking"]; ok {
			t.Fatalf("thinking should not be present: %v", m)
		}

		bodySame := `{"model":"m","reasoning_effort":"high"}`
		outSame, eErrSame := BuildRequest("m", "openai", []byte(bodySame), nil)
		if eErrSame != nil {
			t.Fatalf("unexpected error: %v", eErrSame)
		}
		if string(outSame) != bodySame {
			t.Fatalf("expected raw body passthrough, got: %s", string(outSame))
		}
	})

	t.Run("clean model rewrite without thinking", func(t *testing.T) {
		body := `{"model":"old-model","temperature":0.7}`
		out, eErr := BuildRequest("m", "openai", []byte(body), nil)
		m := decodeOut(t, out, eErr)
		if m["model"] != "m" || m["temperature"] != 0.7 {
			t.Fatalf("model rewrite failed: %v", m)
		}
		if _, ok := m["thinking"]; ok {
			t.Fatalf("thinking should not be present: %v", m)
		}
	})

	t.Run("nil or empty json handling", func(t *testing.T) {
		if _, eErr := BuildRequest("m", "openai", []byte(""), nil); eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Fatalf("empty slice: want ClassTranslation, got %+v", eErr)
		}

		outNull, eErrNull := BuildRequest("m", "openai", []byte("null"), nil)
		if eErrNull == nil || eErrNull.Class != errclass.ClassTranslation {
			t.Fatalf("null JSON: want ClassTranslation, got %+v", eErrNull)
		}
		if !strings.Contains(eErrNull.Message, "JSON null is not a valid request") {
			t.Fatalf("null JSON: unexpected error message: %q", eErrNull.Message)
		}
		if outNull != nil {
			t.Fatalf("null JSON: expected nil output, got %s", string(outNull))
		}

		for _, invalid := range []string{"123", `"string"`, `[]`, `true`} {
			if _, eErr := BuildRequest("m", "openai", []byte(invalid), nil); eErr == nil || eErr.Class != errclass.ClassTranslation {
				t.Fatalf("%s: want ClassTranslation, got %+v", invalid, eErr)
			}
		}
	})
}

// TestBuildRequestThinkingEffort proves budget→effort conversion is
// capability-aware (FR-005): tiers beyond the static low/medium/high
// buckets survive when the model declares them.
func TestBuildRequestThinkingEffort(t *testing.T) {
	// Budget→effort is the fixed host table only: later levels are derived
	// from thresholds, never clamped to the target's declared capability.
	cases := []struct {
		name   string
		ts     *pluginapi.ThinkingSupport
		budget int64
		want   string
	}{
		{"small budget anchors low", nil, 1024, "low"},
		{"mid budget anchors medium", nil, 8192, "medium"},
		{"huge budget anchors xhigh", nil, 131072, "xhigh"},
		{"declared levels do not clamp the derivation", &pluginapi.ThinkingSupport{
			Levels: []string{"low", "medium", "high"}, Max: 64000,
		}, 60000, "xhigh"},
		{"off state renders none", &pluginapi.ThinkingSupport{
			ZeroAllowed: true, Levels: []string{"low", "high"},
		}, 0, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"model":"x","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":%d}}`,
				tc.budget)
			out, eErr := BuildRequest("m", "claude", []byte(body), tc.ts)
			m := decodeOut(t, out, eErr)
			if m["reasoning_effort"] != tc.want {
				t.Fatalf("reasoning_effort=%v want %q", m["reasoning_effort"], tc.want)
			}
		})
	}
	m := mustBuild(t, "claude", `{"max_tokens":64,"thinking":{"type":"disabled"}}`, nil)
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatalf("disabled thinking must not set reasoning_effort: %v", m)
	}
}

func TestBuildRequestClaude(t *testing.T) {
	body := `{
		"model":"x","max_tokens":256,"stream":true,"temperature":0.5,"top_p":0.9,
		"system":[{"type":"text","text":"sys"}],
		"stop_sequences":["END"],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"text","text":"hey"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"res"},{"type":"text","text":"more"}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","id":"t2","name":"g"}]}
		],
		"tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}]
	}`
	m := mustBuild(t, "claude", body, nil)
	if m["model"] != "m" || m["stream"] != true || m["max_tokens"] != float64(256) ||
		m["temperature"] != 0.5 || m["top_p"] != 0.9 {
		t.Fatalf("envelope fields wrong: %v", m)
	}
	if fmt.Sprint(m["stop"]) != "[END]" {
		t.Fatalf("stop wrong: %v", m["stop"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("want 6 messages, got %d: %v", len(msgs), msgs)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "sys" {
		t.Fatalf("system message wrong: %v", sys)
	}
	asst := msgs[2].(map[string]any)
	if asst["role"] != "assistant" || asst["content"] != "hey" {
		t.Fatalf("assistant message wrong: %v", asst)
	}
	calls := asst["tool_calls"].([]any)
	c0 := calls[0].(map[string]any)
	if c0["id"] != "t1" || c0["type"] != "function" {
		t.Fatalf("tool call wrong: %v", c0)
	}
	fn := c0["function"].(map[string]any)
	if fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("tool call function wrong: %v", fn)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "t1" || tool["content"] != "res" {
		t.Fatalf("tool result message wrong: %v", tool)
	}
	user := msgs[4].(map[string]any)
	if user["content"] != "more" {
		t.Fatalf("post-result text wrong: %v", user)
	}
	// thinking block dropped, tool_use kept: content null + tool_calls.
	last := msgs[5].(map[string]any)
	if last["role"] != "assistant" || last["content"] != nil {
		t.Fatalf("thinking-only content wrong: %v", last)
	}
	lastCall := last["tool_calls"].([]any)[0].(map[string]any)
	if lastCall["id"] != "t2" || lastCall["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("missing-input tool call wrong: %v", lastCall)
	}
	tools := m["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["type"] != "function" {
		t.Fatalf("tool type wrong: %v", tl)
	}
	tlf := tl["function"].(map[string]any)
	if tlf["name"] != "f" || tlf["description"] != "d" ||
		fmt.Sprint(tlf["parameters"]) != "map[type:object]" {
		t.Fatalf("tool function wrong: %v", tlf)
	}
}

func TestBuildRequestClaudeEmptyToolSchema(t *testing.T) {
	m := mustBuild(t, "claude", `{"tools":[{"name":"f"}],"messages":[{"role":"user","content":null}]}`, nil)
	fn := m["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fmt.Sprint(fn["parameters"]) != "map[properties:map[] type:object]" {
		t.Fatalf("schema default missing: %v", fn)
	}
	if msgs := m["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("null content must not produce messages: %v", msgs)
	}
}

// FR-005 drift fix (shared.ClaudeMaxTokens): an absent or non-positive
// max_tokens defaults to 4096 instead of silently dropping the cap —
// the same policy the Responses leg applies.
func TestBuildRequestClaudeMaxTokensDefault(t *testing.T) {
	for _, body := range []string{
		`{"messages":[]}`,
		`{"max_tokens":0,"messages":[]}`,
		`{"max_tokens":-5,"messages":[]}`,
	} {
		m := mustBuild(t, "claude", body, nil)
		if m["max_tokens"] != float64(4096) {
			t.Fatalf("%s: max_tokens = %v, want 4096", body, m["max_tokens"])
		}
	}
}

func TestBuildRequestClaudeErrors(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed json", `{`},
		{"malformed block", `{"messages":[{"role":"user","content":[42]}]}`},
		{"user content wrong shape", `{"messages":[{"role":"user","content":42}]}`},
		{"assistant content wrong shape", `{"messages":[{"role":"assistant","content":42}]}`},
		{"image missing source", `{"messages":[{"role":"user","content":[{"type":"image"}]}]}`},
		{"source not an object", `{"messages":[{"role":"user","content":[{"type":"image","source":"x"}]}]}`},
		{"url source missing url", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url"}}]}]}`},
		{"base64 missing data", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`},
		{"unknown source type", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"s3"}}]}]}`},
		{"result bad block type", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"video"}]}]}]}`},
		{"result malformed block", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[42]}]}]}`},
		{"result content wrong shape", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":42}]}]}`},
		{"assistant malformed block", `{"messages":[{"role":"assistant","content":[42]}]}`},
		{"system bad block type", `{"system":[{"type":"doc"}]}`},
		{"system wrong shape", `{"system":123}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "claude", []byte(tc.body), nil)
			if eErr == nil || eErr.Class != errclass.ClassTranslation {
				t.Fatalf("want ClassTranslation, got %+v", eErr)
			}
		})
	}
	// Unknown content-block types and unknown roles are
	// unsupported_protocol_or_parameter (FR-009 kernel), matching the
	// Responses route.
	for _, c := range []struct{ name, body, typ string }{
		{"bad user block type", `{"messages":[{"role":"user","content":[{"type":"video"}]}]}`, `"video"`},
		{"bad assistant block type", `{"messages":[{"role":"assistant","content":[{"type":"video"}]}]}`, `"video"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "claude", []byte(c.body), nil)
			if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
				!strings.Contains(eErr.Message, c.typ) || !strings.Contains(eErr.Message, EndpointPath) {
				t.Fatalf("want ClassUnsupported naming type+endpoint, got %+v", eErr)
			}
		})
	}
	_, eErr := BuildRequest("m", "claude", []byte(`{"messages":[{"role":"robot","content":"x"}]}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
		!strings.Contains(eErr.Message, `"robot"`) {
		t.Fatalf("unsupported role: want ClassUnsupported, got %+v", eErr)
	}
}

func TestBuildRequestClaudeImageURLs(t *testing.T) {
	m := mustBuild(t, "claude",
		`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image","source":{"type":"url","url":"https://x/i.png"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},{"type":"image","source":{"type":"base64","data":"QUJD"}}]}]}`, nil)
	parts := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 4 || parts[0].(map[string]any)["type"] != "text" {
		t.Fatalf("parts wrong: %v", parts)
	}
	if got := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]; got != "https://x/i.png" {
		t.Fatalf("plain url lost: %v", got)
	}
	if got := parts[2].(map[string]any)["image_url"].(map[string]any)["url"]; got != "data:image/png;base64,QUJD" {
		t.Fatalf("base64 not converted to data URL: %v", got)
	}
	if got := parts[3].(map[string]any)["image_url"].(map[string]any)["url"]; got != "data:application/octet-stream;base64,QUJD" {
		t.Fatalf("missing media_type not defaulted: %v", got)
	}
}

func TestBuildRequestClaudeVariants(t *testing.T) {
	t.Run("string system field", func(t *testing.T) {
		m := mustBuild(t, "claude", `{"system":"plain","messages":[{"role":"user","content":"hi"}]}`, nil)
		sys := m["messages"].([]any)[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "plain" {
			t.Fatalf("string system lost: %v", sys)
		}
	})
	t.Run("multi-block system joined with blank lines", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"system":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}],"messages":[{"role":"user","content":"hi"}]}`, nil)
		sys := m["messages"].([]any)[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "s1\n\ns2" {
			t.Fatalf("system join wrong: %v", sys)
		}
	})
	t.Run("string assistant content", func(t *testing.T) {
		m := mustBuild(t, "claude", `{"messages":[{"role":"assistant","content":"str"}]}`, nil)
		a := m["messages"].([]any)[0].(map[string]any)
		if a["role"] != "assistant" || a["content"] != "str" {
			t.Fatalf("assistant string content lost: %v", a)
		}
	})
	t.Run("thinking-only assistant dropped", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("thinking-only assistant not dropped: %v", msgs)
		}
	})
	// FR-005 omission policy parity: redacted_thinking is encrypted
	// reasoning metadata with no Chat Completions representable payload —
	// omitted like plain thinking, never a conversion failure (mirrors the
	// sibling Messages/Responses-route converters).
	t.Run("redacted_thinking omitted alongside thinking", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"redacted_thinking","data":"x"},{"type":"text","text":"answer"}]}]}`, nil)
		msgs := m["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("want one assistant message, got %d: %v", len(msgs), msgs)
		}
		a := msgs[0].(map[string]any)
		if a["content"] != "answer" {
			t.Fatalf("thinking blocks not omitted or text lost: %v", a)
		}
	})
	t.Run("redacted_thinking-only assistant dropped", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("redacted_thinking-only assistant not dropped: %v", msgs)
		}
	})
	t.Run("tool_result without content", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9"}]}]}`, nil)
		tool := m["messages"].([]any)[0].(map[string]any)
		if tool["role"] != "tool" || tool["tool_call_id"] != "t9" || tool["content"] != "" {
			t.Fatalf("contentless tool_result wrong: %v", tool)
		}
	})
	t.Run("tool_result block array content", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}]}`, nil)
		tool := m["messages"].([]any)[0].(map[string]any)
		if tool["content"] != "ab" {
			t.Fatalf("block array result not flattened: %v", tool)
		}
	})
	t.Run("tool_result is_error marker", func(t *testing.T) {
		// is_error must survive as the "[error] " prefix like the
		// Responses-route twin (responses/request.go), never silently lost.
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":"boom","is_error":true}]}]}`, nil)
		tool := m["messages"].([]any)[0].(map[string]any)
		if tool["content"] != "[error] boom" {
			t.Fatalf("is_error marker missing: %v", tool)
		}
	})
}

func TestBuildRequestResponses(t *testing.T) {
	body := `{
		"model":"x","max_output_tokens":99,"temperature":0.7,
		"instructions":"be nice",
		"input":[
			{"type":"message","role":"developer","content":"dev"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"q"},{"type":"input_image","image_url":{"url":"https://x/i.png"}}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"x\":1}"},
			{"type":"function_call","call_id":"c2","name":"g","arguments":""},
			{"type":"function_call_output","call_id":"c1","output":"ok"},
			{"type":"reasoning","summary":[]}
		],
		"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],
		"reasoning":{"effort":"HIGH"}
	}`
	m := mustBuild(t, "openai-response", body, nil)
	if m["max_tokens"] != float64(99) || m["reasoning_effort"] != "HIGH" || m["temperature"] != 0.7 {
		t.Fatalf("envelope fields wrong: %v", m)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("want 6 messages, got %d: %v", len(msgs), msgs)
	}
	if m0 := msgs[0].(map[string]any); m0["role"] != "system" || m0["content"] != "be nice" {
		t.Fatalf("instructions message wrong: %v", m0)
	}
	if m1 := msgs[1].(map[string]any); m1["role"] != "system" || m1["content"] != "dev" {
		t.Fatalf("developer item wrong: %v", m1)
	}
	user := msgs[2].(map[string]any)["content"].([]any)
	if len(user) != 2 || user[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("user parts wrong: %v", user)
	}
	if msgs[3].(map[string]any)["content"] != "a" {
		t.Fatalf("assistant item wrong: %v", msgs[3])
	}
	merged := msgs[4].(map[string]any)
	calls := merged["tool_calls"].([]any)
	if merged["role"] != "assistant" || len(calls) != 2 {
		t.Fatalf("consecutive function_calls not merged: %v", merged)
	}
	c1 := calls[0].(map[string]any)
	if c1["id"] != "c1" || c1["function"].(map[string]any)["arguments"] != `{"x":1}` {
		t.Fatalf("first call wrong: %v", c1)
	}
	if calls[1].(map[string]any)["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("empty arguments not defaulted: %v", calls[1])
	}
	outMsg := msgs[5].(map[string]any)
	if outMsg["role"] != "tool" || outMsg["tool_call_id"] != "c1" || outMsg["content"] != "ok" {
		t.Fatalf("function_call_output wrong: %v", outMsg)
	}
	tools := m["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["type"] != "function" || tl["function"].(map[string]any)["name"] != "f" {
		t.Fatalf("tool wrapping wrong: %v", tl)
	}
}

func TestBuildRequestResponsesVariants(t *testing.T) {
	t.Run("empty string input yields no messages", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"input":""}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("unexpected messages: %v", msgs)
		}
	})
	t.Run("no input and no instructions", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("unexpected messages: %v", msgs)
		}
	})
	t.Run("string input becomes user message", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"input":"hi"}`, nil)
		msgs := m["messages"].([]any)
		if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hi" {
			t.Fatalf("wrong: %v", msgs)
		}
	})
	t.Run("zero max_output_tokens ignored", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"max_output_tokens":0,"input":"hi"}`, nil)
		if _, ok := m["max_tokens"]; ok {
			t.Fatalf("zero max_output_tokens must be omitted: %v", m)
		}
	})
	t.Run("parallel_tool_calls false forwarded", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"input":"hi","parallel_tool_calls":false}`, nil)
		if v, ok := m["parallel_tool_calls"].(bool); !ok || v {
			t.Fatalf("false parallel_tool_calls not forwarded: %v", m)
		}
	})
	t.Run("absent parallel_tool_calls omitted", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"input":"hi"}`, nil)
		if _, ok := m["parallel_tool_calls"]; ok {
			t.Fatalf("absent parallel_tool_calls must be omitted: %v", m)
		}
	})
	t.Run("plain string image url", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/i.png"}]}]}`, nil)
		content := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if content[0].(map[string]any)["image_url"].(map[string]any)["url"] != "https://x/i.png" {
			t.Fatalf("plain-string image_url lost: %v", content)
		}
	})
	t.Run("null message content skipped", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"user","content":null},{"type":"message","role":"system","content":null}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("null content produced messages: %v", msgs)
		}
	})
	t.Run("empty string content skipped", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"user","content":""}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("empty content produced messages: %v", msgs)
		}
	})
	t.Run("empty parts array skipped", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"user","content":[]}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("empty parts produced messages: %v", msgs)
		}
	})
	t.Run("system text parts joined", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"a"},{"type":"output_text","text":"b"}]}]}`, nil)
		sys := m["messages"].([]any)[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "ab" {
			t.Fatalf("system parts wrong: %v", sys)
		}
	})
	t.Run("tool without parameters gets default schema", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"tools":[{"type":"function","name":"f"}],"input":"hi"}`, nil)
		fn := m["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
		if fmt.Sprint(fn["parameters"]) != "map[properties:map[] type:object]" {
			t.Fatalf("schema default missing: %v", fn)
		}
	})
}

func TestBuildRequestResponsesErrors(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed json", `{`},
		{"instructions not a string", `{"instructions":5}`},
		{"input wrong shape", `{"input":5}`},
		{"system with image part", `{"input":[{"type":"message","role":"system","content":[{"type":"input_image","image_url":{"url":"https://x"}}]}]}`},
		{"image missing url", `{"input":[{"type":"message","role":"user","content":[{"type":"input_image"}]}]}`},
		{"malformed content", `{"input":[{"type":"message","role":"user","content":{"bad":1}}]}`},
		{"developer content wrong shape", `{"input":[{"type":"message","role":"developer","content":{"bad":1}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "openai-response", []byte(tc.body), nil)
			if eErr == nil || eErr.Class != errclass.ClassTranslation {
				t.Fatalf("want ClassTranslation, got %+v", eErr)
			}
		})
	}
	// Unknown roles, content part types, and input item types are
	// unsupported_protocol_or_parameter (FR-009 kernel), not translation.
	for _, tc := range []struct{ name, body string }{
		{"unsupported role", `{"input":[{"type":"message","role":"robot","content":"x"}]}`},
		{"unsupported part type", `{"input":[{"type":"message","role":"user","content":[{"type":"audio"}]}]}`},
		{"unsupported item type", `{"input":[{"type":"web_search"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "openai-response", []byte(tc.body), nil)
			if eErr == nil || eErr.Class != errclass.ClassUnsupported {
				t.Fatalf("want ClassUnsupported, got %+v", eErr)
			}
		})
	}
	// A non-function tool type is unsupported_protocol_or_parameter (FR-009).
	_, eErr := BuildRequest("m", "openai-response", []byte(`{"tools":[{"type":"web_search"}]}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("unsupported tool type: want ClassUnsupported, got %+v", eErr)
	}
}

func TestResponsesReasoningEffortPassthrough(t *testing.T) {
	// Preserve every declared effort exactly, including case and spaces;
	// acceptance belongs to the upstream, not local capability metadata.
	out := mustBuild(t, "openai-response", `{"reasoning":{"effort":"xhigh"},"input":"hi"}`, nil)
	if out["reasoning_effort"] != "xhigh" {
		t.Fatalf("xhigh effort = %v", out["reasoning_effort"])
	}
	out = mustBuild(t, "openai-response", `{"reasoning":{"effort":"  XHigh "},"input":"hi"}`, nil)
	if out["reasoning_effort"] != "  XHigh " {
		t.Fatalf("effort changed: %q", out["reasoning_effort"])
	}
	out = mustBuild(t, "openai-response", `{"reasoning":{"effort":"ultra"},"input":"hi"}`, nil)
	if out["reasoning_effort"] != "ultra" {
		t.Fatalf("non-canonical effort must pass through: %v", out["reasoning_effort"])
	}
}

// F-drift pins: tool_choice preserved on both CC-target cross-format legs.
func TestClaudeSourceToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "claude",
		`{"tool_choice":{"type":"auto","name":""},"messages":[{"role":"user","content":"hi"}]}`, nil)
	if m["tool_choice"] != "auto" {
		t.Fatalf("auto tool_choice = %v", m["tool_choice"])
	}
	m = mustBuild(t, "claude",
		`{"tool_choice":{"type":"tool","name":"lookup"},"messages":[{"role":"user","content":"hi"}]}`, nil)
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" ||
		tc["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("named tool_choice = %v", m["tool_choice"])
	}
	m = mustBuild(t, "claude",
		`{"tool_choice":{"type":"any"},"messages":[]}`, nil)
	if m["tool_choice"] != "required" {
		t.Fatalf("any tool_choice = %v", m["tool_choice"])
	}
}

func TestResponsesSourceToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "openai-response",
		`{"tool_choice":"auto","input":"hi"}`, nil)
	if m["tool_choice"] != "auto" {
		t.Fatalf("auto tool_choice = %v", m["tool_choice"])
	}
	m = mustBuild(t, "openai-response",
		`{"tool_choice":{"type":"function","name":"lookup"},"input":"hi"}`, nil)
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" ||
		tc["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("named tool_choice = %v", m["tool_choice"])
	}
}

// tool_choice "none" is natively representable on CC-target legs and is
// preserved instead of rejected (FR-005).

func TestClaudeSourceNoneToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "claude",
		`{"tool_choice":{"type":"none","name":""},"messages":[]}`, nil)
	if m["tool_choice"] != "none" {
		t.Fatalf("none tool_choice = %v", m["tool_choice"])
	}
}

func TestResponsesSourceNoneToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "openai-response",
		`{"tool_choice":"none","input":"hi"}`, nil)
	if m["tool_choice"] != "none" {
		t.Fatalf("none tool_choice = %v", m["tool_choice"])
	}
}

// applyToolChoiceCC maps every normalized kind onto the CC wire format.

func TestApplyToolChoiceCC(t *testing.T) {
	cases := []struct {
		kind, name string
		want       any // nil asserts the field stays absent
	}{
		{shared.ToolChoiceAbsent, "", nil},
		{shared.ToolChoiceAuto, "", "auto"},
		{shared.ToolChoiceAny, "", "required"},
		{shared.ToolChoiceNone, "", "none"},
	}
	for _, tc := range cases {
		req := &ccRequest{}
		applyToolChoiceCC(req, tc.kind, tc.name)
		b, _ := json.Marshal(req)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		got, ok := m["tool_choice"]
		if got != tc.want || ok != (tc.want != nil) {
			t.Fatalf("%s: tool_choice = %v (present=%t), want %v", tc.kind, got, ok, tc.want)
		}
	}

	req := &ccRequest{}
	applyToolChoiceCC(req, shared.ToolChoiceNamed, "lookup")
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(req.ToolChoice, &named); err != nil ||
		named.Type != "function" || named.Function.Name != "lookup" {
		t.Fatalf("named tool_choice = %s", req.ToolChoice)
	}
}

// Exhaustiveness guard: applyToolChoiceCC is infallible because
// shared.DecodeToolChoice emits a closed set. Every kind in
// shared.AllToolChoiceKinds except absent must render a wire value; a new
// kind added to the slice without extending the renderer falls through to
// nothing and fails here.
func TestApplyToolChoiceCCKindsExhaustive(t *testing.T) {
	for _, kind := range shared.AllToolChoiceKinds {
		req := &ccRequest{}
		applyToolChoiceCC(req, kind, "probe")
		if kind == shared.ToolChoiceAbsent {
			if req.ToolChoice != nil {
				t.Fatalf("%s: tool_choice = %s, want absent", kind, req.ToolChoice)
			}
			continue
		}
		if req.ToolChoice == nil {
			t.Fatalf("kind %q rendered no tool_choice; extend applyToolChoiceCC", kind)
		}
	}
}

// Malformed tool_choice fails translation on both CC-target legs instead
// of being silently dropped.
func TestMalformedToolChoiceRejected(t *testing.T) {
	for _, format := range []string{"claude", "openai-response"} {
		body := `{"tool_choice":42,"messages":[]}`
		if format == "openai-response" {
			body = `{"tool_choice":42,"input":"hi"}`
		}
		_, eErr := BuildRequest("m", format, []byte(body), nil)
		if eErr == nil || !strings.Contains(eErr.Message, "malformed tool_choice") {
			t.Fatalf("%s: err = %+v", format, eErr)
		}
	}
}

func TestBuildOpenAIRequestDeveloperRole(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"developer","content":"system instructions"},{"role":"user","content":"hi"}]}`
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai", []byte(body), nil)
	m := decodeOut(t, out, eErr)
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) < 1 {
		t.Fatalf("invalid messages: %v", m["messages"])
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("invalid message format: %v", msgs[0])
	}
	if got := first["role"]; got != "system" {
		t.Fatalf("expected role %q, got %q", "system", got)
	}
}

// A replayed Responses reasoning item maps to reasoning_content on the
// assistant message of its own turn: the upstream requires its chain of
// thought back once tools are in play, and dropping it degrades the tool
// loop silently instead of failing loudly.
func TestResponsesToChatReplaysReasoningOnToolTurn(t *testing.T) {
	m := mustBuildModel(t, "deepseek-v4-pro", "openai-response", `{"model":"x","stream":false,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"rs1","summary":[],"content":[{"type":"reasoning_text","text":"plan A"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"}
	]}`, nil)
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" || asst["reasoning_content"] != "plan A" {
		t.Fatalf("assistant reasoning not replayed: %v", asst)
	}
	if len(asst["tool_calls"].([]any)) != 1 {
		t.Fatalf("tool_calls lost while attaching reasoning: %v", asst)
	}
	if tool := msgs[2].(map[string]any); tool["reasoning_content"] != nil {
		t.Fatalf("reasoning leaked past its tool turn: %v", tool)
	}
}

// Attachment rules: summary text is the fallback when no plaintext content
// arrived, an assistant text message carries it too, and a user turn in
// between drops it rather than leaking into a later turn.
func TestResponsesToChatReasoningAttachmentRules(t *testing.T) {
	t.Run("summary fallback reaches the assistant message", func(t *testing.T) {
		m := mustBuildModel(t, "deepseek-v4-pro", "openai-response", `{"model":"x","input":[
			{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"sum"}],"content":[]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
		]}`, nil)
		asst := m["messages"].([]any)[0].(map[string]any)
		if asst["reasoning_content"] != "sum" {
			t.Fatalf("summary not used as fallback: %v", asst)
		}
	})

	t.Run("orphan reasoning is dropped, not leaked", func(t *testing.T) {
		m := mustBuildModel(t, "deepseek-v4-pro", "openai-response", `{"model":"x","input":[
			{"type":"reasoning","id":"rs1","content":[{"type":"reasoning_text","text":"orphan"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hey"}]}
		]}`, nil)
		for _, raw := range m["messages"].([]any) {
			if raw.(map[string]any)["reasoning_content"] != nil {
				t.Fatalf("orphan reasoning leaked: %v", raw)
			}
		}
	})
}

// One Responses assistant turn can hold both an assistant text message and
// tool calls; it reaches the upstream as two Chat Completions messages, and
// the message that carries the tool calls needs the turn's replayed reasoning
// too, not only the text message in front of it. Other families keep the wire
// shape of earlier releases either way.
func TestResponsesToChatReplaysReasoningOnSplitToolTurn(t *testing.T) {
	const body = `{"model":"x","stream":true,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"rs1","summary":[],"content":[{"type":"reasoning_text","text":"plan A"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reading first"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"g","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"}
	]}`
	for _, tc := range []struct {
		model string
		want  string
	}{
		{"deepseek-v4.1-flash", "plan A"},
		{"glm-5.3", ""},
	} {
		t.Run(tc.model, func(t *testing.T) {
			m := mustBuildModel(t, tc.model, "openai-response", body, nil)
			msgs := m["messages"].([]any)
			if len(msgs) != 4 {
				t.Fatalf("messages = %v", msgs)
			}
			text := msgs[1].(map[string]any)
			if text["role"] != "assistant" || text["content"] != "reading first" {
				t.Fatalf("text message = %v", text)
			}
			calls := msgs[2].(map[string]any)
			if calls["role"] != "assistant" || len(calls["tool_calls"].([]any)) != 2 {
				t.Fatalf("tool-call message = %v", calls)
			}
			if got, ok := reasoningOf(text); got != tc.want || ok != (tc.want != "") {
				t.Fatalf("text message reasoning_content = %q (present %v), want %q", got, ok, tc.want)
			}
			if got, ok := reasoningOf(calls); got != tc.want || ok != (tc.want != "") {
				t.Fatalf("tool-call message reasoning_content = %q (present %v), want %q", got, ok, tc.want)
			}
			if got, ok := reasoningOf(msgs[3].(map[string]any)); ok {
				t.Fatalf("reasoning leaked into the tool result: %q", got)
			}
		})
	}
}

// Fresh reasoning that arrives between two tool calls belongs to the tool call
// that follows it, so it must not be shadowed by the reasoning a previous
// message of the same turn already carries.
func TestResponsesToChatReplaysFreshReasoningBetweenToolCalls(t *testing.T) {
	m := mustBuildModel(t, "deepseek-v4-pro", "openai-response", `{"model":"x","input":[
		{"type":"reasoning","id":"r1","summary":[],"content":[{"type":"reasoning_text","text":"plan A"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"starting"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"reasoning","id":"r2","summary":[],"content":[{"type":"reasoning_text","text":"plan B"}]},
		{"type":"function_call","call_id":"c2","name":"g","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"},
		{"type":"function_call_output","call_id":"c2","output":"ok"}
	]}`, nil)
	msgs := m["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages = %v", msgs)
	}
	for i, want := range []string{"plan A", "plan A", "plan B", "", ""} {
		got, ok := reasoningOf(msgs[i].(map[string]any))
		if got != want || ok != (want != "") {
			t.Fatalf("messages[%d] reasoning_content = %q (present %v), want %q: %v", i, got, ok, want, msgs[i])
		}
	}
}

// The replayed reasoning belongs to the assistant output that immediately
// follows it: a turn boundary drops it instead of carrying it into the next
// turn, while a fresh reasoning item opens a new turn.
// Every assistant message of the turn carries the replayed chain of thought,
// including the tool-call message, whichever order the text and the calls
// arrive in.
func TestResponsesToChatReplaysReasoningOnEveryAssistantMessageOfItsTurn(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"text before calls", `{"model":"x","input":[
			{"type":"reasoning","id":"r1","summary":[],"content":[{"type":"reasoning_text","text":"plan A"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}
		]}`, 2},
		// The proxy's own Responses stream conversion assigns output indices in
		// arrival order, so a Chat Completions upstream that streams
		// reasoning -> tool -> text -> tool reaches clients in this order.
		{"text between calls", `{"model":"x","input":[
			{"type":"reasoning","id":"r1","summary":[],"content":[{"type":"reasoning_text","text":"plan A"}]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"mid"}]},
			{"type":"function_call","call_id":"c2","name":"g","arguments":"{}"}
		]}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustBuildModel(t, "deepseek-v4-pro", "openai-response", tc.body, nil)
			msgs := m["messages"].([]any)
			if len(msgs) != tc.want {
				t.Fatalf("messages = %v", msgs)
			}
			for i, msg := range msgs {
				if got, ok := reasoningOf(msg.(map[string]any)); got != "plan A" || !ok {
					t.Fatalf("messages[%d] reasoning_content = %q (present %v): %v", i, got, ok, msg)
				}
			}
			last := msgs[len(msgs)-1].(map[string]any)
			if calls, ok := last["tool_calls"].([]any); !ok || len(calls) == 0 {
				t.Fatalf("last message is not a tool-call message: %v", last)
			}
		})
	}
}

// An input message that carries no content is dropped, so the calls around it
// keep merging into one tool-call message: opening the replay state must not
// change that merge shape, which is what earlier releases produced.
func TestResponsesToChatKeepsTheToolCallMergeShape(t *testing.T) {
	const body = `{"model":"x","input":[
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"message","role":"user","content":[]},
		{"type":"function_call","call_id":"c2","name":"g","arguments":"{}"}
	]}`
	for _, model := range []string{"glm-5.3", "deepseek-v4-pro"} {
		t.Run(model, func(t *testing.T) {
			m := mustBuildModel(t, model, "openai-response", body, nil)
			msgs := m["messages"].([]any)
			if len(msgs) != 1 {
				t.Fatalf("messages = %v", msgs)
			}
			asst := msgs[0].(map[string]any)
			if calls, ok := asst["tool_calls"].([]any); !ok || len(calls) != 2 {
				t.Fatalf("tool_calls not merged: %v", asst)
			}
			if got, ok := reasoningOf(asst); ok {
				t.Fatalf("reasoning_content present without a reasoning item: %q", got)
			}
		})
	}
}
func TestResponsesToChatReasoningStopsAtTurnBoundary(t *testing.T) {
	t.Run("system message and tool result", func(t *testing.T) {
		m := mustBuildModel(t, "deepseek-v4-pro", "openai-response", `{"model":"x","input":[
			{"type":"reasoning","id":"r1","summary":[],"content":[{"type":"reasoning_text","text":"plan A"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"ok"},
			{"type":"message","role":"system","content":[{"type":"input_text","text":"new instructions"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]},
			{"type":"function_call","call_id":"c2","name":"g","arguments":"{}"},
			{"type":"function_call_output","call_id":"c2","output":"ok"},
			{"type":"reasoning","id":"r2","summary":[],"content":[{"type":"reasoning_text","text":"plan B"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"third"}]}
		]}`, nil)
		msgs := m["messages"].([]any)
		if len(msgs) != 8 {
			t.Fatalf("messages = %v", msgs)
		}
		for i, want := range []string{"plan A", "plan A", "", "", "", "", "", "plan B"} {
			got, ok := reasoningOf(msgs[i].(map[string]any))
			if got != want || ok != (want != "") {
				t.Fatalf("messages[%d] reasoning_content = %q (present %v), want %q: %v", i, got, ok, want, msgs[i])
			}
		}
	})

	// A system or developer message between the replayed reasoning and the
	// assistant output it would attach to is a turn boundary of its own.
	t.Run("system message directly after reasoning", func(t *testing.T) {
		for _, role := range []string{"system", "developer"} {
			m := mustBuildModel(t, "deepseek-v4-pro", "openai-response", `{"model":"x","input":[
				{"type":"reasoning","id":"r1","summary":[],"content":[{"type":"reasoning_text","text":"plan A"}]},
				{"type":"message","role":"`+role+`","content":[{"type":"input_text","text":"instructions"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}
			]}`, nil)
			for i, msg := range m["messages"].([]any) {
				if got, ok := reasoningOf(msg.(map[string]any)); got != "" || ok {
					t.Fatalf("%s: messages[%d] reasoning_content = %q (present %v): %v", role, i, got, ok, msg)
				}
			}
		}
	})
}

// reasoningOf reports the replayed chain of thought and whether the wire body
// carries the field at all: an explicit null or a non-string value is a
// presence the tests have to catch, not an absence.
func reasoningOf(msg map[string]any) (string, bool) {
	raw, ok := msg["reasoning_content"]
	text, _ := raw.(string)
	return text, ok
}

// Anthropic thinking blocks map to reasoning_content (their Chat Completions
// carrier); redacted_thinking stays omitted since it is encrypted metadata.
func TestClaudeToChatReplaysThinkingAsReasoningContent(t *testing.T) {
	m := mustBuildModel(t, "deepseek-v4-pro", "claude", `{"model":"x","max_tokens":16,"messages":[{"role":"assistant","content":[
		{"type":"thinking","thinking":"deliberate","signature":"sig"},
		{"type":"redacted_thinking","data":"opaque"},
		{"type":"text","text":"answer"},
		{"type":"tool_use","id":"t1","name":"f","input":{}}
	]}]}`, nil)
	asst := m["messages"].([]any)[0].(map[string]any)
	if asst["reasoning_content"] != "deliberate" {
		t.Fatalf("thinking not carried as reasoning_content: %v", asst)
	}
	if asst["content"] != "answer" {
		t.Fatalf("text content lost: %v", asst)
	}
	if len(asst["tool_calls"].([]any)) != 1 {
		t.Fatalf("tool_use lost: %v", asst)
	}
}

// Client -> upstream reasoning replay is limited to the families whose
// endpoints require it. Every other CC model keeps the exact wire shape of
// earlier releases: no reasoning_content field at all.
func TestReasoningReplayLimitedToDeepSeekFamily(t *testing.T) {
	respBody := `{"model":"x","input":[
		{"type":"reasoning","id":"rs1","content":[{"type":"reasoning_text","text":"plan"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}
	]}`
	claudeBody := `{"model":"x","max_tokens":16,"messages":[{"role":"assistant","content":[
		{"type":"thinking","thinking":"plan","signature":"s"},
		{"type":"tool_use","id":"t1","name":"f","input":{}}
	]}]}`
	cases := []struct{ model, want string }{
		{"deepseek-v4-pro", "plan"},
		{"deepseek-flash", "plan"},
		{"DeepSeek-V4.1-Flash", "plan"},
		{"glm-5.3", ""},
		{"kimi-k2", ""},
		{"qwen3-max", ""},
		{"", ""},
	}
	for _, tc := range cases {
		for format, body := range map[string]string{"openai-response": respBody, "claude": claudeBody} {
			out, eErr := BuildRequest(tc.model, format, []byte(body), nil)
			if eErr != nil {
				t.Fatalf("%s/%s: unexpected error: %v", tc.model, format, eErr)
			}
			var decoded map[string]any
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("%s/%s: output is not JSON: %v", tc.model, format, err)
			}
			asst := assistantMessageOf(t, decoded)
			got, _ := asst["reasoning_content"].(string)
			if got != tc.want {
				t.Fatalf("%s/%s: reasoning_content = %q, want %q", tc.model, format, got, tc.want)
			}
			if len(asst["tool_calls"].([]any)) != 1 {
				t.Fatalf("%s/%s: tool_calls lost: %v", tc.model, format, asst)
			}
		}
	}
}

func assistantMessageOf(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	for _, raw := range decoded["messages"].([]any) {
		msg, ok := raw.(map[string]any)
		if ok && msg["role"] == "assistant" {
			return msg
		}
	}
	t.Fatalf("no assistant message in %v", decoded["messages"])
	return nil
}

func TestResponsesToChatReasoningPresence(t *testing.T) {
	for _, tc := range []struct {
		fields, want string
		present      bool
	}{
		{`"content":""`, "", true}, {`"content":[{"text":""}]`, "", true},
		{`"summary":[{"text":""}]`, "", true}, {`"content":null`, "", false},
		{`"content":[]`, "", false}, {`"content":[{}]`, "", false},
		{`"content":[{"text":null}]`, "", false}, {`"content":[null]`, "", false},
		{`"summary":[]`, "", false}, {`"summary":null`, "", false},
		{`"summary":[{}, {"text":null},null]`, "", false},
		{`"encrypted_content":"opaque","summary":[]`, "", false},
		{`"encrypted_content":"opaque"`, "", false},
		{`"id":"no-plaintext"`, "", false},
		{`"content":[{"text":"ok"},{"text":7}]`, "", false},
		{`"encrypted_content":"opaque","content":[{"text":""}]`, "", true},
		{`"content":"","summary":[{"text":"sum"}]`, "sum", true},
		{`"content":[{"text":"a"},{"text":"b"}],"summary":[{"text":"sum"}]`, "ab", true},
		{`"content":{},"summary":[{"text":"a"},{"text":"b"}]`, "ab", true},
	} {
		t.Run(tc.fields, func(t *testing.T) {
			for _, model := range []string{"deepseek-v4.1-flash", "glm-5.3"} {
				m := mustBuildModel(t, model, "openai-response", `{"input":[{"type":"reasoning",`+tc.fields+`},{"role":"assistant","content":"comment"},{"type":"function_call","call_id":"a","name":"echo","arguments":"{}"},{"type":"function_call","call_id":"b","name":"echo","arguments":"{}"}]}`, nil)
				msgs := m["messages"].([]any)
				if len(msgs) != 2 {
					t.Fatal(msgs)
				}
				for _, raw := range msgs {
					got, present := reasoningOf(raw.(map[string]any))
					want := tc.want
					if model != "deepseek-v4.1-flash" {
						want = ""
					}
					if got != want || present != (tc.present && model == "deepseek-v4.1-flash") {
						t.Fatalf("%s: %v", model, msgs)
					}
				}
			}
		})
	}
}

func TestResponsesToChatEmptyReasoningLifetime(t *testing.T) {
	for _, first := range []string{`""`, `"old"`} {
		for _, boundary := range []string{
			`{"role":"user","content":""}`, `{"role":"system","content":""}`, `{"role":"developer","content":""}`,
			`{"role":"user","content":"next"}`, `{"role":"system","content":"next"}`, `{"role":"developer","content":"next"}`,
			`{"type":"function_call_output","call_id":"a","output":"ok"}`,
			`{"type":"reasoning","encrypted_content":"opaque","summary":[]}`,
			`{"type":"reasoning","content":""}`, `{"type":"reasoning","content":"new"}`,
		} {
			t.Run(first+boundary, func(t *testing.T) {
				m := mustBuildModel(t, "deepseek-v4.1-flash", "openai-response", `{"input":[{"type":"reasoning","content":`+first+`},{"type":"function_call","call_id":"a","name":"echo"},`+boundary+`,{"type":"function_call","call_id":"b","name":"echo"}]}`, nil)
				msgs := m["messages"].([]any)
				var assistants []map[string]any
				for _, raw := range msgs {
					msg := raw.(map[string]any)
					if msg["role"] == "assistant" {
						assistants = append(assistants, msg)
					} else if _, ok := msg["reasoning_content"]; ok {
						t.Fatal(msg)
					}
				}
				if len(assistants) != 2 {
					t.Fatal(msgs)
				}
				var old string
				json.Unmarshal([]byte(first), &old)
				if got, ok := reasoningOf(assistants[0]); !ok || got != old {
					t.Fatal("old carrier mutated", msgs)
				}
				want, present := "", false
				if boundary == `{"type":"reasoning","content":""}` {
					present = true
				}
				if boundary == `{"type":"reasoning","content":"new"}` {
					want = "new"
					present = true
				}
				if got, ok := reasoningOf(assistants[1]); got != want || ok != present {
					t.Fatal(msgs)
				}
				for i, id := range []string{"a", "b"} {
					calls := assistants[i]["tool_calls"].([]any)
					if len(calls) != 1 || calls[0].(map[string]any)["id"] != id {
						t.Fatal(msgs)
					}
				}
			})
		}
	}
}

func TestClaudeEmptyReasoningStillOmitted(t *testing.T) {
	for _, text := range []string{"", "  \n"} {
		b, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": text}, map[string]any{"type": "text", "text": "answer"}}}}})
		m := mustBuildModel(t, "deepseek-v4.1-flash", "claude", string(b), nil)
		if _, ok := m["messages"].([]any)[0].(map[string]any)["reasoning_content"]; ok {
			t.Fatal(m)
		}
	}
}
