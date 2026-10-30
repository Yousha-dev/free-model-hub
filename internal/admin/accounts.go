package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
	"free-model-hub/internal/state"
)

// StatusPayload constructs the account status overview (exported for root-level tests/compatibility wrappers).
func (s *Service) StatusPayload(req pluginapi.ManagementRequest) map[string]any {
	return s.buildStatusPayload(req)
}

// buildStatusPayload constructs the account status overview (excluding any tokens).
func (s *Service) buildStatusPayload(req pluginapi.ManagementRequest) map[string]any {
	cfg := s.deps.Config()
	current := s.deps.State()
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	accounts := s.listAccountSummaries(ctx, hostbridge.RequestCallbackID(req))
	modelCounts := map[string]int{}
	for _, model := range current.Models {
		modelCounts[model.Realm]++
	}
	return map[string]any{
		"ok":                true,
		"plugin":            s.deps.Identity().ID,
		"version":           s.deps.Identity().Version,
		"enabled_realms":    cfg.EnabledRealms,
		"log_level":         logger.CurrentLevel(),
		"state_dir":         cfg.StateDir,
		"accounts":          accounts,
		"account_count":     len(accounts),
		"model_counts":      modelCounts,
		"models_fetched_at": current.ModelsFetchedAt,
		"restart_pending":   current.RestartPending,
		"settings":          s.settingsView(),
		"scheduler":         s.tasks.Status(),
	}
}

// accountSummary is an account overview entry (excluding credentials).
type accountSummary struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	// FileID is the account identity (credential filename without .json): the key for aliases, and
	// also the unique identifier submitted by the page to /accounts/alias. The first two fields
	// (host's auth_id / auth_index) change upon host rescan and cannot be used as keys.
	FileID string `json:"file_id,omitempty"`
	// Label is the **display name**, chain: account alias -> upstream label -> host label -> filename -> ID.
	// Alias has the highest priority; when not set, behavior is identical to before aliases were introduced.
	Label string `json:"label"`
	// NativeLabel is the upstream name without the alias (upstream label -> host label -> filename -> ID).
	//
	// The page needs it to determine "whether the user-entered name is the original name": reverting to
	// the original name should not store an alias that can never sync with upstream. Since the alias overrides Label in the display chain, it must be provided separately.
	NativeLabel string `json:"native_label,omitempty"`
	// Alias is the user-defined alias; empty means unset. The page uses this to decide whether to skip API key
	// masking (alias is a name chosen by the user, not a credential).
	Alias       string       `json:"alias,omitempty"`
	VendorID    string       `json:"vendor_id"`
	VendorName  string       `json:"vendor_name"`
	Realm       string       `json:"realm"`
	Status      string       `json:"status"`
	Disabled    bool         `json:"disabled"`
	RefreshedAt string       `json:"refreshed_at,omitempty"`
	Quota       *quotaResult `json:"quota,omitempty"`
	// SupportsQuota is the static capability of "whether this provider can query quota", distinct from
	// Quota (query result): when there is no result, it distinguishes "not yet queried" from "never available".
	SupportsQuota bool `json:"supports_quota"`
	// Claim is the plan claim status of this account; when SupportsClaim is false, the frontend does not render the entry.
	Claim         *state.ClaimRecord `json:"claim,omitempty"`
	SupportsClaim bool               `json:"supports_claim"`
	// AuthMode is this account's credential format; "anonymous" accounts carry no secret.
	AuthMode string `json:"auth_mode,omitempty"`
}

// quotaCacheKey returns the unique key for the quota cache: provider + account identity (FileID preferred, AuthID fallback).
//
// The key must contain both the provider and account identity. Quota results were previously stored using Name / Label / AuthIndex
// multiple keys, but these fields are not unique across providers (two accounts having the same nickname or the same auth_index is common), so account A's
// quota could be read by account B — OpenCode ZEN displaying WorkBuddy's "remaining / total" is precisely
// caused by this path.
func quotaCacheKey(vendorID, fileID, authID string) string {
	vendorID = strings.TrimSpace(vendorID)
	accountID := core.FirstNonEmpty(fileID, authID)
	if vendorID == "" || accountID == "" {
		return ""
	}
	return vendorID + "\x00" + accountID
}

// getCachedQuota reads the account quota cache; incomplete identity is treated as a cache miss.
func (s *Service) getCachedQuota(vendorID, fileID, authID string) *quotaResult {
	key := quotaCacheKey(vendorID, fileID, authID)
	if key == "" {
		return nil
	}
	s.quotaMu.RLock()
	defer s.quotaMu.RUnlock()
	q, ok := s.quotaCache[key]
	if !ok {
		return nil
	}
	return &q
}

// cacheQuotaResult writes to the account quota cache; incomplete identity skips writing.
func (s *Service) cacheQuotaResult(q quotaResult) {
	key := quotaCacheKey(q.VendorID, q.UID, q.AuthID)
	if key == "" {
		return
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	s.quotaCache[key] = q
}

func (s *Service) listAccountSummaries(ctx context.Context, callbackID string) []accountSummary {
	current := s.deps.State()
	loaded, failures := s.pool.Load(ctx, callbackID, auth.Options{IncludeDisabled: true})
	for _, failure := range failures {
		logger.Debug("skip account %s: %s", core.FirstNonEmpty(failure.Entry.Name, failure.Entry.ID), failure.Message)
	}
	out := make([]accountSummary, 0, len(loaded))
	for _, account := range loaded {
		entry := account.Entry
		credential := account.Credential
		vendor := account.Vendor
		uid := credential.FileIDValue()
		alias := strings.TrimSpace(current.AccountAliases[uid])
		native := core.FirstNonEmpty(credential.LabelValue(), entry.Label, entry.Name, entry.ID)
		summary := accountSummary{
			AuthID:        core.FirstNonEmpty(entry.ID, entry.Name),
			AuthIndex:     entry.AuthIndex,
			FileID:        uid,
			Alias:         alias,
			Label:         core.FirstNonEmpty(alias, native),
			NativeLabel:   native,
			VendorID:      vendor.ID(),
			VendorName:    vendor.Name(),
			Realm:         credential.RegionValue(),
			Status:        entry.Status,
			Disabled:      entry.Disabled,
			SupportsQuota: supportsQuota(vendor),
			SupportsClaim: supportsClaim(vendor),
			AuthMode:      credential.AuthMode,
		}
		var modTime time.Time
		if !entry.ModTime.IsZero() {
			modTime = entry.ModTime
		} else if path := core.FirstNonEmpty(account.FilePath, entry.Path); path != "" {
			if info, errStat := os.Stat(path); errStat == nil {
				modTime = info.ModTime()
			}
		}
		if !modTime.IsZero() {
			summary.RefreshedAt = modTime.Format(time.RFC3339)
		}
		if record, okRecord := current.Claim[uid]; okRecord {
			claim := record
			summary.Claim = &claim
		}
		// Only read quota cache for providers that support quota queries. Accounts that do not support quota (OpenCode
		// ZEN, Cline, etc.) should not display any "remaining / total", even if fields with the same name exist in the cache.
		if supportsQuota(vendor) {
			if q := s.getCachedQuota(vendor.ID(), uid, summary.AuthID); q != nil {
				summary.Quota = q
				// A successful quota read proves the credential is usable even when
				// the host retained a stale runtime error from an earlier request.
				// Do not mutate host auth state; only avoid displaying a contradictory
				// error in this plugin's account overview.
				summary.Status = displayedAccountStatus(summary.Status, q.OK)
			}
		}
		out = append(out, summary)
	}
	return out
}

func displayedAccountStatus(status string, quotaOK bool) string {
	if strings.EqualFold(strings.TrimSpace(status), "error") && quotaOK {
		return "active"
	}
	return status
}

// supportsQuota reports whether this provider supplies quota queries.
//
// The capability comes from interface assertion, rather than "whether there is this entry in the query result" — the latter makes "not supported"
// and "not yet queried" look identical, so the page can never tell whether to display "-- / --" or "Querying...".
func supportsQuota(vendor core.Vendor) bool {
	_, okQuota := core.As[core.QuotaProvider](vendor)
	return okQuota
}

// supportsClaim reports whether this provider supplies plan claiming.
func supportsClaim(vendor core.Vendor) bool {
	_, okClaimer := core.As[core.PlanClaimer](vendor)
	return okClaimer
}

func (s *Service) buildLogsPayload(req pluginapi.ManagementRequest) map[string]any {
	since := queryInt64(req.Query, "since", 0)
	limit := int(queryInt64(req.Query, "limit", 200))
	entries, newest := logger.Snapshot(uint64(since), limit)
	return map[string]any{"ok": true, "entries": entries, "next": newest}
}

type accountBatchRequest struct {
	AccountIDs []string `json:"account_ids"`
}

// accountEnabledRequest is the request body for enabling/disabling an account.
type accountEnabledRequest struct {
	AccountIDs []string `json:"account_ids"`
	Enabled    bool     `json:"enabled"`
}

// handleAccountEnabledRequest enables/disables accounts in batch.
//
// Disabling is not deletion: credential files are retained, only writing the "disabled" field recognized by the host in JSON, and the host
// will no longer route the account after a disk scan; enabling deletes this field. Neither modifies tokens and both are fully reversible.
func (s *Service) handleAccountEnabledRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountEnabledRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	if len(body.AccountIDs) == 0 {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "account_ids cannot be empty"})
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)

	entries, errList := hostbridge.ListAuths(ctx, callbackID, s.registry)
	if errList != nil {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "error": "failed to read account list: " + errList.Error()})
	}
	wanted := wantedAccountIDs(body.AccountIDs)
	results := make([]accountActionResult, 0, len(body.AccountIDs))
	changed := 0
	for _, entry := range entries {
		if !wanted.matches(entry) || entry.Disabled == !body.Enabled {
			// Do not rewrite disk if already in the target state: host disk scan will trigger another reload.
			continue
		}
		result := accountActionResult{
			AuthID: core.FirstNonEmpty(entry.ID, entry.Name),
			Label:  core.FirstNonEmpty(entry.Label, entry.Name, entry.ID),
		}
		if errSet := s.setAuthDisabled(ctx, callbackID, entry, !body.Enabled); errSet != nil {
			result.Message = errSet.Error()
			results = append(results, result)
			continue
		}
		result.OK = true
		if body.Enabled {
			result.Message = "Enabled"
		} else {
			result.Message = "Disabled"
		}
		changed++
		results = append(results, result)
	}
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "changed": changed, "results": results})
}

// wantedAccountIDs wraps the set of account identifiers submitted by the page into a matcher.
//
// The page uses auth_id as the selection key (the only stable identifier in the account table), but the host's three identifiers
// (ID / Name / AuthIndex) may change after a re-scan, so compare them here once centrally.
func wantedAccountIDs(targets []string) accountIDSet {
	set := make(accountIDSet, len(targets))
	for _, target := range targets {
		set[strings.TrimSpace(target)] = true
	}
	return set
}

type accountIDSet map[string]bool

func (s accountIDSet) matches(entry core.HostAuth) bool {
	return s[entry.ID] || s[entry.Name] || s[entry.AuthIndex]
}

func (s *Service) handleAccountDeleteRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountBatchRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	if len(body.AccountIDs) == 0 {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "account_ids cannot be empty"})
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)

	entries, errList := hostbridge.ListAuths(ctx, callbackID, s.registry)
	if errList != nil {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "error": "failed to read account list: " + errList.Error()})
	}
	wanted := wantedAccountIDs(body.AccountIDs)
	results := make([]accountActionResult, 0, len(body.AccountIDs))
	deleted := 0
	for _, entry := range entries {
		if !wanted.matches(entry) {
			continue
		}
		result := accountActionResult{
			AuthID: core.FirstNonEmpty(entry.ID, entry.Name),
			Label:  core.FirstNonEmpty(entry.Label, entry.Name, entry.ID),
		}
		if errDelete := s.deleteAuthFile(ctx, callbackID, entry); errDelete != nil {
			result.OK = false
			result.Message = errDelete.Error()
			results = append(results, result)
			continue
		}
		result.OK = true
		result.Message = "Deleted"
		deleted++
		results = append(results, result)
	}
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "results": results})
}

func (s *Service) deleteAuthFile(ctx context.Context, callbackID string, entry core.HostAuth) error {
	raw, filePath, okRaw := hostbridge.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
	if !okRaw {
		return fmt.Errorf("failed to read credential, unable to confirm ownership")
	}
	targetPath, errSafe := safeAuthFilePath(s.registry, entry, raw, core.FirstNonEmpty(filePath, entry.Path))
	if errSafe != nil {
		return errSafe
	}
	if errRemove := os.Remove(targetPath); errRemove != nil {
		return fmt.Errorf("failed to delete file: %w", errRemove)
	}
	logger.Info("deleted auth file %s (vendor=%s)", filepath.Base(targetPath), entry.Name)
	return nil
}

// setAuthDisabled writes/clears the disabled flag on the credential file.
//
// Writing back follows the two paths for saving credentials (see authfile.go): preferentially host.auth.save, which allows the host
// to update both disk and memory registries simultaneously; otherwise, write directly to disk and let the host discover it. Validation and deletion
// share the same source—first confirm this credential belongs to this plugin and its path is a writable regular .json file.
func (s *Service) setAuthDisabled(ctx context.Context, callbackID string, entry core.HostAuth, disabled bool) error {
	raw, filePath, okRaw := hostbridge.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
	if !okRaw {
		return fmt.Errorf("failed to read credential, unable to confirm ownership")
	}
	targetPath, errSafe := safeAuthFilePath(s.registry, entry, raw, core.FirstNonEmpty(filePath, entry.Path))
	if errSafe != nil {
		return errSafe
	}
	updated, errUpdate := setCredentialDisabled(raw, disabled)
	if errUpdate != nil {
		return errUpdate
	}
	if errSave := saveAuthFile(callbackID, filepath.Base(targetPath), updated); errSave != nil {
		return errSave
	}
	logger.Info("auth file %s disabled=%t (vendor=%s)", filepath.Base(targetPath), disabled, entry.Name)
	return nil
}

type accountAddRequest struct {
	Vendor string `json:"vendor"`
	APIKey string `json:"api_key"`
	Label  string `json:"label"`
	// CredentialJSON is the provider's native credential JSON (token equivalent to complex credentials).
	// When provided, it takes precedence over api_key: passed as-is to the provider's Parse for normalization, without an api_key envelope.
	CredentialJSON string `json:"credential_json,omitempty"`
}

func (s *Service) handleAccountAddRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountAddRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	vendorID := strings.TrimSpace(body.Vendor)
	apiKey := strings.TrimSpace(body.APIKey)
	label := strings.TrimSpace(body.Label)
	if vendorID == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "vendor cannot be empty"})
	}
	credentialJSON := strings.TrimSpace(body.CredentialJSON)
	if apiKey == "" && credentialJSON == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "api_key cannot be empty"})
	}
	vendor, okVendor := s.registry.ByID(vendorID)
	if !okVendor {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("unknown vendor %q", vendorID)})
	}
	var rawJSON []byte
	seed := label
	if credentialJSON != "" {
		// Native credential JSON: append the vendor ownership field, save to disk as-is, and let the provider Parse normalize it.
		var doc map[string]any
		if err := json.Unmarshal([]byte(credentialJSON), &doc); err != nil || doc == nil {
			return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "credential_json is not a valid JSON object"})
		}
		doc[core.VendorKey] = vendor.ID()
		encoded, errMarshal := json.MarshalIndent(doc, "", "  ")
		if errMarshal != nil {
			return JSONResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": errMarshal.Error()})
		}
		rawJSON = encoded
		if seed == "" {
			seed = core.RandomHex(8)
		}
	} else {
		payload := map[string]any{
			"type":         s.deps.Identity().ProviderKey,
			core.VendorKey: vendor.ID(),
			"api_key":      apiKey,
		}
		if label != "" {
			payload["label"] = label
		}
		encoded, errMarshal := json.MarshalIndent(payload, "", "  ")
		if errMarshal != nil {
			return JSONResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": errMarshal.Error()})
		}
		rawJSON = encoded
		if seed == "" {
			seed = maskedAPIKey(apiKey)
		}
	}
	if seed == "" {
		seed = core.RandomHex(8)
	}
	fileName := core.FileNameFor(vendor.ID(), seed)
	callbackID := hostbridge.RequestCallbackID(req)
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	if errLimit := s.pool.CheckVendorLimit(ctx, callbackID, vendor, fileName); errLimit != nil {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": errLimit.Error()})
	}

	if errSave := saveAuthFile(callbackID, fileName, rawJSON); errSave != nil {
		return JSONResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": errSave.Error()})
	}
	logger.Info("added auth file %s for vendor %s", fileName, vendor.ID())
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "auth_id": fileName, "message": "Account saved successfully"})
}

// maskedAPIKey generates a masked seed for API Keys (for key-based credentials like Cline/OpenCode ZEN).
func maskedAPIKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) <= 8 {
		if trimmed == "" {
			return ""
		}
		return "***"
	}
	return trimmed[:4] + "***" + trimmed[len(trimmed)-4:]
}

// accountAliasRequest is the request body for setting or clearing an account alias.
type accountAliasRequest struct {
	FileID string `json:"file_id"`
	Alias  string `json:"alias"`
}

// handleAccountAliasRequest sets or clears the display alias for an account.
//
// Keys use the account identity (FileID) instead of nickname/email: token refreshes and re-logins do not change the credential filename,
// so the alias is automatically preserved, preventing overwrites between accounts with the same nickname.
//
// The alias is a pure display field (not involved in any ID resolution), so spaces and other characters are allowed, with only trimming at both ends;
// an empty string means clearing and restoring the upstream name. No need to rebuild the host model registry—account names only affect this page.
func (s *Service) handleAccountAliasRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountAliasRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	fileID := strings.TrimSpace(body.FileID)
	if fileID == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "file_id cannot be empty"})
	}
	alias := strings.TrimSpace(body.Alias)
	s.deps.MutateState(func(current *state.State) {
		if alias == "" {
			delete(current.AccountAliases, fileID)
			return
		}
		if current.AccountAliases == nil {
			current.AccountAliases = map[string]string{}
		}
		current.AccountAliases[fileID] = alias
	})
	logger.Info("account %s alias: %q", fileID, alias)
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "alias": alias})
}

type accountActionResult struct {
	AuthID   string `json:"auth_id"`
	Label    string `json:"label"`
	VendorID string `json:"vendor_id,omitempty"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
	// Skipped reports that the current action was "not truly completed".
	//
	// It is distinguished from OK: check-in actions may succeed in request but **receive nothing** (upstream window
	// not open, claim rejected by risk control, or issuance unconfirmed). Treating such results as success would cause
	// the console to display "Check-in completed", making users think they received rewards when they actually didn't.
	Skipped bool `json:"skipped,omitempty"`
}

// accountActionOutcome represents the result of an account action.
type accountActionOutcome struct {
	// Message is a readable description.
	Message string
	// Skipped reports that the action "succeeded in request but was not truly completed".
	Skipped bool
}

func (s *Service) runForAccounts(
	ctx context.Context,
	callbackID string,
	targets []string,
	filter func(core.Vendor, *core.Credential) bool,
	action func(context.Context, core.Vendor, *core.Credential) (accountActionOutcome, error),
) []accountActionResult {
	cfg := s.deps.Config()
	loaded, failures := s.pool.Load(ctx, callbackID, auth.Options{
		Targets:       targets,
		EnabledRealms: cfg.EnabledRealms,
		Filter: func(account auth.Account) bool {
			return filter == nil || filter(account.Vendor, account.Credential)
		},
	})
	results := make([]accountActionResult, 0, len(loaded)+len(failures))
	for _, failure := range failures {
		results = append(results, accountActionResult{
			AuthID: core.FirstNonEmpty(failure.Entry.ID, failure.Entry.Name),
			Label:  core.FirstNonEmpty(failure.Entry.Label, failure.Entry.Name, failure.Entry.ID),
			OK:     false, Message: failure.Message,
		})
	}
	jobResults := auth.RunBatch(loaded, batchConcurrency, func(current auth.Account) accountActionResult {
		result := accountActionResult{
			AuthID:   core.FirstNonEmpty(current.Entry.ID, current.Entry.Name),
			Label:    core.FirstNonEmpty(current.Credential.LabelValue(), current.Entry.Label),
			VendorID: current.Vendor.ID(),
			Realm:    current.Credential.RegionValue(),
		}
		outcome, errAction := action(ctx, current.Vendor, current.Credential)
		if errAction != nil {
			result.OK = false
			result.Message = errAction.Error()
		} else {
			result.OK = true
			result.Skipped = outcome.Skipped
			result.Message = outcome.Message
		}
		return result
	})
	return append(results, jobResults...)
}
