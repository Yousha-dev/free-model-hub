// Package responses implements the OpenAI Responses protocol adapter
// (arch §8): upstream endpoint /v1/responses serving GPT 5.6 Luna and
// any catalog model declared compatible (FR-004). Request translation is
// FR-005, response and stream translation FR-006, acceptance coverage
// AC §D. Responses support is REQUIRED first-release scope, not
// experimental (07-open-questions.md §5).
//
// Per arch §8 this package is isolated from the scheduler and catalog
// manager and uses only stdlib JSON primitives.
package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"free-model-hub/cpasdk/pluginapi"

	"free-model-hub/internal/ocgo/adapter/shared"
	"free-model-hub/internal/ocgo/catalog"
	"free-model-hub/internal/ocgo/errclass"
	"free-model-hub/internal/ocgo/thinking"
)

// EndpointPath is the upstream endpoint path for the Responses route (FR-004).
var EndpointPath = catalog.RouteResponses.EndpointPath()

// BuildRequest translates an inbound request body from sourceFormat
// ("openai" Chat Completions, "claude" Anthropic Messages,
// "openai-response" native Responses passthrough) into a Responses body
// for upstreamModel (FR-005, AC §D). upstreamModel replaces whatever
// client-facing model ID the request carried (FR-003). Declared efforts
// pass through unchanged, regardless of ts. Anthropic budgets use a fixed
// threshold conversion only when no explicit effort is provided.
// Replayed tool-call ids are normalized to the upstream's marked shape on
// every source format (see rewritePassthrough and shared.MarkToolCallID).
// Unknown formats are ClassUnsupported; malformed input is
// ClassTranslation; messages are descriptive and redacted.
func BuildRequest(upstreamModel string, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	return BuildRequestFrom(upstreamModel, shared.NewSourceRequest(sourceFormat, sourceBody), ts)
}

// BuildRequestFrom is BuildRequest's entry point for callers that already own
// a lazy SourceRequest. The typed source representation decodes at most once
// per SourceRequest, so the executor can share the session-derivation decode
// with this conversion. Native "openai-response" passthrough keeps using the
// raw body so input shapes this adapter cannot model are preserved verbatim.
func BuildRequestFrom(upstreamModel string, source *shared.SourceRequest, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	switch source.Format() {
	case "openai-response":
		return rewritePassthrough(upstreamModel, source.Raw())
	case "openai":
		return fromChatCompletions(upstreamModel, source, ts)
	case "claude":
		return fromClaudeMessages(upstreamModel, source, ts)
	default:
		return nil, shared.UnsupportedFormat(source.Format(), EndpointPath)
	}
}

// ---- target wire shape -------------------------------------------------

// rewritePassthrough rewrites the model id and normalizes replayed tool-call
// ids of a native Responses request. The body is returned byte-identical when
// neither needs to change, so the passthrough stays transparent for every
// history without tool calls.
func rewritePassthrough(upstreamModel string, body []byte) ([]byte, *errclass.Error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errclass.Translation("malformed openai-response request JSON: " + err.Error())
	}
	// A literal JSON null body unmarshals without error into a NIL map.
	if req == nil {
		return nil, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	changed := false
	if raw, ok := req["model"]; !ok || string(raw) != `"`+upstreamModel+`"` {
		id, err := json.Marshal(upstreamModel)
		if err != nil {
			return nil, errclass.Translation("model id cannot be represented as JSON")
		}
		req["model"] = id
		changed = true
	}
	if raw, ok := req["input"]; ok {
		if rewritten, idsChanged := markInputCallIDs(raw); idsChanged {
			req["input"] = rewritten
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return nil, errclass.Translation("request body cannot be re-encoded as JSON")
	}
	return out, nil
}

// markInputCallIDs marks every replayed function_call/function_call_output id
// in a native Responses input array. Non-array input (a bare string) and
// arrays holding non-object entries are returned unchanged: shapes this
// adapter does not model stay the upstream's business rather than becoming a
// translation failure here. Non-id fields are carried as raw JSON, so only the
// rewritten ids are re-encoded.
func markInputCallIDs(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return raw, false
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return raw, false
	}
	changed := false
	for i, item := range items {
		var typeName string
		if err := json.Unmarshal(item["type"], &typeName); err != nil {
			continue
		}
		if typeName != "function_call" && typeName != "function_call_output" {
			continue
		}
		var id string
		if rawID, ok := item["call_id"]; ok {
			if err := json.Unmarshal(rawID, &id); err != nil {
				continue
			}
		}
		marked := shared.MarkToolCallID(id, i)
		if marked == id {
			continue
		}
		encoded, err := json.Marshal(marked)
		if err != nil {
			continue
		}
		item["call_id"] = encoded
		changed = true
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(items)
	if err != nil {
		return raw, false
	}
	return out, true
}

type responsesEnvelope struct {
	Model             string            `json:"model"`
	Input             []any             `json:"input,omitempty"`
	Instructions      string            `json:"instructions,omitempty"`
	Tools             []shared.RespTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Reasoning         map[string]any    `json:"reasoning,omitempty"`
	MaxOutputTokens   *int64            `json:"max_output_tokens,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
}

func msgItem(role string, parts []map[string]any) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": parts}
}

// textPart renders message text; assistant history replays with
// output_text, every other role uses input_text.
func textPart(role, text string) map[string]any {
	if role == "assistant" {
		return map[string]any{"type": "output_text", "text": text}
	}
	return map[string]any{"type": "input_text", "text": text}
}

func imagePart(url string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": url}
}

// respToolChoice renders a shared.DecodeToolChoice kind into the Responses
// wire value; absent yields nil so omitempty drops the field. It is
// infallible: shared.DecodeToolChoice emits exactly the closed set below,
// so the switch covers every possible kind (the exhaustiveness test in
// request_test.go enforces this against shared.AllToolChoiceKinds).
func respToolChoice(kind, name string) json.RawMessage {
	switch kind {
	case shared.ToolChoiceAuto:
		return json.RawMessage(`"auto"`)
	case shared.ToolChoiceAny:
		return json.RawMessage(`"required"`)
	case shared.ToolChoiceNone:
		return json.RawMessage(`"none"`)
	case shared.ToolChoiceNamed:
		b, _ := json.Marshal(map[string]any{"type": "function", "name": name})
		return b
	}
	return nil
}

// ---- Chat Completions source -------------------------------------------

// contentParts normalizes an OpenAI-style content field (JSON string or
// typed parts) into Responses input parts for the given role (FR-005
// multimodal preservation). The shared kernel owns validation and the
// empty-input policy: empty-text parts decode to zero parts so empty
// messages drop identically on every route (F5 parity).
func contentParts(raw json.RawMessage, role string) ([]map[string]any, *errclass.Error) {
	parts, eErr := shared.DecodeStringOrParts(raw, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.ImageURL != "" {
			out = append(out, imagePart(p.ImageURL))
			continue
		}
		out = append(out, textPart(role, p.Text))
	}
	return out, nil
}

// fromChatCompletions translates a Chat Completions request into a
// Responses request (FR-005, AC §D): system/developer messages become
// instructions, user/assistant turns become message items (assistant
// replaying output_text), tool_calls history becomes function_call items,
// tool results become function_call_output items, max_tokens/
// max_completion_tokens become max_output_tokens, and reasoning_effort is
// forwarded verbatim (no capability filtering: the upstream owns level
// acceptance).
// tool_choice is decoded by the shared classifier and rendered into the
// Responses wire values ("auto"/"none"/"required", flat
// {type:function,name} for a forced tool); parallel_tool_calls passes
// through as-is.
//
// Explicit omission policy (FR-005 "where compatible"): `stop` has no
// Responses equivalent and is dropped, as are sampling/extras without an
// equivalent (logprobs, frequency_penalty, presence_penalty, n, seed,
// response_format, logit_bias) via struct selection. Tool calls and
// reasoning controls are never dropped silently.
//
// ts is accepted for the executor's uniform adapter fan-out and is
// intentionally ignored here: declared levels pass through untouched.
func fromChatCompletions(upstreamModel string, source *shared.SourceRequest, _ *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	src, eErr := source.Chat()
	if eErr != nil {
		return nil, eErr
	}
	req := &responsesEnvelope{
		Model:             upstreamModel,
		Stream:            src.Stream,
		Temperature:       src.Temperature,
		TopP:              src.TopP,
		ParallelToolCalls: src.ParallelToolCalls,
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	req.ToolChoice = respToolChoice(kind, tcName)
	if src.MaxTokens != nil && *src.MaxTokens > 0 {
		req.MaxOutputTokens = src.MaxTokens
	} else if src.MaxCompletionTokens != nil && *src.MaxCompletionTokens > 0 {
		req.MaxOutputTokens = src.MaxCompletionTokens
	}
	if src.ReasoningEffort != "" {
		req.Reasoning = map[string]any{"effort": src.ReasoningEffort}
	}

	var instr strings.Builder
	callSeq := 0
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "system", "developer":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			// Instructions have no image representation: reject like the
			// sibling routes instead of dropping silently (FR-005).
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, shared.SystemImageRejected()
				}
			}
			if text := shared.JoinTexts(parts); text != "" {
				if instr.Len() > 0 {
					instr.WriteString("\n\n")
				}
				instr.WriteString(text)
			}
		case "user":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
		case "assistant":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
			for _, tc := range m.ToolCalls {
				callID := shared.MarkToolCallID(tc.ID, callSeq)
				callSeq++
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   callID,
					"name":      tc.Function.Name,
					"arguments": shared.DefaultArgs(tc.Function.Arguments),
				})
			}
		case "tool":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, errclass.Translation(fmt.Sprintf(
						"unsupported tool content part type %q; function_call_output carries text only", typ))
				}
			}
			req.Input = append(req.Input, map[string]any{
				"type":    "function_call_output",
				"call_id": shared.MarkToolCallID(m.ToolCallID, callSeq),
				"output":  shared.JoinTexts(parts),
			})
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
	}
	req.Instructions = instr.String()

	for _, t := range src.Tools {
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		req.Tools = append(req.Tools, shared.RespTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  shared.ObjectSchema(t.Function.Parameters),
		})
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}

// ---- Claude Messages source --------------------------------------------

// fromClaudeMessages translates an Anthropic Messages request into a
// Responses request (FR-005, AC §D): system becomes instructions, text/
// image blocks become input parts, tool_use/tool_result become
// function_call/function_call_output items, an enabled thinking budget
// maps to reasoning.effort (the Anthropic budget renders its canonical
// table level; capability filtering stays with the upstream).
//
// Explicit omission policy (FR-005 "where compatible"): stop_sequences is
// dropped (no Responses equivalent); thinking/redacted_thinking blocks are
// dropped because signed Anthropic chain-of-thought has no Responses input
// representation (the thinking *control* maps to reasoning.effort); images
// nested inside tool_result content are rejected descriptively, never
// silently lost (FR-005). The tool_result is_error flag has no Responses
// field and is preserved as an "[error] " marker in the output text rather
// than lost silently.
func fromClaudeMessages(upstreamModel string, source *shared.SourceRequest, _ *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	src, eErr := source.Claude()
	if eErr != nil {
		return nil, eErr
	}
	req := &responsesEnvelope{
		Model:           upstreamModel,
		MaxOutputTokens: &src.MaxTokens,
		Stream:          src.Stream,
		Temperature:     src.Temperature,
		TopP:            src.TopP,
	}
	req.ToolChoice = respToolChoice(src.ToolChoiceKind, src.ToolChoiceName)
	if src.OutputConfig != nil && src.OutputConfig.Effort != "" {
		req.Reasoning = map[string]any{"effort": src.OutputConfig.Effort}
	} else if shared.ThinkingEnabled(src.Thinking) {
		req.Reasoning = map[string]any{"effort": thinking.EffortFromBudget(src.Thinking.BudgetTokens)}
	}
	req.Instructions = src.System

	callSeq := 0
	for _, m := range src.Messages {
		switch m.Role {
		case "user", "assistant":
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
		var parts []map[string]any
		flush := func() {
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
				parts = nil
			}
		}
		if m.Content != "" {
			parts = append(parts, textPart(m.Role, m.Content))
		}
		for _, blk := range m.Blocks {
			switch blk.Kind {
			case "text":
				parts = append(parts, textPart(m.Role, blk.Text))
			case "image":
				parts = append(parts, imagePart(blk.URL))
			case "tool_use":
				flush()
				args := shared.DefaultArgs(string(blk.Input))
				callID := shared.MarkToolCallID(blk.CallID, callSeq)
				callSeq++
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   callID,
					"name":      blk.Name,
					"arguments": args,
				})
			case "tool_result":
				flush()
				output, eErr := shared.ToolResultText(blk.Result, "function_call_output carries text only")
				if eErr != nil {
					return nil, eErr
				}
				if blk.IsError {
					output = shared.ToolResultErrorPrefix + output
				}
				req.Input = append(req.Input, map[string]any{
					"type":    "function_call_output",
					"call_id": shared.MarkToolCallID(blk.CallID, callSeq),
					"output":  output,
				})
			case "thinking", "redacted_thinking":
				// omitted per the FR-005 policy in the doc comment
			default:
				flush()
				return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
			}
		}
		flush()
	}

	for _, t := range src.Tools {
		// Claude tools carry no type field (validation cannot reject here).
		req.Tools = append(req.Tools, shared.RespTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  shared.ObjectSchema(t.InputSchema),
		})
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}
