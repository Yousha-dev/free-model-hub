package admin

import (
	"encoding/json"
	"fmt"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/hostbridge"
)

// TestSaveAuthFileAlignsUID locks that the write path for "add account / paste callback" also normalizes the uid.
//
// The host registers once by filename and once by uid on Windows; a difference only in casing would result in two accounts.
// Normalization is consolidated in hostbridge.SaveAuth; admin should not construct host.auth.save requests manually.
func TestSaveAuthFileAlignsUID(t *testing.T) {
	var handed pluginapi.HostAuthSaveRequest
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthSave {
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		if errUnmarshal := json.Unmarshal(raw, &handed); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return json.RawMessage("{}"), nil
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })

	raw := []byte("{\"vendor\":\"zcode\",\"uid\":\"zcode-eyjh---1kc8\",\"jwt_token\":\"t\"}")
	if errSave := saveAuthFile("cb-1", "zcode-eyJh---1KC8.json", raw); errSave != nil {
		t.Fatalf("saveAuthFile: %v", errSave)
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(handed.JSON, &fields); errUnmarshal != nil {
		t.Fatalf("saved payload is not json: %v", errUnmarshal)
	}
	var uid string
	if errUnmarshal := json.Unmarshal(fields["uid"], &uid); errUnmarshal != nil {
		t.Fatalf("uid: %v", errUnmarshal)
	}
	if uid != "zcode-eyJh---1KC8" {
		t.Fatalf("uid = %q, want consistent with filename", uid)
	}
	var vendor string
	if errUnmarshal := json.Unmarshal(fields["vendor"], &vendor); errUnmarshal != nil || vendor != "zcode" {
		t.Fatalf("vendor = %q (%v), remaining fields must be kept as-is", vendor, errUnmarshal)
	}
}
