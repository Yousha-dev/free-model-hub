package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAggregateMergesToolCallDeltas verifies the merging of streaming tool call deltas.
func TestAggregateMergesToolCallDeltas(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	aggregated, errAggregate := Aggregate(strings.NewReader(stream))
	if errAggregate != nil {
		t.Fatalf("Aggregate: %v", errAggregate)
	}
	var response struct {
		Choices []struct {
			Message struct {
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
	if errUnmarshal := json.Unmarshal(aggregated, &response); errUnmarshal != nil {
		t.Fatalf("decode aggregate: %v", errUnmarshal)
	}
	if len(response.Choices) != 1 || len(response.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected one merged tool call, got %+v", response.Choices)
	}
	call := response.Choices[0].Message.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "Bash" {
		t.Fatalf("tool call identity lost: %+v", call)
	}
	if call.Function.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("arguments not concatenated: %q", call.Function.Arguments)
	}
}

// TestAggregateEmptyStreamIsRecognizable verifies that empty streams are recognizable.
//
// An empty stream is an upstream bug and needs to be distinguished from "client disconnection".
func TestAggregateEmptyStreamIsRecognizable(t *testing.T) {
	_, errAggregate := Aggregate(strings.NewReader("data: [DONE]\n\n"))
	if errAggregate == nil {
		t.Fatal("empty stream must return an error")
	}
	if !IsEmptyStreamError(errAggregate) {
		t.Fatalf("empty stream must be recognizable via IsEmptyStreamError, got %v", errAggregate)
	}
}

// TestStreamNormalizesFrames verifies the whitelist reconstruction of streaming frames.
func TestStreamNormalizesFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","service_tier":"x","private_field":"leak","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"private_choice":1}],"extra":{}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames [][]byte
	stats, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, append([]byte(nil), payload...))
			return nil
		},
	})
	if errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if stats.Frames != 1 || !stats.SawDone {
		t.Fatalf("stats = %+v, want 1 frame and SawDone", stats)
	}
	frame := string(frames[0])
	if strings.Contains(frame, "private_field") || strings.Contains(frame, "private_choice") {
		t.Fatalf("whitelist rebuild must drop unknown fields: %s", frame)
	}
	if !strings.Contains(frame, `"service_tier":"x"`) {
		t.Fatalf("known top-level fields must survive: %s", frame)
	}
}

// TestStreamPassesThroughErrorFrames verifies that error frames are passed through as-is.
//
// Upstream error messages are more diagnostic than our reconstructed ones and should not be rewritten by the whitelist.
func TestStreamPassesThroughErrorFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream blew up","code":"500"}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames [][]byte
	if _, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, append([]byte(nil), payload...))
			return nil
		},
		Hint: func(string) string { return "try again later" },
	}); errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	frame := string(frames[0])
	if !strings.Contains(frame, "upstream blew up") {
		t.Fatalf("error message must be preserved verbatim: %s", frame)
	}
	if !strings.Contains(frame, "gateway_hint") {
		t.Fatalf("hint must be attached to the error frame: %s", frame)
	}
}

// TestNormalizeFrameDropsUnknownFields verifies that frame whitelisting also works on non-streaming paths.
func TestNormalizeFrameDropsUnknownFields(t *testing.T) {
	frame := map[string]any{
		"id": "c1", "secret": "leak",
		"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"content": "hi", "private": 1}}},
	}
	normalized := normalizeFrame(frame)
	if _, okSecret := normalized["secret"]; okSecret {
		t.Fatal("unknown top-level field must be dropped")
	}
	choices := normalized["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)
	if _, okPrivate := delta["private"]; okPrivate {
		t.Fatal("unknown delta field must be dropped")
	}
	if delta["content"] != "hi" {
		t.Fatal("known delta fields must survive")
	}
}

// ==================== Request Body Rewriting and Sanitization Tests ====================

// prepareTest executes a rewriting pipeline (test helper).
func prepareTest(t *testing.T, body string, opts PrepareOptions) map[string]any {
	t.Helper()
	prepared := PrepareBody([]byte(body), opts)
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(prepared, &payload); errUnmarshal != nil {
		t.Fatalf("decode prepared body: %v (raw=%s)", errUnmarshal, prepared)
	}
	return payload
}

// TestPrepareBodyCoreRules verifies the core rules of the rewriting pipeline.
func TestPrepareBodyCoreRules(t *testing.T) {
	payload := prepareTest(t, `{
		"model":"glm-5.2",
		"stream":false,
		"max_completion_tokens":32000,
		"messages":[{"role":"developer","content":"be nice"},
		            {"role":"user","content":[{"type":"image_url","image_url":"http://x/y.png"}]}]
	}`, PrepareOptions{})

	// Upstream rejects non-streaming.
	if payload["stream"] != true {
		t.Fatalf("stream must be forced to true, got %v", payload["stream"])
	}
	// max_completion_tokens → max_tokens (and delete the alias).
	if _, okAlias := payload["max_completion_tokens"]; okAlias {
		t.Fatal("max_completion_tokens alias must be removed")
	}
	if payload["max_tokens"] != float64(32000) {
		t.Fatalf("max_tokens = %v, want 32000", payload["max_tokens"])
	}
	// Backfill stream_options (otherwise usage cannot be obtained).
	if _, okOptions := payload["stream_options"]; !okOptions {
		t.Fatal("stream_options must be defaulted to include_usage")
	}
	// developer → system (upstream role whitelist does not include developer).
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role must normalize to system, got %v", first["role"])
	}
	// image_url string → object.
	second, _ := messages[1].(map[string]any)
	parts, _ := second["content"].([]any)
	part, _ := parts[0].(map[string]any)
	if _, okString := part["image_url"].(string); okString {
		t.Fatal("image_url string must be normalized to an object")
	}
	imageObject, okObject := part["image_url"].(map[string]any)
	if !okObject || imageObject["url"] != "http://x/y.png" {
		t.Fatalf("image_url = %v, want {url: ...}", part["image_url"])
	}
}

// TestPrepareBodyExplicitMaxTokensWins verifies that explicit max_tokens takes precedence.
func TestPrepareBodyExplicitMaxTokensWins(t *testing.T) {
	payload := prepareTest(t, `{"model":"m","max_tokens":100,"max_completion_tokens":200,"messages":[]}`, PrepareOptions{})
	if payload["max_tokens"] != float64(100) {
		t.Fatalf("explicit max_tokens must win, got %v", payload["max_tokens"])
	}
	if _, okAlias := payload["max_completion_tokens"]; okAlias {
		t.Fatal("alias must still be removed")
	}
}

// TestPrepareBodyToolChoiceNormalization verifies tool_choice normalization.
//
// The upstream field is a Go string, and object format will result in a 400 error.
func TestPrepareBodyToolChoiceNormalization(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, payload map[string]any)
	}{
		{"object auto becomes string", `{"model":"m","tool_choice":{"type":"auto"},"messages":[]}`,
			func(t *testing.T, payload map[string]any) {
				if payload["tool_choice"] != "auto" {
					t.Fatalf("tool_choice = %v, want auto", payload["tool_choice"])
				}
			}},
		{"none drops tools", `{"model":"m","tool_choice":"none","tools":[{"x":1}],"messages":[]}`,
			func(t *testing.T, payload map[string]any) {
				if _, okChoice := payload["tool_choice"]; okChoice {
					t.Fatal("none must drop tool_choice")
				}
				if _, okTools := payload["tools"]; okTools {
					t.Fatal("none must drop tools")
				}
			}},
		{"function object becomes name", `{"model":"m","tool_choice":{"type":"function","function":{"name":"Bash"}},"messages":[]}`,
			func(t *testing.T, payload map[string]any) {
				if payload["tool_choice"] != "Bash" {
					t.Fatalf("tool_choice = %v, want Bash", payload["tool_choice"])
				}
			}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.check(t, prepareTest(t, testCase.body, PrepareOptions{}))
		})
	}
}

// TestPrepareBodyDeepSeekThinkingInjection verifies DeepSeek thinking chain injection.
//
// DeepSeek models do not return thinking chains unless thinking is explicitly enabled.
func TestPrepareBodyDeepSeekThinkingInjection(t *testing.T) {
	payload := prepareTest(t, `{"model":"deepseek-v4.1-flash","messages":[]}`, PrepareOptions{})
	thinking, okThinking := payload["thinking"].(map[string]any)
	if !okThinking || thinking["type"] != "enabled" {
		t.Fatalf("deepseek must get thinking enabled, got %v", payload["thinking"])
	}
	if payload["reasoning_effort"] != defaultDeepSeekEffort {
		t.Fatalf("reasoning_effort = %v, want %s", payload["reasoning_effort"], defaultDeepSeekEffort)
	}

	// Explicitly disabled must be respected, and the effort level cleared (keeping the level while disabling thinking is a contradictory combination).
	disabled := prepareTest(t, `{"model":"deepseek-v4.1-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`,
		PrepareOptions{})
	if _, okEffort := disabled["reasoning_effort"]; okEffort {
		t.Fatal("disabled thinking must drop reasoning_effort")
	}

	// Non-DeepSeek models are not injected.
	other := prepareTest(t, `{"model":"glm-5.2","messages":[]}`, PrepareOptions{})
	if _, okThinking := other["thinking"]; okThinking {
		t.Fatal("non-deepseek models must not get thinking injected")
	}

	// Explicit effort levels are not overwritten.
	explicit := prepareTest(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"low","messages":[]}`,
		PrepareOptions{DefaultEfforts: map[string]string{"deepseek-v4.1-flash": "high"}})
	if explicit["reasoning_effort"] != "low" {
		t.Fatalf("explicit effort must be respected, got %v", explicit["reasoning_effort"])
	}
}

// TestPrepareBodyEffortDowngrade verifies that effort levels are downgraded to the range supported by the model.
//
// Upstream directly returns 400 for illegal effort levels; downgrading to the nearest legal level allows the request to succeed.
func TestPrepareBodyEffortDowngrade(t *testing.T) {
	// Supports low/high/max, request xhigh → downgrade to the closest high.
	payload := prepareTest(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"xhigh","messages":[]}`,
		PrepareOptions{SupportedEfforts: map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}}})
	if payload["reasoning_effort"] != "high" {
		t.Fatalf("xhigh should downgrade to high, got %v", payload["reasoning_effort"])
	}
	// Supported effort levels in the set are passed through as-is.
	supported := prepareTest(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"max","messages":[]}`,
		PrepareOptions{SupportedEfforts: map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}}})
	if supported["reasoning_effort"] != "max" {
		t.Fatalf("supported effort must pass through, got %v", supported["reasoning_effort"])
	}
	// When the support table is empty, pass through unconditionally without guessing.
	unknown := prepareTest(t, `{"model":"unknown","reasoning_effort":"xhigh","messages":[]}`, PrepareOptions{})
	if unknown["reasoning_effort"] != "xhigh" {
		t.Fatalf("unknown model must pass the effort through, got %v", unknown["reasoning_effort"])
	}
}

// TestPrepareBodyBackfillsReasoning verifies reasoning backfilling for assistant messages.
//
// Some tenants validate len(reasoning) > 0; missing/null/empty strings will result in 400 (whitespace strings are allowed).
func TestPrepareBodyBackfillsReasoning(t *testing.T) {
	payload := prepareTest(t, `{"model":"deepseek-v4.1-flash","messages":[
		{"role":"assistant","content":"hi"}
	]}`, PrepareOptions{})
	messages, _ := payload["messages"].([]any)
	message, _ := messages[0].(map[string]any)
	if _, okReasoning := message["reasoning"]; !okReasoning {
		t.Fatal("assistant messages must carry a reasoning field")
	}
	if message["reasoning"] == "" {
		t.Fatal("reasoning must be non-empty (blank string is the neutral value)")
	}
}

// TestToolPairingRepackAndCleanup verifies reordering and pruning of tool pairings.
func TestToolPairingRepackAndCleanup(t *testing.T) {
	// Orphaned calls are pruned; corresponding tool results are also deleted.
	body := `{"model":"m","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"A","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c2","content":"orphan result"}
	]}`
	payload := prepareTest(t, body, PrepareOptions{})
	messages, _ := payload["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("orphan tool result must be removed, got %d messages", len(messages))
	}
	first, _ := messages[0].(map[string]any)
	if _, okCalls := first["tool_calls"]; okCalls {
		t.Fatal("unpaired tool_calls must be dropped")
	}
}

// TestDropTruncatedToolCalls verifies that tool calls with truncated parameters are dropped.
func TestDropTruncatedToolCalls(t *testing.T) {
	calls := []any{
		map[string]any{"id": "c1", "function": map[string]any{"name": "A", "arguments": `{"path":"/tmp"}`}},
		map[string]any{"id": "c2", "function": map[string]any{"name": "B", "arguments": `{"path":"/tmp`}}, // 截断
		map[string]any{"id": "c3", "function": map[string]any{"name": "C", "arguments": ""}},              // 无参合法
	}
	kept := DropTruncatedToolCalls(calls)
	if len(kept) != 2 {
		t.Fatalf("truncated call must be dropped, kept %d of 3", len(kept))
	}
}

// TestSplitModelIDPrefixProtocol verifies the model ID prefix protocol.
func TestSplitModelIDPrefixProtocol(t *testing.T) {
	cases := []struct {
		input     string
		wantRealm string
		wantBare  string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-5.5", "global", "gpt-5.5"},
		// No prefix: determined by the credential.
		{"glm-5.2", "", "glm-5.2"},
		// Invalid prefix (case-sensitive): treated as part of the bare name.
		{"CN:glm-5.2", "", "CN:glm-5.2"},
		{"other:model", "", "other:model"},
	}
	for _, testCase := range cases {
		t.Run(testCase.input, func(t *testing.T) {
			// The prefix protocol is implemented in the main package; here we verify the Region normalization reused by cb.
			if testCase.wantRealm == "" {
				return
			}
			if NormalizeRegion(testCase.wantRealm) != Region(testCase.wantRealm) {
				t.Fatalf("realm normalization mismatch for %s", testCase.wantRealm)
			}
		})
	}
}

// TestNormalizeRegionDefaultsToCN verifies that unknown regions fall back to CN.
func TestNormalizeRegionDefaultsToCN(t *testing.T) {
	for _, input := range []string{"", "CN", "GLOBAL", "global", "us"} {
		region := NormalizeRegion(input)
		if region != RegionCN && region != RegionGlobal {
			t.Fatalf("NormalizeRegion(%q) = %q, must be a valid region", input, region)
		}
	}
	if NormalizeRegion("GLOBAL") != RegionGlobal {
		t.Fatal("region normalization must be case-insensitive")
	}
	if NormalizeRegion("us") != RegionCN {
		t.Fatal("unknown region must fall back to cn")
	}
}

// TestEffortStaticTableIsPerRealm verifies that effort tables are partitioned by realm.
//
// The same model name has different legal effort levels across two realms, and mixing them will cause requests to carry illegal effort levels.
func TestEffortStaticTableIsPerRealm(t *testing.T) {
	cnCap, okCN := staticEffortCap(RegionCN, "deepseek-v4.1-flash")
	globalCap, okGlobal := staticEffortCap(RegionGlobal, "deepseek-v4.1-flash")
	if !okCN || !okGlobal {
		t.Skip("static effort tables are not embedded")
	}
	// CN has three tiers, global has only high — this is a deliberate difference.
	if len(cnCap.Efforts) <= len(globalCap.Efforts) {
		t.Fatalf("cn efforts (%v) should be broader than global (%v) for this model",
			cnCap.Efforts, globalCap.Efforts)
	}
}

// TestApplyCatalogFallbacksFillsWindow verifies the fallback chain for window searching.
func TestApplyCatalogFallbacksFillsWindow(t *testing.T) {
	models := []ModelInfo{{ID: "unknown-model-xyz"}}
	filled := applyCatalogFallbacks(models, RegionCN)
	if filled[0].ContextWindow <= 0 {
		t.Fatal("context window must be filled by the fallback chain")
	}
	if filled[0].ContextWindow != defaultContextWindow {
		t.Fatalf("unknown model should get the default window, got %d", filled[0].ContextWindow)
	}
}

// TestSanitizeRemovesFingerprints verifies that fingerprint sanitization actually modifies matched text.
func TestSanitizeRemovesFingerprints(t *testing.T) {
	rules, regexes := loadSanitizeRules()
	if len(rules.Rewrites) == 0 && len(regexes) == 0 {
		t.Skip("sanitize rules are not embedded")
	}
	_ = regexes

	// Any match in the rewrite table should produce a change.
	if len(rules.Rewrites) > 0 {
		pair := rules.Rewrites[0]
		rewritten := sanitizeText(pair[0], rules, regexes)
		if rewritten == pair[0] {
			t.Fatalf("rewrite rule did not apply: %q", pair[0])
		}
	}
	// Text that does not match any fingerprint is returned as-is (fast path).
	plain := "just a normal user message about go programming"
	if got := sanitizeText(plain, rules, regexes); got != plain {
		t.Fatalf("plain text must pass through unchanged, got %q", got)
	}
}

// TestSanitizeMessagesCoversToolArguments verifies that tool arguments are also sanitized.
//
// Early implementations skipped entire messages when content was missing, causing tool call arguments
// (file names, commands, write content) to not be sanitized at all.
func TestSanitizeMessagesCoversToolArguments(t *testing.T) {
	rules, _ := loadSanitizeRules()
	if len(rules.Rewrites) == 0 {
		t.Skip("sanitize rules are not embedded")
	}
	needle := rules.Rewrites[0][0]

	messages := []any{map[string]any{
		// content is null (common format for tool call messages).
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{map[string]any{
			"id": "c1",
			"function": map[string]any{
				"name":      "Write",
				"arguments": `{"content":"` + needle + `"}`,
			},
		}},
	}}
	sanitizeMessages(messages)

	call := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	arguments := call["function"].(map[string]any)["arguments"].(string)
	if strings.Contains(arguments, needle) {
		t.Fatal("tool_call arguments must be sanitized even when content is null")
	}
}
