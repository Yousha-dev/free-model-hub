package autoclaw

import (
	"encoding/json"
	"strings"
	"testing"
)

// preparePayload runs prepareBody and decodes it back into a map (prepareBody is the sole entry point for outbound bodies).
func preparePayload(t *testing.T, body, model string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(prepareBody([]byte(body), model), &payload); err != nil {
		t.Fatalf("unmarshal prepared body: %v", err)
	}
	return payload
}

// messagesPayload builds the request body using a map to avoid writing large chunks of JSON by hand in tests.
func messagesPayload(t *testing.T, extra map[string]any, messages ...map[string]any) string {
	t.Helper()
	list := make([]any, 0, len(messages))
	for _, message := range messages {
		list = append(list, message)
	}
	body := map[string]any{"messages": list}
	for key, value := range extra {
		body[key] = value
	}
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatalf("marshal test body: %v", errMarshal)
	}
	return string(raw)
}

// firstMessage gets messages[0] (fails immediately if it doesn't exist).
func firstMessage(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	messages, okMessages := payload["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		t.Fatalf("messages missing: %#v", payload["messages"])
	}
	message, okMessage := messages[0].(map[string]any)
	if !okMessage {
		t.Fatalf("messages[0] is not an object: %#v", messages[0])
	}
	return message
}

// TestPrepareBodyInjectsIdentityPrefixWithoutSystem covers standard clients without a system message:
// prepends a system message with an identity prefix, applies the model rewrite, deletes the user field, and raises max_tokens to the lower bound.
func TestPrepareBodyInjectsIdentityPrefixWithoutSystem(t *testing.T) {
	payload := preparePayload(t, messagesPayload(t,
		map[string]any{"model": "ac/glm-5.3", "max_tokens": 4096, "user": "u1"},
		map[string]any{"role": "user", "content": "hi"},
	), "glm-5.3")

	if payload["model"] != "glm-5.3" {
		t.Fatalf("model = %v, want glm-5.3", payload["model"])
	}
	if _, okUser := payload["user"]; okUser {
		t.Fatal("user field must be removed")
	}
	if got := payload["max_tokens"].(float64); got != maxTokensFloor {
		t.Fatalf("max_tokens = %v, want %d", got, maxTokensFloor)
	}
	first := firstMessage(t, payload)
	if first["role"] != "system" {
		t.Fatalf("messages[0].role = %v, want system", first["role"])
	}
	text, _ := first["content"].(string)
	if !strings.HasPrefix(text, identityLine) || !strings.Contains(text, "## Tooling") {
		t.Fatalf("injected system content does not satisfy the gate: %q", text)
	}
	if messages := payload["messages"].([]any); len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2 (prefix inserted, original user kept)", len(messages))
	}
}

// TestPrepareBodyPrefixesExistingSystem verifies that the prefix is prepended to the first system prompt while keeping the original text intact.
func TestPrepareBodyPrefixesExistingSystem(t *testing.T) {
	payload := preparePayload(t, messagesPayload(t, nil,
		map[string]any{"role": "system", "content": "You are a helpful assistant."},
		map[string]any{"role": "user", "content": "hi"},
	), "")

	text := firstMessage(t, payload)["content"].(string)
	if !strings.HasPrefix(text, identityLine) {
		t.Fatalf("system content must start with identity line: %q", text)
	}
	if !strings.HasSuffix(text, "\nYou are a helpful assistant.") {
		t.Fatalf("original system text must be kept verbatim after the prefix: %q", text)
	}
}

// TestPrepareBodyIsIdempotent verifies that repeated outbounding (fallbacks / same-provider retries) does not prepend the prefix twice.
func TestPrepareBodyIsIdempotent(t *testing.T) {
	body := messagesPayload(t, nil, map[string]any{"role": "system", "content": "You are a helpful assistant."})
	once := prepareBody([]byte(body), "")
	twice := prepareBody(once, "")

	if string(once) != string(twice) {
		t.Fatalf("prepareBody is not idempotent:\nonce:  %s\ntwice: %s", once, twice)
	}
	if count := strings.Count(string(twice), identityLine); count != 1 {
		t.Fatalf("identity line appears %d times, want 1: %q", count, twice)
	}
}

// TestPrepareBodyPrefixesDeveloperRole verifies that developer and system have equal weight: the prefix is prepended to itself without inserting extra messages.
func TestPrepareBodyPrefixesDeveloperRole(t *testing.T) {
	payload := preparePayload(t, messagesPayload(t, nil,
		map[string]any{"role": "developer", "content": "be brief"},
	), "")

	first := firstMessage(t, payload)
	text, _ := first["content"].(string)
	if first["role"] != "developer" || !strings.HasPrefix(text, identityLine) {
		t.Fatalf("developer message must be prefixed as-is: role=%v content=%q", first["role"], text)
	}
	if messages := payload["messages"].([]any); len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1 (no extra system inserted)", len(messages))
	}
}

// TestPrepareBodyRewritesForeignIdentities verifies that foreign harness identity sentences are rewritten to neutral phrasing,
// and only affects system/developer — the same sentence in user is kept as-is.
func TestPrepareBodyRewritesForeignIdentities(t *testing.T) {
	for _, foreign := range []string{
		"You are Claude Code, Anthropic's official CLI tool for Claude.",
		"you are claude code", // 闸门大小写不敏感，改写也必须不敏感
		"You are Codex, based on GPT-5.",
		"You are an AI agent powered by DeepSeek Harness.",
		"You are ZCode, an interactive coding agent.",
	} {
		payload := preparePayload(t, messagesPayload(t, nil,
			map[string]any{"role": "system", "content": foreign},
			map[string]any{"role": "user", "content": foreign},
		), "")

		systemText := firstMessage(t, payload)["content"].(string)
		lower := strings.ToLower(systemText)
		for _, product := range []string{"claude code", "you are codex", "deepseek harness", "you are zcode"} {
			if strings.Contains(lower, product) {
				t.Fatalf("%q still contains %q: %q", foreign, product, systemText)
			}
		}
		if !strings.HasPrefix(systemText, identityLine) {
			t.Fatalf("%q: identity prefix missing: %q", foreign, systemText)
		}
		messages := payload["messages"].([]any)
		if user := messages[1].(map[string]any); user["content"] != foreign {
			t.Fatalf("user message must not be rewritten: %v", user["content"])
		}
	}
}

// TestPrepareBodyPrefixesTextPartArray verifies that array-form bodies prepend to the first text part.
func TestPrepareBodyPrefixesTextPartArray(t *testing.T) {
	payload := preparePayload(t, messagesPayload(t, nil,
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "be brief"},
		}},
	), "")

	parts := firstMessage(t, payload)["content"].([]any)
	text := parts[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, identityLine) || !strings.HasSuffix(text, "be brief") {
		t.Fatalf("first text part not prefixed: %q", text)
	}
}

// TestPrepareBodyReplacesEmptySystemContent verifies that missing/null bodies fall back to a system message containing only the prefix.
func TestPrepareBodyReplacesEmptySystemContent(t *testing.T) {
	for _, content := range []any{nil, map[string]any{"unexpected": true}} {
		payload := preparePayload(t, messagesPayload(t, nil,
			map[string]any{"role": "system", "content": content},
		), "")
		if text := firstMessage(t, payload)["content"].(string); text != identityPrefix {
			t.Fatalf("content %#v: system content = %q, want identity prefix only", content, text)
		}
	}
}

// TestPrepareBodyNormalizesMaxTokens verifies that only "existing small positive integers" are raised: missing values are not injected,
// and non-positive integers as well as max_completion_tokens are left untouched.
func TestPrepareBodyNormalizesMaxTokens(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		want  any // nil 表示该字段不应出现
	}{
		{name: "缺省不注入", extra: map[string]any{}},
		{name: "小值抬到下限", extra: map[string]any{"max_tokens": 16}, want: float64(maxTokensFloor)},
		{name: "8192 抬到下限", extra: map[string]any{"max_tokens": 8192}, want: float64(maxTokensFloor)},
		{name: "恰在下限保留", extra: map[string]any{"max_tokens": 10000}, want: float64(10000)},
		{name: "超过下限保留", extra: map[string]any{"max_tokens": 12000}, want: float64(12000)},
		{name: "零不动", extra: map[string]any{"max_tokens": 0}, want: float64(0)},
		{name: "负数不动", extra: map[string]any{"max_tokens": -1}, want: float64(-1)},
		{name: "非整数不动", extra: map[string]any{"max_tokens": 1024.5}, want: float64(1024.5)},
		{name: "字符串不动", extra: map[string]any{"max_tokens": "4096"}, want: "4096"},
		{name: "completion 键不动", extra: map[string]any{"max_completion_tokens": 16}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := preparePayload(t, messagesPayload(t, tc.extra,
				map[string]any{"role": "user", "content": "hi"},
			), "")

			got, okGot := payload["max_tokens"]
			if tc.want == nil {
				if okGot {
					t.Fatalf("max_tokens must stay absent, got %v", got)
				}
			} else if got != tc.want {
				t.Fatalf("max_tokens = %v, want %v", got, tc.want)
			}
			if completion, okCompletion := payload["max_completion_tokens"]; okCompletion && completion != float64(16) {
				t.Fatalf("max_completion_tokens must stay untouched: %v", completion)
			}
		})
	}
}

// TestPrepareBodyKeepsUnparsableBody verifies that unparseable request bodies are passed through as-is (unusual formats are left for the upstream to error on).
func TestPrepareBodyKeepsUnparsableBody(t *testing.T) {
	if got := string(prepareBody([]byte("not json"), "m")); got != "not json" {
		t.Fatalf("non-JSON body must pass through, got %q", got)
	}
}
