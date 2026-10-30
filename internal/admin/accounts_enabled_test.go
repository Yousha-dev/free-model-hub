package admin

// This file locks down "enable/disable account": it writes the disabled flag on the credential JSON,
// and the host decides whether to participate in routing based on this field after scanning the disk.
//
// Key constraints:
//   - Only add or remove the disabled key; all other fields must be byte-for-byte semantically equivalent (upstream uid may exceed
//     2^53, going through map[string]any round-trip will lose precision);
//   - Accounts already in the target state are not rewritten to disk—every disk write by the host triggers a scan and reload.

import (
	"encoding/json"
	"net/http"
	"testing"

	"free-model-hub/cpasdk/pluginapi"
)

func TestSetCredentialDisabledKeepsOtherFields(t *testing.T) {
	original := []byte(`{"vendor":"workbuddycn","uid":"9007199254740993","label":"工作号","api_key":"sk-x"}`)

	disabled, errDisable := setCredentialDisabled(original, true)
	if errDisable != nil {
		t.Fatalf("disable: %v", errDisable)
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(disabled, &fields); errUnmarshal != nil {
		t.Fatalf("disabled payload is not json: %v", errUnmarshal)
	}
	if string(fields["disabled"]) != "true" {
		t.Fatalf("disabled = %s, want true", fields["disabled"])
	}
	// uid is an integer exceeding 2^53: using map[string]any would be rewritten by float64 to ...992.
	if got := string(fields["uid"]); got != `"9007199254740993"` {
		t.Fatalf("uid was rewritten: %s", got)
	}
	if string(fields["api_key"]) != `"sk-x"` {
		t.Fatalf("remaining fields must be kept as-is: %s", fields["api_key"])
	}

	enabled, errEnable := setCredentialDisabled(disabled, false)
	if errEnable != nil {
		t.Fatalf("enable: %v", errEnable)
	}
	// Decode with a new map: json.Unmarshal only overwrites present keys, reusing the old map would leave disabled behind.
	var enabledFields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(enabled, &enabledFields); errUnmarshal != nil {
		t.Fatalf("enabled payload is not json: %v", errUnmarshal)
	}
	if _, exists := enabledFields["disabled"]; exists {
		t.Fatalf("enabling must delete the disabled key, actual = %s", enabled)
	}
	if string(enabledFields["uid"]) != `"9007199254740993"` {
		t.Fatalf("uid was rewritten after enabling round-trip: %s", enabledFields["uid"])
	}
}

func TestSetCredentialDisabledRejectsNonObject(t *testing.T) {
	if _, errSet := setCredentialDisabled([]byte(`"not-an-object"`), true); errSet == nil {
		t.Fatal("non-object credentials must be rejected instead of writing back a bad file")
	}
}

// TestAccountEnabledRequestRejectsEmptyTargets locks down the endpoint contract: when there are no targets, it does not guess "all",
// returning 400 directly—batch disabling all accounts is a destructive operation and must have targets explicitly listed by the page.
func TestAccountEnabledRequestRejectsEmptyTargets(t *testing.T) {
	service := newService(t)
	body, errMarshal := json.Marshal(map[string]any{"account_ids": []string{}, "enabled": false})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	response := service.handleAccountEnabledRequest(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Body:   body,
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", response.StatusCode, http.StatusBadRequest, response.Body)
	}
}
