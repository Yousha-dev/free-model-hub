package errclass

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Expected reports are literal fixtures, not outputs of the normalizer or a
// copy of its classification helpers. An empty want means byte-identical.
type reportCase struct {
	name   string
	status int
	body   string
	want   string
}

func reportCases() []reportCase {
	return []reportCase{
		{"auth type", 401, `{"error":{"type":"invalid_request_error","message":"invalid key"}}`, `{"error":{"type":"authentication_error","message":"invalid key"}}`},
		{"auth code", 401, `{"error":{"code":"invalid_request_error","message":"invalid key"}}`, `{"error":{"code":"auth_failure","message":"invalid key"}}`},
		{"permission", 403, `{"error":{"type":"bad_request_error","message":"denied"}}`, `{"error":{"type":"permission_error","message":"denied"}}`},
		{"quota permission", 403, `{"error":{"code":"invalid_request_error","message":"quota depleted"}}`, `{"error":{"code":"quota_exhaustion","message":"quota depleted"}}`},
		{"server", 503, `{"error":{"type":"invalid_request","code":"invalid_request_error","message":"busy"}}`, `{"error":{"type":"server_error","code":"upstream_server_failure","message":"busy"}}`},
		{"response error", 401, `{"response":{"error":{"type":"invalid_request_error","message":"invalid key"}}}`, `{"response":{"error":{"type":"authentication_error","message":"invalid key"}}}`},
		{"body error", 503, `{"body":{"error":{"code":"invalid_request_error","message":"busy"}}}`, `{"body":{"error":{"code":"upstream_server_failure","message":"busy"}}}`},
		{"all host locations", 403, `{"type":"invalid_request_error","code":"invalid_request_error","error":{"type":"invalid_request"},"response":{"error":{"code":"invalid_request_error"}},"body":{"error":{"type":"bad_request_error"}}}`, `{"type":"permission_error","code":"auth_failure","error":{"type":"permission_error"},"response":{"error":{"code":"auth_failure"}},"body":{"error":{"type":"permission_error"}}}`},
		{"case and whitespace", 401, `{"type":" BAD_REQUEST_ERROR ","error":{"code":" Invalid_Request_Error ","message":"unchanged"}}`, `{"type":"authentication_error","error":{"code":"auth_failure","message":"unchanged"}}`},
		{"null error with root marker", 401, `{"error":null,"type":"invalid_request_error"}`, `{"error":null,"type":"authentication_error"}`},
		{"string error with root marker", 401, `{"error":"invalid key","type":"invalid_request_error"}`, `{"error":"invalid key","type":"authentication_error"}`},
		{"unrelated values", 503, `{"error":{"type":"bad_request_error","message":"reason \"exact\"\nline"},"big":9007199254740993,"nested":{"type":"invalid_prompt"},"opaque":"{\"type\":\"invalid_request_error\"}"}`, `{"error":{"type":"server_error","message":"reason \"exact\"\nline"},"big":9007199254740993,"nested":{"type":"invalid_prompt"},"opaque":"{\"type\":\"invalid_request_error\"}"}`},
		{"duplicate code", 503, `{"code":"context_length_exceeded","code":"invalid_request_error"}`, ""},
		{"duplicate escaped code", 503, `{"code":"context_length_exceeded","\u0063ode":"invalid_request_error"}`, ""},
		{"duplicate ancestor", 503, `{"error":{"code":"context_length_exceeded"},"error":{"code":"invalid_request_error"}}`, ""},
		{"duplicate response ancestor", 503, `{"response":{"error":{"code":"invalid_value"}},"response":{"error":{"type":"invalid_request_error"}}}`, ""},
		{"duplicate reason", 401, `{"error":{"type":"invalid_request_error","message":"first reason","message":"second reason"}}`, ""},
		{"duplicate unrelated nested value", 503, `{"error":{"type":"bad_request_error"},"details":[{"note":"first","note":"second"}]}`, ""},
		{"already safe JSON", 401, ` { "error": {"type":"server_error","message":"Invalid credential"} } `, ""},
		{"existing auth escape", 401, `{"error":{"type":" authentication_ERROR ","code":"invalid_request_error"},"type":"bad_request_error"}`, ""},
		{"model escape", 500, `{"error":{"type":"invalid_request_error"},"body":{"error":{"code":"model_not_found"}}}`, ""},
		{"model error escape", 403, `{"code":"model_not_found_error","error":{"type":"invalid_request_error"}}`, ""},
		{"payment authoritative", 402, `{"error":{"type":"invalid_request_error","code":"invalid_request_error","message":"billing"}}`, ""},
		{"rate limit authoritative", 429, `{"error":{"type":"invalid_request_error","code":"invalid_request_error","message":"quota"}}`, ""},
		{"bad request", 400, `{"error":{"type":"invalid_request_error","message":"bad input"}}`, ""},
		{"conflict", 409, `{"type":"invalid_request_error"}`, ""},
		{"too large", 413, `{"type":"invalid_request_error"}`, ""},
		{"unprocessable", 422, `{"type":"invalid_request_error"}`, ""},
		{"thread state", 404, `{"error":{"type":"not_found_error","message":"thread state previous_message_id missing"}}`, ""},
		{"item not persisted", 404, "Item with id abc not found. Items are not persisted when `store` is set to false", ""},
		{"plain text", 401, "Invalid credential", ""},
		{"malformed", 503, `{"error":{"type":"invalid_request_error"`, ""},
		{"null", 401, `null`, ""},
		{"array", 401, `[{"type":"invalid_request_error"}]`, ""},
		{"JSON string", 401, `"{\"type\":\"invalid_request_error\"}"`, ""},
		{"no object markers", 401, `{"error":null,"body":[],"response":{"error":"invalid_request_error"}}`, ""},
		{"uninspected nested marker", 401, `{"detail":{"type":"invalid_request_error"}}`, ""},
	}
}

func TestReportedMessage(t *testing.T) {
	for _, tc := range reportCases() {
		t.Run(tc.name, func(t *testing.T) {
			input := FromStatus(tc.status, tc.body)
			wire := ToEnvelopeError(input)
			if wire.HTTPStatus != tc.status || wire.Code != string(input.Class) || wire.Retryable != input.Retryable {
				t.Fatalf("wire metadata changed: %+v", wire)
			}
			if tc.want == "" {
				if wire.Message != tc.body {
					t.Fatalf("report must be byte-identical: %s -> %s", tc.body, wire.Message)
				}
				return
			}
			decode := func(raw string) any {
				decoder := json.NewDecoder(strings.NewReader(raw))
				decoder.UseNumber() // Do not round large unrelated JSON integers.
				var value any
				if err := decoder.Decode(&value); err != nil {
					t.Fatalf("decode report: %v", err)
				}
				return value
			}
			if !reflect.DeepEqual(decode(wire.Message), decode(tc.want)) {
				t.Fatalf("report = %s, want %s", wire.Message, tc.want)
			}
			if input.Message != tc.body {
				t.Fatal("input error was mutated")
			}
		})
	}
}

func TestSpecificRequestMarkersVetoWholeRewrite(t *testing.T) {
	paths := []string{
		`{"code":%q,"error":{"type":"invalid_request_error"}}`,
		`{"error":{"code":%q},"type":"bad_request_error"}`,
		`{"response":{"error":{"code":%q}},"error":{"type":"invalid_request"}}`,
		`{"body":{"error":{"code":%q}},"code":"invalid_request_error"}`,
	}
	for _, code := range []string{"cyber_policy", "context_length_exceeded", "message_too_big", "string_above_max_length", "invalid_prompt", "invalid_value", "unsupported_value", "previous_response_not_found"} {
		for _, shape := range paths {
			for _, status := range []int{401, 403, 503} {
				body := fmt.Sprintf(shape, " "+code+" ")
				if got := ToEnvelopeError(FromStatus(status, body)).Message; got != body {
					t.Errorf("HTTP %d, %s: specific request fault rewritten: %s", status, code, got)
				}
			}
		}
	}
	for _, body := range []string{
		`{"type":" invalid_prompt ","error":{"type":"invalid_request_error"}}`,
		`{"body":{"error":{"type":"invalid_prompt"}},"type":"bad_request_error"}`,
	} {
		if got := ToEnvelopeError(FromStatus(503, body)).Message; got != body {
			t.Fatalf("specific type rewritten: %s", got)
		}
	}
}

func TestReportsWithoutStatusOrBeforeRetryBoundaryStayUnchanged(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error","message":"reason"}}`
	for _, e := range []*Error{
		FromNetwork(fmt.Errorf("host callback timed out")),
		FromNetwork(fmt.Errorf("i/o timeout")),
		PostFirstByteNetwork(body),
		UpstreamFallback(body),
		{Class: ClassAuth, Message: body, StatusCode: 401},                         // non-retryable
		{Class: ClassUnsupported, Message: body, StatusCode: 401, Retryable: true}, // class mismatch
	} {
		if got := ToEnvelopeError(e).Message; got != e.Message {
			t.Errorf("unexpected rewrite of %+v: %s", e, got)
		}
	}
}
