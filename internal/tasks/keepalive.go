package tasks

// This file is the sole orchestrator of "credential refresh + write-back".
//
// Previously, this was implemented in two places: batch keepalive in the management endpoint, and workbuddy's
// self-initiated host RPC + disk write in RunScheduled(keepalive). The latter caused the provider package to depend on
// internal/hostbridge, violating the boundary where "vendors only depend on core/hostbridge/logger",
// and caused the "merge → host.auth.save → disk write" logic of the two implementations to drift.
//
// Now, providers are responsible for only one thing: implementing core.Refresher to refresh tokens; write-back is handled
// uniformly by this file, so providers no longer need to know about the host.

import (
	"context"
	"sync"

	"free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
)

// KeepaliveResult is the refresh result for an account (part of the management endpoint JSON contract).
type KeepaliveResult struct {
	AuthID   string `json:"auth_id"`
	Label    string `json:"label"`
	VendorID string `json:"vendor_id,omitempty"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// RefreshAccount refreshes a single account's credential, writing it back to the host and disk when it changes.
//
// Write-back goes through deps.SaveRefreshedCredential (implemented by the composition root), so this package doesn't need to know
// about the existence of host RPCs. An empty callbackID indicates a background task (no request scope).
// It returns refreshed to indicate whether the credential actually changed.
//
// Refreshes for the same account are serialized via refreshGate: scheduled keepalive, console "Token Refresh", and
// quota query 401 retries might hit the same account simultaneously, while holding the same old credential
// loaded before the refresh.
func (s *Service) RefreshAccount(ctx context.Context, callbackID string, account *accountContext) (refreshed bool, err error) {
	authID := core.FirstNonEmpty(account.entry.ID, account.entry.Name, account.uid())
	before := account.credential.TokenValue()
	reuse, unlock := s.refreshGate.begin(authID, before)
	defer unlock()
	if reuse {
		// This credential has already been refreshed and written back by a concurrent refresh; refreshing again will only use the old refresh_token
		// and get invalid_grant, turning a normal account into "needs re-authorization".
		return true, nil
	}
	updated, changed, errRefresh := core.RefreshVendor(account.vendor, ctx, account.credential)
	if errRefresh != nil {
		return false, errRefresh
	}
	if !changed {
		return false, nil
	}
	s.refreshGate.finish(authID, before, updated.TokenValue())
	s.deps.SaveRefreshedCredential(ctx, callbackID, account.vendor,
		account.entry, account.rawJSON, updated, account.filePath)
	return true, nil
}

// refreshGate serializes refreshes per account and reuses the result of "the same credential was already refreshed".
//
// Simply locking is not enough: latecomers still hold the pre-refresh snapshot, and refreshing again after unlocking is equivalent to hitting
// the upstream with the old refresh_token. Therefore, we also record a fingerprint of "the access token before and after this refresh",
// and directly reuse that result if the input token matches the input of the last refresh.
//
// The number of entries scales only with the number of accounts (one per account), and does not grow with the number of calls.
type refreshGate struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	done  map[string]refreshDone
}

// refreshDone is the fingerprint of a successfully refreshed credential.
type refreshDone struct {
	before string // Access token before refresh
	after  string // Access token after refresh
}

// begin acquires the refresh right for the account; reuse being true indicates the credential held has already been
// refreshed by a concurrent refresh, and the caller should directly reuse that result instead of hitting the upstream again.
func (g *refreshGate) begin(authID, accessToken string) (reuse bool, unlock func()) {
	lock := g.lockFor(authID)
	lock.Lock()

	g.mu.Lock()
	record, okRecord := g.done[authID]
	g.mu.Unlock()
	// before == after indicates the upstream did not change the token, so the fingerprint cannot be used for deduplication.
	reuse = okRecord && accessToken != "" && record.before != record.after && accessToken == record.before
	return reuse, lock.Unlock
}

// finish records the fingerprint of a successful refresh.
func (g *refreshGate) finish(authID, before, after string) {
	g.mu.Lock()
	if g.done == nil {
		g.done = map[string]refreshDone{}
	}
	g.done[authID] = refreshDone{before: before, after: after}
	g.mu.Unlock()
}

// lockFor gets (and creates if necessary) the refresh lock for an account.
func (g *refreshGate) lockFor(authID string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.locks == nil {
		g.locks = map[string]*sync.Mutex{}
	}
	lock, okLock := g.locks[authID]
	if !okLock {
		lock = &sync.Mutex{}
		g.locks[authID] = lock
	}
	return lock
}

// Keepalive performs token refresh for all (or specified) accounts, for use by management endpoints.
//
// Failures per account do not affect other accounts: reasons are returned individually in the result for the caller to aggregate.
func (s *Service) Keepalive(ctx context.Context, callbackID string, targets []string, concurrency int) []KeepaliveResult {
	loaded, _ := s.deps.Pool().Load(ctx, callbackID, auth.Options{
		Targets:       targets,
		EnabledRealms: s.deps.Config().EnabledRealms,
	})
	jobs := make([]*accountContext, 0, len(loaded))
	for _, item := range loaded {
		jobs = append(jobs, newAccountContext(item))
	}
	return auth.RunBatch(jobs, concurrency, func(job *accountContext) KeepaliveResult {
		return s.keepaliveOne(ctx, callbackID, job)
	})
}

// keepaliveOne handles single-account token refresh and result standardization.
func (s *Service) keepaliveOne(ctx context.Context, callbackID string, account *accountContext) KeepaliveResult {
	label := account.label()
	result := KeepaliveResult{
		AuthID:   core.FirstNonEmpty(account.entry.ID, account.entry.Name),
		Label:    label,
		VendorID: account.vendor.ID(),
		Realm:    account.credential.RegionValue(),
	}
	refreshed, errRefresh := s.RefreshAccount(ctx, callbackID, account)
	if errRefresh != nil {
		result.Message = errRefresh.Error()
		logger.Error("keepalive %s: %v", label, errRefresh)
		return result
	}
	result.OK = true
	if refreshed {
		result.Message = "Token refresh successful"
	} else {
		result.Message = "Credential valid (unchanged)"
	}
	logger.Info("keepalive %s: refreshed=%t", label, refreshed)
	return result
}
