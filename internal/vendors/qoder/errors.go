package qoder

// This file implements Qoder error classification and plugin error construction.
//
// The classification result determines how the host handles the credential: 401 disables the account, 502 switches accounts and retries, 503 tries again later.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"free-model-hub/internal/core"
)

// PluginError is Qoder's upstream error (shared with other providers).
type PluginError = core.Failure

// NewError constructs a Qoder plugin error.
func NewError(code, message string, status int) *PluginError {
	kind := core.ClassifyStatus(status)
	switch {
	case strings.Contains(code, "credential"):
		kind = core.FailureKindCredential
	case strings.Contains(code, "busy"):
		kind = core.FailureKindBusy
	case strings.Contains(code, "transient"):
		kind = core.FailureKindTransient
	}
	retryable := kind == core.FailureKindTransient && strings.Contains(code, "transient")
	return &core.Failure{
		Kind:              kind,
		Status:            status,
		Code:              code,
		Msg:               message,
		RetryableOverride: core.Bool(retryable),
	}
}

// ClassifyCredentialError classifies upstream errors into plugin errors.
func ClassifyCredentialError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	lower := strings.ToLower(message)
	if _, queued := ParseQueueSignal(message); queued {
		return NewError("qoder_model_busy", message, http.StatusServiceUnavailable)
	}
	switch {
	case strings.Contains(message, "HTTP 401"), strings.Contains(message, "HTTP 403"),
		strings.Contains(lower, "unauthorized"), strings.Contains(lower, "forbidden"),
		strings.Contains(lower, "invalid token"), strings.Contains(lower, "token expired"):
		return NewError("qoder_credential_invalid", message, http.StatusUnauthorized)
	case IsTransientTransport(err):
		return NewError("qoder_upstream_transient", message, http.StatusBadGateway)
	default:
		return NewError("qoder_upstream_error", message, http.StatusBadGateway)
	}
}

// classifyCredentialError is a package-internal alias for ClassifyCredentialError.
func classifyCredentialError(err error) error { return ClassifyCredentialError(err) }

// Error types (passed through to the client's error.type field)
const (
	ErrTypeContentPolicy = "content_policy_rejected"
	ErrTypeTransient     = "upstream_transient_error"
	ErrTypeModelBusy     = "upstream_model_busy"
	ErrTypeUpstream      = "upstream_error"
)

// TransientMaxRetries is the extra retry count for transient failures on the same account (see RetryBackoff for backoff duration).
const TransientMaxRetries = 2

// ErrEmptyStream occurs when the upstream successfully establishes a stream (HTTP 200) but closes normally without delivering any valid delta/usage frames
// (aligned with hub "empty upstream stream"): not retried by convention, directly propagated by CallQoderWithOpts,
// preventing the client from receiving an empty assistant message while ending normally as finished.
var ErrEmptyStream = errors.New("empty upstream stream")

// RetryBackoff returns the wait duration before the attempt-th retry (attempt starts from 1).
func RetryBackoff(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}

// transientHTTPCodes are cases where the upstream wraps its provider faults as 418, or directly returns 5xx.
var transientHTTPCodes = map[int]bool{418: true, 500: true, 502: true, 503: true, 504: true}

// clientFaultMarkers are client parameter/permission/content-side issues: retries are ineffective, fail fast.
var clientFaultMarkers = []string{
	"invalid_parameter_error", // e.g., Range of max_tokens validation failed
	"invalid_request_error",
	"authentication_error",
	"permission_error",
	`"Range of `,
	// Upstream content safety audit (log evidence InternalError.Algo.DataInspectionFailed:
	// Input text data may contain inappropriate content.)
	"DataInspectionFailed",
	"inappropriate content",
	"input text data may contain",
	"ContentFilter",
	"SensitiveContent",
}

// contentPolicyMarkers are content moderation categories (user input-side issues, requiring specialized Chinese explanation).
var contentPolicyMarkers = []string{
	"DataInspectionFailed",
	"inappropriate content",
	"input text data may contain",
	"ContentFilter",
	"SensitiveContent",
}

// nonTransientTransportMarkers are configuration/certificate transport errors: retries are meaningless, fail fast.
var nonTransientTransportMarkers = []string{
	"unsupported protocol scheme",
	"invalid url",
	"unknown scheme",
	"x509:", // Certificate verification failed (distinguished from TLS handshake timeout — the latter is retryable)
}

// IsContentPolicy determines whether it is an upstream content safety audit rejection.
func IsContentPolicy(detail string) bool {
	return core.ContainsAny(detail, contentPolicyMarkers)
}

// IsTransientUpstream determines whether an upstream HTTP error is a transient failure (retryable).
//
//   - Client parameter/permission errors (invalid_parameter_error, etc.) -> Never retry
//   - Queue/service not ready signals (isQueued / serviceAvailable=false) -> Transient (retry after upstream suggested duration)
//   - 418 (upstream wrapping provider faults as 418+provider_error) and 5xx -> Transient
//   - Remaining 4xx with provider_error ("Error in upstream response") -> Transient
//   - 401/403/429: Credentials/rate limits have dedicated paths and are not transient retry types
//
// Note: Queue signals arrive via 403 (observed on free models), so queue status must be checked before status codes.
func IsTransientUpstream(code int, detail string) bool {
	if _, queued := ParseQueueSignal(detail); queued {
		return true
	}
	if code == 401 || code == 403 || code == 429 {
		return false
	}
	if core.ContainsAny(detail, clientFaultMarkers) {
		return false
	}
	if transientHTTPCodes[code] || code >= 500 {
		return true
	}
	if code >= 400 && code < 500 && strings.Contains(detail, "provider_error") {
		return true
	}
	return false
}

// IsTransientTransport determines transport-layer transient failures (common for qoder.sh TLS/connection jitter):
// SSL EOF/reset, connection reset/abort, timeouts, etc. Context cancellation, configuration/certificate errors are excluded.
func IsTransientTransport(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Targeted check: Go standard library net.Error timeout (including timeouts wrapped in url.Error)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	if core.ContainsAny(msg, nonTransientTransportMarkers) {
		return false
	}
	for _, k := range []string{"ssl", "eof", "reset", "timed out", "timeout", "deadline exceeded", "broken pipe", "connection"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

// truncateRunes truncates by rune to prevent cutting UTF-8 multi-byte characters and causing client garbled text.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// UpstreamError carries structured upstream errors: HTTP status + details + classified
// error type and client-readable Chinese message.
type UpstreamError struct {
	Status  int    // Upstream HTTP status (0 for in-stream business errors)
	Detail  string // Raw upstream details (fully retained for log troubleshooting)
	ErrType string // content_policy_rejected / upstream_transient_error / upstream_model_busy / upstream_error
	Message string // Client-readable message (length-truncated)
	// Queue is the upstream queue signal (non-empty only when ErrTypeModelBusy), facilitating upper layers to read modelKey/wait duration.
	Queue *QueueSignal
}

func (e *UpstreamError) Error() string { return e.Message }

// FriendlyUpstreamError converts upstream errors into client-readable messages; transient failures provide retry guidance.
// Returns (message, errType). Sentence-by-sentence alignment with hub friendly_upstream_error.
func FriendlyUpstreamError(code int, detail string) (string, string) {
	detail = strings.TrimSpace(detail)
	// 0) Queuing/service not ready: Common for free models, wait a moment and retry; never treat this as a quota or credential error.
	if signal, queued := ParseQueueSignal(detail); queued {
		return fmt.Sprintf("Upstream queuing (Model %s, recommended retry after %s): The model is currently queuing, which is not a quota or credential issue. Upstream details: %s",
				core.FirstNonEmpty(signal.ModelKey, "unknown"), signal.WaitDuration(), truncateRunes(detail, 300)),
			ErrTypeModelBusy
	}
	// 1) Content safety review (deterministic rejection, precedes transient check - retries are ineffective)
	if IsContentPolicy(detail) {
		return "Upstream content safety check failed (DataInspectionFailed): Input may contain inappropriate content, " +
				"this is a deterministic rejection and retries are ineffective. Please check/shorten the input (system prompts, excessively long history, " +
				"tool definitions, or pasted code/text) and try again. Upstream details: " + truncateRunes(detail, 300),
			ErrTypeContentPolicy
	}
	if IsTransientUpstream(code, detail) ||
		(strings.Contains(detail, "provider_error") && !strings.Contains(detail, "invalid_")) {
		d := truncateRunes(detail, 300)
		if d == "" {
			d = "(No details)"
		}
		return fmt.Sprintf("Upstream transient failure (HTTP %d): Gateway automatically retried for the same account but still failed, please try again later. Upstream details: %s", code, d),
			ErrTypeTransient
	}
	return fmt.Sprintf("upstream %d: %s", code, detail), ErrTypeUpstream
}

// NewUpstreamError constructs a structured error based on HTTP status + details and maps it to a friendly message.
func NewUpstreamError(code int, detail string) *UpstreamError {
	msg, et := FriendlyUpstreamError(code, detail)
	err := &UpstreamError{Status: code, Detail: detail, ErrType: et, Message: msg}
	if et == ErrTypeModelBusy {
		if signal, queued := ParseQueueSignal(detail); queued {
			err.Queue = &signal
		}
	}
	return err
}

// NewStreamBusinessError In-stream business error (HTTP 200 frame with code != 0):
// Not an HTTP-layer transient failure; no retry guidance is provided except for content moderation and queuing.
func NewStreamBusinessError(detail string) *UpstreamError {
	if signal, queued := ParseQueueSignal(detail); queued {
		msg, et := FriendlyUpstreamError(503, detail)
		return &UpstreamError{Status: 503, Detail: detail, ErrType: et, Message: msg, Queue: &signal}
	}
	if IsContentPolicy(detail) {
		msg, et := FriendlyUpstreamError(502, detail)
		return &UpstreamError{Status: 502, Detail: detail, ErrType: et, Message: msg}
	}
	return &UpstreamError{Status: 0, Detail: detail, ErrType: ErrTypeUpstream, Message: detail}
}

// WrapTransportError Transport-layer error wrapper: converts transient errors into UpstreamError with friendly messages,
// and returns non-transient errors (such as URL configuration errors) as-is.
func WrapTransportError(err error) error {
	if err == nil {
		return nil
	}
	if IsTransientTransport(err) {
		return NewUpstreamError(502, err.Error())
	}
	return err
}

// ErrorStatus retrieves the HTTP status corresponding to the error. Categorized mapping takes precedence over passing through upstream status:
//   - Content safety rejection -> 400 (user input issue, client should modify input rather than retry)
//   - Transient failure exhaustion -> 502 (do not pass through meaningless status codes like 418 wrapped by upstream)
//   - Other structured errors -> Pass through valid upstream 4xx/5xx
//   - No structured status -> 500
func ErrorStatus(err error) int {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		switch ue.ErrType {
		case ErrTypeContentPolicy:
			return 400
		case ErrTypeTransient:
			return 502
		case ErrTypeModelBusy:
			// 503 instead of 403: Queuing is not a permission/quota issue; 403 would cause the host to flag it as insufficient_quota
			// (Observed in practice: free models are mistakenly flagged as "quota exhausted"). 503 also prompts the host to retry/failover.
			return 503
		}
		if ue.Status >= 400 && ue.Status <= 599 {
			return ue.Status
		}
	}
	return 500
}
