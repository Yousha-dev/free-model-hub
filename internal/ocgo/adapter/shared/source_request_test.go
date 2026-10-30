package shared

import (
	"testing"

	"free-model-hub/internal/ocgo/errclass"
)

// TestCacheSlotDecodeRunsOnce proves the memoizing helper underlying every
// SourceRequest getter: a decode closure runs at most once and both a value
// and a failure are retained across repeated access. A nil slice result is
// distinguishable from "not yet decoded" because readiness is tracked
// separately from the value.
func TestCacheSlotDecodeRunsOnce(t *testing.T) {
	var valueCalls, errCalls, nilCalls int

	valueSlot := &cacheSlot[int]{}
	for i := 0; i < 3; i++ {
		got, eErr := valueSlot.get(func() (int, *errclass.Error) {
			valueCalls++
			return 7, nil
		})
		if eErr != nil || got != 7 {
			t.Fatalf("value get %d = %d, %v; want 7", i, got, eErr)
		}
	}
	if valueCalls != 1 {
		t.Fatalf("value decode calls = %d, want 1", valueCalls)
	}

	boom := errclass.Translation("boom")
	errSlot := &cacheSlot[int]{}
	for i := 0; i < 3; i++ {
		got, eErr := errSlot.get(func() (int, *errclass.Error) {
			errCalls++
			return 0, boom
		})
		if eErr != boom || got != 0 {
			t.Fatalf("error get %d = %d, %v; want cached %v", i, got, eErr, boom)
		}
	}
	if errCalls != 1 {
		t.Fatalf("error decode calls = %d, want 1", errCalls)
	}

	nilSlot := &cacheSlot[[]RespItem]{}
	for i := 0; i < 3; i++ {
		got, eErr := nilSlot.get(func() ([]RespItem, *errclass.Error) {
			nilCalls++
			return nil, nil
		})
		if eErr != nil || got != nil {
			t.Fatalf("nil get %d = %v, %v; want nil, nil", i, got, eErr)
		}
		if !nilSlot.ready {
			t.Fatalf("nil get %d left ready=false; decoded nil is not undecoded", i)
		}
	}
	if nilCalls != 1 {
		t.Fatalf("nil decode calls = %d, want 1", nilCalls)
	}
}

// TestNewSourceRequestPerformsNoParsing proves construction is lazy: a body
// that would fail every typed decoder leaves every cache cold until a getter
// is consulted, and Raw/Format never touch a decoder.
func TestNewSourceRequestPerformsNoParsing(t *testing.T) {
	source := NewSourceRequest("openai-response", []byte(`{not json at all`))
	if source.Format() != "openai-response" {
		t.Fatalf("Format = %q", source.Format())
	}
	if string(source.Raw()) != `{not json at all` {
		t.Fatalf("Raw = %q", source.Raw())
	}
	if source.chat.ready || source.claude.ready || source.responses.ready || source.responseItems.ready {
		t.Fatal("construction or Raw/Format decoded the body")
	}
	if _, eErr := source.Chat(); eErr == nil {
		t.Fatal("Chat on malformed body must fail")
	}
	if !source.chat.ready || source.claude.ready || source.responses.ready || source.responseItems.ready {
		t.Fatal("Chat must populate only its own cache")
	}
}

// TestSourceRequestGettersReuseResults proves each getter returns the same
// decoded object on repeated access instead of re-parsing.
func TestSourceRequestGettersReuseResults(t *testing.T) {
	chatBody := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	chatSource := NewSourceRequest("openai", chatBody)
	chat1, eErr := chatSource.Chat()
	if eErr != nil {
		t.Fatalf("Chat: %v", eErr)
	}
	chat2, eErr := chatSource.Chat()
	if eErr != nil || chat1 != chat2 {
		t.Fatalf("Chat not reused: %p vs %p (%v)", chat1, chat2, eErr)
	}

	claudeBody := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	claudeSource := NewSourceRequest("claude", claudeBody)
	claude1, eErr := claudeSource.Claude()
	if eErr != nil {
		t.Fatalf("Claude: %v", eErr)
	}
	claude2, eErr := claudeSource.Claude()
	if eErr != nil || claude1 != claude2 {
		t.Fatalf("Claude not reused: %p vs %p (%v)", claude1, claude2, eErr)
	}

	respBody := []byte(`{"input":[{"type":"message","role":"user","content":"hi"}]}`)
	respSource := NewSourceRequest("openai-response", respBody)
	env1, eErr := respSource.Responses()
	if eErr != nil {
		t.Fatalf("Responses: %v", eErr)
	}
	env2, eErr := respSource.Responses()
	if eErr != nil || env1 != env2 {
		t.Fatalf("Responses not reused: %p vs %p (%v)", env1, env2, eErr)
	}
	items1, eErr := respSource.ResponseItems()
	if eErr != nil || len(items1) != 1 {
		t.Fatalf("ResponseItems = %v, %v", items1, eErr)
	}
	items2, eErr := respSource.ResponseItems()
	if eErr != nil || &items1[0] != &items2[0] {
		t.Fatalf("ResponseItems not reused: %p vs %p (%v)", &items1[0], &items2[0], eErr)
	}
}

// TestSourceRequestCachesEmptyAndErrorResults proves decoded-empty results and
// failures are retained: neither an empty body nor a malformed one is parsed
// a second time.
func TestSourceRequestCachesEmptyAndErrorResults(t *testing.T) {
	for _, format := range []string{"openai", "claude", "openai-response"} {
		source := NewSourceRequest(format, []byte(`{}`))
		switch format {
		case "openai":
			first, eErr := source.Chat()
			if eErr != nil || first == nil {
				t.Fatalf("%s empty Chat = %v, %v", format, first, eErr)
			}
			second, _ := source.Chat()
			if first != second {
				t.Fatalf("%s empty Chat not cached", format)
			}
		case "claude":
			first, eErr := source.Claude()
			if eErr != nil || first == nil {
				t.Fatalf("%s empty Claude = %v, %v", format, first, eErr)
			}
			second, _ := source.Claude()
			if first != second {
				t.Fatalf("%s empty Claude not cached", format)
			}
		case "openai-response":
			first, eErr := source.Responses()
			if eErr != nil || first == nil {
				t.Fatalf("%s empty Responses = %v, %v", format, first, eErr)
			}
			second, _ := source.Responses()
			if first != second {
				t.Fatalf("%s empty Responses not cached", format)
			}
			items, eErr := source.ResponseItems()
			if eErr != nil || items != nil {
				t.Fatalf("%s empty items = %v, %v", format, items, eErr)
			}
			if !source.responseItems.ready {
				t.Fatalf("%s empty items not marked ready", format)
			}
		}
	}

	malformed := NewSourceRequest("openai", []byte(`{"messages":`))
	first, firstErr := malformed.Chat()
	second, secondErr := malformed.Chat()
	if first != nil || firstErr == nil || firstErr != secondErr {
		t.Fatalf("malformed Chat not cached: %v/%v vs %v/%v", first, firstErr, second, secondErr)
	}
}

// TestSourceRequestResponsesEnvelopeAndItemsIndependent proves the envelope
// and input-item decodes stay separate: an envelope with a malformed input
// field decodes cleanly, and only ResponseItems reports the failure. This is
// what keeps a converter's tool-choice/instructions validation ahead of the
// input-item error, exactly as before the cache existed.
func TestSourceRequestResponsesEnvelopeAndItemsIndependent(t *testing.T) {
	source := NewSourceRequest("openai-response", []byte(`{"model":"x","input":{"unexpected":true}}`))
	env, eErr := source.Responses()
	if eErr != nil || env == nil {
		t.Fatalf("envelope decode = %v, %v", env, eErr)
	}
	if source.responseItems.ready {
		t.Fatal("Responses must not decode input items")
	}
	if _, eErr := source.ResponseItems(); eErr == nil {
		t.Fatal("ResponseItems must report the malformed input")
	}
}

// TestSourceRequestErrorClassificationPinned pins the exact wording every
// getter surfaces, since session derivation now shares these errors with the
// adapters and any drift would change request-visible messages.
func TestSourceRequestErrorClassificationPinned(t *testing.T) {
	cases := []struct {
		name   string
		get    func(*SourceRequest) (bool, *errclass.Error)
		format string
		body   string
		want   string
	}{
		{"chat", func(s *SourceRequest) (bool, *errclass.Error) { _, e := s.Chat(); return e != nil, e }, "openai", `{"messages":`, "malformed openai request JSON: unexpected end of JSON input"},
		{"claude", func(s *SourceRequest) (bool, *errclass.Error) { _, e := s.Claude(); return e != nil, e }, "claude", `{"messages":`, "malformed claude request JSON: unexpected end of JSON input"},
		{"responses", func(s *SourceRequest) (bool, *errclass.Error) { _, e := s.Responses(); return e != nil, e }, "openai-response", `{"input":`, "malformed openai-response request JSON: unexpected end of JSON input"},
		{"items", func(s *SourceRequest) (bool, *errclass.Error) { _, e := s.ResponseItems(); return e != nil, e }, "openai-response", `{"input":{"bad":1}}`, "input must be a string or an array of items: json: cannot unmarshal object into Go value of type []shared.RespItem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := NewSourceRequest(tc.format, []byte(tc.body))
			failed, eErr := tc.get(source)
			if !failed || eErr == nil {
				t.Fatalf("expected failure, got failed=%t err=%v", failed, eErr)
			}
			if eErr.Class != errclass.ClassTranslation {
				t.Fatalf("class = %s, want %s", eErr.Class, errclass.ClassTranslation)
			}
			if eErr.Message != tc.want {
				t.Fatalf("message = %q, want %q", eErr.Message, tc.want)
			}
		})
	}
}

// Guard that a cached error keeps its identity: the same pointer is returned
// on every access, which callers may rely on for stable reporting.
func TestSourceRequestCachedErrorIdentity(t *testing.T) {
	source := NewSourceRequest("openai", []byte(`{`))
	_, first := source.Chat()
	_, second := source.Chat()
	if first == nil || first != second {
		t.Fatalf("cached error identity = %v / %v", first, second)
	}
}
