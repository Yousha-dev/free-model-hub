package zcode

import (
	"net/http"
	"strings"
	"testing"

	"free-model-hub/internal/core"
)

func TestClassifyOrderingCaptchaBeforeAuth(t *testing.T) {
	// 403 + captcha text is a captcha challenge, not a credential failure—checking 403 first would incorrectly kill the account.
	risk := Classify(403, `{"code":3007,"msg":"captcha verify failed"}`)
	if risk.HTTPStatus() != 403 {
		t.Fatalf("Captcha challenge should map to soft cooldown (403), got %d", risk.HTTPStatus())
	}

	// Pure 403 indicates a credential failure.
	dead := Classify(403, `{"msg":"forbidden"}`)
	if dead.HTTPStatus() != 403 {
		t.Fatalf("Credential expiration should map to 403, got %d", dead.HTTPStatus())
	}
	if !strings.Contains(strings.ToLower(dead.Msg), "forbidden") && !strings.Contains(dead.Msg, "凭证") {
		t.Fatalf("unexpected message: %q", dead.Msg)
	}

	// 401 -> Authentication failed.
	if got := Classify(401, `{}`); got.HTTPStatus() != 401 {
		t.Fatalf("401 should map to 401, got %d", got.HTTPStatus())
	}
}

func TestClassifyBusinessCodeWithoutStatus(t *testing.T) {
	// When the upstream wraps business errors in HTTP 200, status=0 must be derived from Kind,
	// and 200 must not be passed through as an erroneous HTTP status code to the host.
	errQuota := Classify(0, `{"code":1005,"msg":"daily quota exhausted"}`)
	if errQuota.HTTPStatus() != 402 {
		t.Fatalf("Quota exhaustion should map to 402, got %d", errQuota.HTTPStatus())
	}
}

// TestClassify500WrappedPlanConcurrencyLimit verifies that when the upstream wraps start plan concurrency limit
// into 500, it is classified as SoftRate based on the message rather than falling into the 5xx upstream failure branch which triggers circuit breaking.
func TestClassify500WrappedPlanConcurrencyLimit(t *testing.T) {
	body := `{"message":"model concurrency limit exceeded","type":"server_error","code":"internal_server_error"}`
	if failure := Classify(http.StatusInternalServerError, body); failure.Kind != core.FailureKindSoftRate {
		t.Fatalf("kind = %v, want SoftRate", failure.Kind)
	}
}

// TestClassifyModelBucketExhausted verifies that 1005 + "exceed quota limit" is classified as
// model-level avoidance (ModelBlocked) rather than account-level cooldown (HardCredit): when an event gift pack's
// single model bucket is exhausted, other models on the same account (such as Flash) should remain available.
func TestClassifyModelBucketExhausted(t *testing.T) {
	body := `{"code":1005,"msg":"exceed quota limit","logid":"x"}`
	failure := Classify(200, body)
	if failure.Kind != core.FailureKindModelBlocked {
		t.Fatalf("kind = %v, want ModelBlocked", failure.Kind)
	}
	if failure.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", failure.HTTPStatus())
	}
}

// TestClassifyAccountLevelQuotaStillHardCredit verifies that account-level quota signals still use
// HardCredit: 402, and 1005 with other wordings (such as claim quota exhausted) do not trigger model avoidance.
func TestClassifyAccountLevelQuotaStillHardCredit(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"payment_required", http.StatusPaymentRequired, `{"msg":"no credit"}`},
		{"code1005_other_wording", 200, `{"code":1005,"msg":"daily claim quota used"}`},
		{"insufficient_marker", 200, `{"code":7,"msg":"insufficient balance"}`},
	}
	for _, testCase := range cases {
		failure := Classify(testCase.status, testCase.body)
		if failure.Kind != core.FailureKindHardCredit {
			t.Fatalf("%s: kind = %v, want HardCredit", testCase.name, failure.Kind)
		}
	}
}

// TestClassifyModelBucketExhaustedPriority locks in the classification priority for model bucket exhaustion:
// 402 is account-level debt and cannot be preempted by single model bucket; 403 is still credential failure; 429 triggers model avoidance.
func TestClassifyModelBucketExhaustedPriority(t *testing.T) {
	body := `{"code":1005,"msg":"exceed quota limit","logid":"x"}`
	cases := []struct {
		name   string
		status int
		kind   core.FailureKind
		http   int
	}{
		{"payment_required_wins", http.StatusPaymentRequired, core.FailureKindHardCredit, http.StatusPaymentRequired},
		{"forbidden_is_session_dead", http.StatusForbidden, core.FailureKindSessionDead, http.StatusForbidden},
		{"too_many_requests_is_model_blocked", http.StatusTooManyRequests, core.FailureKindModelBlocked, http.StatusTooManyRequests},
	}
	for _, testCase := range cases {
		failure := Classify(testCase.status, body)
		if failure.Kind != testCase.kind || failure.HTTPStatus() != testCase.http {
			t.Fatalf("%s: kind=%v status=%d, want kind=%v status=%d",
				testCase.name, failure.Kind, failure.HTTPStatus(), testCase.kind, testCase.http)
		}
	}
}

// TestClassifyModelNotAllowedStatus verifies that 3006 inside a 200 envelope is converted to a semantic status code,
// rather than passing 200 through as an erroneous HTTP status code to the host.
func TestClassifyModelNotAllowedStatus(t *testing.T) {
	failure := Classify(http.StatusOK, `{"code":3006,"msg":"model not allowed"}`)
	if failure.Kind != core.FailureKindModelBlocked {
		t.Fatalf("kind = %v, want ModelBlocked", failure.Kind)
	}
	if failure.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", failure.HTTPStatus())
	}
}
