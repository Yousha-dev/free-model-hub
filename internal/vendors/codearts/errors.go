package codearts

// This file implements upstream error classification for Huawei Cloud CodeArts.

import (
	"encoding/json"
	"net/http"
	"strings"

	"free-model-hub/internal/core"
)

// Classify categorizes Huawei Cloud CodeArts upstream responses into shared upstream errors.
func Classify(status int, body string) *core.Failure {
	var payload struct {
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
		Error        string `json:"error"`
		Message      string `json:"message"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := core.FirstNonEmpty(payload.ErrorMessage, payload.Message, payload.Error, body)
	code := core.FirstNonEmpty(payload.ErrorCode, payload.Error)

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || strings.Contains(code, "ExpiredToken") || strings.Contains(code, "InvalidToken"):
		return &core.Failure{Kind: core.FailureKindCredential, Status: status, Code: code, Msg: msg}
	case strings.Contains(body, "benefit not found") || strings.Contains(code, "InferHub.4004.200"):
		return &core.Failure{Kind: core.FailureKindCredential, Status: status, Code: code, Msg: "Free welfare model quota has not been claimed. Please claim it in the admin panel or enable automatic claiming."}
	case status == http.StatusTooManyRequests || strings.Contains(code, "Throttling"):
		return &core.Failure{Kind: core.FailureKindRateLimit, Status: status, Code: code, Msg: msg}
	case status >= 500:
		return &core.Failure{Kind: core.FailureKindTransient, Status: status, Code: code, Msg: msg}
	default:
		return &core.Failure{Kind: core.FailureKindClient, Status: status, Code: code, Msg: msg}
	}
}
