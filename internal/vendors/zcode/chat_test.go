package zcode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"free-model-hub/internal/core"
)

// Conversation conversion test: request side (tools / tool_calls / tool role / multimodal) and
// response side (tool_use -> tool_calls, including streaming input_json_delta).

// anthropicBody decodes the result of a request conversion.
func anthropicBody(t *testing.T, payload string, cred *Credential) map[string]any {
	t.Helper()
	if cred == nil {
		cred = &Credential{APIKey: "k.s"}
	}
	raw, model, errConvert := openAIToAnthropicBody([]byte(payload), false, cred)
	if errConvert != nil {
		t.Fatalf("convert request: %v", errConvert)
	}
	if model == "" {
		t.Fatal("model must not be empty")
	}
	var out map[string]any
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		t.Fatalf("decode converted body: %v", errUnmarshal)
	}
	return out
}

func TestConvertRequestMapsToolsAndToolCalls(t *testing.T) {
	body := anthropicBody(t, `{
		"model": "glm-5.3",
		"messages": [
			{"role": "user", "content": "list files"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function",
				 "function": {"name": "bash", "arguments": "{\"cmd\":\"ls\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "a.txt"}
		],
		"tools": [{"type": "function", "function": {
			"name": "bash",
			"description": "run a command",
			"parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}
		}}],
		"tool_choice": "auto"
	}`, nil)

	// Normalize the model name to official casing.
	if body["model"] != "GLM-5.3" {
		t.Fatalf("model = %v, want GLM-5.3", body["model"])
	}

	// Map tools to Anthropic format (name / description / input_schema).
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 entry", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "bash" || tool["description"] != "run a command" {
		t.Fatalf("unexpected tool entry: %v", tool)
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("input_schema not mapped: %v", tool["input_schema"])
	}

	messages, _ := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d entries, want 3", len(messages))
	}

	// Assistant's tool_calls -> tool_use block, arguments parsed into an object.
	assistant, _ := messages[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant blocks = %v, want 1 tool_use", assistant["content"])
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "bash" {
		t.Fatalf("unexpected tool_use block: %v", block)
	}
	input, _ := block["input"].(map[string]any)
	if input["cmd"] != "ls" {
		t.Fatalf("tool arguments not decoded: %v", block["input"])
	}

	// role=tool -> tool_result block under user message.
	toolMsg, _ := messages[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Fatalf("tool result role = %v, want user", toolMsg["role"])
	}
	resultBlocks, _ := toolMsg["content"].([]any)
	result, _ := resultBlocks[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "call_1" {
		t.Fatalf("unexpected tool_result: %v", result)
	}
	if result["content"] != "a.txt" {
		t.Fatalf("tool result content = %v", result["content"])
	}
}

func TestConvertRequestExtractsSystemAndImages(t *testing.T) {
	body := anthropicBody(t, `{
		"model": "GLM-5.3-Flash",
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "developer", "content": "use tools"},
			{"role": "user", "content": [
				{"type": "text", "text": "what is this"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}},
				{"type": "image_url", "image_url": {"url": "https://example.com/a.png"}}
			]}
		]
	}`, nil)

	system, _ := body["system"].(string)
	if !strings.Contains(system, "be brief") || !strings.Contains(system, "use tools") {
		t.Fatalf("system not merged from system+developer: %q", system)
	}

	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("system/developer must not become messages: %v", messages)
	}
	user, _ := messages[0].(map[string]any)
	blocks, _ := user["content"].([]any)
	// Keep data: URLs only; external images cannot be backfilled and are discarded.
	if len(blocks) != 2 {
		t.Fatalf("blocks = %v, want text + 1 image", blocks)
	}
	image, _ := blocks[1].(map[string]any)
	if image["type"] != "image" {
		t.Fatalf("unexpected image block: %v", image)
	}
	source, _ := image["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "AAAA" {
		t.Fatalf("unexpected image source: %v", source)
	}
}

func TestConvertRequestToolChoiceMappings(t *testing.T) {
	// When tool_choice=none, tools should not be included.
	body := anthropicBody(t, `{
		"model": "GLM-5.3",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type": "function", "function": {"name": "bash", "parameters": {"type": "object"}}}],
		"tool_choice": "none"
	}`, nil)
	if _, exists := body["tools"]; exists {
		t.Fatal("tool_choice=none must drop tools")
	}

	// tool_choice={"type":"function"} maps to forcing a specific tool.
	body = anthropicBody(t, `{
		"model": "GLM-5.3",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type": "function", "function": {"name": "bash", "parameters": {"type": "object"}}}],
		"tool_choice": {"type": "function", "function": {"name": "bash"}}
	}`, nil)
	choice, _ := body["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "bash" {
		t.Fatalf("unexpected tool_choice: %v", choice)
	}
}

func TestConvertRequestClampsMaxTokens(t *testing.T) {
	// Out-of-bounds values must be clamped, otherwise upstream returns 400/1210.
	body := anthropicBody(t, `{"model":"GLM-5.3","max_tokens":999999,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if got := int(body["max_tokens"].(float64)); got != DefaultMaxTokens {
		t.Fatalf("max_tokens = %d, want clamped to %d", got, DefaultMaxTokens)
	}

	// Legal values are kept as-is.
	body = anthropicBody(t, `{"model":"GLM-5.3","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if got := int(body["max_tokens"].(float64)); got != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", got)
	}
}

func TestConvertResponseMapsToolUse(t *testing.T) {
	raw := []byte(`{
		"id": "msg_1",
		"model": "GLM-5.3",
		"stop_reason": "tool_use",
		"content": [
			{"type": "text", "text": "let me check"},
			{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": {"cmd": "ls"}}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`)
	out, errConvert := anthropicToOpenAICompletion(raw, "GLM-5.3")
	if errConvert != nil {
		t.Fatalf("convert response: %v", errConvert)
	}

	var resp map[string]any
	if errUnmarshal := json.Unmarshal(out, &resp); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}

	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}

	message, _ := choice["message"].(map[string]any)
	if message["content"] != "let me check" {
		t.Fatalf("content = %v", message["content"])
	}
	toolCalls, _ := message["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %v, want 1", message["tool_calls"])
	}
	call, _ := toolCalls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if call["id"] != "toolu_1" || call["type"] != "function" || fn["name"] != "bash" {
		t.Fatalf("unexpected tool call: %v", call)
	}
	// Arguments must be a JSON string (OpenAI format).
	args, _ := fn["arguments"].(string)
	var parsed map[string]any
	if errUnmarshal := json.Unmarshal([]byte(args), &parsed); errUnmarshal != nil || parsed["cmd"] != "ls" {
		t.Fatalf("arguments not a JSON object string: %v", fn["arguments"])
	}

	usage, _ := resp["usage"].(map[string]any)
	if usage["total_tokens"] != float64(15) {
		t.Fatalf("usage = %v", usage)
	}
}

// collectChunks drives a complete stream and collects all outputs, consistent with ChatStream's lifecycle:
// emitStart -> feed one by one -> append final chunk on EOF (emitFinish is idempotent, not repeated on normal completion).
func collectChunks(t *testing.T, events []string) []map[string]any {
	t.Helper()
	converter := newStreamConverter("GLM-5.3")
	var out []map[string]any
	appendChunk := func(raw []byte) error {
		var chunk map[string]any
		if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
			t.Fatalf("decode chunk: %v", errUnmarshal)
		}
		out = append(out, chunk)
		return nil
	}
	if errStart := converter.emitStart(appendChunk); errStart != nil {
		t.Fatalf("emit start: %v", errStart)
	}
	for _, event := range events {
		var parsed map[string]any
		if errUnmarshal := json.Unmarshal([]byte(event), &parsed); errUnmarshal != nil {
			t.Fatalf("decode event: %v", errUnmarshal)
		}
		if errFeed := converter.feed(parsed, appendChunk); errFeed != nil {
			t.Fatalf("feed: %v", errFeed)
		}
	}
	if errFinish := converter.emitFinish("", appendChunk); errFinish != nil {
		t.Fatalf("emit finish: %v", errFinish)
	}
	return out
}

func TestStreamConverterEmitsToolCalls(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"bash"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
	})

	// The first chunk is the role declaration.
	first := chunks[0]["choices"].([]any)[0].(map[string]any)
	if delta := first["delta"].(map[string]any); delta["role"] != "assistant" {
		t.Fatalf("first chunk must declare role: %v", delta)
	}

	// content_block_start provides id/name.
	startChoice := chunks[1]["choices"].([]any)[0].(map[string]any)
	startDelta := startChoice["delta"].(map[string]any)
	startCalls := startDelta["tool_calls"].([]any)
	startCall := startCalls[0].(map[string]any)
	if startCall["id"] != "toolu_9" || startCall["index"] != float64(0) {
		t.Fatalf("unexpected tool_call start: %v", startCall)
	}
	if fn := startCall["function"].(map[string]any); fn["name"] != "bash" || fn["arguments"] != "" {
		t.Fatalf("unexpected function start: %v", startCall["function"])
	}

	// Two input_json_delta segments accumulated to the same index.
	var arguments strings.Builder
	for _, chunk := range chunks[2:4] {
		choice := chunk["choices"].([]any)[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		calls := delta["tool_calls"].([]any)
		call := calls[0].(map[string]any)
		if call["index"] != float64(0) {
			t.Fatalf("delta must target index 0: %v", call)
		}
		arguments.WriteString(call["function"].(map[string]any)["arguments"].(string))
	}
	if arguments.String() != `{"cmd":"ls"}` {
		t.Fatalf("arguments = %q, want {\"cmd\":\"ls\"}", arguments.String())
	}

	// The last chunk gives finish_reason=tool_calls.
	last := chunks[len(chunks)-1]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", last["finish_reason"])
	}
}

func TestStreamConverterEmitsTextDeltas(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	})

	var text strings.Builder
	for _, chunk := range chunks {
		choice := chunk["choices"].([]any)[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			text.WriteString(content)
		}
	}
	if text.String() != "hello world" {
		t.Fatalf("text = %q", text.String())
	}

	last := chunks[len(chunks)-1]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", last["finish_reason"])
	}
}

func TestClassifyEmbeddedError(t *testing.T) {
	// Normal Anthropic responses should not be judged as errors.
	if errUpstream := classifyEmbeddedError([]byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}]}`)); errUpstream != nil {
		t.Fatalf("valid response misclassified: %v", errUpstream)
	}
	// 200-wrapped business errors must be recognized, otherwise the error body will be treated as model output.
	if errUpstream := classifyEmbeddedError([]byte(`{"code":1005,"msg":"daily quota exhausted"}`)); errUpstream == nil {
		t.Fatal("embedded business error not detected")
	}
}

func TestResolveTargetURLPrefersStartPlanAndUltraGateway(t *testing.T) {
	// When JWT is available, prioritize the Start Plan channel.
	target, errResolve := resolveTargetURL("", &Credential{JWTToken: "a.b.c", APIKey: "k.s"})
	if errResolve != nil {
		t.Fatalf("unexpected error: %v", errResolve)
	}
	if target != ZCodeOrigin+PathPlanMessages {
		t.Fatalf("target = %q, want %q", target, ZCodeOrigin+PathPlanMessages)
	}

	// When only an API Key is held, default to the ZCode Ultra gateway (to avoid DNS failure when directly connecting to api.z.ai from CN).
	target, errResolve = resolveTargetURL("", &Credential{APIKey: "k.s"})
	if errResolve != nil {
		t.Fatalf("unexpected error: %v", errResolve)
	}
	if target != ZCodeOrigin+PathUltraZAIMessages {
		t.Fatalf("target = %q, want %q", target, ZCodeOrigin+PathUltraZAIMessages)
	}

	// When baseURL is explicitly specified, use the custom base address.
	target, errResolve = resolveTargetURL("https://custom.zcode.test/", &Credential{APIKey: "k.s"})
	if errResolve != nil || target != "https://custom.zcode.test"+PathAPIMessages {
		t.Fatalf("custom target = %q, err = %v", target, errResolve)
	}

	// Return an error when both are missing.
	if _, errResolve := resolveTargetURL("", &Credential{}); errResolve == nil {
		t.Fatal("Must reject when JWT and API Key are missing")
	}
}

func TestApplyStartPlanSystemIsIdempotent(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"system":   "user instructions",
	}
	applyStartPlanSystem(body, "GLM-5.3")
	first, _ := body["system"].([]any)
	if len(first) != 3 {
		t.Fatalf("system block count = %d, want 3 (cli_prefix / stable / dynamic)", len(first))
	}

	// Repeated calls must not repeatedly concatenate (upstream reviews content, exceeding 3 blocks will be judged as a 3012 exception).
	applyStartPlanSystem(body, "GLM-5.3")
	second, _ := body["system"].([]any)
	if len(second) != len(first) {
		t.Fatalf("Duplicate injection changed the block count: %d != %d", len(second), len(first))
	}

	// The first chunk is the CLI prefix (without cache_control), and the last chunk embeds the model name and user system (Output Style slot).
	head, _ := second[0].(map[string]any)
	if !strings.Contains(head["text"].(string), "ZCode") {
		t.Fatalf("First block should be the official identity block: %v", head)
	}
	if head["cache_control"] != nil {
		t.Fatalf("First CLI prefix block should not carry cache_control: %v", head)
	}
	tail, _ := second[len(second)-1].(map[string]any)
	tailText, _ := tail["text"].(string)
	if !strings.Contains(tailText, "account:zai-start-plan/GLM-5.3") {
		t.Fatalf("Dynamic block should contain the current model identifier: %s", tailText)
	}
	if !strings.Contains(tailText, "# Output Style: custom\nuser instructions") {
		t.Fatalf("User system should be embedded in the Output Style slot of the dynamic block: %s", tailText)
	}
}

func TestConvertMessagesMergesConsecutiveToolResults(t *testing.T) {
	body := anthropicBody(t, `{
		"model": "GLM-5.3",
		"messages": [
			{"role": "user", "content": "check two files"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "read", "arguments": "{}"}},
				{"id": "call_2", "type": "function", "function": {"name": "read", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "file_a"},
			{"role": "tool", "tool_call_id": "call_2", "content": "file_b"}
		]
	}`, nil)
	messages, _ := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("Consecutive tool messages should be merged into a single user message, got %d messages: %v", len(messages), messages)
	}
	toolUser, _ := messages[2].(map[string]any)
	blocks, _ := toolUser["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("Should contain 2 tool_result blocks after merging, got %d: %v", len(blocks), blocks)
	}
}

func TestApplyCacheControl(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "first"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "reply"}}},
		},
	}
	applyCacheControl(body)

	messages := body["messages"].([]any)
	// The last block of the last non-system message should carry cache_control.
	last := messages[1].(map[string]any)
	blocks := last["content"].([]any)
	block := blocks[len(blocks)-1].(map[string]any)
	cc, _ := block["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" {
		t.Fatalf("cache_control not appended: %v", block)
	}
	// The previous one should not be modified.
	first := messages[0].(map[string]any)
	if _, isString := first["content"].(string); !isString {
		t.Fatalf("Non-last message should not be rewritten: %v", first["content"])
	}
}

// ---- Start Plan Outbound Gate ----

// TestChatGateSerializesPerAccount verifies that the gate for the same account is serial: when the holder has not released it, latercomers
// queue up and can acquire it again after release; different accounts do not block each other.
func TestChatGateSerializesPerAccount(t *testing.T) {
	gate := newChatGate()

	release, errAcquire := gate.acquire(context.Background(), "uid:user-a")
	if errAcquire != nil {
		t.Fatalf("first acquire: %v", errAcquire)
	}

	ctxQuick, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, errSecond := gate.acquire(ctxQuick, "uid:user-a"); errSecond == nil {
		t.Fatal("second acquire must block while the slot is held")
	}

	release()
	releaseAgain, errThird := gate.acquire(context.Background(), "uid:user-a")
	if errThird != nil {
		t.Fatalf("acquire after release: %v", errThird)
	}
	releaseAgain()

	releaseOther, errOther := gate.acquire(context.Background(), "uid:user-b")
	if errOther != nil {
		t.Fatalf("other account must not be blocked: %v", errOther)
	}
	releaseOther()
}

// TestChatGateRespectsCancelledContext verifies that queued entities exit immediately when the host cancels the request.
func TestChatGateRespectsCancelledContext(t *testing.T) {
	gate := newChatGate()
	release, errAcquire := gate.acquire(context.Background(), "uid:cancel")
	if errAcquire != nil {
		t.Fatalf("first acquire: %v", errAcquire)
	}
	defer release()

	ctxCancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, errSecond := gate.acquire(ctxCancelled, "uid:cancel"); errSecond == nil {
		t.Fatal("cancelled context must not acquire a held slot")
	}
}

// TestHoldStartPlanSlotSkipsAPIKeyChannel verifies that the pure API Key channel bypasses the gate: this channel
// has no 3008/3009/3010 concurrency limits and cannot be blocked in the queue by in-flight conversations of the same account.
func TestHoldStartPlanSlotSkipsAPIKeyChannel(t *testing.T) {
	v := &adapter{gate: newChatGate()}
	cred := &Credential{APIKey: "k.s"} // 无 JWT，走网关通道

	release, errHold := v.holdStartPlanSlot(context.Background(), cred)
	if errHold != nil {
		t.Fatalf("api-key channel must not queue: %v", errHold)
	}
	release()

	if _, errNil := v.holdStartPlanSlot(context.Background(), nil); errNil != nil {
		t.Fatalf("nil credential must fall through: %v", errNil)
	}
}

// TestHoldStartPlanSlotCancelWhileQueuedIsClientFault verifies that when the host disconnects while queued, it is handled as
// a client cancellation (no retry, no counting as upstream failure) rather than being treated as an upstream 5xx.
func TestHoldStartPlanSlotCancelWhileQueuedIsClientFault(t *testing.T) {
	v := &adapter{gate: newChatGate()}
	native := &Credential{JWTToken: "jwt", UserID: "u-queued"}

	held, errHeld := v.gate.acquire(context.Background(), native.AccountKey())
	if errHeld != nil {
		t.Fatalf("hold slot: %v", errHeld)
	}
	defer held()

	ctxCancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, errHold := v.holdStartPlanSlot(ctxCancelled, native)
	failure, ok := errHold.(*core.Failure)
	if !ok || failure.Kind != core.FailureKindClient {
		t.Fatalf("cancel while queued = %v, want client failure", errHold)
	}
}

func TestStreamUsageCumulativeAndTerminalOnly(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":0,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`,
		`{"type":"message_delta","delta":{},"usage":{"output_tokens":4}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	})
	// Produce only role, content, and final chunk; pure usage updates cannot send finish_reason prematurely.
	if len(chunks) != 3 {
		t.Fatalf("expected role/content/finish, got %v", chunks)
	}
	for i, chunk := range chunks {
		choices := chunk["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("chunk %d has no choice: %v", i, chunk)
		}
		if i < len(chunks)-1 {
			if _, ok := chunk["usage"]; ok {
				t.Fatalf("interim usage on chunk %d: %v", i, chunk)
			}
			if choices[0].(map[string]any)["finish_reason"] != nil {
				t.Fatalf("premature finish: %v", chunk)
			}
		}
	}
	u := usageOf(t, chunks[len(chunks)-1])
	assertUsage(t, u, 200, 9, 209)
	details := u["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(80) || details["cache_creation_tokens"] != float64(20) {
		t.Fatalf("cache detail = %v", details)
	}
}

func TestStreamUsageExplicitZeroAndAbsent(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}}`,
	})
	assertUsage(t, usageOf(t, chunks[len(chunks)-1]), 2, 3, 5)
	for _, chunk := range collectChunks(t, []string{`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`}) {
		if _, ok := chunk["usage"]; ok {
			t.Fatalf("invented usage: %v", chunk)
		}
	}
}

func TestStreamUsageToolCall(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`{"type":"content_block_start","content_block":{"type":"tool_use","id":"tool_1","name":"bash"}}`,
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
	})
	last := chunks[len(chunks)-1]
	if last["choices"].([]any)[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("tool finish lost: %v", last)
	}
	assertUsage(t, usageOf(t, last), 10, 7, 17)
}

func TestNonstreamAndCollapsedUsage(t *testing.T) {
	raw := []byte(`{"id":"msg_cache","model":"GLM-5.3-Flash","stop_reason":"end_turn","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":100,"output_tokens":9,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}`)
	converted, err := anthropicToOpenAICompletion(raw, "GLM-5.3-Flash")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(converted, &document); err != nil {
		t.Fatal(err)
	}
	assertUsage(t, usageOf(t, document), 200, 9, 209)
	var chunks []map[string]any
	err = emitCollapsedCompletion(converted, "GLM-5.3-Flash", func(b []byte) error {
		var chunk map[string]any
		if err := json.Unmarshal(b, &chunk); err != nil {
			return err
		}
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks[:len(chunks)-1] {
		if _, ok := chunk["usage"]; ok {
			t.Fatalf("collapsed interim usage: %v", chunk)
		}
	}
	assertUsage(t, usageOf(t, chunks[len(chunks)-1]), 200, 9, 209)
}

func TestStreamUsageZeroSnapshot(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":4}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":0}}`,
	})
	assertUsage(t, usageOf(t, chunks[len(chunks)-1]), 0, 0, 0)
}

func TestStreamUsageMessageStopFallback(t *testing.T) {
	for _, tool := range []bool{false, true} {
		events := []string{`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`}
		if tool {
			events = append(events, `{"type":"content_block_start","content_block":{"type":"tool_use","id":"tool_1","name":"bash"}}`)
		}
		events = append(events,
			`{"type":"message_delta","usage":{"output_tokens":7}}`,
			`{"type":"message_stop"}`, `{"type":"message_stop"}`)
		chunks := collectChunks(t, events)
		finishes := 0
		for _, chunk := range chunks {
			choice := chunk["choices"].([]any)[0].(map[string]any)
			if choice["finish_reason"] == nil {
				continue
			}
			finishes++
			want := "stop"
			if tool {
				want = "tool_calls"
			}
			if choice["finish_reason"] != want {
				t.Fatalf("fallback finish=%v, want %s", choice["finish_reason"], want)
			}
			assertUsage(t, usageOf(t, chunk), 10, 7, 17)
		}
		if finishes != 1 {
			t.Fatalf("fallback finishes=%d, want 1", finishes)
		}
	}
	// message_stop after a normal final chunk cannot overwrite length, nor can it resend usage.
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	})
	if len(chunks) != 2 || chunks[1]["choices"].([]any)[0].(map[string]any)["finish_reason"] != "length" {
		t.Fatalf("normal terminal duplicated or overwritten: %v", chunks)
	}
}

func TestStreamUsageEOFFallback(t *testing.T) {
	// Upstream neither gives stop_reason nor sends message_stop, closing the stream with only usage:
	// ChatStream must append a final chunk at EOF, along with the merged usage.
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`,
		`{"type":"message_delta","usage":{"output_tokens":7}}`,
	})
	if len(chunks) != 3 {
		t.Fatalf("expected role/content/finish, got %v", chunks)
	}
	last := chunks[len(chunks)-1]
	choice := last["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("EOF fallback finish = %v, want stop", choice["finish_reason"])
	}
	assertUsage(t, usageOf(t, last), 10, 7, 17)
}

func TestMissingUsageOmittedInAllResponsePaths(t *testing.T) {
	converted, err := anthropicToOpenAICompletion([]byte(`{"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`), "glm")
	if err != nil {
		t.Fatal(err)
	}
	check := func(b []byte) error {
		var chunk map[string]any
		if err := json.Unmarshal(b, &chunk); err != nil {
			return err
		}
		if _, ok := chunk["usage"]; ok {
			t.Errorf("missing usage must be omitted: %s", b)
		}
		return nil
	}
	if err := check(converted); err != nil {
		t.Fatal(err)
	}
	if err := emitCollapsedCompletion(converted, "glm", check); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range collectChunks(t, []string{`{"type":"message_stop"}`}) {
		if _, ok := chunk["usage"]; ok {
			t.Fatalf("missing stream usage must be omitted: %v", chunk)
		}
	}
}

func TestStreamUsageErrorAndSinkFailure(t *testing.T) {
	converter := newStreamConverter("glm")
	var chunks [][]byte
	emit := func(b []byte) error { chunks = append(chunks, b); return nil }
	input := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\ndata: {\"type\":\"error\",\"error\":{\"message\":\"interrupted\"}}\n\n"
	err := parseAnthropicSSE(strings.NewReader(input), converter, emit)
	if err == nil || !strings.Contains(err.Error(), "interrupted") || len(chunks) != 0 {
		t.Fatalf("error must propagate without a success/usage chunk: chunks=%q err=%v", chunks, err)
	}
	want := errors.New("client disconnected")
	for _, collapsed := range []bool{false, true} {
		calls := 0
		sink := func([]byte) error { calls++; return want }
		if collapsed {
			err = emitCollapsedCompletion([]byte(`{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"total_tokens":12}}`), "glm", sink)
		} else {
			err = converter.feed(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}, sink)
		}
		if !errors.Is(err, want) || calls != 1 {
			t.Fatalf("sink error lost or retried: collapsed=%v calls=%d err=%v", collapsed, calls, err)
		}
	}
}

func assertUsage(t *testing.T, usage map[string]any, input, output, total float64) {
	t.Helper()
	if usage["prompt_tokens"] != input || usage["completion_tokens"] != output || usage["total_tokens"] != total {
		t.Fatalf("usage = %v, want input=%v output=%v total=%v", usage, input, output, total)
	}
}

// usageOf extracts the usage field from the payload; streaming chunks and non-streaming full documents share the same schema.
func usageOf(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("missing usage: %v", payload)
	}
	return usage
}
