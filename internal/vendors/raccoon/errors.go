package raccoon

// This file implements upstream error classification for RedPanda.
//
// Classification is based on Agent2API's Classify() (HTTP and in-stream share the same
// signal criteria to prevent drift between the two paths):
//   - 401/403 -> Session invalid;
//   - 429 -> Rate limited;
//   - Quota text (points/quota/balance + insufficient/exhausted, or business code 14018) -> hard_credit;
//   - Queue envelope -> busy (switch account without penalizing it).
//
// Direction: Better to miss a classification (do not penalize account, just retry with another) than to misclassify a valid account.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"free-model-hub/internal/core"
)

// quotaBusinessCode is the dedicated business code for "insufficient quota" (captured from actual traffic).
const quotaBusinessCode = 14018

// queueUnavailable reports whether the response body is an upstream queue envelope.
func queueUnavailable(body string) bool {
	var probe struct {
		Data struct {
			ServiceAvailable bool `json:"serviceAvailable"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return false
	}
	return !probe.Data.ServiceAvailable
}

// quotaSignal reports whether the text indeed indicates insufficient quota/balance.
//
// `insufficient` alone is too broad ("context window is insufficient" would also match),
// it must be paired with a quota keyword to count; similarly, `token` does not trigger session invalidation criteria.
func quotaSignal(lower string) bool {
	if strings.Contains(lower, "14018") || strings.Contains(lower, "credits exhausted") {
		return true
	}
	if strings.Contains(lower, "insufficient") {
		for _, word := range []string{"point", "credit", "balance", "quota", "余额", "积分", "额度"} {
			if strings.Contains(lower, word) {
				return true
			}
		}
	}
	for _, pair := range [][2]string{
		{"积分", "不足"}, {"积分", "耗尽"}, {"额度", "不足"}, {"额度", "耗尽"},
		{"余额", "不足"}, {"余额", "耗尽"}, {"quota", "exceeded"}, {"quota", "exhausted"},
	} {
		if strings.Contains(lower, pair[0]) && strings.Contains(lower, pair[1]) {
			return true
		}
	}
	return false
}

// Classify classifies an upstream error by HTTP status code and response body.
func Classify(status int, body string) *core.Failure {
	lower := strings.ToLower(body)
	kind := classifyKind(status, lower, body)
	return &core.Failure{
		Status: status,
		Msg:    core.Truncate(errorMessage(body), 300),
		Kind:   kind,
	}
}

// ClassifyBusiness classifies by business code in HTTP 2xx/4xx body (code != 0).
func ClassifyBusiness(status int, code int64, message string) *core.Failure {
	kind := core.FailureKindClient
	switch {
	case code == quotaBusinessCode:
		kind = core.FailureKindHardCredit
	case strings.Contains(message, "失效") || strings.Contains(message, "重新登录") || strings.Contains(message, "not_found"):
		kind = core.FailureKindSessionDead
	}
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = fmt.Sprintf("Upstream business error (code=%d)", code)
	}
	return &core.Failure{
		Status: status,
		Msg:    core.Truncate(msg, 300),
		Kind:   kind,
	}
}

// classifyKind normalizes HTTP status and text into a FailureKind.
func classifyKind(status int, lower, body string) core.FailureKind {
	switch {
	// Check authentication first: 403 might just be a permission issue; falling back to quota text could wrongly ban the account.
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return core.FailureKindSessionDead
	case status == http.StatusTooManyRequests:
		return core.FailureKindSoftRate
	case status >= 500:
		return core.FailureKindServer
	case status == http.StatusNotFound:
		return core.FailureKindNotFound
	case status == http.StatusPaymentRequired:
		return core.FailureKindHardCredit
	}
	if queueUnavailable(body) {
		return core.FailureKindBusy
	}
	if quotaSignal(lower) {
		return core.FailureKindHardCredit
	}
	if status >= 400 {
		return core.FailureKindClient
	}
	return core.FailureKindTransient
}

// errorMessage extracts human-readable text from the error body (message / msg / detail / error.message).
func errorMessage(body string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return strings.TrimSpace(body)
	}
	if msg := core.GetString(payload, "message", "msg", "detail"); msg != "" {
		return msg
	}
	if inner, okInner := payload["error"].(map[string]any); okInner {
		return core.GetString(inner, "message")
	}
	return strings.TrimSpace(body)
}
