package trae

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"free-model-hub/cpasdk/pluginapi"
)

// TestSessionStoresAreIsolated ensures that "login sessions and callback listeners belong to the instance".
//
// This guards against pre-refactoring defects: session tables and listener flags were package-level variables,
// so a login initiated by one App could be claimed by another, and two Apps could not each maintain their own listeners.
func TestSessionStoresAreIsolated(t *testing.T) {
	first := NewSessionStore()
	second := NewSessionStore()

	first.register(&PendingSession{
		SessionID: "traecn_first", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-a", TraceID: "trace-a", State: "pending", CreatedAt: time.Now(),
	})

	if _, ok := second.get("traecn_first"); ok {
		t.Fatal("the second instance should not see the login session of the first instance")
	}
	if matched := second.matchCallback(&CallbackInfo{TraceID: "trace-a"}); matched != nil {
		t.Fatalf("callback should not be claimed by another instance: %+v", matched)
	}
	if matched := first.matchCallback(&CallbackInfo{TraceID: "trace-a"}); matched == nil {
		t.Fatal("the owning instance should be able to claim its own callback")
	}
}

// TestCallbackURLFollowsActualListener ensures that "the callback address in the login link"
// matches the actual listening port (falling back to a random port when 18080 is occupied).
func TestCallbackURLFollowsActualListener(t *testing.T) {
	store := NewSessionStore()
	if err := store.EnsureCallbackServer(); err != nil {
		t.Fatalf("ensure callback server: %v", err)
	}
	addr := store.callbackURL()
	if !strings.HasSuffix(addr, "/authorize") {
		t.Fatalf("callback url = %q, want .../authorize", addr)
	}

	started, errStart := LoginStart(context.Background(), ConfigFor(RegionCN), store)
	if errStart != nil {
		t.Fatalf("login start: %v", errStart)
	}
	if !strings.Contains(started.URL, url.QueryEscape(addr)) {
		t.Fatalf("login url does not contain the current instance callback address: %s (want encoded %s)", started.URL, url.QueryEscape(addr))
	}
}

// TestCallbackHandlerOnlyClaimsOwnSession verifies that the callback handler only recognizes the current instance session.
func TestCallbackHandlerOnlyClaimsOwnSession(t *testing.T) {
	session := &PendingSession{
		SessionID: "traecn_owned", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-owned", TraceID: "trace-owned", State: "pending", CreatedAt: time.Now(),
	}
	owner := NewSessionStore()
	owner.register(session)
	other := NewSessionStore()

	record := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/authorize?loginTraceID=trace-owned&refreshToken=rt-test", nil)
	other.handleAuthorizeCallback(record, req)
	if record.Code != http.StatusNotFound {
		t.Fatalf("response for non-owning instance = %d, want 404", record.Code)
	}
	if session.State != "pending" {
		t.Fatalf("non-owning instance modified session state: %s", session.State)
	}
}

// pastedCallback is a structural sample of a real browser callback (with sensitive values scrubbed): after Trae authorization succeeds,
// the browser stays at 127.0.0.1:18080/authorize, and the entire address contains userJwt and userInfo.
//
// Deliberately **excludes** refreshToken / userJwt.RefreshToken: including them would cause completeLogin to
// exchange for tokens upstream, requiring the unit test to go online; what we want to verify here is that "pasting can be parsed and produce credentials".
const pastedCallback = "http://127.0.0.1:18080/authorize?isRedirect=true&scope=solo" +
	"&loginTraceID=trace-paste-001&machine_id=machine-paste-001&device_id=device-paste-001" +
	"&userJwt=%7B%22ClientID%22%3A%22en1oxy7wnw8j9n%22%2C%22Token%22%3A%22jwt-access-token%22%2C%22TokenExpireAt%22%3A1793607527194%7D" +
	"&userInfo=%7B%22Region%22%3A%22CN%22%2C%22ScreenName%22%3A%22tester%22%2C%22TenantID%22%3A%22tenant-1%22%2C%22UserID%22%3A%22user-1%22%7D"

// TestSubmitCallbackCompletesPendingSession locks down the fallback path in remote deployments: when the browser cannot reach
// the plugin's 127.0.0.1 listener, the user pasting back the callback link should complete the login and generate credentials.
func TestSubmitCallbackCompletesPendingSession(t *testing.T) {
	store := NewSessionStore()
	store.register(&PendingSession{
		SessionID: "traecn_paste", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-paste-001", TraceID: "trace-paste-001", State: "pending", CreatedAt: time.Now(),
	})

	result, errSubmit := store.SubmitCallback(context.Background(), "traecn_paste", pastedCallback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success (message=%q)", result.Status, result.Message)
	}
	if result.Auth.Label != "Trae-tester" {
		t.Fatalf("label = %q, want Trae-tester", result.Auth.Label)
	}

	var storage map[string]any
	if err := json.Unmarshal(result.Auth.StorageJSON, &storage); err != nil {
		t.Fatalf("decode storage json: %v", err)
	}
	if storage["access_token"] != "jwt-access-token" {
		t.Fatalf("access_token = %v, want jwt-access-token", storage["access_token"])
	}
	if storage["uid"] != "user-1" || storage["enterprise_id"] != "tenant-1" {
		t.Fatalf("uid/enterprise_id = %v/%v, want user-1/tenant-1", storage["uid"], storage["enterprise_id"])
	}
	if result.Auth.FileName != "traecn-user-1.json" {
		t.Fatalf("file name = %q, want traecn-user-1.json", result.Auth.FileName)
	}

	// Polling should immediately get the success result for the same session.
	polled, errPoll := LoginPoll(context.Background(), "traecn_paste", store)
	if errPoll != nil {
		t.Fatalf("poll: %v", errPoll)
	}
	if polled.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("polled status = %q, want success", polled.Status)
	}
}

// TestSubmitCallbackRejectsForeignSession locks down strict claim validation for pasted data: links with mismatched traces
// must error out and **must not** fall back to the "most recent pending session" like HTTP callbacks do—otherwise a single
// paste could write credentials into another login session.
func TestSubmitCallbackRejectsForeignSession(t *testing.T) {
	store := NewSessionStore()
	session := &PendingSession{
		SessionID: "traecn_owner", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-owner", TraceID: "trace-owner", State: "pending", CreatedAt: time.Now(),
	}
	store.register(session)

	_, errSubmit := store.SubmitCallback(context.Background(), "traecn_owner", pastedCallback)
	if errSubmit == nil {
		t.Fatal("callbacks with mismatched traces must report an error")
	}
	if session.State != "pending" || session.AuthData != nil {
		t.Fatalf("session was modified by an unauthorized callback: state=%s", session.State)
	}
}

// TestSubmitCallbackRejectsExpiredSession locks down the prompt for expired sessions: users see
// "re-initiate and paste immediately" rather than a "has expired" message with no action suggestion.
func TestSubmitCallbackRejectsExpiredSession(t *testing.T) {
	store := NewSessionStore()
	store.register(&PendingSession{
		SessionID: "traecn_old", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-paste-001", TraceID: "trace-paste-001", State: "pending",
		CreatedAt: time.Now().Add(-20 * time.Minute),
	})

	_, errSubmit := store.SubmitCallback(context.Background(), "traecn_old", pastedCallback)
	if errSubmit == nil {
		t.Fatal("expired sessions must report an error")
	}
	if !strings.Contains(errSubmit.Error(), "paste") {
		t.Fatalf("expired-session error must tell the user to paste the link, got %q", errSubmit.Error())
	}
}

// TestParseCallbackAcceptsHostPrefixedURL locks down the tolerance for pasted strings: what the browser copies out
// may be in path form without a scheme, and ParseCallback should be able to supply the host itself.
func TestParseCallbackAcceptsHostPrefixedURL(t *testing.T) {
	info, errParse := ParseCallback("/authorize?loginTraceID=trace-paste-001&refreshToken=rt-only")
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if info.TraceID != "trace-paste-001" || info.RefreshToken != "rt-only" {
		t.Fatalf("parsed = %+v, want trace-paste-001 / rt-only", info)
	}
}
