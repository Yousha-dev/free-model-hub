package opencodezen

import (
	"net/http"
	"testing"

	"free-model-hub/internal/core"
)

// freeUsageLimitBody is the raw upstream response when the free tier quota is exhausted (tested on 2026-10-26
// POST https://opencode.ai/inference/v1/chat/completions, HTTP 429,
// Retry-After points to UTC 0:00 of the next day).
const freeUsageLimitBody = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}`

// TestClassifyFreeUsageLimitIsRateLimit locks down the classification for free tier quota exhaustion.
//
// The official v2 client gets the same 429 on the same egress IP, so this is a quota issue rather than
// a client identifier issue: handle as rate_limit, let the host short-circuit for cooldown, and do not misjudge it as a credential failure.
func TestClassifyFreeUsageLimitIsRateLimit(t *testing.T) {
	failure := Classify(http.StatusTooManyRequests, freeUsageLimitBody)
	if failure.Kind != core.FailureKindRateLimit {
		t.Fatalf("kind = %s, expected %s", failure.Kind, core.FailureKindRateLimit)
	}
	if got := failure.HTTPStatus(); got != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, expected 429", got)
	}
	if failure.Msg != freeUsageLimitBody {
		t.Fatalf("upstream original text was rewritten: %q", failure.Msg)
	}
}

// TestClassifyFreeTierErrorIsClient locks down the old 403 client risk-control classification.
//
// FreeTierError is a hard rejection for "unofficial clients" and is unrelated to credentials; handle as client
// so the account can be kept to continue trying other models.
func TestClassifyFreeTierErrorIsClient(t *testing.T) {
	body := `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode."}}`
	failure := Classify(http.StatusForbidden, body)
	if failure.Kind != core.FailureKindClient {
		t.Fatalf("kind = %s, expected %s", failure.Kind, core.FailureKindClient)
	}
}
