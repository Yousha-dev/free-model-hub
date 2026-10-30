package workbuddy

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"free-model-hub/internal/core"
)

// uuidV7HexRe is the official client's hard-validation regex for conversationRequestId
// (bundle offset ~7309100): 32-digit hexadecimal form of lowercase UUIDv7.
var uuidV7HexRe = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)

// TestNewUUIDV7HexFormat verifies that the generated session ID is a valid hyphenless UUIDv7.
func TestNewUUIDV7HexFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewUUIDv7Hex()
		if len(id) != 32 {
			t.Fatalf("id length = %d, want 32", len(id))
		}
		if !uuidV7HexRe.MatchString(id) {
			t.Fatalf("id %q does not match UUIDv7 hex pattern", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

// TestChatHeadersMatchGenuineClient asserts that chat request headers match the genuine CLI direct-send format:
// Three-segment version UA, attribution headers (X-Product=SaaS, etc.), UUIDv7 session family, W3C trace family,
// and without a set of headers that the genuine client does not send (Origin/Referer/device fingerprint family, etc.).
func TestChatHeadersMatchGenuineClient(t *testing.T) {
	rt := newRuntime(core.Env{})
	rt.configure(core.Settings{MachineSalt: "salt-1"}, map[string]string{})
	cred := &Credential{AccessToken: "token", UID: "a707ecea-6211-4e4e-a1b4-13335f8ed5bc", realm: RegionCN}
	c := NewClient(rt, Options{})

	req, errReq := http.NewRequest(http.MethodPost, GetEndpoints(RegionCN).ChatBase+chatCompletionsPath, nil)
	if errReq != nil {
		t.Fatal(errReq)
	}
	meta := ChatMetaFromBody([]byte(`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":"hi"}]}`))
	c.applyChatHeaders(req, cred, meta, "")

	// Attribution and version.
	ua := req.Header.Get("User-Agent")
	if ua != "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0" {
		t.Fatalf("User-Agent = %q", ua)
	}
	for key, want := range map[string]string{
		"X-Product":           "SaaS",
		"X-IDE-Type":          "WorkBuddy",
		"X-IDE-Name":          "WorkBuddy",
		"X-IDE-Version":       "5.8.3",
		"X-Agent-Intent":      "craft",
		"X-Agent-Purpose":     "conversation",
		"X-Agent-Type":        "main",
		"X-Private-Data":      "false",
		"X-User-Id":           cred.UID,
		"X-Domain":            "www.workbuddy.cn",
		"X-CodeBuddy-Request": "1",
	} {
		if got := req.Header.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	// Session family: UUIDv7 form, Request-ID and Message-ID have the same value.
	for _, key := range []string{"X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID"} {
		if got := req.Header.Get(key); !uuidV7HexRe.MatchString(got) {
			t.Fatalf("%s = %q is not a UUIDv7 hex", key, got)
		}
	}
	if req.Header.Get("X-Conversation-Message-ID") != req.Header.Get("X-Request-ID") {
		t.Fatal("X-Conversation-Message-ID and X-Request-ID must share the message id")
	}
	if req.Header.Get("X-Conversation-Request-ID") == req.Header.Get("X-Conversation-Message-ID") {
		t.Fatal("conversation request id must differ from the message id")
	}

	// Trace family: source-homogenous random, session ID is not reused.
	traceID := req.Header.Get("X-Trace-ID")
	spanID := req.Header.Get("X-B3-SpanId")
	parentSpanID := req.Header.Get("X-B3-ParentSpanId")
	if len(traceID) != 32 || len(spanID) != 16 || len(parentSpanID) != 16 {
		t.Fatalf("trace id lengths = %d/%d/%d", len(traceID), len(spanID), len(parentSpanID))
	}
	if req.Header.Get("traceparent") != "00-"+traceID+"-"+spanID+"-01" {
		t.Fatalf("traceparent = %q", req.Header.Get("traceparent"))
	}
	if req.Header.Get("b3") != traceID+"-"+spanID+"-1-"+parentSpanID {
		t.Fatalf("b3 = %q", req.Header.Get("b3"))
	}
	if req.Header.Get("X-B3-TraceId") != traceID || req.Header.Get("X-B3-Sampled") != "1" {
		t.Fatal("X-B3-TraceId/X-B3-Sampled mismatch")
	}
	if traceID == req.Header.Get("X-Conversation-Request-ID") {
		t.Fatal("trace id must not reuse the conversation request id")
	}

	// Headers not sent by genuine chat requests.
	for _, key := range []string{
		"Origin", "Referer", "Accept-Language",
		"X-Device-Token", "X-Machine-ID", "X-Session-ID", "X-Root-Request-ID",
	} {
		if got := req.Header.Get(key); got != "" {
			t.Fatalf("%s must not be sent on chat, got %q", key, got)
		}
	}
	if !strings.Contains(req.Header.Get("Accept"), "text/event-stream") {
		t.Fatalf("Accept = %q", req.Header.Get("Accept"))
	}

	// Second construction: both trace family and turn ID are new.
	second, errSecond := http.NewRequest(http.MethodPost, req.URL.String(), nil)
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	c.applyChatHeaders(second, cred, ChatMetaFromBody([]byte(`{"model":"kimi-k3.1-pro"}`)), "")
	if second.Header.Get("X-Trace-ID") == traceID {
		t.Fatal("trace id must be regenerated per request")
	}
}
