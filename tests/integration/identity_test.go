package tests

// This file covers the consistency of account identities (FileID) across various paths.

import (
	"strings"
	"testing"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// Simulate the three identifiers (actual value forms) given by the host for the same account record.
func TestAccountIdentityIsStableAcrossPaths(t *testing.T) {
	entries := []core.HostAuth{
		// Regular accounts with a filename (Qoder / ZCode / WorkBuddy are all of this type).
		{ID: "qodercn-nick1456600333", Name: "qodercn-nick1456600333.json", AuthIndex: "019fa299-3d61-7192-b903-0145508e658c"},
		{ID: "zcode-ac90---6IY9", Name: "zcode-ac90---6IY9.json", AuthIndex: "a1b2c3d4"},
		{ID: "workbuddycn-79fdc1fc", Name: "workbuddycn-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", AuthIndex: "deadbeef"},
	}
	for _, entry := range entries {
		got := hostbridge.AccountIdentity(entry)
		// Identity takes the Name (disk filename), which is the same identifier used during credential parsing;
		// IDs may be truncated (hosts truncate long filenames) and cannot be used as identities.
		want := strings.TrimSuffix(entry.Name, ".json")
		if got != want {
			t.Fatalf("hostbridge.AccountIdentity(%+v) = %q, want %q", entry, got, want)
		}
		// Key: Identity has nothing to do with AuthIndex - early check-ins used AuthIndex and displays used Name,
		// so the same account yielded two identities. This assertion locks in that regression.
		if got == entry.AuthIndex {
			t.Fatalf("Account identity must not equal AuthIndex (causes check-in/display misalignment): %q", got)
		}
	}
}

// Runtime accounts have no Name (host built-in provider) and must fall back to ID.
func TestAccountIdentityFallsBackToID(t *testing.T) {
	if got := hostbridge.AccountIdentity(core.HostAuth{ID: "runtime-1", AuthIndex: "idx"}); got != "runtime-1" {
		t.Fatalf("got %q, want runtime-1", got)
	}
	// Returns an empty string when all three are empty, leaving the caller to decide the disposal (rather than inventing an identity out of thin air).
	if got := hostbridge.AccountIdentity(core.HostAuth{}); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	// The .json suffix must be stripped, otherwise it will be inconsistent with the filename during credential parsing.
	if got := hostbridge.AccountIdentity(core.HostAuth{Name: "cline-me@x.com.json"}); got != "cline-me@x.com" {
		t.Fatalf("got %q, want cline-me@x.com", got)
	}
}

// End-to-end: For the same account record, the check-in path and display path must resolve to the same account identity,
// and remain consistent even after the FileID fallback in parseVendorCredential.
func TestCheckinAndDisplayResolveSameIdentity(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// Qoder credential (UID taken from filename) - this is precisely the problematic form.
	const qoderCredential = `{"type":"freehub","vendor":"qodercn","region":"cn",
		"device_token":"dt-abc","refresh_token":"drt-xyz","label":"nick"}`
	entry := core.HostAuth{
		ID:        "qodercn-nick1456600333",
		Name:      "qodercn-nick1456600333.json",
		AuthIndex: "019fa299-3d61-7192-b903-0145508e658c",
	}

	identity := hostbridge.AccountIdentity(entry)
	credential, okParse := testApp.Registry().ParseCredential([]byte(qoderCredential), identity, nil)
	if !okParse {
		t.Fatal("qoder credential not parsed")
	}

	// Core assertion: The FileID parsed from the credential matches the filenames passed into both paths.
	if got := credential.FileIDValue(); got != identity {
		t.Fatalf("FileID = %q, want %q", got, identity)
	}
	// And it must no longer be AuthIndex - which was the root cause of check-in/display misalignments.
	if credential.FileIDValue() == entry.AuthIndex {
		t.Fatal("FileID must not equal AuthIndex")
	}
	if credential.FileIDValue() == "" {
		t.Fatal("FileID must not be empty (check-in records will never be found otherwise)")
	}
}

// FileID fallback: When the provider's Parse does not set FileID, the root layer must backfill it.
func TestParseVendorCredentialFillsFileID(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// WorkBuddy's UID is the upstream UUID; the FileID must still be the filename.
	const wbCredential = `{"type":"freehub","vendor":"workbuddycn",
		"auth":{"accessToken":"at","refreshToken":"rt","realm":"cn"},
		"account":{"uid":"79fdc1fc-de43-40da-b6f0-fe05d9e4367b"}}`
	credential, okParse := testApp.Registry().ParseCredential([]byte(wbCredential), "workbuddycn-79fdc1fc.json", nil)
	if !okParse {
		t.Fatal("workbuddy credential not parsed")
	}

	if got := credential.FileIDValue(); got != "workbuddycn-79fdc1fc" {
		t.Fatalf("FileID = %q, want workbuddycn-79fdc1fc", got)
	}
	// The upstream UID must be preserved as-is: device fingerprinting and the X-Uid header both rely on it; changing it would cause upstream to treat it as a new device.
	if got := credential.UIDValue(); got != "79fdc1fc-de43-40da-b6f0-fe05d9e4367b" {
		t.Fatalf("UID = %q (upstream identity must not be overwritten by FileID)", got)
	}
}

// Upstream identifiers do not participate in any identity determination.
func TestUpstreamUIDIsNotUsedForIdentity(t *testing.T) {
	credential := &core.Credential{UID: "upstream-uuid", FileID: "file-id"}
	if credential.FileIDValue() == credential.UIDValue() {
		t.Fatal("The two fields must have independent semantics")
	}
}
