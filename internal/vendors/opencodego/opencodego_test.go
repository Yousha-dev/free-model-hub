package opencodego

import (
	"encoding/json"
	"strings"
	"testing"

	"free-model-hub/internal/ocgo/catalog"
)

type captureSink struct{ payloads []string }

func (s *captureSink) Emit(payload []byte) error {
	s.payloads = append(s.payloads, string(payload))
	return nil
}

func (s *captureSink) Close(string) error { return nil }

func TestEmitEventsUnwrapsSSEFrames(t *testing.T) {
	sink := &captureSink{}
	events := [][]byte{
		[]byte("data: {\"a\":1}\n\n"),
		[]byte("event: ping\ndata: {\"b\":2}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}
	if err := emitEvents(sink, events); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sink.payloads, "|") != `{"a":1}|{"b":2}` {
		t.Fatalf("unexpected payloads: %v", sink.payloads)
	}
}

func TestBuildRequestTranslatesChatToMessages(t *testing.T) {
	rec := catalog.ModelRecord{UpstreamID: "minimax-m3", Protocol: catalog.RouteMessages}
	body, eErr := buildRequest(rec, []byte(`{"model":"opencode-go/minimax-m3","max_tokens":64,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`), true)
	if eErr != nil {
		t.Fatalf("translate: %v", eErr)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "minimax-m3" || out["stream"] != true {
		t.Fatalf("unexpected messages body: %s", body)
	}
	if _, ok := out["system"]; !ok {
		t.Fatalf("system prompt should move to the messages system field: %s", body)
	}
}

func TestBuildRequestKeepsChatRoute(t *testing.T) {
	rec := catalog.ModelRecord{UpstreamID: "glm-5.2", Protocol: catalog.RouteChatCompletions}
	body, eErr := buildRequest(rec, []byte(`{"model":"opencode-go/glm-5.2","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`), false)
	if eErr != nil {
		t.Fatalf("translate: %v", eErr)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "glm-5.2" || out["stream"] != false {
		t.Fatalf("unexpected chat body: %s", body)
	}
}

func TestSessionIDIsStableAcrossTurns(t *testing.T) {
	first := sessionID([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"q1"}]}`))
	later := sessionID([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"q1"},{"role":"assistant","content":"a"},{"role":"user","content":"q2"}]}`))
	if first != later || len(first) != 64 {
		t.Fatalf("session id should depend only on the opening messages: %s vs %s", first, later)
	}
}
