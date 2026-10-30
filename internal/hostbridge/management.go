package hostbridge

// This file provides common context and callback ID resolution for management requests.
// Shared by admin, tasks, and app to avoid duplication.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
)

// ManagementContext constructs a context with timeout and callback ID for upstream calls in management interfaces.
//
// The parent context takes req.RequestContext(): it represents the lifecycle of this management request (ends when the host cancels),
// so the upstream call terminates along with the request cancellation.
func ManagementContext(req pluginapi.ManagementRequest, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(req.RequestContext(), timeout)
	return WithCallbackID(ctx, RequestCallbackID(req)), cancel
}

// RequestCallbackID reads the callback ID passed in by the host.
func RequestCallbackID(req pluginapi.ManagementRequest) string {
	if req.Headers == nil {
		return ""
	}
	return strings.TrimSpace(req.Headers.Get("X-Host-Callback-Id"))
}

// ErrorStatus returns the HTTP status code corresponding to the error (default 500).
func ErrorStatus(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	type statusCoder interface{ StatusCode() int }
	if coded, ok := err.(statusCoder); ok {
		if status := coded.StatusCode(); status > 0 {
			return status
		}
	}
	return http.StatusInternalServerError
}
