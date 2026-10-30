package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// This file defines the neutral contract for provider errors.
//
// Background: The plugin translates "upstream errors" into HTTP status codes recognized by the host,
// which the host uses to decide on cooling down, disabling, or rotation. Previously, the root layer
// only recognized workbuddy.Error, and all other provider errors degenerated into 502 retryable
// (401 did not invalidate credentials, 429 did not trigger rate-limit handling). Now, each provider's

// UpstreamError is the upstream error categorized by the provider.
//
// Method names deliberately avoid Kind / Status / Message: each error struct already uses these
// field names, and identical field and method names cannot coexist in Go.
type UpstreamError interface {
	error
	// HTTPStatus returns the HTTP status code mapped to the host.
	HTTPStatus() int
	// Retryable reports whether the host should retry the same request with a different credential.
	Retryable() bool
	// UpstreamKind returns the error category identifier (used for logs and error codes).
	UpstreamKind() string
}

// UpstreamDetail is implemented by errors that can provide the original upstream response body.
//
// Purpose: In streaming error frames and logs, prioritize showing the original upstream text to the user
// (which has higher diagnostic value) rather than the category description rewritten by the plugin.
type UpstreamDetail interface {
	error
	UpstreamMessage() string
}

// ToPluginError translates a provider error into a host contract error.
//
// Uncategorized errors are treated as 502 retryable: those are usually local issues (codec, connection),
// and retrying with a different account is safe.
func ToPluginError(err error) error {
	if err == nil {
		return nil
	}
	var upstreamErr UpstreamError
	if errors.As(err, &upstreamErr) {
		status := upstreamErr.HTTPStatus()
		if status <= 0 {
			status = http.StatusBadGateway
		}
		code := upstreamCode(upstreamErr.UpstreamKind())
		if upstreamErr.Retryable() {
			return RetryablePluginError(code, upstreamErr.Error(), status)
		}
		return NewPluginError(code, upstreamErr.Error(), status)
	}
	return RetryablePluginError("upstream_request_failed", err.Error(), http.StatusBadGateway)
}

// upstreamCode generates an error code: upstream_<kind>.
func upstreamCode(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "upstream_error"
	}
	return "upstream_" + kind
}

// envelope is the JSON envelope for the CPA plugin ABI, aligned with the host cpasdk/pluginabi.Envelope.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

// envelopeError carries an optional HTTP status code; the host uses it to decide whether to cool down or rotate the current credential.
type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// PluginError is a structured error internal to the plugin: it can carry an HTTP status code and a retryable flag.
//
// HTTPStatus is one of the most important contracts between the plugin and the host: the host uses it to determine
// credential handling (402 long cool-down / 429 short cool-down / 401 disable / 403 soft cool-down), see conductor_cooldown.go.
type PluginError struct {
	Code       string
	Message    string
	Retryable  bool
	HTTPStatus int
}

// Error implements the error interface.
func (e *PluginError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// NewPluginError constructs a plugin error with an HTTP status code.
func NewPluginError(code, message string, status int) *PluginError {
	return &PluginError{Code: code, Message: message, HTTPStatus: status}
}

// RetryablePluginError constructs a retryable plugin error: the host will retry the same request with a different credential.
func RetryablePluginError(code, message string, status int) *PluginError {
	return &PluginError{Code: code, Message: message, Retryable: true, HTTPStatus: status}
}

// OKEnvelope serializes an arbitrary result into a success envelope.
func OKEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal result: %w", errMarshal)
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// ErrorEnvelope constructs a failure envelope (without an HTTP status code, treated as 500 by the host).
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// ErrorEnvelopeFromError converts a business error into an envelope while preserving the HTTP status code.
//
// Use type assertion rather than errors.As for *PluginError: there is no error wrapping chain inside the plugin,
// and assertion is both sufficient and avoids misjudging wrapped errors as structured errors.
func ErrorEnvelopeFromError(err error) []byte {
	if err == nil {
		return ErrorEnvelope("plugin_error", "unknown error")
	}
	if pluginErr, ok := err.(*PluginError); ok {
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
			Code:       pluginErr.Code,
			Message:    pluginErr.Message,
			Retryable:  pluginErr.Retryable,
			HTTPStatus: pluginErr.HTTPStatus,
		}})
		return raw
	}
	return ErrorEnvelope("plugin_error", err.Error())
}

// ---------------------------------------------------------------------------
// Upstream error categories
//
// Individual providers are only responsible for translating upstream responses into FailureKind/Failure here; host status codes,
// retryable semantics, and error code strings are provided uniformly by this file. This prevents providers from importing
// each other or duplicating HTTPStatus/Retryable mappings.

// FailureKind is a disposable category of upstream error.
type FailureKind string

const (
	FailureKindNone            FailureKind = "none"
	FailureKindHardCredit      FailureKind = "hard_credit"
	FailureKindSoftRate        FailureKind = "soft_rate"
	FailureKindSessionDead     FailureKind = "session_dead"
	FailureKindNotFound        FailureKind = "not_found"
	FailureKindServer          FailureKind = "server"
	FailureKindContentBlocked  FailureKind = "content_blocked"
	FailureKindChannelRejected FailureKind = "channel_rejected"
	FailureKindBadParams       FailureKind = "bad_params"
	FailureKindAccountFault    FailureKind = "account_fault"
	FailureKindModelBlocked    FailureKind = "model_blocked"
	FailureKindWAFBlock        FailureKind = "waf_block"
	FailureKindPromptTooLong   FailureKind = "prompt_too_long"
	FailureKindImageInvalid    FailureKind = "image_invalid"
	FailureKindClient          FailureKind = "client"

	// Compatible with short categories for generic providers.
	FailureKindCredential FailureKind = "credential"
	FailureKindRateLimit  FailureKind = "rate_limit"
	FailureKindTransient  FailureKind = "transient"
	FailureKindQuota      FailureKind = "quota"
	FailureKindBusy       FailureKind = "busy"
)

// String returns a stable category name (used for logs and error codes).
func (k FailureKind) String() string {
	if k == "" {
		return "unknown"
	}
	return string(k)
}

// Failure is a categorized upstream error.
//
// Msg preserves the original upstream text; Code is a readable form of the upstream business code (can be empty). RetryAfter /
// ResetAt is the wait information explicitly specified by the upstream; a zero value indicates it was not provided.
type Failure struct {
	Kind       FailureKind
	Status     int
	Code       string
	Msg        string
	RetryAfter time.Duration
	ResetAt    time.Time

	// RetryableOverride allows the provider to override the general retryable determination (nil means derived via FailureKind).
	RetryableOverride *bool
}

// Failure implements the error interface.
func (e *Failure) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return fmt.Sprintf("upstream %s [%d/%s]: %s", e.Kind, e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("upstream %s [%d]: %s", e.Kind, e.Status, e.Msg)
}

// HTTPStatus returns the HTTP status code mapped to the host.
//
// Upstream status codes are usually already recognized by the host (401/402/429/5xx) and are passed through with priority; only
// when Status==0 (network-layer failure or business error in a 200 response body) is it derived according to FailureKind.
func (e *Failure) HTTPStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	if e.Status > 0 {
		return e.Status
	}
	switch e.Kind {
	case FailureKindHardCredit, FailureKindQuota:
		return http.StatusPaymentRequired
	case FailureKindSoftRate, FailureKindRateLimit:
		return http.StatusTooManyRequests
	case FailureKindSessionDead, FailureKindCredential:
		return http.StatusUnauthorized
	case FailureKindNotFound, FailureKindModelBlocked:
		return http.StatusNotFound
	case FailureKindServer, FailureKindTransient:
		return http.StatusBadGateway
	case FailureKindWAFBlock:
		return http.StatusForbidden
	case FailureKindBusy:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// Retryable reports whether the host should switch to another credential and retry the same request.
func (e *Failure) Retryable() bool {
	if e == nil {
		return false
	}
	if e.RetryableOverride != nil {
		return *e.RetryableOverride
	}
	switch e.Kind {
	case FailureKindContentBlocked, FailureKindChannelRejected, FailureKindPromptTooLong,
		FailureKindImageInvalid, FailureKindBadParams, FailureKindClient, FailureKindCredential:
		return false
	default:
		return true
	}
}

// UpstreamKind returns the error classification identifier.
func (e *Failure) UpstreamKind() string { return e.Kind.String() }

// UpstreamMessage returns the raw upstream body (implementing core.UpstreamDetail).
func (e *Failure) UpstreamMessage() string {
	if e == nil {
		return ""
	}
	return e.Msg
}

// Bool returns a bool pointer (used to construct RetryableOverride).
func Bool(value bool) *bool { return &value }

// ClassifyStatus is the general HTTP status classification: callers can append business code rules on top of this.
func ClassifyStatus(status int) FailureKind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return FailureKindCredential
	case status == http.StatusPaymentRequired:
		return FailureKindHardCredit
	case status == http.StatusTooManyRequests:
		return FailureKindRateLimit
	case status >= 500:
		return FailureKindTransient
	case status >= 400:
		return FailureKindClient
	default:
		return FailureKindTransient
	}
}

// Truncate truncates overly long upstream bodies to prevent error messages from flooding the logs.
func Truncate(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if limit <= 0 || len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}
