package errclass

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CPA v8.0.32 (911f49c) classifies request faults at these four locations.
// It does not consume the plugin envelope's Retryable field. Generic markers
// on credential/server HTTP failures can therefore stop otherwise-valid key
// failover. Only those ambiguous markers are rewritten; specific request faults
// veto the rewrite, even when they occur at another inspected location.
var reportPaths = [][]string{nil, {"error"}, {"response", "error"}, {"body", "error"}}

func reportedMessage(e *Error, message string) string {
	if !e.Retryable {
		return message
	}
	var reportType, reportCode json.RawMessage
	switch {
	case e.StatusCode == 401 && e.Class == ClassAuth:
		reportType, reportCode = json.RawMessage(`"authentication_error"`), json.RawMessage(`"auth_failure"`)
	case e.StatusCode == 403 && e.Class == ClassAuth:
		reportType, reportCode = json.RawMessage(`"permission_error"`), json.RawMessage(`"auth_failure"`)
	case e.StatusCode == 403 && e.Class == ClassQuota:
		reportType, reportCode = json.RawMessage(`"permission_error"`), json.RawMessage(`"quota_exhaustion"`)
	case e.StatusCode >= 500 && e.StatusCode <= 599 && e.Class == ClassUpstream:
		reportType, reportCode = json.RawMessage(`"server_error"`), json.RawMessage(`"upstream_server_failure"`)
	default:
		// In particular, CPA already rotates on 402/429. Statusless transport
		// errors keep their actual cause; a callback deadline is not proof of
		// an I/O timeout. Request faults and post-emission errors stay intact.
		return message
	}
	root := reportObject([]byte(message))
	if root == nil {
		return message
	}
	// Go maps retain the last duplicate key, but CPA's GJSON classifier reads
	// the first. Do not collapse an ambiguous body and lose a request-fault
	// marker or an upstream reason that the host would otherwise see.
	decoder := json.NewDecoder(strings.NewReader(message))
	decoder.UseNumber()
	if !uniqueReportKeys(decoder) {
		return message
	}
	for _, path := range reportPaths {
		obj := reportObjectAt(root, path)
		typeName, code := reportString(obj["type"]), reportString(obj["code"])
		if typeName == "invalid_prompt" || (e.StatusCode == 401 && typeName == "authentication_error") {
			return message
		}
		switch code {
		case "cyber_policy", "context_length_exceeded", "message_too_big", "string_above_max_length",
			"invalid_prompt", "invalid_value", "unsupported_value", "previous_response_not_found",
			"model_not_found", "model_not_found_error":
			return message
		}
	}
	raw := json.RawMessage(message)
	changed := false
	for _, path := range reportPaths {
		var rewritten bool
		raw, rewritten = rewriteReportMarkers(raw, path, reportType, reportCode)
		changed = changed || rewritten
	}
	if !changed {
		return message
	}
	return string(raw)
}

// uniqueReportKeys consumes one JSON value, rejecting duplicate keys in any
// object, including reason fields and objects nested inside arrays. Rejection
// means preserve the original report, not reject the upstream response.
func uniqueReportKeys(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return false
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return false
			}
			seen[key] = true
			if !uniqueReportKeys(decoder) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim('}')
	case json.Delim('['):
		for decoder.More() {
			if !uniqueReportKeys(decoder) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim(']')
	default:
		return true
	}
}

func reportObject(raw []byte) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	return obj // JSON null is nil, not a writable object.
}

func reportObjectAt(root map[string]json.RawMessage, path []string) map[string]json.RawMessage {
	for _, key := range path {
		root = reportObject(root[key])
	}
	return root
}

func reportString(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func rewriteReportMarkers(raw json.RawMessage, path []string, reportType, reportCode json.RawMessage) (json.RawMessage, bool) {
	obj := reportObject(raw)
	if obj == nil {
		return raw, false
	}
	changed := false
	if len(path) > 0 {
		obj[path[0]], changed = rewriteReportMarkers(obj[path[0]], path[1:], reportType, reportCode)
	} else {
		switch reportString(obj["type"]) {
		case "invalid_request", "invalid_request_error", "bad_request_error":
			obj["type"], changed = reportType, true
		}
		if reportString(obj["code"]) == "invalid_request_error" {
			obj["code"], changed = reportCode, true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		// Internal invariant: entries are validated RawMessages or static JSON
		// literals above, never arbitrary values supplied by another caller.
		panic(fmt.Sprintf("encode validated error report: %v", err))
	}
	return out, true
}
