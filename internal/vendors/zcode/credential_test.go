package zcode

import (
	"encoding/json"
	"testing"
)

func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		provider string
		want     bool
	}{
		{
			name:     "explicit vendor",
			raw:      map[string]any{"vendor": "zcode", "api_key": "abc.123"},
			fileName: "any.json",
			want:     true,
		},
		{
			name:     "filename prefix",
			raw:      map[string]any{"api_key": "abc.123"},
			fileName: "zcode-acc1.json",
			want:     true,
		},
		{
			name:     "foreign vendor",
			raw:      map[string]any{"vendor": "qodercn"},
			fileName: "zcode-test.json",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LooksLikeCredential(tc.raw, tc.fileName, tc.provider)
			if got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestMaskedKey(t *testing.T) {
	if got := MaskedKey("1234567890abcdef"); got != "1234***cdef" {
		t.Fatalf("unexpected masked key: %s", got)
	}
}

func TestNewDeviceProfileIsCoherent(t *testing.T) {
	// Profiles must be complete sets, not fake computers cobbled together from a Cartesian product of fields.
	validOS := map[string]map[string]bool{
		"darwin": {"22.6.0": true, "23.6.0": true, "24.5.0": true, "24.6.0": true, "25.5.0": true},
		"win32":  {"10.0.19045": true, "10.0.22000": true, "10.0.22621": true, "10.0.22631": true, "10.0.26100": true, "10.0.26200": true},
	}
	validLocale := map[string]bool{
		"zh-CN|Asia/Shanghai": true, "en-US|America/New_York": true, "en-US|America/Los_Angeles": true,
		"en-GB|Europe/London": true, "de-DE|Europe/Berlin": true, "ja-JP|Asia/Tokyo": true,
		"ko-KR|Asia/Seoul": true, "en-SG|Asia/Singapore": true,
	}

	for i := 0; i < 200; i++ {
		profile := NewDeviceProfile()
		if validOS[profile.Platform] == nil {
			t.Fatalf("Platform should not be %q (official desktop primary form is darwin/win32)", profile.Platform)
		}
		if !validOS[profile.Platform][profile.OSVersion] {
			t.Fatalf("os_version %q does not match platform %q", profile.OSVersion, profile.Platform)
		}
		if !validLocale[profile.Language+"|"+profile.Timezone] {
			t.Fatalf("Unrealistic language/timezone combination: %s/%s", profile.Language, profile.Timezone)
		}
		if len(profile.DeviceMID) != 36 {
			t.Fatalf("device_mid should be UUID: %q", profile.DeviceMID)
		}
		if profile.Platform == "win32" && profile.Arch != "x64" {
			t.Fatalf("win32 should not be configured with %q", profile.Arch)
		}
	}
}

func TestDeviceProfileIdentityHeaders(t *testing.T) {
	profile := DeviceProfile{
		Platform: "win32", Arch: "x64", OSVersion: "10.0.22631",
		Language: "zh-CN", Timezone: "Asia/Shanghai",
		Screen: "1920x1080", DeviceMID: "11111111-2222-4333-8444-555555555555",
	}
	headers := map[string]string{}
	BuildIdentityHeaders(profile).Apply(headers)

	checks := map[string]string{
		"X-Platform":          "win32-x64",
		"X-Os-Category":       "windows",
		"X-Os-Version":        "10.0.22631",
		"X-Client-Language":   "zh-CN",
		"X-Client-Timezone":   "Asia/Shanghai",
		"X-Device-Mid":        "11111111-2222-4333-8444-555555555555",
		"X-ZCode-App-Version": ClientAppVersion,
		"X-Title":             ClientTitle,
	}
	for key, want := range checks {
		if headers[key] != want {
			t.Fatalf("%s = %q, want %q", key, headers[key], want)
		}
	}
}

func TestTraceHeadersOmitsForbiddenKeys(t *testing.T) {
	// The Plan channel (start-plan) only allows three trace headers; including x-query-id / x-session-id
	// will trigger upstream 3012 risk control.
	headers := BuildTraceHeaders()
	for _, forbidden := range []string{"x-query-id", "x-session-id"} {
		if _, exists := headers[forbidden]; exists {
			t.Fatalf("Plan channel must not send %s (triggers upstream 3012 risk control)", forbidden)
		}
	}
	for _, required := range []string{"x-request-id", "x-zcode-session-type", "x-zcode-trace-id"} {
		if headers[required] == "" {
			t.Fatalf("Missing required tracking header %s", required)
		}
	}
	if headers["x-zcode-session-type"] != "main" {
		t.Fatalf("x-zcode-session-type = %q", headers["x-zcode-session-type"])
	}
}

func TestParseCredentialRoundTrip(t *testing.T) {
	original := &Credential{
		APIKey:   "abc12345.secret99",
		JWTToken: "header.payload.signature",
		Email:    "User@Example.com",
		UserID:   "u-1",
		AuthMode: "oauth",
		Label:    "me",
		Profile:  NewDeviceProfile(),
	}
	raw, errMarshal := original.StorageJSON()
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}

	parsed, errParse := ParseCredential(raw)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if parsed.APIKey != original.APIKey || parsed.JWTToken != original.JWTToken {
		t.Fatalf("Credential fields did not round-trip: %+v", parsed)
	}
	if parsed.Email != "user@example.com" {
		t.Fatalf("Email should be normalized to lowercase: %q", parsed.Email)
	}
	// device_mid must be persisted: if it changes, the upstream considers it a new device.
	if parsed.Profile.DeviceMID != original.Profile.DeviceMID {
		t.Fatalf("device_mid did not round-trip: %q != %q", parsed.Profile.DeviceMID, original.Profile.DeviceMID)
	}
	if parsed.Profile.Platform != original.Profile.Platform {
		t.Fatalf("Profile platform did not round-trip: %q", parsed.Profile.Platform)
	}
}

func TestParseCredentialRequiresSomeToken(t *testing.T) {
	if _, errParse := ParseCredential([]byte(`{"vendor":"zcode"}`)); errParse == nil {
		t.Fatal("Must error out when there are no credentials")
	}
}

func TestOutboundTokenPrefersAPIKey(t *testing.T) {
	// OutboundToken is used for account label masking: API Key is preferred when available, otherwise fall back to JWT.
	cred := &Credential{APIKey: "key.s", JWTToken: "a.b.c"}
	if got := cred.OutboundToken(); got != "key.s" {
		t.Fatalf("OutboundToken = %q, want the API key", got)
	}
	// Fall back to JWT if only JWT is available.
	if got := (&Credential{JWTToken: "a.b.c"}).OutboundToken(); got != "a.b.c" {
		t.Fatalf("OutboundToken = %q, want the JWT", got)
	}
}

func TestLooksLikeCredentialRejectsForeign(t *testing.T) {
	// Must be strict: mistakenly swallowing another party's credentials would cause the host to overwrite their account with this plugin's structure.
	cases := []struct {
		raw      map[string]any
		fileName string
		want     bool
	}{
		{map[string]any{"vendor": "qodercn", "api_key": "a.b"}, "zcode-x.json", false},
		{map[string]any{"vendor": "zcode"}, "any.json", true},
		{map[string]any{"api_key": "a.b"}, "zcode-x.json", true},
		// Raw credentials without any ZCode traces: do not claim.
		{map[string]any{"api_key": "sk-abcdef"}, "random.json", false},
	}
	for _, tc := range cases {
		if got := LooksLikeCredential(tc.raw, tc.fileName, ""); got != tc.want {
			t.Fatalf("LooksLikeCredential(%v, %s) = %v, want %v", tc.raw, tc.fileName, got, tc.want)
		}
	}
}

func TestUserIDFromJWT(t *testing.T) {
	// base64url of {"user_id":"u-42"} (unpadded).
	token := "header." + "eyJ1c2VyX2lkIjoidS00MiJ9" + ".signature"
	if got := UserIDFromJWT(token); got != "u-42" {
		t.Fatalf("UserIDFromJWT = %q, want u-42", got)
	}
	if got := UserIDFromJWT("not-a-jwt"); got != "" {
		t.Fatalf("Invalid JWT should return empty string, got %q", got)
	}
}

func TestMergeStorageJSONPreservesUserKeys(t *testing.T) {
	original := []byte(`{"type":"freehub","vendor":"zcode","api_key":"old","disabled":true,"note":"mine"}`)
	updated := &Credential{APIKey: "new", JWTToken: "a.b.c", AuthMode: "oauth"}
	merged, errMerge := MergeStorageJSON(original, updated)
	if errMerge != nil {
		t.Fatalf("merge: %v", errMerge)
	}

	var out map[string]any
	if errUnmarshal := json.Unmarshal(merged, &out); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if out["api_key"] != "new" || out["jwt_token"] != "a.b.c" {
		t.Fatalf("Credential not updated: %v", out)
	}
	// Fields manually maintained by the user must be preserved, otherwise a refresh would wipe out the configuration.
	if out["disabled"] != true || out["note"] != "mine" {
		t.Fatalf("User fields lost: %v", out)
	}
}

// TestAccountKeyPrioritizesIdentity verifies that the gate key prioritizes user_id, falls back to email,
// and uses the outbound credential fingerprint when both are missing to prevent unrelated accounts from sharing the same gate.
func TestAccountKeyPrioritizesIdentity(t *testing.T) {
	cases := []struct {
		cred Credential
		want string
	}{
		{Credential{UserID: "u1", Email: "a@b.c", APIKey: "k1"}, "uid:u1"},
		{Credential{Email: "a@b.c", APIKey: "k1"}, "email:a@b.c"},
	}
	for _, tc := range cases {
		if got := tc.cred.AccountKey(); got != tc.want {
			t.Fatalf("AccountKey() = %q, want %q", got, tc.want)
		}
	}
	first := (&Credential{APIKey: "k1"}).AccountKey()
	second := (&Credential{APIKey: "k2"}).AccountKey()
	if first == "" || first == second {
		t.Fatalf("token fingerprint must be non-empty and distinct: %q vs %q", first, second)
	}
	if (&Credential{}).AccountKey() != "" {
		t.Fatal("credential without any identity must yield empty key")
	}
}
