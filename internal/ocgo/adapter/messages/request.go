package messages

import (
	"encoding/json"
	"fmt"
	"strings"

	"free-model-hub/cpasdk/pluginapi"

	"free-model-hub/internal/ocgo/adapter/shared"
	"free-model-hub/internal/ocgo/errclass"
)

type anthropicBlock = map[string]any

func textBlock(text string) anthropicBlock {
	return anthropicBlock{"type": "text", "text": text}
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []anthropicBlock
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// messagesRequest is the outbound Anthropic encode shape — intentionally
// divergent from shared.ClaudeTool decode type: typed stop sequences +
// named items vs raw-ignore + tool_choice.
type messagesRequest struct {
	Model         string                     `json:"model"`
	MaxTokens     int64                      `json:"max_tokens"`
	System        any                        `json:"system,omitempty"` // string or []anthropicBlock
	Messages      []anthropicMessage         `json:"messages"`
	StopSequences []string                   `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool            `json:"tools,omitempty"`
	ToolChoice    any                        `json:"tool_choice,omitempty"` // {type,name?,disable_parallel_tool_use?}
	OutputConfig  *shared.ClaudeOutputConfig `json:"output_config,omitempty"`
	Stream        bool                       `json:"stream,omitempty"`
	Temperature   *float64                   `json:"temperature,omitempty"`
	TopP          *float64                   `json:"top_p,omitempty"`
}

// BuildRequest translates an inbound request body from sourceFormat
// ("openai" Chat Completions, "openai-response" Responses, "claude"
// passthrough) into an Anthropic Messages body for upstreamModel
// (FR-005). Declared efforts map unchanged to output_config.effort;
// capability metadata does not validate them or infer a thinking budget.
// Unknown formats are ClassUnsupported; malformed input is
// ClassTranslation. Errors are descriptive and redacted — no silent loss.
func BuildRequest(upstreamModel string, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	return BuildRequestFrom(upstreamModel, shared.NewSourceRequest(sourceFormat, sourceBody), ts)
}

// BuildRequestFrom is BuildRequest's entry point for callers that already own
// a lazy SourceRequest. The typed source representation decodes at most once
// per SourceRequest, so the executor can share the session-derivation decode
// with this conversion. Native "claude" passthrough keeps using the raw body.
func BuildRequestFrom(upstreamModel string, source *shared.SourceRequest, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	switch source.Format() {
	case "claude":
		return shared.RewriteModelID(upstreamModel, source.Raw(), "claude")
	case "openai":
		return fromChatCompletions(upstreamModel, source, ts)
	case "openai-response":
		return fromResponses(upstreamModel, source, ts)
	default:
		return nil, shared.UnsupportedFormat(source.Format(), EndpointPath)
	}
}

// contentParts normalizes an OpenAI-style content field into Anthropic
// blocks via the shared kernel, which owns per-part ordering, validation,
// and the empty-input policy (FR-005 multimodal preservation, F5 parity).
func contentParts(raw json.RawMessage) ([]anthropicBlock, *errclass.Error) {
	return shared.ClaudeBlocksFromOpenAI(raw, EndpointPath)
}

// hasImage reports whether any normalized content block carries image
// content (FR-005).
func hasImage(blocks []anthropicBlock) bool {
	for _, blk := range blocks {
		if blk["type"] == "image" {
			return true
		}
	}
	return false
}

// decodeStop accepts the OpenAI stop field as a string or array of strings.
func decodeStop(raw json.RawMessage) ([]string, *errclass.Error) {
	if !shared.HasContent(raw) {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, errclass.Translation("stop must be a string or an array of strings")
	}
	return many, nil
}

// systemField renders collected system text as a single string when one
// piece, or text blocks when several (AC §C: system represented correctly).
func systemField(parts []string) any {
	switch len(parts) {
	case 0:
		return nil
	case 1:
		return parts[0]
	default:
		blocks := make([]anthropicBlock, len(parts))
		for i, p := range parts {
			blocks[i] = textBlock(p)
		}
		return blocks
	}
}

// msgBuilder groups consecutive blocks into alternating-role Anthropic
// messages. Buckets with distinct keys merge only when both canonical
// roles are "user" (tool_result + later user text in one turn) — the
// Messages contract rejects consecutive same-role messages.
type msgBuilder struct {
	curKey string
	cur    *anthropicMessage
	msgs   []anthropicMessage
}

func (b *msgBuilder) add(key, role string, block anthropicBlock) {
	if b.cur == nil || (b.curKey != key && !(role == "user" && b.cur.Role == "user")) {
		b.flush()
		b.cur = &anthropicMessage{Role: role, Content: []anthropicBlock{}}
		b.curKey = key
	}
	b.cur.Content = append(b.cur.Content.([]anthropicBlock), block)
}

func (b *msgBuilder) flush() {
	if b.cur == nil {
		return
	}
	blocks := b.cur.Content.([]anthropicBlock)
	if len(blocks) == 1 && blocks[0]["type"] == "text" {
		b.cur.Content = blocks[0]["text"]
	}
	b.msgs = append(b.msgs, *b.cur)
	b.cur = nil
	b.curKey = ""
}

// finalize renders declared effort using Anthropic's output_config.effort.
// Preserve the exact value, including unknown levels, and let the upstream
// decide whether it is valid. Do not infer budgets or adjust sampling.
func finalize(req *messagesRequest, system []string, effort string, _ *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	req.System = systemField(system)
	if req.Messages == nil {
		req.Messages = []anthropicMessage{}
	}
	if effort != "" {
		req.OutputConfig = &shared.ClaudeOutputConfig{Effort: effort}
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}

// fromChatCompletions translates a Chat Completions request into a
// Messages request (FR-005, AC §C): leading system/developer messages
// become the top-level system field, assistant tool_calls become tool_use
// blocks, tool results become user tool_result blocks, stop becomes
// stop_sequences, tools become input_schema definitions, reasoning_effort
// passes through unchanged as output_config.effort.
//
// Explicit omission policy (FR-005): fields with no Messages equivalent
// (logprobs, frequency_penalty, n, ...) are omitted by struct selection;
// everything representable is mapped or rejected descriptively.
func fromChatCompletions(upstreamModel string, source *shared.SourceRequest, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	src, eErr := source.Chat()
	if eErr != nil {
		return nil, eErr
	}
	// The shared kernel owns the FR-005 defaulting policy: a positive
	// max_tokens (or max_completion_tokens fallback) passes through;
	// absent, zero, or negative defaults to 4096.
	maxTokens := int64(0)
	if src.MaxTokens != nil {
		maxTokens = *src.MaxTokens
	} else if src.MaxCompletionTokens != nil {
		maxTokens = *src.MaxCompletionTokens
	}
	req := &messagesRequest{
		Model:       upstreamModel,
		MaxTokens:   shared.ClaudeMaxTokens(maxTokens),
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}
	stop, eErr := decodeStop(src.Stop)
	if eErr != nil {
		return nil, eErr
	}
	req.StopSequences = stop

	var system []string
	var b msgBuilder
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "system", "developer":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			if hasImage(blocks) {
				return nil, shared.SystemImageRejected()
			}
			system = append(system, shared.JoinTexts(blocks))
		case "assistant":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			if hasImage(blocks) {
				return nil, errclass.Translation("assistant messages cannot carry image content")
			}
			if text := shared.JoinTexts(blocks); text != "" {
				b.add("assistant", "assistant", textBlock(text))
			}
			for _, tc := range m.ToolCalls {
				input, eErr := shared.DecodeArgs(tc.Function.Arguments)
				if eErr != nil {
					return nil, eErr
				}
				b.add("assistant", "assistant", anthropicBlock{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
		case "tool":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			content := any(shared.JoinTexts(blocks))
			if hasImage(blocks) {
				content = blocks // mixed parts keep their per-part order
			}
			b.add("user:tool", "user", anthropicBlock{
				"type": "tool_result", "tool_use_id": m.ToolCallID, "content": content,
			})
		case "user":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			for _, blk := range blocks { // part order is preserved (FR-005)
				b.add("user", "user", blk)
			}
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
	}
	b.flush()
	req.Messages = b.msgs

	for _, t := range src.Tools {
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		req.Tools = append(req.Tools, anthropicTool{
			Name: t.Function.Name, Description: t.Function.Description,
			InputSchema: shared.ObjectSchema(t.Function.Parameters),
		})
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	if eErr := applyToolChoiceMessages(req, kind, tcName, src.ParallelToolCalls); eErr != nil {
		return nil, eErr
	}
	return finalize(req, system, src.ReasoningEffort, ts)
}

// fromResponses translates an OpenAI Responses request into a Messages
// request (FR-005): instructions become the system field, message items
// map like Chat Completions messages, function_call/function_call_output
// items map to tool_use/tool_result blocks, reasoning summaries are kept
// as best-effort thinking blocks (signatures unavailable upstream).
func fromResponses(upstreamModel string, source *shared.SourceRequest, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	src, eErr := source.Responses()
	if eErr != nil {
		return nil, eErr
	}
	// Same shared kernel policy as the Chat Completions leg (FR-005).
	maxTokens := int64(0)
	if src.MaxOutputTokens != nil {
		maxTokens = *src.MaxOutputTokens
	}
	req := &messagesRequest{
		Model:       upstreamModel,
		MaxTokens:   shared.ClaudeMaxTokens(maxTokens),
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}

	var system []string
	instr, eErr := src.DecodeInstructions()
	if eErr != nil {
		return nil, eErr
	}
	if instr != "" {
		system = append(system, instr)
	}

	var b msgBuilder
	items, eErr := source.ResponseItems()
	if eErr != nil {
		return nil, eErr
	}
	for _, item := range items {
		switch item.Type {
		case "message":
			blocks, eErr := contentParts(item.Content)
			if eErr != nil {
				return nil, eErr
			}
			switch item.Role {
			case "user":
				for _, blk := range blocks { // part order is preserved (FR-005)
					b.add("user", "user", blk)
				}
			case "assistant":
				if hasImage(blocks) {
					return nil, errclass.Translation("assistant messages cannot carry image content")
				}
				if text := shared.JoinTexts(blocks); text != "" {
					b.add("assistant", "assistant", textBlock(text))
				}
			case "system", "developer":
				if hasImage(blocks) {
					return nil, shared.SystemImageRejected()
				}
				system = append(system, shared.JoinTexts(blocks))
			default:
				return nil, shared.ValidateRole(item.Role, EndpointPath)
			}
		case "function_call":
			input, eErr := shared.DecodeArgs(item.Arguments)
			if eErr != nil {
				return nil, eErr
			}
			b.add("assistant", "assistant", anthropicBlock{
				"type": "tool_use", "id": item.CallID, "name": item.Name, "input": input,
			})
		case "function_call_output":
			blocks, eErr := contentParts(item.Output)
			if eErr != nil {
				return nil, eErr
			}
			content := any(shared.JoinTexts(blocks))
			if hasImage(blocks) {
				content = blocks // preserve images inside the associated tool result
			}
			b.add("user:tool", "user", anthropicBlock{
				"type": "tool_result", "tool_use_id": item.CallID, "content": content,
			})
		case "reasoning":
			// Best-effort: keep summary text as a thinking block;
			// encrypted_content has no Anthropic equivalent, so the
			// signature is omitted rather than fabricated (FR-005).
			var sb strings.Builder
			for i, s := range item.Summary {
				if i > 0 {
					sb.WriteString("\n")
				}
				if s.Text != nil {
					sb.WriteString(*s.Text)
				}
			}
			if sb.Len() > 0 {
				b.add("assistant", "assistant", anthropicBlock{
					"type": "thinking", "thinking": sb.String(),
				})
			}
		default:
			return nil, shared.UnsupportedInputItemType(item.Type)
		}
	}
	b.flush()
	req.Messages = b.msgs

	for _, t := range src.Tools {
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		schema := shared.ObjectSchema(t.Parameters)
		req.Tools = append(req.Tools, anthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: schema,
		})
	}
	effort := ""
	if src.Reasoning != nil {
		effort = src.Reasoning.Effort
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	if eErr := applyToolChoiceMessages(req, kind, tcName, src.ParallelToolCalls); eErr != nil {
		return nil, eErr
	}
	return finalize(req, system, effort, ts)
}

// applyToolChoiceMessages maps a normalized tool choice onto the outbound
// Messages request. "none" has no Anthropic equivalent and fails
// descriptively rather than silently unforcing the client constraint
// (FR-005 explicit policy). An explicit parallel_tool_calls=false is a
// client constraint too (FR-005): it becomes disable_parallel_tool_use on
// the synthesized or decoded tool_choice — synthesized as {"type":"auto"}
// when no tool_choice was sent, so the flag never rides alone.
func applyToolChoiceMessages(req *messagesRequest, kind, name string, parallel *bool) *errclass.Error {
	noParallel := parallel != nil && !*parallel
	if kind == shared.ToolChoiceAbsent && noParallel {
		kind = shared.ToolChoiceAuto
	}
	switch kind {
	case shared.ToolChoiceAbsent:
		return nil
	case shared.ToolChoiceAuto:
		req.ToolChoice = toolChoiceBlock("auto", "", noParallel)
	case shared.ToolChoiceAny:
		req.ToolChoice = toolChoiceBlock("any", "", noParallel)
	case shared.ToolChoiceNamed:
		req.ToolChoice = toolChoiceBlock("tool", name, noParallel)
	default:
		return &errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("tool_choice %q cannot be represented for %s", kind, EndpointPath),
		}
	}
	return nil
}

// toolChoiceBlock renders one outbound tool_choice object; noParallel is
// Anthropic's disable_parallel_tool_use flag.
func toolChoiceBlock(typ, name string, noParallel bool) map[string]any {
	tc := map[string]any{"type": typ}
	if name != "" {
		tc["name"] = name
	}
	if noParallel {
		tc["disable_parallel_tool_use"] = true
	}
	return tc
}
