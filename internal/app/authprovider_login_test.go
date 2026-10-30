package app

// Regression for SMS verification code login credential filename (#55):
// The filename must be based on the account identity — re-login for the same account uses the same name, different accounts use different names.
// Display names can duplicate (two accounts with the same nickname) and can also be sanitized into an empty string by SanitizeFileComponent,
// using it alone as the filename would cause the second account to silently overwrite the first.

import (
	"context"
	"encoding/json"
	"testing"

	"free-model-hub/internal/core"
)

// smsLoginStubVendor reuses the core.Vendor minimal implementation of writeStubVendor,
// adding SMS login and credential parsing.
type smsLoginStubVendor struct {
	*writeStubVendor
	uid   string
	token string
	label string
}

func (v *smsLoginStubVendor) ID() string { return "smsstub" }

func (v *smsLoginStubVendor) Parse(raw []byte, _ string) (*core.Credential, error) {
	var payload struct {
		UID   string `json:"uid"`
		Token string `json:"accessToken"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return &core.Credential{UID: payload.UID, Token: payload.Token}, nil
}

func (v *smsLoginStubVendor) SendLoginCode(context.Context, string) (string, error) {
	return "session-1", nil
}

func (v *smsLoginStubVendor) VerifyLoginCode(context.Context, string, string, string) ([]byte, string, error) {
	storage := map[string]any{"accessToken": v.token}
	if v.uid != "" {
		storage["uid"] = v.uid
	}
	raw, errMarshal := json.Marshal(storage)
	return raw, v.label, errMarshal
}

// TestVerifyLoginSMSNamesFileByCredentialUID covers three judgments:
// Different UID -> different filename; re-login with the same UID -> same name; missing UID -> fallback to credential fingerprint,
// no longer falling back to a fixed placeholder name.
func TestVerifyLoginSMSNamesFileByCredentialUID(t *testing.T) {
	vendor := &smsLoginStubVendor{writeStubVendor: &writeStubVendor{}, label: "同名用户"}
	registry := core.NewRegistry()
	if errRegister := registry.Register(vendor); errRegister != nil {
		t.Fatalf("register stub vendor: %v", errRegister)
	}
	a := &App{registry: registry}
	login := func(uid, token string) string {
		t.Helper()
		vendor.uid, vendor.token = uid, token
		fileName, _, _, errVerify := a.VerifyLoginSMS(context.Background(), vendor.ID(), "session-1", "13800000000", "123456")
		if errVerify != nil {
			t.Fatalf("VerifyLoginSMS(uid=%q token=%q): %v", uid, token, errVerify)
		}
		return fileName
	}

	first := login("u-1", "token-1")
	if first != "smsstub-u-1.json" {
		t.Fatalf("file name = %q, want smsstub-u-1.json (UID must be the primary key)", first)
	}
	if second := login("u-2", "token-2"); second == first {
		t.Fatalf("different accounts fell into the same file name %q (the latter login will overwrite the previous one)", first)
	}
	if again := login("u-1", "token-3"); again != first {
		t.Fatalf("re-login of the same account changed the file name: %q / %q (will result in duplicate accounts)", first, again)
	}

	// UID missing and display name sanitized to empty string: fall back to the access token fingerprint, still one name per account.
	fingerprintOne := login("", "token-4")
	fingerprintTwo := login("", "token-5")
	if fingerprintOne == fingerprintTwo {
		t.Fatalf("different credentials without UID fell into the same file name %q", fingerprintOne)
	}
	if fingerprintOne != login("", "token-4") {
		t.Fatalf("re-login of the same credential changed the file name (%q), re-login will result in duplicate accounts", fingerprintOne)
	}
	if fingerprintOne == "smsstub-account.json" {
		t.Fatalf("file name reverted to fixed placeholder name: %q", fingerprintOne)
	}
}
