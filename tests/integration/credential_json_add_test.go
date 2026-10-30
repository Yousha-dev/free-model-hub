package tests

// credential_json add channel: AutoClaw Global token pair credential (no api_key format)
// Saved via the credential_json field of /accounts/add, with ownership populated by the server.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAccountAddCredentialJSONAutoClawGlobal(t *testing.T) {
	setupTestPlugin(t)

	credential := map[string]any{
		"accessToken":  "at-test-123",
		"refreshToken": "rt-test-456",
		"region":       "global",
	}
	encoded, _ := json.Marshal(credential)

	response := managementCallWithBody(t, "POST", "/accounts/add", map[string]any{
		"vendor":          "autoclawglobal",
		"credential_json": string(encoded),
		"label":           "intl-test",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/add = %d: %s", response.StatusCode, response.Body)
	}
	var payload struct {
		OK      bool   `json:"ok"`
		AuthID  string `json:"auth_id"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode response: %v (%s)", errUnmarshal, response.Body)
	}
	if !payload.OK {
		t.Fatalf("add failed: %s", response.Body)
	}
	if !strings.HasPrefix(payload.AuthID, "autoclawglobal-") {
		t.Fatalf("auth file name = %q, want autoclawglobal-*", payload.AuthID)
	}
}

func TestAccountAddCredentialJSONRejectsMalformed(t *testing.T) {
	setupTestPlugin(t)

	response := managementCallWithBody(t, "POST", "/accounts/add", map[string]any{
		"vendor":          "autoclawglobal",
		"credential_json": "not-an-object",
	})
	// The channel must reject malformed JSON (400 Bad Request) rather than persisting a bad file.
	if response.StatusCode == http.StatusOK {
		var payload struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(response.Body, &payload)
		if payload.OK {
			t.Fatalf("malformed credential_json should not be accepted: %s", response.Body)
		}
		if !strings.Contains(string(response.Body), "JSON") {
			t.Fatalf("unexpected rejection reason: %s", response.Body)
		}
	}
}
