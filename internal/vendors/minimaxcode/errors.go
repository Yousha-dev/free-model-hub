package minimaxcode

// This file implements upstream error classification for MiniMax Code.
//
// Upstream expresses errors using a mix of three mechanisms: HTTP status codes, business codes wrapped in 200 responses,
// and "numeric code: natural language" text in streaming bodies (e.g., `50110:insufficient balance`).
// The classification result determines how the host handles the account:
//   - Credential invalidation -> disable account, stop routing;
//   - Balance/quota exhaustion -> 402 long cool-down until the next day;
//   - Rate limiting -> 429 short cool-down;
//   - 5xx / Network -> 502 switch account and retry;
//   - Request itself is malformed -> 4xx do not penalize account.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"free-model-hub/internal/core"
)

// Keyword table. Evaluation order matters: credential invalidation comes before balance, and balance before rate limiting --
// upstream may write "login expired" as a variant of balance phrasing, and checking balance first would misclassify it as 402.
var (
	credentialMarkers = []string{
		"token invalid", "invalid token", "token expired", "token is expired",
		"unauthorized", "unauthenticated", "login required", "not logged in",
		"凭证无效", "凭证已过期", "登录已过期", "请重新登录", "未登录",
		// Standard error code for OAuth refresh failure: when refresh_token is revoked or rotation fails,
		// upstream returns 400 invalid_grant, which by status code alone would be classified as a client error.
		"invalid_grant", "invalid refresh token", "invalid_refresh_token", "token revoked",
	}
	creditMarkers = []string{
		"insufficient balance", "insufficient_balance", "insufficient credit",
		"no credit", "credit exhausted", "credits exhausted", "out of credit",
		"quota exceeded", "quota exhausted", "payment required",
		"余额不足", "积分不足", "额度不足", "额度用尽", "积分用完", "余额用完",
	}
	rateLimitMarkers = []string{
		"rate limit", "rate-limit", "rate-limited", "too many requests",
		"请求过于频繁", "限流",
	}
)

// Classify translates an upstream failure into a shared upstream error.
//
// Pass 0 for status to indicate "no error signal at the HTTP layer" (in-stream business errors, response body error text).
// In this case, classification is determined entirely by the body, and Status must be zeroed out -- otherwise Failure.HTTPStatus
// would pass through 200 as-is, causing the host to treat a clear balance exhaustion as a success.
func Classify(status int, body string) *core.Failure {
	lower := strings.ToLower(body)
	kind := core.FailureKindTransient
	if status >= http.StatusBadRequest {
		kind = core.ClassifyStatus(status)
	}
	if core.ContainsAny(lower, credentialMarkers) {
		kind = core.FailureKindCredential
	} else if core.ContainsAny(lower, creditMarkers) {
		kind = core.FailureKindHardCredit
	} else if core.ContainsAny(lower, rateLimitMarkers) {
		kind = core.FailureKindRateLimit
	}

	// When the status code and classification disagree, the classification takes precedence (e.g., 400 with body token expired:
	// the host needs 401 to deactivate a bad account). Pass through only when the classification is derived from the status code.
	passthrough := status >= http.StatusBadRequest && kind == core.ClassifyStatus(status)
	if !passthrough {
		status = 0
	}
	return &core.Failure{
		Kind:   kind,
		Status: status,
		Code:   upstreamCode(body),
		Msg:    core.Truncate(body, 300),
	}
}

// upstreamCode extracts the upstream business code from the body.
//
// Format: `{"code":50110,...}` or in-stream text `50110:insufficient balance (1008)`.
// Returns an empty string if not found -- do not fabricate a seemingly existing error code.
func upstreamCode(body string) string {
	var payload struct {
		Code      json.RawMessage `json:"code"`
		ErrorCode json.RawMessage `json:"error_code"`
		Status    json.RawMessage `json:"status_code"`
	}
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal == nil {
		for _, raw := range []json.RawMessage{payload.Code, payload.ErrorCode, payload.Status} {
			if code := strings.Trim(strings.TrimSpace(string(raw)), `"`); code != "" && code != "null" {
				return code
			}
		}
	}
	trimmed := strings.TrimSpace(body)
	digits := 0
	for digits < len(trimmed) && trimmed[digits] >= '0' && trimmed[digits] <= '9' {
		digits++
	}
	if digits >= 4 && digits < len(trimmed) && trimmed[digits] == ':' {
		return trimmed[:digits]
	}
	return ""
}

// isCredentialFailure reports whether the error belongs to the "credential unavailable" category.
//
// Before persisting the login, use this to separate two types of failures: credentials themselves unavailable (do not write to disk, directly error out so the user
// retries) versus network jitter (token already acquired, does not affect persistence).
func isCredentialFailure(err error) bool {
	var failure *core.Failure
	if errors.As(err, &failure) {
		return failure.Kind == core.FailureKindCredential
	}
	return false
}
