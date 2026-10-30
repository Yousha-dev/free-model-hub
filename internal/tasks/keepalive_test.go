package tasks

// Deduplication semantics for concurrent refreshes.
//
// Scheduled keep-alive, console "Token Refresh", and quota query 401 retries all share RefreshAccount.
// All three load the account beforehand and hold the same old credential; upstream services like MiniMax rotate
// the refresh_token upon refresh, causing the later request to receive invalid_grant—mistakenly flagging
// a normal account as "requiring re-authorization". This lock ensures "the same snapshot is refreshed and written back only once".

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
)

// rotatingVendor simulates an upstream service that rotates tokens upon refresh and tracks the refresh count.
type rotatingVendor struct {
	id    string
	calls atomic.Int32
}

func (v *rotatingVendor) ID() string                                     { return v.id }
func (v *rotatingVendor) Name() string                                   { return v.id }
func (v *rotatingVendor) Region() string                                 { return "cn" }
func (v *rotatingVendor) ModelPrefix() string                            { return "" }
func (v *rotatingVendor) Match(string, string, map[string]any) bool      { return false }
func (v *rotatingVendor) Parse([]byte, string) (*core.Credential, error) { return nil, nil }
func (v *rotatingVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (v *rotatingVendor) Execute(context.Context, *core.Credential, *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *rotatingVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	return nil
}

func (v *rotatingVendor) Refresh(_ context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	// Slow down slightly to ensure concurrent calls actually block on the lock.
	time.Sleep(20 * time.Millisecond)
	round := v.calls.Add(1)
	cred.Token = fmt.Sprintf("token-%d", round)
	cred.RefreshToken = fmt.Sprintf("refresh-%d", round)
	return cred, true, nil
}

// TestRefreshAccountDeduplicatesConcurrentRefresh verifies that concurrent refreshes hit the upstream only once,
// write back only once, and subsequent refreshes with new credentials proceed normally (deduplication does not mistakenly block legitimate subsequent refreshes).
func TestRefreshAccountDeduplicatesConcurrentRefresh(t *testing.T) {
	vendor := &rotatingVendor{id: "zcodetest"}
	deps := newFakeDeps(time.Now())
	service := &Service{deps: deps}

	// Each concurrent entry point has its own credential snapshot, consistent with production (each loads from the account pool).
	newAccount := func(token string) *accountContext {
		return &accountContext{
			entry:      core.HostAuth{ID: "zcodetest-1"},
			vendor:     vendor,
			credential: &core.Credential{FileID: "zcodetest-1", Token: token, RefreshToken: "refresh-0"},
		}
	}

	const callers = 4
	var wait sync.WaitGroup
	results := make([]bool, callers)
	errors := make([]error, callers)
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errors[index] = service.RefreshAccount(context.Background(), "", newAccount("token-0"))
		}(i)
	}
	wait.Wait()

	if got := vendor.calls.Load(); got != 1 {
		t.Fatalf("upstream refresh calls = %d, want 1 (the same snapshot should only be refreshed once)", got)
	}
	if deps.refreshSaves != 1 {
		t.Fatalf("credential write-backs = %d, want 1", deps.refreshSaves)
	}
	for index := range results {
		if errors[index] != nil {
			t.Fatalf("caller %d failed: %v", index, errors[index])
		}
		if !results[index] {
			t.Fatalf("caller %d must report refreshed (credential already updated by concurrent refresh)", index)
		}
	}

	// The next refresh after getting the new token is a genuine round and must not be blocked by deduplication.
	if refreshed, err := service.RefreshAccount(context.Background(), "", newAccount("token-1")); err != nil || !refreshed {
		t.Fatalf("follow-up refresh = %v, %v; want true, nil", refreshed, err)
	}
	if got := vendor.calls.Load(); got != 2 {
		t.Fatalf("upstream refresh calls = %d, want 2", got)
	}
}
