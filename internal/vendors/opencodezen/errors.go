package opencodezen

// This file implements upstream error classification for OpenCode ZEN.

import (
	"strings"

	"free-model-hub/internal/core"
)

// Classify categorizes an upstream error by HTTP status code and response body.
func Classify(status int, body string) *core.Failure {
	kind := core.ClassifyStatus(status)
	// FreeTierError is not a credential corruption, but client risk control imposed by the upstream on this model;
	// treating it as a client error allows keeping the account to continue trying other models.
	if strings.Contains(body, "FreeTierError") || strings.Contains(body, "free tier can only be used") {
		kind = core.FailureKindClient
	}
	return &core.Failure{
		Status: status,
		Msg:    core.Truncate(body, 300),
		Kind:   kind,
	}
}
