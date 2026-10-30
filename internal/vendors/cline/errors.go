package cline

import (
	"strings"

	"free-model-hub/internal/core"
)

// This file implements upstream error classification for Cline.
//
// The purpose of classification is not just "neat logging", but to determine how the host should handle this account:
//   - Invalid credential (401/403) -> Let the host mark the account as bad and stop routing;
//   - Rate limit (429) -> Keep the account and retry later;
//   - Upstream jitter (5xx / network) -> Keep the account and retry later;
//   - Request itself is invalid (400/422) -> Keep the account (it's an issue with this request, not the account).

// Classify classifies an upstream error by HTTP status code.
func Classify(status int, body string) *core.Failure {
	// A 403 ENTITLEMENT_ERROR means "this model is not in your plan" (Cline
	// Pass models on a free account). The credential is fine, so it must not
	// be reported as a credential failure, which would make the host cool the
	// account down and trigger needless token refreshes.
	if strings.Contains(body, "ENTITLEMENT_ERROR") || strings.Contains(body, "not subscribed to required model plan") {
		return &core.Failure{Msg: core.Truncate(body, 300), Kind: core.FailureKindModelBlocked, RetryableOverride: core.Bool(false)}
	}
	return &core.Failure{
		Status: status,
		Msg:    core.Truncate(body, 300),
		Kind:   core.ClassifyStatus(status),
	}
}
