package trae

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"free-model-hub/internal/core"
)

// This file implements upstream error classification for the Trae family: parsing Trae family upstream error codes and business codes.

// Classify categorizes the Trae upstream response into a shared upstream error.
func Classify(status int, body string) *core.Failure {
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := core.FirstNonEmpty(payload.Message, payload.Msg, body)
	code := ""
	if payload.Code != 0 {
		code = intString(payload.Code)
	}

	switch {
	case status == http.StatusUnauthorized || payload.Code == 4001 || payload.Code == 4008:
		return &core.Failure{Kind: core.FailureKindCredential, Status: status, Code: code, Msg: msg}
	case payload.Code == 1005 || strings.Contains(strings.ToLower(msg), "credit") || strings.Contains(msg, "积分不足"):
		return &core.Failure{Kind: core.FailureKindCredential, Status: status, Code: "1005", Msg: "Insufficient account credits"}
	case status == http.StatusTooManyRequests || payload.Code == 429 || payload.Code == 9074:
		return &core.Failure{Kind: core.FailureKindRateLimit, Status: status, Code: code, Msg: msg}
	case status >= 500:
		return &core.Failure{Kind: core.FailureKindTransient, Status: status, Code: code, Msg: msg}
	default:
		return &core.Failure{Kind: core.FailureKindClient, Status: status, Code: code, Msg: msg}
	}
}

func intString(value int) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}
