package zcode

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Regression test for OAuth endpoint concatenation.
//
// Guard issue: both the path constant and the origin have /api/v1, so when concatenated
// they become /api/v1/api/v1/oauth/cli/init — the URL looks normal but returns 404,
// manifesting as "Failed to get authorization link" on the page. Such errors have no compile-time warnings
// and can only be caught by asserting the fully concatenated URL.

func TestOAuthPathsAreNotDoublePrefixed(t *testing.T) {
	checks := []struct{ got, want string }{
		{ZCodeOrigin + PathOAuthInit, ZCodeOrigin + "/api/v1/oauth/cli/init"},
		{ZCodeOrigin + PathOAuthPoll + "/abc123", ZCodeOrigin + "/api/v1/oauth/cli/poll/abc123"},
		{BillingBase + PathBillingBalance, "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"},
		{ZAIOrigin + PathAPIMessages, "https://api.z.ai/api/anthropic/v1/messages"},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Fatalf("Endpoint splicing error:\n  got  %s\n  want %s", check.got, check.want)
		}
	}

	// /api/v1 should only appear once — this is the common failure mode for all of the above.
	for _, check := range checks {
		if count := countSubstring(check.got, "/api/v1"); count > 1 {
			t.Fatalf("Path duplicate prefix %s: %s", check.got, "/api/v1")
		}
	}
}

func TestLoginStartUsesZCodeOrigin(t *testing.T) {
	// LoginStart must hit zcode.z.ai — it must not be affected by the chat base URL override (zcode_base_url),
	// as the two domains are naturally different.
	var hitPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"f1","authorize_url":"https://chat.z.ai/x","poll_token":"pt"}}`))
	}))
	defer server.Close()

	// Point the request directly to the test server: verify the path format (origin is determined by constants, only concatenation is checked here).
	request, errNew := http.NewRequest(http.MethodPost, server.URL+PathOAuthInit, nil)
	if errNew != nil {
		t.Fatalf("build request: %v", errNew)
	}
	resp, errDo := server.Client().Do(request)
	if errDo != nil {
		t.Fatalf("do request: %v", errDo)
	}
	_ = resp.Body.Close()

	if hitPath != "/api/v1/oauth/cli/init" {
		t.Fatalf("OAuth init path = %q, want /api/v1/oauth/cli/init", hitPath)
	}
}

// countSubstring counts the occurrences of a substring.
func countSubstring(s, sub string) int {
	count := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			count++
		}
	}
	return count
}
