package hostbridge

// Unit test for uid case normalization (on Windows, the host registers by filename and uid once each).

import (
	"encoding/json"
	"fmt"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
)

// TestAlignCredentialUIDRewritesCaseVariant verifies that a case-insensitively equal uid
// is rewritten to the filename spelling while leaving all other fields completely untouched.
func TestAlignCredentialUIDRewritesCaseVariant(t *testing.T) {
	raw := []byte("{\n  \"uid\": \"zcode-eyjh---1kc8\",\n  \"jwt_token\": \"t\",\n  \"expires_at\": 9007199254740993,\n  \"note\": \"a<b&c\"\n}")
	aligned := AlignCredentialUID(raw, "zcode-eyJh---1KC8.json")

	var fields map[string]json.RawMessage
	if errParse := json.Unmarshal(aligned, &fields); errParse != nil {
		t.Fatalf("aligned payload is not json: %v", errParse)
	}
	var uid string
	if errParse := json.Unmarshal(fields["uid"], &uid); errParse != nil {
		t.Fatalf("decode uid: %v", errParse)
	}
	if uid != "zcode-eyJh---1KC8" {
		t.Fatalf("uid = %q, expected filename spelling", uid)
	}
	// Replace uid only: numbers do not lose precision due to float64 roundtrips, and escapes are not rewritten.
	if got := string(fields["expires_at"]); got != "9007199254740993" {
		t.Fatalf("expires_at = %s, value was rewritten", got)
	}
	if got := string(fields["note"]); got != "\"a<b&c\"" {
		t.Fatalf("note = %s, escape sequence was rewritten", got)
	}
	var token string
	if errParse := json.Unmarshal(fields["jwt_token"], &token); errParse != nil || token != "t" {
		t.Fatalf("jwt_token = %v (%v)", token, errParse)
	}
}

// TestAlignCredentialUIDLeavesOthersAlone verifies that inputs that should not be changed are returned as-is.
func TestAlignCredentialUIDLeavesOthersAlone(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		file    string
	}{
		{"已一致", "{\"uid\":\"zcode-AbC\"}", "zcode-AbC.json"},
		{"供应商自有 uid", "{\"uid\":\"a707ecea-6211-4e4e\"}", "workbuddycn-A707ECEA.json"},
		{"无 uid 字段", "{\"jwt_token\":\"t\"}", "zcode-AbC.json"},
		{"uid 非字符串", "{\"uid\":123}", "zcode-123.json"},
		{"非 JSON", "not json", "x.json"},
		{"JSON 数组", "[\"uid\"]", "x.json"},
		{"文件名为空", "{\"uid\":\"ZCODE-abc\"}", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := AlignCredentialUID([]byte(testCase.payload), testCase.file)
			if string(got) != testCase.payload {
				t.Fatalf("payload must pass through, got %s", got)
			}
		})
	}
}

// TestAlignCredentialUIDAcceptsUppercaseExtension locks in that the .JSON extension is also stripped.
func TestAlignCredentialUIDAcceptsUppercaseExtension(t *testing.T) {
	aligned := AlignCredentialUID([]byte("{\"uid\":\"qodercn-abcd\"}"), "qodercn-ABCD.JSON")
	var fields map[string]json.RawMessage
	if errParse := json.Unmarshal(aligned, &fields); errParse != nil {
		t.Fatalf("aligned payload is not json: %v", errParse)
	}
	var uid string
	if errParse := json.Unmarshal(fields["uid"], &uid); errParse != nil {
		t.Fatalf("decode uid: %v", errParse)
	}
	if uid != "qodercn-ABCD" {
		t.Fatalf("uid = %q", uid)
	}
}

// TestSaveAuthAlignsBeforeHandoff locks in that "the uid handed to the host is normalized": there is only one
// path for hostbridge.SaveAuth, and the JSON received by the host is identical to what the caller persists.
func TestSaveAuthAlignsBeforeHandoff(t *testing.T) {
	var handed pluginapi.HostAuthSaveRequest
	Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
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
	t.Cleanup(func() { Configure(nil) })

	payload, errSave := SaveAuth("cb-1", "zcode-eyJh---1KC8.json", []byte("{\"uid\":\"zcode-eyjh---1kc8\"}"))
	if errSave != nil {
		t.Fatalf("SaveAuth: %v", errSave)
	}
	if handed.Name != "zcode-eyJh---1KC8.json" {
		t.Fatalf("name = %q", handed.Name)
	}
	for name, raw := range map[string][]byte{"host": handed.JSON, "returned": payload} {
		var fields map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(raw, &fields); errUnmarshal != nil {
			t.Fatalf("%s payload is not json: %v", name, errUnmarshal)
		}
		var uid string
		if errUnmarshal := json.Unmarshal(fields["uid"], &uid); errUnmarshal != nil {
			t.Fatalf("%s uid: %v", name, errUnmarshal)
		}
		if uid != "zcode-eyJh---1KC8" {
			t.Fatalf("%s uid = %q, want filename spelling", name, uid)
		}
	}
}
