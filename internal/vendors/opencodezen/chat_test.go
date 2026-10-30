package opencodezen

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"testing"
)

// This file locks down the encoding rules for the ZEN session ID.
//
// This encoding is a hard threshold for the free tier: early implementations filled the time part with random hex,
// which matched the shape but had the wrong encoding, causing the upstream to always return 403 FreeTierError,
// and the error message gave no indication that it was an encoding issue. Therefore, we assert the "shape" and "time part encoding" separately here.

func TestSessionIDShape(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	id := sessionIDFor(body)

	if !canonicalSessionPattern.MatchString(id) {
		t.Fatalf("session %q does not match canonical format", id)
	}
	if !descendingTimePartPattern.MatchString(id[4:16]) {
		t.Fatalf("session %q 的时间部分 %q 不是 descending 编码（免费层会 403）", id, id[4:16])
	}
	// Random signals must also fall within the same shape (fallback path when the conversation cannot obtain a seed).
	if got := sessionIDFor([]byte(`{"messages":[]}`)); !canonicalSessionPattern.MatchString(got) ||
		!descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("fallback session %q is non-compliant", got)
	}
	// Non-JSON request bodies must also not panic, and must produce a compliant session.
	if got := sessionIDFor([]byte("not json")); !canonicalSessionPattern.MatchString(got) ||
		!descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("non-JSON request body session %q is non-compliant", got)
	}
}

// TestSessionIDStablePerConversation verifies that the same conversation yields the same session.
//
// If the entire history is used as a seed, the session will change for every turn in multi-turn conversations, making the upstream prompt cache
// impossible to hit; this pins it down to "looking only at the first user message".
func TestSessionIDStablePerConversation(t *testing.T) {
	first := []byte(`{"messages":[{"role":"user","content":"讲个笑话"},{"role":"assistant","content":"好的"}]}`)
	grown := []byte(`{"messages":[{"role":"user","content":"讲个笑话"},{"role":"assistant","content":"好的"},` +
		`{"role":"user","content":"再来一个"},{"role":"assistant","content":"好"}]}`)
	if sessionIDFor(first) != sessionIDFor(grown) {
		t.Fatal("session changed for the same conversation after history grew, prompt cache will be invalidated")
	}

	other := []byte(`{"messages":[{"role":"user","content":"讲个别的"}]}`)
	if sessionIDFor(first) == sessionIDFor(other) {
		t.Fatal("different conversations got the same session")
	}
}

// TestCanonicalSessionIDPreservesOfficial verifies that an officially minted session is preserved as-is.
func TestCanonicalSessionIDPreservesOfficial(t *testing.T) {
	// descendingTimePart produces a time part ending in ffe for any hash.
	official := canonicalSessionID("any-signal")
	if got := canonicalSessionID(official); got != official {
		t.Fatalf("official format session was re-encoded: %q -> %q", official, got)
	}
	// When the shape is correct but the encoding is wrong (random hex), it must be re-encoded, otherwise the upstream returns 403.
	randomHex := "ses_" + "0123456789ab" + "ABCDEFGHIJKLMN"
	if !canonicalSessionPattern.MatchString(randomHex) {
		t.Fatal("test sample itself is invalid")
	}
	if got := canonicalSessionID(randomHex); !descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("random time part was not re-encoded: %q", got)
	}
}

// TestDescendingTimePartEndsWithFFE verifies that the lower 12 bits of the time part fall on ffe.
func TestDescendingTimePartEndsWithFFE(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{9}ffe$`)
	for _, seed := range []string{"a", "b", "hello world", "\x00\x01", "讲个笑话"} {
		id := canonicalSessionID(seed)
		if !pattern.MatchString(id[4:16]) {
			t.Errorf("seed %q 的时间部分 %q 未以 ffe 收尾", seed, id[4:16])
		}
	}
}

// TestUserAgentMatchesV2Shape locks down the v2 client identifier.
//
// The 1.x two-part UA (opencode/1.18.31 (os arch; go)) has been rejected by the upstream; the v2 shape is taken from
// the official packages/core/src/app.ts App.useragent, and the version is taken from the official
// packages/cli/package.json.
func TestUserAgentMatchesV2Shape(t *testing.T) {
	if got := UserAgent(); got != "opencode/latest/2.1.4/cli" {
		t.Fatalf("UserAgent = %q, expected opencode/latest/2.1.4/cli", got)
	}
}

// TestApplyChatHeadersCarriesSessionMirrors locks down the mirroring relationship of session headers.
//
// The official v2 client writes x-opencode-session-id / x-opencode-session /
// x-session-affinity / X-Session-Id simultaneously; missing x-opencode-session-id is judged as an unofficial client.
func TestApplyChatHeadersCarriesSessionMirrors(t *testing.T) {
	req, errNew := http.NewRequest(http.MethodPost, "https://example.invalid/chat", nil)
	if errNew != nil {
		t.Fatalf("new request: %v", errNew)
	}
	ApplyChatHeaders(req, "key", "ses_abc")

	for _, name := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id"} {
		if got := req.Header.Get(name); got != "ses_abc" {
			t.Fatalf("%s = %q, expected ses_abc", name, got)
		}
	}
	if got := req.Header.Get("x-opencode-client"); got != "cli" {
		t.Fatalf("x-opencode-client = %q, expected cli", got)
	}
	if got := req.Header.Get("User-Agent"); got != UserAgent() {
		t.Fatalf("User-Agent = %q", got)
	}
}

func TestChatEndpointUsesInferenceLane(t *testing.T) {
	// Free and paid tiers share the same inference lane (official v2 distinguishes them by credentials, not paths).
	if got, want := chatEndpoint(""), "https://opencode.ai/inference/openai"+ChatPath; got != want {
		t.Fatalf("egress = %q, expected %q", got, want)
	}
	// Uses the override value when a self-built/self-hosted gateway is configured, making it easier for users to self-recover.
	if got, want := chatEndpoint("https://gateway.example/zen/"), "https://gateway.example/zen"+ChatPath; got != want {
		t.Fatalf("overridden egress = %q, expected %q", got, want)
	}
}

// TestModelsEndpointStripsOpenAILayer locks down that the models endpoint has one less /openai layer than chat.
//
// /inference/openai/v1/models returns 404 in practice, the models list is at /inference/v1/models;
// it must be able to fall back even when the override value only specifies one layer, otherwise self-built gateways cannot get the model list.
func TestModelsEndpointStripsOpenAILayer(t *testing.T) {
	if got, want := ModelsBaseURLFor(""), "https://opencode.ai/inference"; got != want {
		t.Fatalf("manifest egress = %q, expected %q", got, want)
	}
	if got, want := ModelsBaseURLFor("https://gw.example/inference/openai/"), "https://gw.example/inference"; got != want {
		t.Fatalf("overridden manifest egress = %q, expected %q", got, want)
	}
}

func TestNewChatRequestFreeTierUsesAnonymousCredential(t *testing.T) {
	// mimo-v2.6-flash-free has a price of zero in the catalog, and based on unit price judgment, it should enter the free tier.
	catalog := modelsDevCatalog{
		"mimo-v2.6-flash-free": {Cost: &modelsDevCost{}},
	}
	body := []byte(`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	req, errReq := newChatRequest(context.Background(), "", ChatRequest{APIKey: "oc_sk_secret", Body: body}, catalog)
	if errReq != nil {
		t.Fatalf("newChatRequest: %v", errReq)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+freeTierCredential {
		t.Fatalf("free tier credential = %q, expected anonymous credential", got)
	}
	if got := req.URL.String(); got != DefaultBaseURL+ChatPath {
		t.Fatalf("free tier egress = %q", got)
	}

	payload := decodePayload(t, req)
	if payload["stream"] != true {
		t.Fatal("free tier did not enforce stream")
	}
	opt, _ := payload["stream_options"].(map[string]any)
	if opt == nil || opt["include_usage"] != true {
		t.Fatal("free tier did not require stream_options.include_usage")
	}
	// The official client only passes through session-owned tools and does not supplement them by model; this confirms we do not inject them.
	if _, ok := payload["tools"]; ok {
		t.Fatal("free tier should not inject tool definitions")
	}
}

// TestFreeTierDetectionUsesPriceNotName locks down the free tier detection criteria.
//
// The official implementation uses "unit price is 0" to determine free tier (packages/core/src/plugin/provider/opencode.ts),
// big-pickle in the catalog has a zero price but lacks the "-free" suffix, and is still the default model for official logout state.
// Judging by name would wrongly push it onto the paid lane.
func TestFreeTierDetectionUsesPriceNotName(t *testing.T) {
	catalog := modelsDevCatalog{
		"big-pickle":           {Cost: &modelsDevCost{}},
		"glm-5.3":              {Cost: &modelsDevCost{Input: 1.5, Output: 6}},
		"payment-free-in-name": {Cost: &modelsDevCost{Input: 2, Output: 4}},
	}

	if !isFreeTierModel("big-pickle", catalog) {
		t.Fatal("zero price but models without free suffix should be determined as free")
	}
	if isFreeTierModel("glm-5.3", catalog) {
		t.Fatal("models with a price cannot be determined as free")
	}
	if isFreeTierModel("payment-free-in-name", catalog) {
		t.Fatal("when catalog says it is paid, free in the name cannot override the price")
	}
	// Falls back to name-based judgment when the catalog is missing, preventing free requests from mistakenly using user keys.
	if !isFreeTierModel("some-model-free", nil) {
		t.Fatal("when catalog is unavailable, should fall back to determining by name")
	}
	if isFreeTierModel("glm-5.3", nil) {
		t.Fatal("when catalog is unavailable and name has no free, should be treated as paid")
	}
}

func TestNewChatRequestPaidKeepsUserCredential(t *testing.T) {
	catalog := modelsDevCatalog{
		"glm-5.3": {Cost: &modelsDevCost{Input: 1.5, Output: 6}},
	}
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	req, errReq := newChatRequest(context.Background(), "", ChatRequest{APIKey: "oc_sk_secret", Body: body}, catalog)
	if errReq != nil {
		t.Fatalf("newChatRequest: %v", errReq)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer oc_sk_secret" {
		t.Fatalf("paid credential = %q", got)
	}
	if got := req.URL.String(); got != DefaultBaseURL+ChatPath {
		t.Fatalf("paid egress = %q", got)
	}
	if _, ok := decodePayload(t, req)["tools"]; ok {
		t.Fatal("paid requests should not be injected with free tier tools")
	}
}

// TestMaxTokensNormalizedToCompletionTokens locks down the upper limit field name.
//
// Official 2.1.4 uniformly sends max_completion_tokens for the opencode family
// (detectMaxTokensField in packages/ai/src/protocols/openai-chat.ts falls to the default branch).
func TestMaxTokensNormalizedToCompletionTokens(t *testing.T) {
	out := decodeJSON(t, normalizeMaxTokens([]byte(`{"model":"glm-5.3","max_tokens":256}`)))
	if got := out["max_completion_tokens"]; got != float64(256) {
		t.Fatalf("max_completion_tokens = %v, expected 256", got)
	}
	if _, exists := out["max_tokens"]; exists {
		t.Fatal("max_tokens should be removed to avoid having two limit fields simultaneously")
	}

	// Keeps it as-is if max_completion_tokens is already exclusively provided.
	same := decodeJSON(t, normalizeMaxTokens([]byte(`{"model":"glm-5.3","max_completion_tokens":64}`)))
	if got := same["max_completion_tokens"]; got != float64(64) {
		t.Fatalf("max_completion_tokens = %v, expected 64", got)
	}

	// When both are provided, max_completion_tokens takes precedence.
	both := decodeJSON(t, normalizeMaxTokens([]byte(`{"max_tokens":10,"max_completion_tokens":99}`)))
	if got := both["max_completion_tokens"]; got != float64(99) {
		t.Fatalf("max_completion_tokens = %v, expected 99 to be kept", got)
	}
}

// decodePayload extracts and decodes the request body.
func decodePayload(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	raw, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		t.Fatalf("read body: %v", errRead)
	}
	return decodeJSON(t, raw)
}

func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal body: %v", errUnmarshal)
	}
	return payload
}
