package trae

import (
	"testing"
)

func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		region   Region
		want     bool
	}{
		{
			name:     "traecn explicit",
			raw:      map[string]any{"vendor": "traecn"},
			fileName: "any.json",
			region:   RegionCN,
			want:     true,
		},
		{
			name:     "traeglobal explicit",
			raw:      map[string]any{"vendor": "traeglobal"},
			fileName: "any.json",
			region:   RegionGlobal,
			want:     true,
		},
		{
			name:     "cross rejection",
			raw:      map[string]any{"vendor": "traeglobal"},
			fileName: "any.json",
			region:   RegionCN,
			want:     false,
		},
		{
			name:     "generic trae with global region",
			raw:      map[string]any{"region": "global"},
			fileName: "trae-acc.json",
			region:   RegionGlobal,
			want:     true,
		},
		{
			name:     "generic trae without region defaults to cn",
			raw:      map[string]any{},
			fileName: "trae-acc.json",
			region:   RegionCN,
			want:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LooksLikeCredential(tc.raw, tc.fileName, "", tc.region)
			if got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestLooksLikeCredentialRejectsSolo(t *testing.T) {
	// TRAE SOLO used to be an independent instance, sharing the same upstream account and login state with the CN version: same account
	// secondary login would invalidate the token of the previous instance, so this instance has been removed. In old credentials
	// vendor=traesolo must fall through to "explicitly belonging to another provider and refusing to claim", otherwise it will be treated
	// as an available account by the CN version, resulting in continuous 401 errors upstream.
	cn := ConfigFor(RegionCN)
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
	}{
		{"explicit vendor", map[string]any{"vendor": "traesolo", "access_token": "t"}, "any.json"},
		{"filename prefix", map[string]any{"vendor": "traesolo", "access_token": "t"}, "traesolo-u1.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if LooksLikeCredentialFor(tc.raw, tc.fileName, "", cn) {
				t.Fatal("SOLO credential should not be claimed by CN version")
			}
		})
	}
}

func TestParseCredentialNestedAuth(t *testing.T) {
	raw := []byte(`{
		"auth": {"accessToken": "atk_123", "refreshToken": "rtk_456", "expiresAt": 1700000000},
		"account": {"uid": "user_789", "nickname": "Alice"}
	}`)
	cred, err := ParseCredentialFor(raw, ConfigFor(RegionCN))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cred.AccessToken != "atk_123" || cred.RefreshToken != "rtk_456" || cred.UID != "user_789" {
		t.Fatalf("unexpected parsed result: %+v", cred)
	}
	if cred.Vendor != VendorIDCN || cred.Region != RegionCN {
		t.Fatalf("vendor/region = %s/%s", cred.Vendor, cred.Region)
	}
}
