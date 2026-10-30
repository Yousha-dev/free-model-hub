package core

import (
	"strings"
	"sync"
)

// CreditTable is a concurrently safe map of "Model ID -> Billing Multiplier Text".
//
// Providers supply multipliers to the host in only one form: full replacement upon probing, full snapshot upon console reads.
// This logic was previously copied across cline, qoder, raccoon, autoclaw, trae, and opencodezen
// (even with identical field names), and is now consolidated here. Zero-value usable.
type CreditTable struct {
	mu      sync.RWMutex
	byModel map[string]string
}

// Replace performs a full table replacement with the results of a probe round.
//
// A nil or empty table indicates "no multipliers for this round"; readers will receive nil and display "—" rather than
// retaining stale data from the previous round. The table is sanitized (for empty IDs/texts) when provided by the caller.
func (t *CreditTable) Replace(credits map[string]string) {
	next := make(map[string]string, len(credits))
	for id, label := range credits {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if label = strings.TrimSpace(label); label != "" {
			next[id] = label
		}
	}
	t.store(next)
}

// store accepts the sanitized table without repeated filtering.
func (t *CreditTable) store(byModel map[string]string) {
	t.mu.Lock()
	t.byModel = byModel
	t.mu.Unlock()
}

// FillCredits normalizes and writes a batch of model multiplier texts into t: id takes the external model ID,
// label takes the upstream raw text (returns an empty string if missing). Empty IDs and texts are skipped—unknowns are not written.
//
// "Iterate models -> normalize -> full table replacement" was previously copied across six providers (with even
// identical field names); after consolidating here, each provider only needs to supply two field-getter functions.
func FillCredits[T any](t *CreditTable, models []T, id func(T) string, label func(T) string) {
	next := make(map[string]string, len(models))
	for _, model := range models {
		modelID := strings.TrimSpace(id(model))
		if modelID == "" {
			continue
		}
		raw := strings.TrimSpace(label(model))
		if raw == "" {
			continue
		}
		if rate := FormatRate(raw); rate != "" {
			next[modelID] = rate
		}
	}
	t.store(next)
}

// Snapshot returns a full copy of the table; returns nil if empty, allowing callers to determine "unknown".
func (t *CreditTable) Snapshot() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.byModel) == 0 {
		return nil
	}
	out := make(map[string]string, len(t.byModel))
	for id, label := range t.byModel {
		out[id] = label
	}
	return out
}
