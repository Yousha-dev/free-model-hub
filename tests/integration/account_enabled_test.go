package tests

// This file locks down the end-to-end behavior of "batch enable/disable accounts".
//
// Disabling is not deletion: credential files and tokens are retained, only the host's disabled flag is written in JSON,
// and after the host scans the disk, the account exits routing; enabling removes the flag to restore it. Therefore, the batch endpoint must operate
// by account_ids to affect only the selected accounts.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"free-model-hub/internal/core"
)

// accountEnabledResult is the response shape of POST /accounts/enabled.
type accountEnabledResult struct {
	OK      bool `json:"ok"`
	Changed int  `json:"changed"`
	Results []struct {
		AuthID  string `json:"auth_id"`
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	} `json:"results"`
}

// hostAuthByName gets the current status of an account in the mock host.
func hostAuthByName(t *testing.T, host *fakeHost, name string) core.HostAuth {
	t.Helper()
	for _, entry := range host.SnapshotAuthFiles() {
		if entry.Name == name || entry.ID == name {
			return entry
		}
	}
	t.Fatalf("account %q not found in host", name)
	return core.HostAuth{}
}

// TestAccountEnableDisableWritesDisabledFlag covers the entire disable -> enable lifecycle:
// Disabling writes disabled=true and retains the token, while enabling deletes the flag.
func TestAccountEnableDisableWritesDisabledFlag(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	name := "traecn-toggle-1.json"
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, []byte(traeCredentialJSON), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", path)

	response := managementCallWithBody(t, "POST", "/accounts/enabled", map[string]any{
		"account_ids": []string{name},
		"enabled":     false,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/enabled = %d: %s", response.StatusCode, response.Body)
	}
	var payload accountEnabledResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v (%s)", errUnmarshal, response.Body)
	}
	if !payload.OK || payload.Changed != 1 {
		t.Fatalf("Failed to deactivate: %s", response.Body)
	}
	if !hostAuthByName(t, host, name).Disabled {
		t.Fatal("Account on host side must be marked as disabled after deactivation")
	}

	// The credential file remains in place, and the token is preserved as-is (disabling does not equal deletion).
	onDisk, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("Deactivation should not delete credential file: %v", errRead)
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(onDisk, &fields); errUnmarshal != nil {
		t.Fatalf("Credential is no longer valid JSON: %v", errUnmarshal)
	}
	if string(fields["access_token"]) != `"test-access-token"` {
		t.Fatalf("停用不应改动 token，实际 = %s", fields["access_token"])
	}

	// Disabling a second time: already in the target state, should not write to disk redundantly.
	repeat := managementCallWithBody(t, "POST", "/accounts/enabled", map[string]any{
		"account_ids": []string{name},
		"enabled":     false,
	})
	var repeatPayload accountEnabledResult
	if errUnmarshal := json.Unmarshal(repeat.Body, &repeatPayload); errUnmarshal != nil {
		t.Fatalf("decode repeat: %v", errUnmarshal)
	}
	if repeatPayload.Changed != 0 {
		t.Fatalf("Should not write to disk repeatedly when already in target state, changed = %d", repeatPayload.Changed)
	}

	enableResponse := managementCallWithBody(t, "POST", "/accounts/enabled", map[string]any{
		"account_ids": []string{name},
		"enabled":     true,
	})
	if enableResponse.StatusCode != http.StatusOK {
		t.Fatalf("Enable = %d: %s", enableResponse.StatusCode, enableResponse.Body)
	}
	if hostAuthByName(t, host, name).Disabled {
		t.Fatal("Account on host side must be restored to available after enabling")
	}
	onDisk, errRead = os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read after enable: %v", errRead)
	}
	if errUnmarshal := json.Unmarshal(onDisk, &fields); errUnmarshal != nil {
		t.Fatalf("enable payload: %v", errUnmarshal)
	}
	if _, exists := fields["disabled"]; exists {
		t.Fatalf("Enabling must delete the disabled key, actual = %s", onDisk)
	}
}

// TestAccountEnabledOnlyTouchesTargets verifies that the batch endpoint modifies only the selected accounts.
func TestAccountEnabledOnlyTouchesTargets(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	dir := t.TempDir()
	for _, name := range []string{"traecn-batch-1.json", "traecn-batch-2.json"} {
		if errWrite := os.WriteFile(filepath.Join(dir, name), []byte(traeCredentialJSON), 0o600); errWrite != nil {
			t.Fatalf("write %s: %v", name, errWrite)
		}
	}
	host.addAccount("1", "traecn-batch-1.json", traeCredentialJSON)
	host.addAccount("2", "traecn-batch-2.json", traeCredentialJSON)
	host.SetAuthFilePath("1", filepath.Join(dir, "traecn-batch-1.json"))
	host.SetAuthFilePath("2", filepath.Join(dir, "traecn-batch-2.json"))

	response := managementCallWithBody(t, "POST", "/accounts/enabled", map[string]any{
		"account_ids": []string{"traecn-batch-1.json"},
		"enabled":     false,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/enabled = %d: %s", response.StatusCode, response.Body)
	}
	if !hostAuthByName(t, host, "traecn-batch-1.json").Disabled {
		t.Fatal("Selected accounts should be deactivated")
	}
	if hostAuthByName(t, host, "traecn-batch-2.json").Disabled {
		t.Fatal("Unselected accounts should not be deactivated")
	}
}
