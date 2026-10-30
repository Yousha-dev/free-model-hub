// Package errclass implements FR-009 error classification and the
// resolved §7 retry semantics (07-open-questions.md).
package errclass

import (
	"fmt"
	"strings"

	"free-model-hub/cpasdk/pluginabi"
)

// Class is a stable error-category identifier (FR-009) used as the
// envelope error Code.
type Class string

const (
	ClassAuth         Class = "auth_failure"
	ClassInvalidModel Class = "invalid_model"
	ClassUnsupported  Class = "unsupported_protocol_or_parameter"
	ClassRateLimit    Class = "rate_limit"
	ClassQuota        Class = "quota_exhaustion"
	ClassBilling      Class = "billing"
	ClassUpstream     Class = "upstream_server_failure"
	ClassNetwork      Class = "timeout_or_network_failure"
	ClassTranslation  Class = "translation_failure"
)

// Error describes classified request errors and retryability for CPA execution.
type Error struct {
	Class      Class
	Message    string
	StatusCode int
	Retryable  bool
}

func (e *Error) Error() string {
	return string(e.Class) + ": " + e.Message
}

// FromStatus classifies an upstream HTTP status per resolved §7.
// Network/timeout failures (no status) use FromNetwork instead. A redacted
// message containing "quota" (case-insensitive) upgrades 429/402/403 to
// ClassQuota — a bounded heuristic given no documented upstream quota
// taxonomy.
func FromStatus(status int, message string) *Error {
	msg := message
	lowerMsg := strings.ToLower(msg)
	quota := strings.Contains(lowerMsg, "quota")
	quotaClass := func(base Class) Class {
		if quota {
			return ClassQuota
		}
		return base
	}
	switch status {
	case 401:
		return ensureMessage(&Error{Class: ClassAuth, Message: msg, StatusCode: status, Retryable: true})
	case 403:
		return ensureMessage(&Error{Class: quotaClass(ClassAuth), Message: msg, StatusCode: status, Retryable: true})
	case 402:
		return ensureMessage(&Error{Class: quotaClass(ClassBilling), Message: msg, StatusCode: status, Retryable: true})
	case 404:
		return ensureMessage(&Error{Class: ClassInvalidModel, Message: msg, StatusCode: status})
	case 429:
		return ensureMessage(&Error{Class: quotaClass(ClassRateLimit), Message: msg, StatusCode: status, Retryable: true})
	}
	if status >= 500 {
		return ensureMessage(&Error{Class: ClassUpstream, Message: msg, StatusCode: status, Retryable: true})
	}
	return ensureMessage(&Error{Class: ClassUnsupported, Message: msg, StatusCode: status})
}

// FromNetwork classifies a PRE-first-byte network/timeout failure; always
// retryable (§7). Failures observed after the first upstream byte must use
// PostFirstByteNetwork instead.
func FromNetwork(err error) *Error {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return ensureMessage(&Error{Class: ClassNetwork, Message: msg, Retryable: true})
}

// PostFirstByteNetwork classifies a network/timeout failure seen AFTER the
// first upstream byte was received: part of the response may already be
// delivered downstream, so a retry would risk duplicate delivery of a half
// stream — never retryable (§7).
func PostFirstByteNetwork(msg string) *Error {
	return ensureMessage(&Error{Class: ClassNetwork, Message: msg, Retryable: false})
}

// Translation reports an FR-005/FR-006 translation failure; never retryable.
func Translation(msg string) *Error {
	return ensureMessage(&Error{Class: ClassTranslation, Message: msg})
}

// UpstreamFallback builds a ClassUpstream error for the adapter fallback
// sites (chunk/failure errors) that have no upstream status code. Resolved
// §7 makes upstream server failures retryable; the message is redacted at
// construction.
func UpstreamFallback(msg string) *Error {
	return ensureMessage(&Error{Class: ClassUpstream, Message: msg, Retryable: true})
}

// ensureMessage keeps a classified error self-describing. An empty message
// surfaces to the operator as the host's generic "plugin call failed"
// placeholder (and as a blank reason in logs), hiding both the failure class
// and the upstream cause, so a class-derived fallback is substituted instead.
func ensureMessage(e *Error) *Error {
	if strings.TrimSpace(e.Message) == "" {
		e.Message = fallbackMessage(e)
	}
	return e
}

// fallbackMessage names the failure class (and the upstream status when one is
// known) for errors that carry no detail of their own, e.g. an upstream that
// refused a request with an empty body.
func fallbackMessage(e *Error) string {
	class := string(e.Class)
	if class == "" {
		class = "unclassified"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("upstream returned HTTP %d (%s) without a message", e.StatusCode, class)
	}
	return fmt.Sprintf("%s without a message", class)
}

// ToEnvelopeError converts to the SDK wire type. Upstream reason text is kept;
// generic JSON request-fault markers on eligible HTTP failures are normalized
// so the host can continue credential failover (see report.go).
func ToEnvelopeError(e *Error) pluginabi.Error {
	status := e.StatusCode
	if status == 0 {
		switch e.Class {
		case ClassUnsupported, ClassTranslation:
			status = 400
		}
	}
	message := e.Message
	if strings.TrimSpace(message) == "" {
		// Defense in depth for errors built outside the constructors: the
		// host substitutes its generic placeholder for an empty message.
		message = fallbackMessage(e)
	}
	return pluginabi.Error{
		Code:       string(e.Class),
		Message:    reportedMessage(e, message),
		Retryable:  e.Retryable,
		HTTPStatus: status,
	}
}
