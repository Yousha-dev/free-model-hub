package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"free-model-hub/internal/admin"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// KeylessAccountsKey turns automatic keyless accounts on or off (default on).
const KeylessAccountsKey = "keyless_accounts"

// provisionKeyless writes one keyless account per vendor that supports it.
//
// It runs once per state directory and vendor: a marker file records that the
// account was offered, so an account the user deleted stays deleted. The host
// may still be finishing plugin registration, so listing is retried briefly.
func (a *App) provisionKeyless(ctx context.Context) {
	cfg := a.Config()
	if !admin.ParseBool(cfg.Raw[KeylessAccountsKey], true) {
		return
	}
	var entries []core.HostAuth
	for attempt := 0; ; attempt++ {
		listed, errList := hostbridge.ListAuths(ctx, "", a.registry)
		if errList == nil {
			entries = listed
			break
		}
		if attempt >= 10 {
			logger.Info("keyless accounts skipped: host auth list unavailable: %v", errList)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	for _, vendor := range a.registry.Vendors() {
		provider, ok := core.As[core.KeylessProvider](vendor)
		if !ok || len(provider.KeylessCredentialJSON()) == 0 {
			continue
		}
		vendorID := strings.ToLower(vendor.ID())
		marker := filepath.Join(cfg.StateDir, "keyless-"+vendorID+".offered")
		if _, errStat := os.Stat(marker); errStat == nil {
			continue
		}
		if !hasVendorAuth(entries, vendorID) {
			name := vendorID + "-free.json"
			if _, errSave := hostbridge.SaveAuth("", name, provider.KeylessCredentialJSON()); errSave != nil {
				logger.Info("keyless account %s not saved: %v", name, errSave)
				continue
			}
			logger.Info("added keyless account %s", name)
		}
		if errDir := os.MkdirAll(cfg.StateDir, 0o700); errDir == nil {
			_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
		}
	}
}

func hasVendorAuth(entries []core.HostAuth, vendorID string) bool {
	for _, entry := range entries {
		if core.FileNameMatchesConvention(strings.TrimSuffix(strings.ToLower(entry.Name), ".json"), vendorID) {
			return true
		}
	}
	return false
}
