package minimaxcode

// Error classification for the login and refresh phases.
//
// When refresh_token is revoked, upstream returns 400 invalid_grant: by status code it
// would be classified as a "client error", so the refresh failure is repeatedly retried as
// upstream jitter—the account is neither marked bad nor prompted for re-authorization. Here we lock revocation errors to map to credential failure (401).

import (
	"net/http"
	"testing"

	"free-model-hub/internal/core"
)

func TestClassifyRevokedRefreshToken(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantKind   core.FailureKind
		wantStatus int
	}{
		{
			name:       "invalid_grant 是撤销不是参数错误",
			status:     http.StatusBadRequest,
			body:       `{"error":"invalid_grant","error_description":"refresh token revoked"}`,
			wantKind:   core.FailureKindCredential,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid_refresh_token 同样按凭证失效",
			status:     http.StatusBadRequest,
			body:       `{"error":"invalid_refresh_token"}`,
			wantKind:   core.FailureKindCredential,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "401 保持凭证失效",
			status:     http.StatusUnauthorized,
			body:       `{"error":"unauthorized"}`,
			wantKind:   core.FailureKindCredential,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "5xx 仍是可重试抖动",
			status:     http.StatusInternalServerError,
			body:       `upstream unavailable`,
			wantKind:   core.FailureKindTransient,
			wantStatus: http.StatusInternalServerError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := Classify(test.status, test.body)
			if failure.Kind != test.wantKind {
				t.Fatalf("kind = %s, want %s", failure.Kind, test.wantKind)
			}
			if got := failure.HTTPStatus(); got != test.wantStatus {
				t.Fatalf("HTTPStatus = %d, want %d", got, test.wantStatus)
			}
			if wantCredential := test.wantKind == core.FailureKindCredential; isCredentialFailure(failure) != wantCredential {
				t.Fatalf("isCredentialFailure = %v, want %v", !wantCredential, wantCredential)
			}
		})
	}
}

// TestIsCredentialFailureIgnoresPlainErrors verifies that plain errors are not treated as credential failures:
// Login validation failures caused by network timeouts should not force the user to re-authorize.
func TestIsCredentialFailureIgnoresPlainErrors(t *testing.T) {
	if isCredentialFailure(errPlain) {
		t.Fatal("plain error must not be treated as a credential failure")
	}
	if isCredentialFailure(nil) {
		t.Fatal("nil error must not be treated as a credential failure")
	}
}

var errPlain = &plainError{}

type plainError struct{}

func (e *plainError) Error() string { return "network timeout" }
