package tests

// This file locks down the state semantics of the account overview "Remaining / Total" column.
//
// Real incident: OpenCode ZEN upstream has no quota endpoint, its account in the overview permanently stayed
// in "Querying...". The root cause was not a query failure, but the page treating "no result" uniformly as "querying",
// so the two states "does not support quota" and "not queried yet" were completely indistinguishable in the data.
//
// The fix splits this into two things:
//  1. The backend provides the static capability supports_quota (endpoint assertion);
//  2. The quota cache **writes both on success and failure**, allowing "unsupported" to be remembered as a result.
//
// Both assertions must pass, otherwise the page will regress back to "always querying".

import (
	"encoding/json"
	"net/http"
	"testing"
)

// See testutil_test.go for managementCall and managementCallWithBody.

// statusAccounts fetches /status once and extracts the account list.
func statusAccounts(t *testing.T) []struct {
	AuthID        string `json:"auth_id"`
	VendorID      string `json:"vendor_id"`
	SupportsQuota bool   `json:"supports_quota"`
	Quota         *struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	} `json:"quota"`
} {
	t.Helper()
	resp := managementCall(t, "GET", "/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status = %d: %s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Accounts []struct {
			AuthID        string `json:"auth_id"`
			VendorID      string `json:"vendor_id"`
			SupportsQuota bool   `json:"supports_quota"`
			Quota         *struct {
				OK      bool   `json:"ok"`
				Message string `json:"message"`
			} `json:"quota"`
		} `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode status: %v (%s)", errUnmarshal, resp.Body)
	}
	return payload.Accounts
}

// TestQuotaUnsupportedAccountNeverStaysQuerying covers the display contract for accounts that do not support quotas.
//
// OpenCode ZEN cannot implement QuotaProvider, therefore:
//   - supports_quota in /status must be false (the page displays "-- / --" based on this);
//   - the /quotas result must not contain this account, preventing it from appearing in the quota query window;
//   - /status still retains the account itself for display in the account table.
func TestQuotaUnsupportedAccountNeverStaysQuerying(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.AddAccount("1", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode","label":"zen-test"}`)

	accounts := statusAccounts(t)
	if len(accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %+v", len(accounts), accounts)
	}
	account := accounts[0]
	if account.VendorID != "opencodezen" {
		t.Fatalf("vendor = %q, want opencodezen", account.VendorID)
	}
	if account.SupportsQuota {
		t.Fatal("opencodezen has no quota API: supports_quota must be false, " +
			"otherwise the console shows「查询中…」forever")
	}
	if account.Quota != nil {
		t.Fatalf("quota must be absent before the first query: %+v", account.Quota)
	}

	// Query quota once: unsupported must also yield a "determined" result.
	resp := managementCall(t, "POST", "/quotas")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/quotas = %d: %s", resp.StatusCode, resp.Body)
	}

	accounts = statusAccounts(t)
	if len(accounts) != 1 {
		t.Fatalf("want 1 account after quota query, got %d", len(accounts))
	}
	var quotaResponse struct {
		Results []struct {
			AuthID string `json:"auth_id"`
		} `json:"results"`
	}
	if errDecode := json.Unmarshal([]byte(resp.Body), &quotaResponse); errDecode != nil {
		t.Fatalf("decode /quotas response: %v", errDecode)
	}
	if len(quotaResponse.Results) != 0 {
		t.Fatalf("unsupported account must not enter quota results: %+v", quotaResponse.Results)
	}
	accounts = statusAccounts(t)
	if len(accounts) != 1 || accounts[0].Quota != nil {
		t.Fatalf("unsupported account should remain visible without quota result: %+v", accounts)
	}
}

// TestClineAlsoReportsNoQuota confirms the fix is not an OpenCode-specific patch.
//
// Cline, like OpenCode ZEN, also has no quota endpoint. Both must report
// supports_quota=false in /status so the page immediately displays "-- / --" instead of "Querying...".
func TestClineAlsoReportsNoQuota(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.AddAccount("1", "cline-test.json", `{"type":"cline","refreshToken":"workos:rt-test"}`)

	accounts := statusAccounts(t)
	if len(accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %+v", len(accounts), accounts)
	}
	if accounts[0].VendorID != "cline" {
		t.Fatalf("vendor = %q, want cline", accounts[0].VendorID)
	}
	if accounts[0].SupportsQuota {
		t.Fatal("cline has no quota API: supports_quota must be false")
	}
	if accounts[0].Quota != nil {
		t.Fatalf("quota must be absent before any query: %+v", accounts[0].Quota)
	}
}
