package tests

// Cross-layer regression for case-insensitive uid normalization: StorageJSON returned to the host by host parsing (auth.parse)
// Must match the file name in spelling, otherwise an extra account will be registered on Windows.

import (
	"encoding/json"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
)

func TestAuthParseAlignsUIDWithFileName(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
		FileName: "zcode-eyJh---1KC8.json",
		RawJSON:  []byte("{\"vendor\":\"zcode\",\"uid\":\"zcode-eyjh---1kc8\",\"jwt_token\":\"t\",\"api_key\":\"k\"}"),
	})
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode parse response: %v", errUnmarshal)
	}
	if !response.Handled {
		t.Fatalf("zcode credential must be handled: %s", string(result))
	}

	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(response.Auth.StorageJSON, &fields); errUnmarshal != nil {
		t.Fatalf("storage json: %v", errUnmarshal)
	}
	var uid string
	if errUnmarshal := json.Unmarshal(fields["uid"], &uid); errUnmarshal != nil {
		t.Fatalf("uid: %v", errUnmarshal)
	}
	if uid != "zcode-eyJh---1KC8" {
		t.Fatalf("uid = %q, want filename spelling (otherwise the host will register an extra entry)", uid)
	}
	// Other fields are kept as-is, and the ID is still derived from the file name.
	var token string
	if errUnmarshal := json.Unmarshal(fields["jwt_token"], &token); errUnmarshal != nil || token != "t" {
		t.Fatalf("jwt_token = %q (%v)", token, errUnmarshal)
	}
	if response.Auth.ID != "zcode-eyJh---1KC8" {
		t.Fatalf("id = %q", response.Auth.ID)
	}
}
