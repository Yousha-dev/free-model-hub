package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
)

// TestIsAuthExpiredOnlyMatches401 locks down that "refresh is only worth attempting once on a 401".
//
// Triggering a keep-alive on any 4xx/5xx during quota query is the easiest approach, but the cost is that if the upstream updates or the account
// lacks permission, it makes an unnecessary extra upstream call and might mask genuine errors. Recognize 401 only.
func TestIsAuthExpiredOnlyMatches401(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"401 插件错误", core.NewPluginError("upstream_credential", "invalid access token", http.StatusUnauthorized), true},
		{"401 上游分类错误", &core.Failure{Kind: core.FailureKindCredential, Status: 401, Msg: "invalid access token"}, true},
		{"403 权限问题", core.NewPluginError("upstream_credential", "forbidden", http.StatusForbidden), false},
		{"502 上游异常", core.NewPluginError("upstream_request_failed", "bad gateway", http.StatusBadGateway), false},
		{"非凭证的分类错误", &core.Failure{Kind: core.FailureKindServer, Status: 500}, false},
		{"普通错误", errors.New("boom"), false},
		{"wrapped 401", fmt.Errorf("query failed: %w", core.NewPluginError("upstream_credential", "expired", http.StatusUnauthorized)), true},
	}
	for _, c := range cases {
		if got := isAuthExpired(c.err); got != c.want {
			t.Fatalf("%s: isAuthExpired = %v, want %v", c.name, got, c.want)
		}
	}
}

// quotaVendor / plainVendor are used to verify that "capability flags" and "query results" are two separate things.
type quotaCapableVendor struct{ fakeVendor }

func (v *quotaCapableVendor) Quota(context.Context, *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	return &pluginapi.QuotaFetchResponse{}, nil
}

// TestSupportsQuotaComesFromCapability locks down the source of state determination for the quota column.
//
// The page previously used "whether the entry exists in the results" to infer the state, so "upstream has no quota API" (OpenCode
// ZEN) and "not yet queried" were indistinguishable, leaving accounts permanently stuck at "Querying...". Capabilities must come from API assertions.
func TestSupportsQuotaComesFromCapability(t *testing.T) {
	plain := &fakeVendor{Meta: core.Meta{VendorID: "plain"}}
	if supportsQuota(plain) {
		t.Fatal("vendor without QuotaProvider must report supports_quota=false")
	}
	capable := &quotaCapableVendor{fakeVendor{Meta: core.Meta{VendorID: "capable"}}}
	if !supportsQuota(capable) {
		t.Fatal("vendor implementing QuotaProvider must report supports_quota=true")
	}
}

// TestQuotaCacheKeyIsolatedByVendorAndAccount locks down the uniqueness of quota cache keys.
//
// The key must contain both the provider and account identity: previously Name / Label / AuthIndex were also used as keys,
// causing cross-provider name collisions to bleed values into each other (the incident where OpenCode ZEN displayed WorkBuddy's quota).
func TestQuotaCacheKeyIsolatedByVendorAndAccount(t *testing.T) {
	if quotaCacheKey("workbuddycn", "a", "") == quotaCacheKey("opencodezen", "a", "") {
		t.Fatal("cache key must include vendor: same account id across vendors must not collide")
	}
	if quotaCacheKey("workbuddycn", "a", "") == quotaCacheKey("workbuddycn", "b", "") {
		t.Fatal("cache key must include account identity")
	}
	if quotaCacheKey("", "a", "") != "" || quotaCacheKey("v", "", "") != "" {
		t.Fatal("incomplete identity must produce no key")
	}
	if quotaCacheKey("v", "", "auth-1") == "" {
		t.Fatal("AuthID must serve as a fallback identity when FileID is empty")
	}
}

// TestQuotaCacheIsolatedByVendor locks down the read side: identically named accounts across different providers do not share quotas.
func TestQuotaCacheIsolatedByVendor(t *testing.T) {
	s := &Service{quotaCache: map[string]quotaResult{}}
	s.cacheQuotaResult(quotaResult{VendorID: "workbuddycn", UID: "acc-1", Remain: 10, Total: 20})
	if q := s.getCachedQuota("opencodezen", "acc-1", "auth-1"); q != nil {
		t.Fatalf("must not read another vendor's quota: %+v", q)
	}
	if q := s.getCachedQuota("workbuddycn", "acc-1", "auth-1"); q == nil || q.Remain != 10 {
		t.Fatalf("own quota must be readable, got %+v", q)
	}
}
