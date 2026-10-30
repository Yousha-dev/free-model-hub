package errclass

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
)

func TestClassConstants(t *testing.T) {
	want := map[Class]string{
		ClassAuth:         "auth_failure",
		ClassInvalidModel: "invalid_model",
		ClassUnsupported:  "unsupported_protocol_or_parameter",
		ClassRateLimit:    "rate_limit",
		ClassQuota:        "quota_exhaustion",
		ClassBilling:      "billing",
		ClassUpstream:     "upstream_server_failure",
		ClassNetwork:      "timeout_or_network_failure",
		ClassTranslation:  "translation_failure",
	}
	for cls, s := range want {
		if string(cls) != s {
			t.Errorf("Class(%q) constant = %q, want %q", s, string(cls), s)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	e := &Error{Class: ClassRateLimit, Message: "slow down", StatusCode: 429}
	if got := e.Error(); got != "rate_limit: slow down" {
		t.Errorf("Error() = %q", got)
	}
}

func TestFromStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		message   string
		class     Class
		retryable bool
	}{
		{"401 auth failure", 401, "bad key", ClassAuth, true},
		{"401 quota text stays auth", 401, "quota exhausted for key", ClassAuth, true},
		{"403 forbidden auth failure", 403, "no access", ClassAuth, true},
		{"402 billing failure", 402, "payment required", ClassBilling, true},
		{"404 invalid model", 404, "no such model", ClassInvalidModel, false},
		{"429 rate limit", 429, "too many", ClassRateLimit, true},
		{"429 quota text upgrades", 429, "quota exceeded for project", ClassQuota, true},
		{"429 Go usage limit", 429, "GoUsageLimitError", ClassRateLimit, true},
		{"403 quota text upgrades", 403, "quota exhausted", ClassQuota, true},
		{"402 quota text bills as quota", 402, "quota depleted", ClassQuota, true},
		{"408 client timeout unsupported", 408, "request timeout", ClassUnsupported, false},
		{"502 upstream", 502, "bad gateway", ClassUpstream, true},
		{"500 upstream", 500, "boom", ClassUpstream, true},
		{"599 upstream", 599, "weird", ClassUpstream, true},
		{"400 unsupported", 400, "bad field", ClassUnsupported, false},
		{"418 unsupported", 418, "teapot", ClassUnsupported, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FromStatus(tc.status, tc.message)
			if got.Class != tc.class || got.Retryable != tc.retryable ||
				got.StatusCode != tc.status || got.Message != tc.message {
				t.Errorf("FromStatus(%d,%q) = %+v", tc.status, tc.message, got)
			}
		})
	}
}

func TestFromNetwork(t *testing.T) {
	e := FromNetwork(errors.New("dial tcp: refused"))
	if e.Class != ClassNetwork || !e.Retryable || e.StatusCode != 0 ||
		e.Message != "dial tcp: refused" {
		t.Errorf("FromNetwork(err) = %+v", e)
	}
	nilErr := FromNetwork(nil)
	// A nil cause still yields a message: the host renders an empty one as its
	// generic "plugin call failed" placeholder, which hides the class.
	if nilErr.Message == "" || !nilErr.Retryable {
		t.Errorf("FromNetwork(nil) = %+v", nilErr)
	}
}

// Regression: every classified error must carry a message. The host replaces an
// empty message with "plugin call failed", hiding both the class and the
// upstream cause from the operator.
func TestClassifiedErrorsAlwaysCarryAMessage(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string // substring the fallback must name
	}{
		{"status without a message", FromStatus(http.StatusBadRequest, ""), "400"},
		{"upstream status without a message", FromStatus(http.StatusBadGateway, ""), "upstream_server_failure"},
		{"network without a cause", FromNetwork(nil), "timeout_or_network_failure"},
		{"post-first-byte without a message", PostFirstByteNetwork(""), "timeout_or_network_failure"},
		{"translation without a message", Translation(""), "translation_failure"},
		{"upstream fallback without a message", UpstreamFallback(""), "upstream_server_failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.TrimSpace(tc.err.Message) == "" {
				t.Fatalf("message is empty: %+v", tc.err)
			}
			if !strings.Contains(tc.err.Message, tc.want) {
				t.Fatalf("message %q does not name the failure (%q)", tc.err.Message, tc.want)
			}
			wire := ToEnvelopeError(tc.err)
			if strings.TrimSpace(wire.Message) == "" || wire.Message == "plugin call failed" {
				t.Fatalf("envelope message = %q", wire.Message)
			}
		})
	}
	// A real reason is never replaced by the fallback.
	if got := FromStatus(http.StatusBadRequest, "upstream says no").Message; got != "upstream says no" {
		t.Fatalf("real message replaced: %q", got)
	}
}

// Errors built as struct literals bypass the constructors; the envelope edge
// still substitutes a fallback so the host never prints its generic
// "plugin call failed" placeholder in place of the failure class.
func TestEnvelopeFillsAnEmptyLiteralMessage(t *testing.T) {
	wire := ToEnvelopeError(&Error{Class: ClassQuota, StatusCode: http.StatusTooManyRequests})
	if strings.TrimSpace(wire.Message) == "" || wire.Message == "plugin call failed" {
		t.Fatalf("envelope message = %q", wire.Message)
	}
	if !strings.Contains(wire.Message, "quota_exhaustion") || !strings.Contains(wire.Message, "429") {
		t.Fatalf("fallback must name class and status: %q", wire.Message)
	}
}

func TestTranslation(t *testing.T) {
	e := Translation("cannot map tool calls")
	if e.Class != ClassTranslation || e.Retryable || e.StatusCode != 0 {
		t.Errorf("Translation() = %+v", e)
	}
}

func TestUpstreamFallback(t *testing.T) {
	e := UpstreamFallback("upstream stream broke: Bearer sk-secret99 mid-chunk")
	if e.Class != ClassUpstream {
		t.Errorf("Class = %q, want %q", e.Class, ClassUpstream)
	}
	if !e.Retryable {
		t.Error("Retryable = false, want true (§7: upstream server failures are retryable)")
	}
	if e.StatusCode != 0 {
		t.Errorf("StatusCode = %+v", e)
	}
	want := "upstream stream broke: Bearer sk-secret99 mid-chunk"
	if e.Message != want {
		t.Errorf("Message = %q, want verbatim %q", e.Message, want)
	}
}

func TestPostFirstByteNetwork(t *testing.T) {
	e := PostFirstByteNetwork("stream read after first byte: Bearer sk-secret99 dropped")
	if e.Class != ClassNetwork {
		t.Errorf("Class = %q, want %q", e.Class, ClassNetwork)
	}
	if e.Retryable {
		t.Error("Retryable = true, want false (§7: post-first-byte failures must never retry)")
	}
	if e.StatusCode != 0 {
		t.Errorf("StatusCode = %+v", e)
	}
	want := "stream read after first byte: Bearer sk-secret99 dropped"
	if e.Message != want {
		t.Errorf("Message = %q, want verbatim %q", e.Message, want)
	}
}

func TestToEnvelopeError(t *testing.T) {
	in := &Error{Class: ClassAuth, Message: "Bearer sk-secret failed", StatusCode: 401}
	got := ToEnvelopeError(in)
	want := pluginabi.Error{Code: "auth_failure", Message: "Bearer sk-secret failed", Retryable: false, HTTPStatus: 401}
	if got != want {
		t.Errorf("ToEnvelopeError = %+v, want %+v", got, want)
	}

	// Issue #2: ClassUnsupported and ClassTranslation with StatusCode 0 default to 400.
	unsupported := &Error{Class: ClassUnsupported, Message: "unsupported input"}
	got = ToEnvelopeError(unsupported)
	if got.HTTPStatus != 400 || got.Code != "unsupported_protocol_or_parameter" {
		t.Errorf("ToEnvelopeError(unsupported) = %+v, want HTTPStatus 400", got)
	}

	translation := Translation("cannot map tool calls")
	got = ToEnvelopeError(translation)
	if got.HTTPStatus != 400 || got.Code != "translation_failure" {
		t.Errorf("ToEnvelopeError(translation) = %+v, want HTTPStatus 400", got)
	}

	// Explicit status code is preserved.
	explicit := &Error{Class: ClassUnsupported, Message: "bad param", StatusCode: 422}
	got = ToEnvelopeError(explicit)
	if got.HTTPStatus != 422 {
		t.Errorf("ToEnvelopeError(explicit) = %+v, want HTTPStatus 422", got)
	}

	// Other classes with StatusCode 0 remain 0.
	netErr := FromNetwork(errors.New("connection reset"))
	got = ToEnvelopeError(netErr)
	if got.HTTPStatus != 0 {
		t.Errorf("ToEnvelopeError(network) = %+v, want HTTPStatus 0", got)
	}
}

// Redaction was removed by operator decision (v0.1.13): a message reaches the
// envelope verbatim so an upstream reason is diagnosable. This pins the new
// policy, including the credential-shaped inputs the old scanner rewrote.
func TestMessagesAreReportedVerbatim(t *testing.T) {
	unchanged := []string{
		"plain failure",
		"Authorization: Bearer abc123 bad",
		"bearer lower-case tok!",
		"two: Bearer a1 and Bearer b2 end",
		"upstream said x-api-key: sk-ant-api03-9f2Kx7QwLp end",
		`{"error":{"message":"invalid x-api-key sk-live-ab12cd34ef"}}`,
		`Get "https://gw.example/v1?api_key=SECRET9key": dial tcp 1.2.3.4:443`,
		"https://gw.example/v1?api_key=S3cret&model=gpt&token=tok999 still here",
	}
	for _, in := range unchanged {
		if got := FromStatus(http.StatusBadRequest, in).Message; got != in {
			t.Errorf("FromStatus message = %q, want verbatim %q", got, in)
		}
		if got := UpstreamFallback(in).Message; got != in {
			t.Errorf("UpstreamFallback message = %q, want verbatim %q", got, in)
		}
		if got := ToEnvelopeError(&Error{Class: ClassUpstream, Message: in}).Message; got != in {
			t.Errorf("envelope message = %q, want verbatim %q", got, in)
		}
	}
}
