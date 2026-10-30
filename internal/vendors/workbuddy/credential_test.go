package workbuddy

import (
	"encoding/json"
	"testing"

	"free-model-hub/internal/core"
)

// TestParseCredentialBothShapes verifies that both forms of credentials can be parsed.
func TestParseCredentialBothShapes(t *testing.T) {
	nested := `{"auth":{"accessToken":"at","refreshToken":"rt","domain":"www.codebuddy.cn"},
	             "account":{"uid":"u1","nickname":"昵称"},"device_token":"dt"}`
	flat := `{"accessToken":"at","refreshToken":"rt","uid":"u1","domain":"www.workbuddy.ai","device_token":"dt"}`

	for _, testCase := range []struct {
		name       string
		raw        string
		wantRealm  Region
		wantUID    string
		wantDevice string
	}{
		{"nested", nested, RegionCN, "u1", "dt"},
		// Flat form infers domain by domain: the workbuddy.ai family is the Global version.
		{"flat global", flat, RegionGlobal, "u1", "dt"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			credential, errParse := ParseCredential([]byte(testCase.raw), RegionCN)
			if errParse != nil {
				t.Fatalf("ParseCredential: %v", errParse)
			}
			if credential.Realm() != testCase.wantRealm {
				t.Fatalf("realm = %s, want %s", credential.Realm(), testCase.wantRealm)
			}
			if credential.UIDValue() != testCase.wantUID {
				t.Fatalf("uid = %q, want %q", credential.UIDValue(), testCase.wantUID)
			}
			if credential.DeviceTokenValue() != testCase.wantDevice {
				t.Fatalf("device_token = %q", credential.DeviceTokenValue())
			}
		})
	}
}

// TestParseCredentialRejectsEmpty verifies that empty credentials are rejected.
func TestParseCredentialRejectsEmpty(t *testing.T) {
	if _, errParse := ParseCredential([]byte(`{}`), RegionCN); errParse == nil {
		t.Fatal("credential without any token must be rejected")
	}
	if _, errParse := ParseCredential(nil, RegionCN); errParse == nil {
		t.Fatal("empty credential must be rejected")
	}
}

// TestMergeStorageJSONKeepsUserFields verifies that user-maintained fields are preserved during write-back.
//
// The host auth file may contain disabled / prefix / proxy_url / note / weight, etc.
// which are fields added by the user; reconstructive write-back would silently drop them.
func TestMergeStorageJSONKeepsUserFields(t *testing.T) {
	original := []byte(`{"auth":{"accessToken":"old"},"disabled":true,"note":"我的备注","weight":3}`)
	credential := &Credential{AccessToken: "new", RefreshToken: "rt", UID: "u1"}
	credential.SetRealm(RegionCN)

	merged, errMerge := MergeStorageJSON(original, credential)
	if errMerge != nil {
		t.Fatalf("MergeStorageJSON: %v", errMerge)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(merged, &payload); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if payload["disabled"] != true {
		t.Fatal("user field 'disabled' must be preserved")
	}
	if payload["note"] != "我的备注" {
		t.Fatal("user field 'note' must be preserved")
	}
	auth, okAuth := payload["auth"].(map[string]any)
	if !okAuth || auth["accessToken"] != "new" {
		t.Fatalf("token must be updated: %v", payload["auth"])
	}
}

// TestStorageJSONAlwaysCarriesType verifies that written-back credentials always carry the type field.
//
// The host uses type to directly determine ownership (with it, plugin iteration is skipped). When this field is missing, ownership depends on
// the plugin iteration order, and it might be snatched by an overly broad preceding plugin, causing the account to be assigned to another provider on the host side.
func TestStorageJSONAlwaysCarriesType(t *testing.T) {
	credential := &Credential{AccessToken: "at", RefreshToken: "rt", UID: "u1"}
	credential.SetRealm(RegionCN)

	raw, errStorage := credential.StorageJSON()
	if errStorage != nil {
		t.Fatalf("StorageJSON: %v", errStorage)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if payload["type"] != ProviderKey {
		t.Fatalf("type = %v, want %q", payload["type"], ProviderKey)
	}

	// Merged write-back: also fill in type if missing from the original file (old credentials copied by users from elsewhere).
	merged, errMerge := MergeStorageJSON([]byte(`{"auth":{"accessToken":"old"},"uid":"u1"}`), credential)
	if errMerge != nil {
		t.Fatalf("MergeStorageJSON: %v", errMerge)
	}
	var mergedPayload map[string]any
	if errUnmarshal := json.Unmarshal(merged, &mergedPayload); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if mergedPayload["type"] != ProviderKey {
		t.Fatalf("merged type = %v, want %q", mergedPayload["type"], ProviderKey)
	}
	// User fields must still be preserved.
	if mergedPayload["uid"] != "u1" {
		t.Fatal("merge must preserve user fields")
	}
}

// TestLooksLikeCredential verifies credential ownership determination.
//
// The determination must be strict: the host feeds all non-built-in format files to each plugin in turn,
// and mistakenly swallowing another's credentials would cause the host to rewrite the other's account file using this plugin's structure (silently).
func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		provider string
		want     bool
	}{
		// Strong evidence: explicit declaration.
		{"host provider hint", map[string]any{}, "x.json", ProviderKey, true},
		{"type field", map[string]any{"type": ProviderKey}, "x.json", "", true},
		{"type field uppercase", map[string]any{"type": "FreeHub"}, "x.json", "", true},
		{"legacy workbuddy type", map[string]any{"type": "workbuddy"}, "x.json", "", true},
		{"legacy codebuddy type", map[string]any{"type": "codebuddy"}, "x.json", "", true},
		{"domain workbuddy.ai", map[string]any{"domain": "www.workbuddy.ai"}, "x.json", "", true},
		{"domain codebuddy.cn", map[string]any{"domain": "www.codebuddy.cn"}, "x.json", "", true},
		{"nested domain workbuddy.ai", map[string]any{"auth": map[string]any{"domain": "api.workbuddy.ai"}}, "x.json", "", true},

		// Filename convention. CN and GLOBAL are independent providers, and both must be recognized by this protocol
		// (the specific region is determined by the prefix matching of core.ResolveVendor).
		{"filename exact", map[string]any{"foo": "bar"}, "workbuddy.json", "", true},
		{"filename cn vendor", map[string]any{"foo": "bar"}, "workbuddycn-abc.json", "", true},
		{"filename global vendor", map[string]any{"foo": "bar"}, "workbuddyglobal-abc.json", "", true},
		{"filename underscore suffix", map[string]any{"foo": "bar"}, "workbuddy_abc.json", "", true},
		{"filename contains segment", map[string]any{"foo": "bar"}, "my-workbuddy.json", "", true},

		// Nested structure exclusive to this plugin.
		{"nested auth pair", map[string]any{
			"auth": map[string]any{"accessToken": "a", "refreshToken": "r"},
		}, "x.json", "", true},

		// **Must reject**: General fields are insufficient to determine ownership.
		{"top-level accessToken only", map[string]any{"accessToken": "a"}, "x.json", "", false},
		{"top-level both tokens", map[string]any{"accessToken": "a", "refreshToken": "r"}, "x.json", "", false},
		{"device_token only", map[string]any{"device_token": "d"}, "x.json", "", false},
		{"nested accessToken without refresh", map[string]any{
			"auth": map[string]any{"accessToken": "a"},
		}, "x.json", "", false},

		// Clear foreign provider.
		{"foreign type", map[string]any{"type": "qoder"}, "x.json", "qoder", false},
		{"foreign filename", map[string]any{"access_token": "x"}, "claude.json", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := LooksLikeCredential(testCase.raw, testCase.fileName, testCase.provider); got != testCase.want {
				t.Fatalf("LooksLikeCredential = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestParseCredentialStillAcceptsFlatShape verifies that after tightening ownership determination,
// flat shapes (manually written credentials) can still be parsed — strict determination does not mean narrow parsing.
func TestParseCredentialStillAcceptsFlatShape(t *testing.T) {
	flat := `{"accessToken":"at","refreshToken":"rt","uid":"u1","domain":"www.codebuddy.cn"}`
	credential, errParse := ParseCredential([]byte(flat), RegionCN)
	if errParse != nil {
		t.Fatalf("flat credential must still parse: %v", errParse)
	}
	if credential.UIDValue() != "u1" || credential.AccessTokenValue() != "at" {
		t.Fatalf("flat credential parsed incorrectly: %+v", credential)
	}
	// But ownership determination does not permit it just because it can be parsed (unless the filename matches the convention).
	if LooksLikeCredential(map[string]any{"accessToken": "at", "refreshToken": "rt"}, "mystery.json", "") {
		t.Fatal("a parseable flat credential from an unknown file must not be claimed")
	}
}

// TestFingerprintStability verifies the stability and isolation of device fingerprints.
func TestFingerprintStability(t *testing.T) {
	credentialA := testTaskCredential("uid-a")
	credentialB := testTaskCredential("uid-b")

	rt := newRuntime(core.Env{})
	machineA := rt.fingerprint(credentialA, "machine")
	if machineA != rt.fingerprint(credentialA, "machine") {
		t.Fatal("fingerprint must be stable")
	}
	if len(machineA) != 36 {
		t.Fatalf("fingerprint length = %d, want 36", len(machineA))
	}
	if rt.fingerprint(credentialB, "machine") == machineA {
		t.Fatal("different accounts must get different fingerprints")
	}
	if rt.fingerprint(credentialA, "session") == machineA {
		t.Fatal("different purposes must get different fingerprints")
	}
}
