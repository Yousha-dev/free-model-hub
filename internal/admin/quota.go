package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	accountpkg "free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/state"
)

// quotaJob is an account task within quota queries.
type quotaJob struct {
	entry      core.HostAuth
	vendor     core.Vendor
	credential *core.Credential
}

// identity returns the account identity field of the task (shared between result and cache).
func (j quotaJob) identity() quotaResult {
	return quotaResult{
		AuthID:     core.FirstNonEmpty(j.entry.ID, j.entry.Name),
		AuthIndex:  j.entry.AuthIndex,
		Name:       j.entry.Name,
		UID:        j.credential.FileIDValue(),
		Label:      core.FirstNonEmpty(j.credential.LabelValue(), j.entry.Label, j.entry.Name),
		VendorID:   j.vendor.ID(),
		VendorName: j.vendor.Name(),
		Realm:      j.credential.RegionValue(),
	}
}

// quotaResult is the quota result for a single account.
type quotaResult struct {
	AuthID     string `json:"auth_id"`
	AuthIndex  string `json:"auth_index,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
	Label      string `json:"label"`
	VendorID   string `json:"vendor_id,omitempty"`
	VendorName string `json:"vendor_name,omitempty"`
	Realm      string `json:"realm"`
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Remain     int64  `json:"remain"`
	Total      int64  `json:"total"`
	// Unlimited reports that remaining quota is unlimited, and the presentation layer displays "Unlimited" accordingly.
	Unlimited bool                    `json:"unlimited,omitempty"`
	Metrics   []pluginapi.QuotaMetric `json:"metrics,omitempty"`
}

// quotaBatchOutcome is the outcome for a single account in a batch query round.
//
// Claim status and quota results are carried separately: quota results are cached and read multiple times,
// while claim status is persisted once in a batch at the end. Their lifecycles differ.
type quotaBatchOutcome struct {
	result quotaResult
	claim  *pluginapi.ClaimStatus
}

func (s *Service) handleQuotasRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountBatchRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)

	cfg := s.deps.Config()
	loaded, failures := s.pool.Load(ctx, callbackID, accountpkg.Options{
		Targets:       body.AccountIDs,
		EnabledRealms: cfg.EnabledRealms,
	})
	results := make([]quotaResult, 0, len(failures)+len(loaded))
	for _, failure := range failures {
		results = append(results, quotaResult{
			AuthID:  core.FirstNonEmpty(failure.Entry.ID, failure.Entry.Name),
			Label:   core.FirstNonEmpty(failure.Entry.Label, failure.Entry.Name, failure.Entry.ID),
			Message: failure.Message,
		})
	}
	if len(loaded) == 0 {
		return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
	}

	// The query runs in a **background scope** (empty callback ID + independent timeout), decoupled from this management RPC:
	// Page refreshes/closures cause the host to cancel this RPC. If the query is tied to its context, this round
	// would fail to write a single account to the cache, leaving the account table stuck showing "Querying...".
	// Do not hit upstream repeatedly if a round is already running: wait for it to finish, then read back the cache.
	jobs := make([]quotaJob, 0, len(loaded))
	for _, account := range loaded {
		job := quotaJob{entry: account.Entry, vendor: account.Vendor, credential: account.Credential}
		// Providers that do not support quota queries (such as OpenCode ZEN) skip query rounds, do not write to cache,
		// and are excluded from /quotas results — based on supports_quota=false from /status, the page directly displays
		// "-- / --", avoiding a result destined to fail.
		if !supportsQuota(account.Vendor) {
			continue
		}
		jobs = append(jobs, job)
	}
	if len(jobs) == 0 {
		// All items failed or account does not support quota: no round to run, return results directly.
		return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
	}
	done, owner := s.acquireQuotaRun()
	if owner {
		go s.runQuotaBatch(done, jobs)
	}
	select {
	case <-done:
	case <-ctx.Done():
		// Page has navigated away: do not block the background round, return current cache directly (which may be partially completed).
	}

	for _, job := range jobs {
		identity := job.identity()
		if cached := s.getCachedQuota(identity.VendorID, identity.UID, identity.AuthID); cached != nil {
			results = append(results, *cached)
			continue
		}
		identity.Message = "Quota query not completed yet, please refresh later"
		results = append(results, identity)
	}
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
}

// acquireQuotaRun acquires execution rights for the "quota query round".
//
// Returns the completion signal for this round and whether it is the initiator. If a round is already running, returns its completion signal,
// allowing the caller to wait for the same round to finish, preventing duplicate upstream hits for the same batch of accounts during page refresh.
func (s *Service) acquireQuotaRun() (chan struct{}, bool) {
	s.quotaRunMu.Lock()
	defer s.quotaRunMu.Unlock()
	if s.quotaRun != nil {
		return s.quotaRun, false
	}
	s.quotaRun = make(chan struct{})
	return s.quotaRun, true
}

// finishQuotaRun concludes the current round: first closes the completion signal, then releases execution rights.
func (s *Service) finishQuotaRun(done chan struct{}) {
	s.quotaRunMu.Lock()
	if s.quotaRun == done {
		s.quotaRun = nil
	}
	s.quotaRunMu.Unlock()
	close(done)
}

// runQuotaBatch queries quotas for all accounts in a background scope, writing to the cache one by one.
//
// The cache is updated as each account completes (both successes and failures), allowing the page to
// see completed accounts even if refreshed midway through a round, instead of reverting the entire batch to "Querying".
func (s *Service) runQuotaBatch(done chan struct{}, jobs []quotaJob) {
	defer s.finishQuotaRun(done)

	ctx, cancel := context.WithTimeout(hostbridge.WithCallbackID(context.Background(), ""), requestTimeout)
	defer cancel()

	outcomes := accountpkg.RunBatch(jobs, batchConcurrency, func(current quotaJob) (outcome quotaBatchOutcome) {
		outcome.result = current.identity()
		// **Cache both successes and failures**: The cache is the single source of truth for the account table. Caching only successes would
		// cause failed accounts to revert to "No result" on the next /status, leaving the page permanently stuck at "Querying..." —
		// whereas conclusions like "upstream has no quota API" are already certain and will not change.
		defer func() {
			result := outcome.result
			s.cacheQuotaResult(result)
		}()
		quota, errQuota := s.queryQuota(ctx, current)
		if errQuota != nil {
			outcome.result.Message = errQuota.Error()
			return outcome
		}
		result := &outcome.result
		if quota.Claim != nil {
			outcome.claim = quota.Claim
		}
		result.OK = true
		result.Summary = accountpkg.Summary(quota)
		totals := accountpkg.Totals(quota)
		result.Remain, result.Total, result.Unlimited = totals.Remaining, totals.Total, totals.Unlimited
		result.Metrics = quota.Summary
		return outcome
	})

	// Claim status is refreshed alongside quotas, but **persisted only at the end of the batch**: Mutate writes
	// state.json every time, and writing per-account amplifies a single "quota query" into N disk writes.
	claims := map[string]state.ClaimRecord{}
	for _, outcome := range outcomes {
		if outcome.claim != nil {
			claims[outcome.result.UID] = state.ClaimRecord{
				ClaimStatus: *outcome.claim,
				CheckedAt:   time.Now().Format(time.RFC3339),
			}
		}
	}
	if len(claims) > 0 {
		s.deps.MutateState(func(current *state.State) {
			for uid, record := range claims {
				current.Claim[uid] = record
			}
		})
	}
}

// queryQuota queries the quota for a single account (including a keep-alive retry for expired credentials).
//
// Responsible solely for fetching data; caching and persisting claim status are handled uniformly by runQuotaBatch.
func (s *Service) queryQuota(ctx context.Context, job quotaJob) (*pluginapi.QuotaFetchResponse, error) {
	quotaProvider, okQuota := core.As[core.QuotaProvider](job.vendor)
	if !okQuota {
		return nil, core.QuotaUnsupported(job.vendor)
	}
	quota, errQuota := quotaProvider.Quota(ctx, job.credential)
	if errQuota != nil && isAuthExpired(errQuota) {
		// Credential expiration error: refresh to keep-alive once before retrying. MiniMax access tokens have
		// only a 1-hour validity period. Without refreshing, the quota column will show 401 continuously, and users would have to manually think to click
		// "One-click Keep-Alive" — a path that also requires them to recognize this as a credential issue first.
		if refreshed := s.refreshedCredential(ctx, "", job); refreshed != nil {
			quota, errQuota = quotaProvider.Quota(ctx, refreshed)
		}
	}
	return quota, errQuota
}

// isAuthExpired determines whether an error belongs to the "credential expired/invalid" category (401).
//
// Recognizes 401 only: 403/404 often imply lack of account permissions or upstream revisions, where refreshing will not help,
// and blind refreshing only results in redundant upstream hits.
func isAuthExpired(err error) bool {
	var failure *core.Failure
	if errors.As(err, &failure) {
		return failure.Kind == core.FailureKindCredential || failure.HTTPStatus() == http.StatusUnauthorized
	}
	var pluginErr *core.PluginError
	if errors.As(err, &pluginErr) {
		return pluginErr.HTTPStatus == http.StatusUnauthorized
	}
	return false
}

// refreshedCredential runs a keep-alive check for a single account and returns the refreshed credential.
//
// Reuses the existing keep-alive channel (which handles refreshing + disk writing + hot-updating the host), so quota queries do not
// need to reinvent a separate refresh logic. Returns nil if keep-alive fails or the account disappears, letting the caller report the original error.
func (s *Service) refreshedCredential(ctx context.Context, callbackID string, target quotaJob) *core.Credential {
	authID := core.FirstNonEmpty(target.entry.ID, target.entry.Name)
	if authID == "" {
		return nil
	}
	s.tasks.Keepalive(ctx, callbackID, []string{authID}, 1)

	loaded, _ := s.pool.Load(ctx, callbackID, accountpkg.Options{
		Targets:       []string{authID},
		EnabledRealms: s.deps.Config().EnabledRealms,
	})
	for _, account := range loaded {
		if account.Vendor.ID() == target.vendor.ID() {
			return account.Credential
		}
	}
	return nil
}
