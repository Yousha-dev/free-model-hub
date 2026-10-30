package workbuddy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
)

func TestClassifyOrderingMatters(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   core.FailureKind
	}{
		// 11102 must be checked first: it can be either 400 or 404, and checking it later would cause it to be swallowed by the 404 branch.
		{"model blocked on 400", 400, `{"code":11102,"msg":"service info not found"}`, core.FailureKindModelBlocked},
		{"model blocked on 404", 404, `{"code":11102,"msg":"service info not found"}`, core.FailureKindModelBlocked},
		// 429 must take precedence over quota keywords: 429 responses often contain cross-cutting wording like "quota exceeded",
		// and checking quota first would mistakenly put the rate limit on cooldown until the next day, wasting an account.
		{"429 with credit wording stays rate limit", 429, `{"code":1,"msg":"quota exceeded, too many requests"}`, core.FailureKindSoftRate},
		// True quota exhaustion is captured by 402 or business code 14018.
		{"402 is hard credit", 402, `{"code":1,"msg":"payment required"}`, core.FailureKindHardCredit},
		{"429 with 14018 is hard credit", 429, `{"code":14018,"msg":"insufficient"}`, core.FailureKindHardCredit},
		// Session invalidation takes precedence over account failure.
		{"session dead", 401, `{"code":12153,"msg":"Offline user session not found"}`, core.FailureKindSessionDead},
		// Account failures cannot be determined by 11140 (this code also carries model-level rate limit text); check keywords only.
		{"account fault by keyword", 400, `{"msg":"request illegal"}`, core.FailureKindAccountFault},
		// When 11140 carries rate limit text, it must be classified as a rate limit rather than an account failure—
		// judging by code alone would treat a merely rate-limited account as banned.
		{"11140 with rate-limit wording is soft rate", 400, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, core.FailureKindSoftRate},
		// Context overflow is a request-level error and must be evaluated before 404/5xx/WAF.
		{"prompt too long on 400", 400, `{"code":11115,"msg":"prompt is too long"}`, core.FailureKindPromptTooLong},
		{"prompt too long on 413", 413, `{"code":11115,"msg":"prompt is too long"}`, core.FailureKindPromptTooLong},
		// WAF: 403 without a business envelope.
		{"waf block", 403, `<html>Request blocked by WAF</html>`, core.FailureKindWAFBlock},
		{"403 with envelope is not waf", 403, `{"code":1000,"msg":"quota exceeded"}`, core.FailureKindHardCredit},
		// Invalid image.
		{"image invalid", 400, `{"code":11135,"msg":"invalid_image_data"}`, core.FailureKindImageInvalid},
		// Content policy block.
		{"content blocked", 400, `{"msg":"blocked by security policy"}`, core.FailureKindContentBlocked},
		// Channel validation failure: the upstream wraps it in HTTP 500, but it must still be handled as a request-level rejection.
		{"channel rejected on 500", 500, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, core.FailureKindChannelRejected},
		{"channel rejected on 200", 200, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, core.FailureKindChannelRejected},
		// 11128 appearing only in the requestId does not count as a channel validation failure.
		{"11128 in requestId is not channel rejected", 400, `{"code":12345,"msg":"bad request","requestId":"trace-11128-abc"}`, core.FailureKindClient},
		// Invalid parameters.
		{"bad params", 400, `{"msg":"Unmarshal chat params failed"}`, core.FailureKindBadParams},
		// Other 4xx.
		{"other client error", 400, `{"code":12345,"msg":"whatever"}`, core.FailureKindClient},
		// Sporadic 404 (without 11102 marker).
		{"plain 404", 404, `{"msg":"not found"}`, core.FailureKindNotFound},
		// Upstream failure.
		{"server error", 500, `{"msg":"internal error"}`, core.FailureKindServer},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Classify(testCase.status, testCase.body)
			if got.Kind != testCase.want {
				t.Fatalf("Classify(%d, %q).Kind = %s, want %s",
					testCase.status, testCase.body, got.Kind, testCase.want)
			}
		})
	}
}

// TestClassifyModelBlockedNeedsFieldBoundary verifies that 11102 only recognizes standalone fields.
//
// If a full-text substring match were used, "11102" appearing in the requestId would mistakenly avoid an available model.
func TestClassifyModelBlockedNeedsFieldBoundary(t *testing.T) {
	// 11102 appears only in requestId: should not be judged as model unavailable.
	body := `{"code":12345,"msg":"bad request","requestId":"trace-11102-abc"}`
	got := Classify(http.StatusBadRequest, body)
	if got.Kind == core.FailureKindModelBlocked {
		t.Fatalf("11102 inside requestId must not trigger model blocking: %s", got.Kind)
	}
	// The code field is actually 11102: should match.
	if got := Classify(http.StatusBadRequest, `{"code":11102}`); got.Kind != core.FailureKindModelBlocked {
		t.Fatalf("explicit code 11102 must be model blocked, got %s", got.Kind)
	}
}

// TestClassifyRetryableAndStatus verifies retry determination and status code mapping.
//
// The host determines credential disposal based on HTTP status codes; this mapping table is the destination of the "account disposal matrix".
func TestClassifyRetryableAndStatus(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantStatus    int
	}{
		{"rate limit is retryable", 429, `{"msg":"rate limit"}`, true, 429},
		{"content blocked is not retryable", 400, `{"msg":"blocked by security policy"}`, false, 400},
		{"prompt too long is not retryable", 400, `{"msg":"prompt is too long"}`, false, 400},
		{"image invalid is not retryable", 400, `{"msg":"invalid_image_data"}`, false, 400},
		{"bad params is not retryable", 400, `{"msg":"Unmarshal chat params failed"}`, false, 400},
		// Channel validation failure: upstream expresses this with 500, but the host must treat it as a request-level error (400 does not cool down the account).
		{"channel rejected is not retryable", 500, `{"code":11128,"msg":"unapproved channel"}`, false, 400},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			classified := Classify(testCase.status, testCase.body)
			if classified.Retryable() != testCase.wantRetryable {
				t.Fatalf("Retryable() = %v, want %v", classified.Retryable(), testCase.wantRetryable)
			}
			if classified.HTTPStatus() != testCase.wantStatus {
				t.Fatalf("HTTPStatus() = %d, want %d", classified.HTTPStatus(), testCase.wantStatus)
			}
		})
	}
}

// TestParseRateResetAndRetryAfter verifies parsing of upstream wait information.
func TestParseRateResetAndRetryAfter(t *testing.T) {
	// Chinese text takes priority.
	reset := ParseRateReset(`{"msg":"额度恢复将在 2026-09-27 04:00:00 重置"}`)
	if reset.IsZero() {
		t.Fatal("chinese rate reset wording must be parsed")
	}
	if reset.Hour() != 4 {
		t.Fatalf("reset hour = %d, want 4", reset.Hour())
	}

	header := http.Header{}
	header.Set("Retry-After", "30")
	if got := ParseRetryAfter(header, time.Now()); got != 30*time.Second {
		t.Fatalf("Retry-After = %v, want 30s", got)
	}
	// Absurd values are clamped (upstream occasionally returns extremely large values).
	header.Set("Retry-After", "999999")
	if got := ParseRetryAfter(header, time.Now()); got > time.Hour*2 {
		t.Fatalf("Retry-After must be capped, got %v", got)
	}
	// HTTP-Date formats are ignored (not purely numeric).
	header.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if got := ParseRetryAfter(header, time.Now()); got != 0 {
		t.Fatalf("http-date Retry-After must be ignored, got %v", got)
	}
}

// TestChannelRejectedKeepsReadableDetail verifies that 11128 does not degrade into a bare numeric code.
//
// The host directly returns non-retryable errors to the client; both the error code and message must retain 11128 and the upstream description,
// otherwise the user will only see "no auth available / last upstream error: 11128".
func TestChannelRejectedKeepsReadableDetail(t *testing.T) {
	body := `{"actions":["EDIT_INPUT","SUBMIT_FEEDBACK"],"code":11128,` +
		`"msg":"Illegal API invocation from an unapproved channel",` +
		`"displayMsg":{"en":"Request blocked. Please send it again"}}`
	classified := Classify(http.StatusInternalServerError, body)
	pluginErr, okPlugin := core.ToPluginError(classified).(*core.PluginError)
	if !okPlugin {
		t.Fatalf("ToPluginError() = %T, want *core.PluginError", core.ToPluginError(classified))
	}
	if pluginErr.Code != "upstream_channel_rejected" {
		t.Fatalf("code = %q, want upstream_channel_rejected", pluginErr.Code)
	}
	if pluginErr.Retryable || pluginErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("retryable=%v status=%d, want false/400", pluginErr.Retryable, pluginErr.HTTPStatus)
	}
	for _, want := range []string{"11128", "unapproved channel"} {
		if !strings.Contains(pluginErr.Message, want) {
			t.Fatalf("message %q must contain %q", pluginErr.Message, want)
		}
	}
}

// channelRejectedBody is the original 11128 body from Issue #42: the real response after upstream added channel validation.
const channelRejectedBody = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel",` +
	`"requestId":"01a176819f987048670eda9f9a912d5",` +
	`"displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"},` +
	`"displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client. Please resend from the official client.",` +
	`"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。",` +
	`"zh-hant":"本次呼叫未通過通路驗證。"},` +
	`"actions":["EDIT_INPUT","SUBMIT_FEEDBACK"]}`

// TestChannelRejectedMessageFoldsMultilingualBody verifies that the folded 11128 message remains readable and actionable.
//
// The upstream body is JSON with en/zh/zh-hant text; direct passthrough would only make users see a chunk of 500 errors;
// after folding, it must still retain the upstream msg, Chinese prompt, and requestId (the only credential that can align when reporting to upstream).
func TestChannelRejectedMessageFoldsMultilingualBody(t *testing.T) {
	failure := Classify(http.StatusInternalServerError, channelRejectedBody)
	if failure.Kind != core.FailureKindChannelRejected || failure.Status != http.StatusBadRequest {
		t.Fatalf("kind=%s status=%d, want channel_rejected/400", failure.Kind, failure.Status)
	}
	if failure.Retryable() {
		t.Fatal("channel rejection must not be retryable: switching accounts still fails channel validation")
	}
	for _, want := range []string{
		"Illegal API invocation from an unapproved channel",
		"这次调用没通过渠道校验",
		"code=11128",
		"requestId=01a176819f987048670eda9f9a912d5",
	} {
		if !strings.Contains(failure.Msg, want) {
			t.Fatalf("message %q must contain %q", failure.Msg, want)
		}
	}
	if strings.Contains(failure.Msg, "displayTips") {
		t.Fatalf("message must not dump the raw envelope: %q", failure.Msg)
	}
}

// TestChannelRejectedKeepsRawBodyInLog verifies that interception evidence goes into the logs.
//
// After message folding, the body is no longer returned with the error; the log must retain the full raw text (including requestId),
// otherwise the information in the Issue #42 screenshots will be completely lost during troubleshooting.
func TestChannelRejectedKeepsRawBodyInLog(t *testing.T) {
	_, since := logger.Snapshot(0, 1)
	enrichClassifyError(http.StatusInternalServerError, channelRejectedBody, http.Header{})
	entries, _ := logger.Snapshot(since, 200)
	for _, entry := range entries {
		if strings.Contains(entry.Message, "channel verification") &&
			strings.Contains(entry.Message, "01a176819f987048670eda9f9a912d5") {
			return
		}
	}
	t.Fatalf("channel rejection must be logged with the upstream requestId, got %d entries", len(entries))
}

// TestChannelRejectedFallsBackToRawBody verifies that non-JSON bodies are not swallowed by folding.
func TestChannelRejectedFallsBackToRawBody(t *testing.T) {
	failure := Classify(http.StatusForbidden, "Illegal API invocation from an unapproved channel")
	if failure.Kind != core.FailureKindChannelRejected {
		t.Fatalf("kind=%s, want channel_rejected", failure.Kind)
	}
	if !strings.Contains(failure.Msg, "Illegal API invocation from an unapproved channel") {
		t.Fatalf("message %q must keep the raw body", failure.Msg)
	}
}
