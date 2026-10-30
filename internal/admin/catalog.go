// This file is the model catalog: static list cache, aliases, disabled sets, name-collision merging, and refresh.
//
// Placed in the admin package because it serves only the admin page and model registration chain: all catalog writes
// come from management APIs or registration replies, and reads/writes go through host RPC and state.json with no secondary
// callers. Creating a separate package for this would only cause admin and it to mutually import each other.
package admin

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
	"free-model-hub/internal/state"
)

// The model catalog manages the registration view of models: disabled sets, aliases, extra models,
// static model cache, and name-collision merging.
//
// These rules are independent of the transport layer; the management API only handles request decoding and response wrapping, keeping all rules here.

// ModelSettings is a snapshot of global configurations concerning the model layer.
type ModelSettings struct {
	EnabledRealms  []string
	ExtraModels    []string
	DisabledModels []string
	// ModelNameTag is the model name channel tag mode (off/prefix/suffix).
	ModelNameTag string
	// ContextWindowFor returns the context window fallback for extra models (default 0 means unspecified).
	ContextWindowFor func(modelID string) int64
}

// Catalog holds model-related process-level caches.
type Catalog struct {
	store    *state.Store
	registry *core.Registry

	mu       sync.RWMutex
	settings ModelSettings
	disabled map[string]bool
	aliases  map[string]string
	// cached is the list of **official IDs** most recently obtained by each provider (for console readback).
	cached map[string]cachedVendor

	// probing records in-progress probes by provider (singleflight).
	// probeMu protects it; probeAt is the last probe time, used to avoid hitting upstream repeatedly within TTL.
	probeMu sync.Mutex
	probing map[string]chan struct{}
	probeAt map[string]time.Time
}

// cachedVendor is a console readback cache for a provider.
//
// Credits and model lists belong to the same probe result and are placed in the same entry rather than
// a separate table: they must advance and retreat together. Storing them separately would sooner or later lead to mismatches
// where "models are updated but credits belong to the previous batch".
type cachedVendor struct {
	models []pluginapi.ModelInfo
	// credits are official model ID -> quota/fee multiplier text (e.g., "x0.05", "free").
	// Host contract pluginapi.ModelInfo lacks this field and can only be provided separately
	// by the provider's ConsoleModels capability.
	credits map[string]string
}

// New creates a Catalog.
func NewCatalog(store *state.Store, registry *core.Registry) *Catalog {
	return &Catalog{
		store:    store,
		registry: registry,
		disabled: map[string]bool{},
		aliases:  map[string]string{},
		cached:   map[string]cachedVendor{},
		probing:  map[string]chan struct{}{},
		probeAt:  map[string]time.Time{},
	}
}

// Configure updates global settings and recalculates the disabled set (configuration declarations ∪ state records).
func (c *Catalog) Configure(settings ModelSettings) {
	snap := settings
	snap.ExtraModels = append([]string(nil), settings.ExtraModels...)
	snap.DisabledModels = append([]string(nil), settings.DisabledModels...)
	snap.EnabledRealms = append([]string(nil), settings.EnabledRealms...)
	c.mu.Lock()
	c.settings = snap
	c.mu.Unlock()
	c.reloadDisabled()
}

// ReloadFromState recalculates the disabled set and alias table using a state snapshot.
func (c *Catalog) ReloadFromState(snapshot state.State) {
	c.mu.Lock()
	c.reloadAliasesLocked(snapshot.ModelAliases)
	c.mu.Unlock()
	c.reloadDisabledWith(snapshot.DisabledModels)
}

// RefreshFromState directly reads the storage snapshot and recalculates.
func (c *Catalog) RefreshFromState() {
	if c.store == nil {
		return
	}
	c.ReloadFromState(c.store.Snapshot())
}

func (c *Catalog) reloadDisabled() {
	if c.store == nil {
		c.reloadDisabledWith(nil)
		return
	}
	c.reloadDisabledWith(c.store.Snapshot().DisabledModels)
}

func (c *Catalog) reloadDisabledWith(stateDisabled []string) {
	c.mu.Lock()
	merged := make([]string, 0, len(c.settings.DisabledModels)+len(stateDisabled))
	merged = append(merged, c.settings.DisabledModels...)
	merged = append(merged, stateDisabled...)
	set := make(map[string]bool, len(merged)*2)
	for _, item := range merged {
		for _, key := range c.ExpandKeys(item) {
			if key != "" {
				set[key] = true
			}
		}
	}
	c.disabled = set
	c.mu.Unlock()
}

// RealmEnabled reports whether a realm is enabled (empty realm is considered enabled).
func (c *Catalog) RealmEnabled(region string) bool {
	c.mu.RLock()
	enabled := append([]string(nil), c.settings.EnabledRealms...)
	c.mu.RUnlock()
	return core.RealmEnabled(enabled, region)
}

// ExpandKeys expands model identifiers into query keys.
//
//   - Bare name: returned as-is (applies to all providers);
//   - Provider ID prefix: normalizes vendorKey;
//   - Realm alias: expands into vendorKey for each provider in that realm.
func (c *Catalog) ExpandKeys(modelID string) []string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	prefix, bare := c.registry.SplitModelID(trimmed)
	if prefix == "" {
		return []string{bare}
	}
	if _, okVendor := c.registry.ByID(prefix); okVendor {
		return []string{c.registry.ModelKeyFor(prefix, bare)}
	}
	keys := make([]string, 0, 4)
	for _, vendor := range c.registry.Vendors() {
		if vendor.Region() == prefix {
			keys = append(keys, c.registry.ModelKeyFor(vendor.ID(), bare))
		}
	}
	if len(keys) > 0 {
		return keys
	}
	return []string{bare}
}

// ExpandDisabledList expands disabled items in the configuration and stably sorts them.
func (c *Catalog) ExpandDisabledList(items []string) []string {
	set := make(map[string]bool, len(items)*2)
	for _, item := range items {
		for _, key := range c.ExpandKeys(item) {
			if key != "" {
				set[key] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// IsDisabled determines whether a model under a provider is disabled.
//
// **Only applies to models currently existing in the list**: disabled keys are derived from an official ID (`vendor:id`).
// Once a provider changes its external ID, the old key points to a non-existent model. Without this step, the old key would
// mistakenly judge a new model with the same name as disabled — a real incident: after Qoder changed from upstream key to normalized slug,
// the legacy `qodercn:dfmodel` caused the new `deepseek-flash` to report "model is disabled".
//
// Deliberately **does not delete** old keys in the state: lists may temporarily fail to be fetched due to probe failures, and deletion would cause
// user configurations to be permanently lost. Keeping them only disables the judgment, and they automatically take effect again once the list recovers.
func (c *Catalog) IsDisabled(vendorID, modelID string) bool {
	bare := strings.TrimSpace(modelID)
	if bare == "" {
		return false
	}
	if prefix, rest := c.registry.SplitModelID(bare); prefix != "" {
		if vendor, okVendor := c.vendorForPrefix(prefix); okVendor && vendor.ID() != vendorID {
			return false
		}
		bare = rest
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.disabled[bare] && !c.disabled[c.registry.ModelKeyFor(vendorID, bare)] {
		return false
	}
	return c.registeredLocked(vendorID, bare)
}

// registeredLocked reports whether this official ID still exists in the provider's current list.
//
// An empty list means it has not been fetched yet (cold start or probe failure), making it impossible to determine if "the model has disappeared",
// Treat as "existing" — a network glitch cannot change authentication semantics. Caller must hold read lock.
func (c *Catalog) registeredLocked(vendorID, modelID string) bool {
	models := c.cached[vendorID].models
	if len(models) == 0 {
		return true
	}
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model.ID), modelID) {
			return true
		}
	}
	return false
}

// vendorForPrefix parses a restricted prefix (provider ID or region alias) into a provider instance.
func (c *Catalog) vendorForPrefix(prefix string) (core.Vendor, bool) {
	if vendor, okVendor := c.registry.ByID(prefix); okVendor {
		return vendor, true
	}
	for _, vendor := range c.registry.Vendors() {
		if vendor.Region() == prefix {
			return vendor, true
		}
	}
	return nil, false
}

// FilterForVendor removes disabled models under a provider.
func (c *Catalog) FilterForVendor(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if c.IsDisabled(vendorID, model.ID) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// SetDisabled batch sets the disabled status, persists to disk, and returns the normalized list.
func (c *Catalog) SetDisabled(targets []string, disabled bool) []string {
	normalized := make([]string, 0, len(targets))
	for _, target := range targets {
		normalized = append(normalized, c.ExpandKeys(target)...)
	}
	if c.store == nil {
		return normalized
	}
	var next []string
	c.store.Mutate(func(s *state.State) {
		current := make(map[string]bool, len(s.DisabledModels))
		for _, id := range s.DisabledModels {
			current[strings.TrimSpace(id)] = true
		}
		for _, key := range normalized {
			if key == "" {
				continue
			}
			if disabled {
				current[key] = true
			} else {
				delete(current, key)
			}
		}
		next = make([]string, 0, len(current))
		for id := range current {
			next = append(next, id)
		}
		sort.Strings(next)
		s.DisabledModels = next
		s.RestartPending = true
	})
	c.reloadDisabledWith(next)
	return next
}

// RestartPending reports whether there are model changes pending host reload.
func (c *Catalog) RestartPending() bool {
	if c.store == nil {
		return false
	}
	return c.store.Snapshot().RestartPending
}

// ClearRestartPending clears the pending activation flag after the host rebuilds the model registry.
func (c *Catalog) ClearRestartPending() {
	if c.store == nil {
		return
	}
	if !c.store.Snapshot().RestartPending {
		return
	}
	c.store.Mutate(func(s *state.State) { s.RestartPending = false })
}

// CacheStatic records a provider's official model list (for console readback).
//
// Allow writing empty list: caller uses empty value to express "explicit clear", and `registeredLocked` also relies
// on the conservative semantic of "empty list = unknown". **Do not use this for probe responses**; use CacheProbedModels instead.
//
// Multiplier is a probe product and is cleared along with the list: when a new list has not yet been assigned a multiplier, keeping the previous batch's
// multiplier will cause a mismatch.
func (c *Catalog) CacheStatic(vendorID string, models []pluginapi.ModelInfo) {
	c.mu.Lock()
	c.cached[vendorID] = cachedVendor{models: append([]pluginapi.ModelInfo(nil), models...)}
	c.mu.Unlock()
}

// CacheProbedModels records an official list (including multipliers) obtained from a probe and persists to disk.
//
// Probe failures (upstream glitches, temporarily unavailable credentials, empty after filtering due to free-tier renaming) are indistinguishable
// from "models actually going entirely offline" in response payloads, while the former is far more common: a single transient 401 making an entire provider
// disappear from the console is an incident that has actually occurred. Therefore, empty results only log and do not modify the cache.
//
// After persisting, plugin reloads (host restart/reinstall) can be backfilled by LoadCachedState, so the console doesn't have to wait
// for another round of upstream probing. Batch probing (StaticModels / RefreshFromUpstream) switches to cacheProbed,
// and persists once uniformly at the end of the entire round.
//
// Returns whether the cache was written.
func (c *Catalog) CacheProbedModels(vendorID string, models []pluginapi.ModelInfo) bool {
	if !c.cacheProbed(vendorID, models) {
		return false
	}
	c.persistProbed()
	return true
}

// cacheProbed only updates the memory cache, and the caller decides when to persist.
//
// Multiplier and list are written together here: both are products of the same probe, and sharing an entry prevents
// the mismatch of "the list is new, but the multiplier is still from the previous batch".
func (c *Catalog) cacheProbed(vendorID string, models []pluginapi.ModelInfo) bool {
	if len(models) == 0 {
		logger.Info("models: %s probe returned 0 model(s); keeping %d cached",
			vendorID, len(c.Registered(vendorID)))
		return false
	}
	// Get multiplier before entering critical section: ModelCredits is implemented by the provider, calling it while holding the Catalog lock
	// is equivalent to nesting two locks, which is an avoidable deadlock risk.
	credits := c.creditsFromProvider(vendorID)
	c.mu.Lock()
	c.cached[vendorID] = cachedVendor{
		models:  append([]pluginapi.ModelInfo(nil), models...),
		credits: credits,
	}
	c.mu.Unlock()
	return true
}

// creditsFromProvider reads the multiplier table reported by the provider (official model ID → text).
//
// Only providers implementing CreditsProvider have multipliers; the host contract does not have this field,
// so it cannot be inferred from models. Empty values are not written, avoiding treating "not provided" as "free".
// Multiplier normalization (FormatRate) is done by each provider at their own layer, only whitespace trimming is done here,
// without secondary formatting — otherwise legitimate values like "0" would be repeatedly rewritten.
// Callers must call outside of the Catalog write lock.
func (c *Catalog) creditsFromProvider(vendorID string) map[string]string {
	vendor, okVendor := c.registry.ByID(vendorID)
	if !okVendor {
		return nil
	}
	provider, okProvider := core.As[core.CreditsProvider](vendor)
	if !okProvider {
		return nil
	}
	credits := make(map[string]string)
	for id, value := range provider.ModelCredits() {
		id = strings.TrimSpace(id)
		value = strings.TrimSpace(value)
		if id != "" && value != "" {
			credits[id] = value
		}
	}
	if len(credits) == 0 {
		return nil
	}
	return credits
}

// persistProbed writes the current list to state.json (does not write if content hasn't changed).
//
// Host loading N credentials will call model.for_auth N times, and blind persistence would magnify a single startup into dozens of
// state.json writes. Comparison is done outside the lock after reading the snapshot, and Mutate is entered only when there is an actual change.
func (c *Catalog) persistProbed() {
	if c.store == nil {
		return
	}
	flat := c.CachedState()
	if len(flat) == 0 || equalCachedModels(c.store.Snapshot().Models, flat) {
		return
	}
	now := time.Now().Format(time.RFC3339)
	c.store.Mutate(func(s *state.State) {
		s.Models = flat
		s.ModelsFetchedAt = now
	})
}

// equalCachedModels compares whether two lists are equivalent (ignoring order).
func equalCachedModels(a, b []core.CachedModel) bool {
	if len(a) != len(b) {
		return false
	}
	type key struct{ vendor, id string }
	index := make(map[key]core.CachedModel, len(a))
	for _, item := range a {
		index[key{item.VendorID, item.ID}] = item
	}
	for _, item := range b {
		prev, ok := index[key{item.VendorID, item.ID}]
		if !ok || prev.Name != item.Name || prev.ContextLength != item.ContextLength ||
			prev.MaxOutputTokens != item.MaxOutputTokens || prev.Credits != item.Credits {
			return false
		}
	}
	return true
}

// Registered returns a provider's most recent official model list.
func (c *Catalog) Registered(vendorID string) []pluginapi.ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]pluginapi.ModelInfo(nil), c.cached[vendorID].models...)
}

const (
	// modelProbeTTL is the cooldown period after a failed supplementary probe: upstream is not hit repeatedly during this time,
	// preventing page refreshes from amplifying a single failure into upstream pressure. Success does not set a cooldown.
	modelProbeTTL = 30 * time.Second
	// modelProbeWait is the upper limit for list requests waiting for a supplementary probe: timeout renders existing cache first,
	// and the probe continues to run in the background, visible on the next refresh.
	modelProbeWait = 3 * time.Second
	// modelProbeTimeout is the upstream call limit for the supplementary probe itself.
	modelProbeTimeout = 30 * time.Second
)

// EnsureModels triggers a probe fill for a provider when the cache is empty (does not wait for completion).
//
// Why this is needed: the console list reads exclusively from this memory cache, which is only written to by probe paths.
// Host credential loading uses model.for_auth. If the static probe hasn't finished after a plugin restart, the page
// would lack models for the entire provider—manifesting as "sometimes present, sometimes missing". Here we actively refill
// once when the cache is empty, using singleflight + cooldown to ensure at most one upstream request per page load.
//
// Start without waiting: the caller unifies WaitProbes after collecting all providers, preventing individual
// serial waits from slowing down the page by N times the upstream latency.
func (c *Catalog) EnsureModels(vendor core.Vendor) {
	vendorID := vendor.ID()
	if len(c.Registered(vendorID)) > 0 {
		return
	}

	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	if c.probing[vendorID] != nil {
		return
	}
	if last, ok := c.probeAt[vendorID]; ok && time.Since(last) < modelProbeTTL {
		return
	}
	pending := make(chan struct{})
	c.probing[vendorID] = pending
	go c.probeVendor(vendor, pending)
}

// WaitProbes waits for all ongoing probe fills, up to the given timeout.
//
// After a timeout, probes still finish in the background and write to the cache, visible on the next refresh.
func (c *Catalog) WaitProbes(timeout time.Duration) {
	c.probeMu.Lock()
	pending := make([]chan struct{}, 0, len(c.probing))
	for _, done := range c.probing {
		pending = append(pending, done)
	}
	c.probeMu.Unlock()
	if len(pending) == 0 {
		return
	}
	deadline := time.After(timeout)
	for _, done := range pending {
		select {
		case <-done:
		case <-deadline:
			return
		}
	}
}

// ProbesRunning returns the count of currently ongoing probe fills.
func (c *Catalog) ProbesRunning() int {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	return len(c.probing)
}

// probeVendor fetches the provider list once in the background and writes it to the console cache.
//
// Use background context instead of management request context: page refresh cancels management RPC,
// and probes should not be aborted halfway because of this (see quota query for the same class of issue).
//
// Cooldown applies only to failures: another probe fill should proceed as normal after success, otherwise a single success
// would block all probe fills for the next 30 seconds.
func (c *Catalog) probeVendor(vendor core.Vendor, done chan struct{}) {
	vendorID := vendor.ID()
	ctx, cancel := context.WithTimeout(context.Background(), modelProbeTimeout)
	models, errModels := vendor.StaticModels(ctx)
	cancel()

	wrote := false
	if errModels != nil {
		logger.Info("models: %s console probe failed: %v", vendorID, errModels)
	} else {
		wrote = c.CacheProbedModels(vendorID, models)
		if wrote {
			logger.Info("models: %s console probe cached %d model(s)", vendorID, len(models))
		}
	}

	c.probeMu.Lock()
	if c.probing[vendorID] == done {
		delete(c.probing, vendorID)
	}
	if wrote {
		delete(c.probeAt, vendorID)
	} else {
		c.probeAt[vendorID] = time.Now()
	}
	c.probeMu.Unlock()
	close(done)
}

// LoadCachedState restores the model cache for all providers from state.json and notifies providers to warm up.
//
// Entries for decommissioned providers are directly discarded: the cache is persisted by vendor_id, and once a provider is removed,
// these entries will never be read again. Keeping them would just write them back to state.json round after round with CachedState,
// causing a batch of non-existent models to persistently linger in the console and configuration.
func (c *Catalog) LoadCachedState(cached []core.CachedModel) {
	entries := map[string]cachedVendor{}
	for _, item := range cached {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		vendorID := cachedVendorID(item)
		if _, okVendor := c.registry.ByID(vendorID); !okVendor {
			continue
		}
		entry := entries[vendorID]
		// Reuse registration conversion, keeping capability fields (images, tools, parameters) in sync with real-time probing;
		// manually assembling ModelInfo would miss SupportedInputModalities / SupportedParameters,
		// causing the console to display "Not Supported" after a restart until the next successful probe round.
		model := core.ModelInfoToPluginAPI(vendorID, core.ModelDescriptor{
			ID:              item.ID,
			Name:            item.Name,
			Description:     item.Description,
			ContextLength:   item.ContextLength,
			MaxOutputTokens: item.MaxOutputTokens,
			SupportsImages:  item.SupportsImages,
			SupportsTools:   item.SupportsTool,
		})
		model.Thinking = thinkingFromCached(item)
		entry.models = append(entry.models, model)
		// Rates are normalized by the provider before writing to the cache (cacheProbed → creditsFromProvider),
		// so we simply restore them as-is here; formatting again would reinterpret valid values like 0.00X.
		if rate := strings.TrimSpace(item.Credits); rate != "" {
			if entry.credits == nil {
				entry.credits = map[string]string{}
			}
			entry.credits[item.ID] = rate
		}
		entries[vendorID] = entry
	}
	c.mu.Lock()
	for vendorID, entry := range entries {
		c.cached[vendorID] = entry
	}
	c.mu.Unlock()

	for _, vendor := range c.registry.Vendors() {
		loader, okLoader := core.As[core.ModelCacheLoader](vendor)
		if !okLoader {
			continue
		}
		loader.LoadModelCache(cachedForVendor(cached, vendor.ID()))
	}
}

// CachedState flattens the current model cache of each provider into the state.json structure.
func (c *Catalog) CachedState() []core.CachedModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]core.CachedModel, 0, 64)
	for vendorID, entry := range c.cached {
		for _, model := range entry.models {
			cachedModel := core.CachedModel{
				VendorID: vendorID, ID: model.ID, Name: model.Name,
				Description: model.Description, ContextLength: model.ContextLength,
				MaxOutputTokens: model.MaxCompletionTokens,
				Credits:         entry.credits[model.ID],
			}
			if vendor, okVendor := c.registry.ByID(vendorID); okVendor {
				cachedModel.Realm = vendor.Region()
			}
			if model.Thinking != nil {
				cachedModel.Efforts = append([]string(nil), model.Thinking.Levels...)
				cachedModel.DefaultEffort = model.Thinking.Levels[0]
			}
			cachedModel.SupportsImages = core.ModelSupportsImages(model)
			cachedModel.SupportsTool = core.ModelSupportsTools(model)
			out = append(out, cachedModel)
		}
	}
	return out
}

// CreditsFor returns the rate description for a provider's official model; returns an empty string if unknown.
func (c *Catalog) CreditsFor(vendorID, modelID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cached[vendorID].credits[strings.TrimSpace(modelID)]
}

// cachedVendorID resolves the owning provider of a cache entry.
//
// Old states lack vendor_id: back then only WorkBuddy wrote model caches, backfilled by realm.
func cachedVendorID(item core.CachedModel) string {
	if vendorID := strings.TrimSpace(item.VendorID); vendorID != "" {
		return vendorID
	}
	if item.Realm == "global" {
		return "workbuddyglobal"
	}
	return "workbuddycn"
}

func cachedForVendor(cached []core.CachedModel, vendorID string) []core.CachedModel {
	out := make([]core.CachedModel, 0, len(cached))
	for _, item := range cached {
		id := item.VendorID
		if id == "" {
			switch item.Realm {
			case "global":
				id = "workbuddyglobal"
			default:
				id = "workbuddycn"
			}
		}
		if id == vendorID {
			out = append(out, item)
		}
	}
	return out
}

func thinkingFromCached(item core.CachedModel) *pluginapi.ThinkingSupport {
	if len(item.Efforts) == 0 && item.DefaultEffort == "" {
		return nil
	}
	return &pluginapi.ThinkingSupport{Levels: append([]string(nil), item.Efforts...), DynamicAllowed: item.DefaultEffort != ""}
}

// aliasScopedKey is the key for the alias table: <vendorID>:<official model ID>.
func aliasScopedKey(vendorID, modelID string) string {
	return strings.ToLower(strings.TrimSpace(vendorID)) + ":" + strings.TrimSpace(modelID)
}

func (c *Catalog) reloadAliasesLocked(aliases map[string]string) {
	next := make(map[string]string, len(aliases))
	for key, alias := range aliases {
		trimmedKey := strings.TrimSpace(key)
		trimmedAlias := strings.TrimSpace(alias)
		if trimmedKey == "" || trimmedAlias == "" {
			continue
		}
		next[trimmedKey] = trimmedAlias
	}
	c.aliases = next
}

// AliasFor returns the alias set for a provider's model; returns an empty string if not set.
func (c *Catalog) AliasFor(vendorID, modelID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.aliases[aliasScopedKey(vendorID, modelID)]
}

// ApplyAliases replaces a provider's model list with aliases (external IDs).
func (c *Catalog) ApplyAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(models) == 0 || (c.settings.ModelNameTag == ModelNameTagOff && len(c.aliases) == 0) {
		return models
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	// Conflict determination uses the ID set constructed from **this input list**: registeredLocked would judge
	// any name as "already existing" for providers with an empty cache, causing identifiers to be dropped entirely—
	// some items in the same list have identifiers while others don't, precisely reviving the ambiguity this feature aims to eliminate.
	present := make(map[string]bool, len(models))
	for _, model := range models {
		present[strings.ToLower(strings.TrimSpace(model.ID))] = true
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		bare := strings.TrimSpace(model.ID)
		manual := c.aliases[prefix+bare]
		tagged := c.taggedModelID(vendorID, bare)
		switch {
		case manual != "":
			model.ID = manual
			model.Name = manual
			model.DisplayName = manual
		case tagged != "" && !present[strings.ToLower(tagged)]:
			// Auto-channel identifier not overridden: manual alias takes precedence; drop the identifier if the name after the tag
			// collides with **other models in the same provider's list** (prevents routing ambiguity; cross-provider lists are not merged anyway).
			model.ID = tagged
			model.Name = tagged
			model.DisplayName = tagged
		}
		out = append(out, model)
	}
	return out
}

// taggedModelID adds a channel identifier to the model ID based on the current mode; returns an empty string if off or result is empty.
//
// Identifiers are uniformly generated by **provider** (not distinguished between accounts): zcode's GLM-5.3
// appears externally as zcode-GLM-5.3 regardless of how many accounts there are.
func (c *Catalog) taggedModelID(vendorID, modelID string) string {
	switch c.settings.ModelNameTag {
	case ModelNameTagPrefix:
		return vendorID + "-" + modelID
	case ModelNameTagSuffix:
		return modelID + "-" + vendorID
	}
	return ""
}

// ResolveOutboundModel restores a client-requested model name to the upstream official ID.
//
// Only aliases **pointing to the currently registered model** take effect. When a provider changes its external ID (e.g., Qoder changes from upstream
// key to a normalized slug), the old alias would point to a non-existent ID; restoring it anyway would cause subsequent
// disable checks and outbound routing to hit that stale ID, manifesting as "enabled in the list, but reports disabled when called".
//
// Similar to IsDisabled, old aliases are not deleted here: the manifest might be temporarily unavailable due to a probe failure,
// and deleting them would cause user configuration to be permanently lost. Just ignore it; it will automatically take effect once the manifest recovers.
func (c *Catalog) ResolveOutboundModel(vendorID, model string) string {
	requested := strings.TrimSpace(model)
	if requested == "" {
		return model
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	c.mu.RLock()
	defer c.mu.RUnlock()
	for key, alias := range c.aliases {
		if !strings.EqualFold(alias, requested) || !strings.HasPrefix(key, prefix) {
			continue
		}
		bare := strings.TrimPrefix(key, prefix)
		if c.registeredLocked(vendorID, bare) {
			return bare
		}
		logger.Debug("models: ignoring alias %q -> %q (%s no longer registers it)", alias, bare, vendorID)
	}
	// Restoration of automatic channel identifiers: try **both forms** (prefix / suffix), independent of the current mode.
	// After a mode switch (e.g., prefix→off/suffix), the host registry still holds the old name, and requests with the old
	// identifier held by the client must still be restorable, otherwise names with identifiers will pass through directly to the upstream and report invalid model.
	for _, bare := range c.untagModelID(vendorID, requested) {
		if c.registeredLocked(vendorID, bare) {
			return bare
		}
	}
	return model
}

// untagModelID attempts to restore a model ID with channel identifiers back to the official ID, returning all candidates
// (one each for prefix stripping and suffix stripping; returns empty if neither matches).
func (c *Catalog) untagModelID(vendorID, requested string) []string {
	vendorLower := strings.ToLower(strings.TrimSpace(vendorID))
	requestedLower := strings.ToLower(requested)
	var out []string
	if lead := vendorLower + "-"; strings.HasPrefix(requestedLower, lead) && len(requested) > len(lead) {
		out = append(out, requested[len(lead):])
	}
	if tail := "-" + vendorLower; strings.HasSuffix(requestedLower, tail) && len(requested) > len(tail) {
		out = append(out, requested[:len(requested)-len(tail)])
	}
	return out
}

// SetAlias sets or clears an alias for a model.
//
// Aliases are unique within the same provider: clear any old model occupying the same alias name upon writing, otherwise reverse lookup would hit two.
func (c *Catalog) SetAlias(vendorID, bare, alias string) {
	if c.store == nil {
		return
	}
	scoped := aliasScopedKey(vendorID, bare)
	alias = strings.TrimSpace(alias)
	var next map[string]string
	c.store.Mutate(func(s *state.State) {
		if s.ModelAliases == nil {
			s.ModelAliases = map[string]string{}
		}
		s.RestartPending = true
		if alias == "" {
			delete(s.ModelAliases, scoped)
		} else {
			prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
			for existing, existingAlias := range s.ModelAliases {
				if existing != scoped && strings.EqualFold(existingAlias, alias) && strings.HasPrefix(existing, prefix) {
					delete(s.ModelAliases, existing)
				}
			}
			s.ModelAliases[scoped] = alias
		}
		next = make(map[string]string, len(s.ModelAliases))
		for key, value := range s.ModelAliases {
			next[key] = value
		}
	})
	c.mu.Lock()
	c.reloadAliasesLocked(next)
	c.mu.Unlock()
}

// VendorForAliasScope parses the <vendor>:<model> format for management interface validation.
func (c *Catalog) VendorForAliasScope(scope string) (string, string) {
	return c.registry.SplitModelID(strings.TrimSpace(scope))
}

// StaticModels returns the union of models from all enabled providers with credentials (merging same names).
//
// Diagnostic convention: when a provider does not appear in the results, a readable reason must be left here (domain not enabled /
// no available credentials / probe failed). Otherwise, users only see "the model list is empty" and have nowhere to start.
func (c *Catalog) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	var all []pluginapi.ModelInfo
	var firstErr error
	active := hostbridge.ActiveVendorSet(ctx, "", c.registry)
	for _, vendor := range c.registry.Vendors() {
		if !c.RealmEnabled(vendor.Region()) {
			logger.Debug("models: skip %s (realm %q not enabled)", vendor.ID(), vendor.Region())
			continue
		}
		if !active[vendor.ID()] {
			logger.Debug("models: skip %s (no usable credential)", vendor.ID())
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			logger.Info("models: %s probe failed: %v", vendor.ID(), errModels)
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		logger.Info("models: %s registered %d model(s)", vendor.ID(), len(models))
		// Cache the official ID manifest for management page read-back (the registration path will subsequently be switched to aliases).
		c.cacheProbed(vendor.ID(), models)
		all = append(all, c.ApplyAliases(vendor.ID(), c.FilterForVendor(vendor.ID(), models))...)
	}
	// Persist to disk uniformly after the entire probe round ends: writing per provider would amplify a single registration into N disk writes.
	c.persistProbed()
	if len(all) == 0 && firstErr != nil {
		return nil, firstErr
	}
	for _, extra := range c.ExtraModels() {
		all = append(all, c.ApplyAliases(extra.OwnedBy, []pluginapi.ModelInfo{extra})...)
	}
	return MergeSameNameModels(all), nil
}

// RefreshFromUpstream actively refreshes the model manifest for all enabled providers with credentials, returning the item count.
func (c *Catalog) RefreshFromUpstream(ctx context.Context) (int, error) {
	total := 0
	var firstErr error
	active := hostbridge.ActiveVendorSet(ctx, "", c.registry)
	for _, vendor := range c.registry.Vendors() {
		if !c.RealmEnabled(vendor.Region()) || !active[vendor.ID()] {
			logger.Debug("models: refresh skip %s (realm enabled=%t, credential=%t)",
				vendor.ID(), c.RealmEnabled(vendor.Region()), active[vendor.ID()])
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			logger.Info("models: refresh %s failed: %v", vendor.ID(), errModels)
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		logger.Info("models: refresh %s got %d model(s)", vendor.ID(), len(models))
		c.cacheProbed(vendor.ID(), models)
		total += len(models)
	}
	c.persistProbed()
	if total == 0 && firstErr != nil {
		return 0, firstErr
	}
	return total, nil
}

// ExtraModels converts extra model names in the configuration into model entries.
func (c *Catalog) ExtraModels() []pluginapi.ModelInfo {
	c.mu.RLock()
	settings := c.settings
	c.mu.RUnlock()
	out := make([]pluginapi.ModelInfo, 0, len(settings.ExtraModels))
	for _, name := range settings.ExtraModels {
		bare := strings.TrimSpace(name)
		if bare == "" {
			continue
		}
		if vendorID, bareModel := c.registry.SplitModelID(bare); vendorID != "" {
			if vendor, okVendor := c.registry.ByID(vendorID); okVendor &&
				c.RealmEnabled(vendor.Region()) && !c.IsDisabled(vendor.ID(), bareModel) {
				out = append(out, c.extraModelInfo(vendor.ID(), bareModel, settings))
			}
			continue
		}
		for _, vendor := range c.registry.Vendors() {
			if !c.RealmEnabled(vendor.Region()) || c.IsDisabled(vendor.ID(), bare) {
				continue
			}
			out = append(out, c.extraModelInfo(vendor.ID(), bare, settings))
		}
	}
	return out
}

func (c *Catalog) extraModelInfo(vendorID, bareModel string, settings ModelSettings) pluginapi.ModelInfo {
	var contextLength int64
	if settings.ContextWindowFor != nil {
		contextLength = settings.ContextWindowFor(bareModel)
	}
	return core.ModelInfoToPluginAPI(vendorID, core.ModelDescriptor{
		ID:            bareModel,
		Name:          bareModel,
		ContextLength: contextLength,
	})
}

// MergeSameNameModels merges entries with the same bare model name, taking the optimal union of parameters.
//
// The host matches model IDs using EqualsFold, so entries with the same name are considered the same model from its perspective;
// not merging them would cause /v1/models to list duplicates. Window/output takes the max, capabilities take OR, tiers take union.
func MergeSameNameModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	merged := make([]pluginapi.ModelInfo, 0, len(models))
	seen := make(map[string]int, len(models))
	for _, model := range models {
		bare := strings.TrimSpace(model.ID)
		if bare == "" {
			continue
		}
		key := strings.ToLower(bare)
		index, okSeen := seen[key]
		if !okSeen {
			seen[key] = len(merged)
			merged = append(merged, model)
			continue
		}
		target := &merged[index]
		if model.ContextLength > target.ContextLength {
			target.ContextLength = model.ContextLength
			target.InputTokenLimit = model.InputTokenLimit
		}
		if model.MaxCompletionTokens > target.MaxCompletionTokens {
			target.MaxCompletionTokens = model.MaxCompletionTokens
			target.OutputTokenLimit = model.OutputTokenLimit
		}
		if target.Description == "" {
			target.Description = model.Description
		}
		if core.ModelSupportsImages(model) {
			target.SupportedInputModalities = appendUnique(target.SupportedInputModalities, "image")
		}
		// Capabilities take OR: if any entry supports tool calling, the merged result must declare both methods and parameters simultaneously.
		// Merging only tool_calls would leave a contradictory combination of "methods present, parameters missing", causing the client to
		// hide the tool switch, and a model that actually supports tools would lose its capability.
		if core.ModelSupportsTools(*target) || core.ModelSupportsTools(model) {
			target.SupportedGenerationMethods = appendUnique(target.SupportedGenerationMethods, "tool_calls")
			for _, param := range core.ToolParameters() {
				target.SupportedParameters = appendUnique(target.SupportedParameters, param)
			}
		}
		target.Thinking = mergeThinking(target.Thinking, model.Thinking)
	}
	return merged
}

func mergeThinking(a, b *pluginapi.ThinkingSupport) *pluginapi.ThinkingSupport {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	levels := append([]string(nil), a.Levels...)
	for _, level := range b.Levels {
		levels = appendUnique(levels, level)
	}
	return &pluginapi.ThinkingSupport{
		Levels:         levels,
		DynamicAllowed: a.DynamicAllowed || b.DynamicAllowed,
		ZeroAllowed:    a.ZeroAllowed || b.ZeroAllowed,
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
