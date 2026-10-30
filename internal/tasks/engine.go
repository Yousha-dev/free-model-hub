package tasks

// This file implements keep-alive scheduling: wakes up at configured top of the hour to refresh tokens for all accounts.

import (
	"context"
	"sort"
	"time"

	"free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
)

const (
	// schedulerWakeGrace is the grace period for late wake-up (network stack needs time after sleep recovery).
	schedulerWakeGrace = 5 * time.Second
	// schedulerLateThreshold is the threshold to determine if a wake-up is "late".
	schedulerLateThreshold = 1 * time.Second
	// schedulerReconfigure is the configuration change signal.
	schedulerReconfigure = "reconfigure"
	// accountGap is the operation interval between accounts.
	accountGap = 800 * time.Millisecond
)

// DefaultKeepaliveHours are the default top-of-the-hour slots for keep-alive.
var DefaultKeepaliveHours = []int{22}

// runScheduler is the background scheduling loop.
func (s *Service) runScheduler(ctx context.Context) {
	logger.Info("keepalive scheduler started")
	s.setRunning(true)
	defer func() {
		s.setRunning(false)
		logger.Info("keepalive scheduler stopped")
	}()
	for {
		next := NextWake(s.deps.Now(), s.KeepaliveHours())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.pokeCh:
			timer.Stop()
			continue
		case <-timer.C:
			if delay := time.Since(next); delay > schedulerLateThreshold {
				if !core.SleepContext(ctx, schedulerWakeGrace) {
					return
				}
			}
			s.runKeepaliveAll(ctx)
		}
	}
}

func (s *Service) setRunning(running bool) {
	s.schedulerMu.Lock()
	s.running = running
	s.schedulerMu.Unlock()
}

// KeepaliveHours returns the effective keep-alive hours (page settings take precedence over defaults).
func (s *Service) KeepaliveHours() []int {
	if hours := NormalizeHours(s.deps.State().TaskSchedules[core.TaskKindKeepalive]); len(hours) > 0 {
		return hours
	}
	return append([]int(nil), DefaultKeepaliveHours...)
}

// schedulerStatus returns the scheduler status (for management page display).
func (s *Service) schedulerStatus() map[string]any {
	s.schedulerMu.Lock()
	running := s.running
	s.schedulerMu.Unlock()
	hours := s.KeepaliveHours()
	return map[string]any{
		"running":         running,
		"keepalive_hours": hours,
		"next_at":         NextWake(s.deps.Now(), hours).Format(time.RFC3339),
	}
}

// runKeepaliveAll refreshes tokens for all accounts.
func (s *Service) runKeepaliveAll(ctx context.Context) {
	logger.Info("keepalive: starting for all accounts")
	count := 0
	s.forEachAccount(ctx, "", nil, func(ctx context.Context, account *accountContext) error {
		result := s.keepaliveOne(ctx, "", account)
		if !result.OK {
			logger.Error("keepalive %s: %s", account.label(), result.Message)
			return nil
		}
		logger.Info("keepalive %s: %s", account.label(), result.Message)
		count++
		return nil
	})
	logger.Info("keepalive: finished for %d account(s)", count)
}

// forEachAccount iterates over available accounts under this plugin; empty targets mean all.
func (s *Service) forEachAccount(ctx context.Context, callbackID string, targets []string, fn func(context.Context, *accountContext) error) {
	cfg := s.deps.Config()
	accounts, failures := s.deps.Pool().Load(ctx, callbackID, auth.Options{
		Targets:       targets,
		EnabledRealms: cfg.EnabledRealms,
	})
	for _, failure := range failures {
		logger.Debug("skip account %s: %s", core.FirstNonEmpty(failure.Entry.Name, failure.Entry.ID), failure.Message)
	}
	for _, account := range accounts {
		if errRun := fn(ctx, newAccountContext(account)); errRun != nil {
			logger.Debug("account %s failed: %v", core.FirstNonEmpty(account.Entry.Label, account.Entry.Name), errRun)
		}
		if !core.SleepContext(ctx, accountGap) {
			return
		}
	}
}

// accountContext is the context for a single account during iteration.
//
// Besides identity and credentials, it also carries raw JSON and source file path: both are needed
// to merge new credentials back into the host and disk after refreshing (see keepalive.go).
type accountContext struct {
	entry      core.HostAuth
	vendor     core.Vendor
	credential *core.Credential
	rawJSON    []byte
	filePath   string
}

// newAccountContext converts an account pool entry into an iteration context.
func newAccountContext(item auth.Account) *accountContext {
	if item.FilePath != "" {
		item.Credential.SetFilePath(item.FilePath)
	}
	return &accountContext{
		entry: item.Entry, vendor: item.Vendor, credential: item.Credential,
		rawJSON: item.RawJSON, filePath: item.FilePath,
	}
}

func (a *accountContext) uid() string { return a.credential.FileIDValue() }

func (a *accountContext) label() string {
	return core.FirstNonEmpty(a.credential.LabelValue(), a.entry.Label, a.entry.Name, a.entry.ID)
}

// NormalizeHours deduplicates, sorts, and filters invalid hours.
func NormalizeHours(hours []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(hours))
	for _, hour := range hours {
		if hour < 0 || hour > 23 || seen[hour] {
			continue
		}
		seen[hour] = true
		out = append(out, hour)
	}
	sort.Ints(out)
	return out
}

// NextWake returns the next top-of-the-hour wake-up time; checks again in an hour if none is set.
func NextWake(now time.Time, hours []int) time.Time {
	hours = NormalizeHours(hours)
	if len(hours) == 0 {
		return now.Add(time.Hour)
	}
	for _, hour := range hours {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if candidate.After(now) {
			return candidate
		}
	}
	tomorrow := now.AddDate(0, 0, 1)
	return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), hours[0], 0, 0, 0, now.Location())
}
