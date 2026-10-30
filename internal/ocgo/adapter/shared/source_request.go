package shared

import (
	"encoding/json"

	"free-model-hub/internal/ocgo/errclass"
)

// cacheSlot memoizes one lazy decode for the lifetime of a SourceRequest:
// decode runs at most once, and both a successful value and a failure are
// retained. ready separates "decoded to a nil value" from "not yet decoded",
// so an empty request body is never re-parsed.
type cacheSlot[T any] struct {
	ready bool
	value T
	eErr  *errclass.Error
}

func (c *cacheSlot[T]) get(decode func() (T, *errclass.Error)) (T, *errclass.Error) {
	if !c.ready {
		c.value, c.eErr = decode()
		c.ready = true
	}
	return c.value, c.eErr
}

// SourceRequest is a request-local, lazily-decoded view of one inbound
// request body. It exists so a single execution decodes each typed source
// representation at most once: session-ID derivation and the adapter
// conversion share the same object instead of re-parsing the body.
//
// The body is never rewritten; Raw always returns the original bytes. A
// SourceRequest is used synchronously during request preparation and is
// neither shared across requests nor safe for concurrent use. Consumers
// must treat every returned value as read-only.
type SourceRequest struct {
	format string
	raw    []byte

	chat          cacheSlot[*ChatCompletionsRequest]
	claude        cacheSlot[*ClaudeRequestRecord]
	responses     cacheSlot[*ResponsesRequest]
	responseItems cacheSlot[[]RespItem]
}

// NewSourceRequest binds a request body to its source format. It performs no
// parsing, validation, or format checking: the body is only decoded when a
// getter is called.
func NewSourceRequest(format string, body []byte) *SourceRequest {
	return &SourceRequest{format: format, raw: body}
}

// Format returns the source protocol name the request was constructed with.
func (s *SourceRequest) Format() string { return s.format }

// Raw returns the original request body bytes, never a rewritten copy.
func (s *SourceRequest) Raw() []byte { return s.raw }

// Chat decodes the body as a Chat Completions request (the openai source
// format). The result and any translation failure are cached, so repeated
// access re-parses nothing. The decoded value is read-only.
func (s *SourceRequest) Chat() (*ChatCompletionsRequest, *errclass.Error) {
	return s.chat.get(func() (*ChatCompletionsRequest, *errclass.Error) {
		var req ChatCompletionsRequest
		if err := json.Unmarshal(s.raw, &req); err != nil {
			return nil, errclass.Translation("malformed openai request JSON: " + err.Error())
		}
		return &req, nil
	})
}

// Claude decodes the body as an Anthropic Messages request (the claude source
// format) through the shared Claude-request kernel, caching both the record
// and any translation failure. The decoded value is read-only.
func (s *SourceRequest) Claude() (*ClaudeRequestRecord, *errclass.Error) {
	return s.claude.get(func() (*ClaudeRequestRecord, *errclass.Error) {
		return DecodeClaudeMessages(s.raw)
	})
}

// Responses decodes the body as a Responses request envelope (the
// openai-response source format), caching both the envelope and any
// translation failure. It is deliberately separate from ResponseItems so a
// converter that validates tool choice or instructions before touching input
// items keeps the existing error order. The decoded value is read-only.
func (s *SourceRequest) Responses() (*ResponsesRequest, *errclass.Error) {
	return s.responses.get(func() (*ResponsesRequest, *errclass.Error) {
		var req ResponsesRequest
		if err := json.Unmarshal(s.raw, &req); err != nil {
			return nil, errclass.Translation("malformed openai-response request JSON: " + err.Error())
		}
		return &req, nil
	})
}

// ResponseItems decodes the envelope's input field into item form through the
// shared DecodeInputItems kernel, caching the item slice and any failure. The
// envelope is decoded on demand and itself cached, so a caller never decodes
// the body twice. The returned slice is read-only to consumers.
func (s *SourceRequest) ResponseItems() ([]RespItem, *errclass.Error) {
	return s.responseItems.get(func() ([]RespItem, *errclass.Error) {
		env, eErr := s.Responses()
		if eErr != nil {
			return nil, eErr
		}
		return env.DecodeInputItems()
	})
}
