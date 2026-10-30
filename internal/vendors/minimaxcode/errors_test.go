package minimaxcode

import (
	"net/http"
	"testing"

	"free-model-hub/internal/core"
)

func TestClassifyCredentialBeatsBalanceWording(t *testing.T) {
	// Upstream writes login expiration as a variant of balance phrasing; credential checks must precede balance checks.
	got := Classify(http.StatusBadRequest, "token invalid, balance unchanged")
	if got.Kind != "credential" {
		t.Fatalf("kind = %s, want credential", got.Kind)
	}
	if got.HTTPStatus() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got.HTTPStatus())
	}
	if got.Retryable() {
		t.Fatal("credential errors must not rotate accounts")
	}
}

func TestClassifyStatusCodesWinWhenNoMarker(t *testing.T) {
	cases := []struct {
		status int
		kind   string
	}{
		{http.StatusUnauthorized, "credential"},
		{http.StatusForbidden, "credential"},
		{http.StatusPaymentRequired, "hard_credit"},
		{http.StatusTooManyRequests, "rate_limit"},
		{http.StatusInternalServerError, "transient"},
		{http.StatusBadRequest, "client"},
	}
	for _, tc := range cases {
		got := Classify(tc.status, "plain upstream body")
		if string(got.Kind) != tc.kind {
			t.Fatalf("Classify(%d).Kind = %s, want %s", tc.status, got.Kind, tc.kind)
		}
		if got.HTTPStatus() != tc.status {
			t.Fatalf("Classify(%d) status = %d, want passthrough", tc.status, got.HTTPStatus())
		}
	}
}

func TestClassifyStreamBodyWithoutStatus(t *testing.T) {
	// In-stream business errors have no HTTP status: they must be classified by body, and 200 must not be passed through to the host.
	cases := []struct {
		body string
		kind string
		want int
	}{
		{"50110:insufficient balance (1008)", "hard_credit", http.StatusPaymentRequired},
		{"1008: too many requests", "rate_limit", http.StatusTooManyRequests},
		{"1001: token expired", "credential", http.StatusUnauthorized},
		// Stateless, no keywords: treat as a retryable transient failure (account switching and retrying is safe).
		{"provider_error: connection reset", "transient", http.StatusBadGateway},
	}
	for _, tc := range cases {
		got := Classify(0, tc.body)
		if string(got.Kind) != tc.kind {
			t.Fatalf("Classify(0, %q).Kind = %s, want %s", tc.body, got.Kind, tc.kind)
		}
		if got.HTTPStatus() != tc.want {
			t.Fatalf("Classify(0, %q) status = %d, want %d", tc.body, got.HTTPStatus(), tc.want)
		}
	}
}

func TestClassifyKeepsUpstreamCode(t *testing.T) {
	if got := Classify(429, `{"code":50110,"message":"insufficient balance"}`); got.Code != "50110" {
		t.Fatalf("json code = %q, want 50110", got.Code)
	}
	if got := Classify(0, "50110:insufficient balance (1008)"); got.Code != "50110" {
		t.Fatalf("text code = %q, want 50110", got.Code)
	}
	if got := Classify(http.StatusBadGateway, "gateway down"); got.Code != "" {
		t.Fatalf("code = %q, want empty (must not invent one)", got.Code)
	}
}

func TestClassifyBalanceIsRetryableThroughCooldown(t *testing.T) {
	got := Classify(0, "50110:insufficient balance (1008)")
	if !got.Retryable() {
		t.Fatal("credit exhaustion should rotate to another account")
	}
}

// TestClassifySurvivesToPluginError locks in "classification results reach the host".
//
// No matter how correct the classification is, if the adapter wraps it in fmt.Errorf, the host will only see a
// 502 retryable error, and bad credentials will never be disabled—this was a real bug before the refactoring.
func TestClassifySurvivesToPluginError(t *testing.T) {
	failure := Classify(401, `{"code":1001,"message":"token expired"}`)
	pluginErr, ok := core.ToPluginError(failure).(*core.PluginError)
	if !ok {
		t.Fatalf("ToPluginError returned %T, want *core.PluginError", core.ToPluginError(failure))
	}
	if pluginErr.HTTPStatus != 401 {
		t.Fatalf("host status = %d, want 401 (bad credential must be disabled)", pluginErr.HTTPStatus)
	}
	if pluginErr.Retryable {
		t.Fatal("credential failure must not rotate accounts")
	}
}
