package tests

// This file locks down account isolation for the quota cache.
//
// Real incident: An OpenCode ZEN Free account showed WorkBuddy's "Remaining / Total" in the overview.
// The root cause was not in the UI, but in the quota cache key: the cache was stored by Name / Label / AuthIndex,
// and these fields are not unique across providers (two accounts with the same nickname would hit), so accounts
// that do not support quota queries would also read results from other accounts.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestQuotaCacheIsolatedPerAccount reproduces and locks down "cross-account quota bleeding".
//
// Two accounts intentionally use the same display name: the old implementation would bleed Trae's quota to OpenCode ZEN.
func TestQuotaCacheIsolatedPerAccount(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "traecn-shared.json", "{\"vendor\":\"traecn\",\"region\":\"cn\",\"access_token\":\"test-access-token\",\"uid\":\"2487573687317868\",\"label\":\"shared-account\"}")
	host.addAccount("2", "opencodezen-shared.json", "{\"api_key\":\"sk-test-opencode\",\"label\":\"shared-account\"}")
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   traeQuotaFixture,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: "{\"code\":404}"}
	})

	if resp := managementCall(t, "POST", "/quotas"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/quotas = %d: %s", resp.StatusCode, resp.Body)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		for _, account := range statusAccounts(t) {
			if account.VendorID == "traecn" && account.Quota != nil && account.Quota.OK {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("trae quota should be cached, got %+v", statusAccounts(t))
	}

	for _, account := range statusAccounts(t) {
		if account.VendorID != "opencodezen" {
			continue
		}
		if account.SupportsQuota {
			t.Fatal("opencodezen must report supports_quota=false")
		}
		if account.Quota != nil {
			t.Fatalf("opencodezen must never inherit another account's quota: %+v", account.Quota)
		}
	}
}
