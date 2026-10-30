package autoclaw

// This file implements upstream error classification for AutoClaw.
//
// Classification based on Agent2API's classify():
//   - Business code 410000 -> Session expired (credential disabled, re-login required);
//   - Balance/quota messages & HTTP 402 -> hard_credit (long cooldown);
//   - HTTP 429 -> Rate limit (short cooldown);
//   - Others classified by generic status codes.

import (
	"net/http"
	"strconv"
	"strings"

	"free-model-hub/internal/core"
)

// hardCreditMarkers are balance/quota related messages (Chinese and English, from upstream testing).
var hardCreditMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "not enough credit",
	"insufficient balance", "balance not enough", "limit reached", "usage limit",
	"余额不足", "额度不足", "积分不足", "积分用完", "额度用尽", "没有积分", "配额不足",
}

// deadMarkers are session expiration related messages.
var deadMarkers = []string{
	"not logged in", "unauthorized", "invalid token", "token expired",
	"login required", "session expired", "offline user session", "auth.inject failed",
	"未登录", "登录已过期", "请先登录", "token 失效",
}

// Classify categorizes an upstream error by HTTP status code, business code, and response body.
func Classify(status int, code int64, body string) *core.Failure {
	if DeadCode(code) {
		return failure(http.StatusUnauthorized, code, body, core.FailureKindSessionDead)
	}
	lower := strings.ToLower(body)
	for _, marker := range hardCreditMarkers {
		if strings.Contains(lower, marker) {
			return failure(http.StatusPaymentRequired, code, body, core.FailureKindHardCredit)
		}
	}
	for _, marker := range deadMarkers {
		if strings.Contains(lower, marker) {
			return failure(http.StatusUnauthorized, code, body, core.FailureKindSessionDead)
		}
	}
	switch {
	case status == http.StatusPaymentRequired:
		return failure(status, code, body, core.FailureKindHardCredit)
	case status == http.StatusTooManyRequests:
		return failure(status, code, body, core.FailureKindSoftRate)
	}
	kind := core.ClassifyStatus(status)
	return &core.Failure{
		Status: status,
		Code:   codeText(code),
		Msg:    core.Truncate(body, 300),
		Kind:   kind,
	}
}

// failure constructs a Failure with a business code.
func failure(status int, code int64, body string, kind core.FailureKind) *core.Failure {
	return &core.Failure{
		Status: status,
		Code:   codeText(code),
		Msg:    core.Truncate(body, 300),
		Kind:   kind,
	}
}

// codeText renders the business code into the Code field (0 means no business code).
func codeText(code int64) string {
	if code <= 0 {
		return ""
	}
	return strconv.FormatInt(code, 10)
}
