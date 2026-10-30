package tests

// This file locks down the end-to-end behavior of "account alias": aliases are displayed with priority, stored by account identity,
// and retained even after upstream credentials change.
//
// Real pain point: some platforms (like WorkBuddy/Trae) return only a non-editable default nickname,
// making it impossible to tell accounts apart in the overview when using multiple accounts; moreover, nicknames are not unique across accounts and cannot be used as alias keys.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// aliasAccount retains only alias-related fields.
type aliasAccount struct {
	FileID      string `json:"file_id"`
	Label       string `json:"label"`
	NativeLabel string `json:"native_label"`
	Alias       string `json:"alias"`
}

func aliasAccountsByFileID(t *testing.T) map[string]aliasAccount {
	t.Helper()
	resp := managementCall(t, "GET", "/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status = %d: %s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Accounts []aliasAccount `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode status: %v (%s)", errUnmarshal, resp.Body)
	}
	out := make(map[string]aliasAccount, len(payload.Accounts))
	for _, account := range payload.Accounts {
		out[account.FileID] = account
	}
	return out
}

func setAccountAlias(t *testing.T, fileID, alias string) {
	t.Helper()
	resp := managementCallWithBody(t, "POST", "/accounts/alias", map[string]string{"file_id": fileID, "alias": alias})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/alias %q = %d: %s", fileID, resp.StatusCode, resp.Body)
	}
}

// traecnSharedCredential creates a Trae credential with the fixed nickname shared-account.
func traecnSharedCredential(accessToken string) string {
	return fmt.Sprintf(`{"vendor":"traecn","region":"cn","access_token":%q,"uid":"2487573687317868","label":"shared-account"}`, accessToken)
}

// TestAccountAliasWinsDisplayAndIsolatesAccounts covers the main alias workflow:
// Upstream nicknames and identically named accounts do not interfere with each other, aliases are displayed with priority, retained after token rotation, and restored when cleared.
func TestAccountAliasWinsDisplayAndIsolatesAccounts(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "traecn-shared.json", traecnSharedCredential("test-access-token"))
	host.addAccount("2", "opencodezen-shared.json", `{"api_key":"sk-test-opencode","label":"shared-account"}`)

	// No alias set: behavior is consistent with before aliases were introduced - both accounts display their respective upstream nicknames.
	baseline := aliasAccountsByFileID(t)
	for _, fileID := range []string{"traecn-shared", "opencodezen-shared"} {
		account, ok := baseline[fileID]
		if !ok {
			t.Fatalf("missing account %q in /status: %+v", fileID, baseline)
		}
		if account.Label != "shared-account" || account.Alias != "" {
			t.Fatalf("%s baseline = %+v, want upstream nickname and no alias", fileID, account)
		}
	}

	setAccountAlias(t, "traecn-shared", "工作号")
	afterSet := aliasAccountsByFileID(t)
	if got := afterSet["traecn-shared"]; got.Label != "工作号" || got.Alias != "工作号" {
		t.Fatalf("alias must win the display chain: %+v", got)
	}
	// Keyed by account identity rather than nickname: another account with the same nickname cannot be renamed along the way.
	if got := afterSet["opencodezen-shared"]; got.Label != "shared-account" || got.Alias != "" {
		t.Fatalf("same-nickname account must keep its own name: %+v", got)
	}

	// Token refresh / re-login writes back to the same credential file: the file name remains unchanged, and the alias must be retained.
	host.SetAccountCredential("1", traecnSharedCredential("rotated-access-token"))
	if got := aliasAccountsByFileID(t)["traecn-shared"]; got.Label != "工作号" || got.Alias != "工作号" {
		t.Fatalf("alias must survive token rotation: %+v", got)
	}

	// Clearing the alias restores the upstream name.
	setAccountAlias(t, "traecn-shared", "")
	if got := aliasAccountsByFileID(t)["traecn-shared"]; got.Label != "shared-account" || got.Alias != "" {
		t.Fatalf("clearing an alias must restore the upstream name: %+v", got)
	}
}

// TestAccountNativeLabelStaysUpstream locks down the semantics of native_label: it is always the upstream name,
// and does not change with the alias - the page relies on it to determine that "filling back the original name = no alias set".
func TestAccountNativeLabelStaysUpstream(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "traecn-native.json", traecnSharedCredential("test-access-token"))

	setAccountAlias(t, "traecn-native", "工作号")
	account := aliasAccountsByFileID(t)["traecn-native"]
	if account.Label != "工作号" || account.Alias != "工作号" {
		t.Fatalf("aliased account = %+v, want label 工作号", account)
	}
	if account.NativeLabel != "shared-account" {
		t.Fatalf("native_label = %q, want 上游名称 shared-account", account.NativeLabel)
	}
}
