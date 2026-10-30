// Package auth provides unified account enumeration, credential parsing, deduplication, batch execution, and login session registration.
//
// admin, tasks, quota, and keepalive each previously implemented their own version of "list auth -> fetch JSON ->
// parse credentials -> filter -> deduplicate", with inconsistent deduplication keys. This package consolidates that pipeline.
//
// Login sessions (SessionRegistry) are also housed here: they share the "isolated per app instance" lifecycle
// with credential enumeration, and providers only use them via core.Env.Logins without needing to know about this package.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// Resolver is injected by the root layer: it parses raw credentials into (Credential, Vendor).
type Resolver func(ctx context.Context, callbackID string, storageJSON []byte, authID string, attrs map[string]string) (*core.Credential, core.Vendor, error)

// Pool is the entry point for account enumeration and batch parsing.
type Pool struct {
	Registry *core.Registry
	Resolve  Resolver
}

// NewPool constructs an account pool.
func NewPool(registry *core.Registry, resolve Resolver) *Pool {
	return &Pool{Registry: registry, Resolve: resolve}
}

// Account represents a parsed and available account.
type Account struct {
	Entry      core.HostAuth
	Credential *core.Credential
	Vendor     core.Vendor
	RawJSON    []byte
	FilePath   string
}

// Failure represents an unparsable account record.
type Failure struct {
	Entry   core.HostAuth
	Message string
}

// Options controls account filtering.
//
// EnabledRealms directly provides a list of realms instead of a judgment closure: realm filtering semantics
// are implemented in a single place (core.RealmEnabled), so callers don't need to wrap their own closures—which was the source of previous duplication.
// Leaving it empty means no restriction, consistent with core.RealmEnabled(nil, _) == true.
type Options struct {
	Targets         []string
	Filter          func(Account) bool
	EnabledRealms   []string
	IncludeDisabled bool
}

// Load returns available accounts and failure entries, deduplicated by credential FileID.
func (p *Pool) Load(ctx context.Context, callbackID string, opts Options) ([]Account, []Failure) {
	entries, errList := hostbridge.ListAuths(ctx, callbackID, p.Registry)
	if errList != nil {
		return nil, []Failure{{Message: "failed to read account list: " + errList.Error()}}
	}
	wanted := map[string]bool{}
	for _, target := range opts.Targets {
		if trimmed := strings.TrimSpace(target); trimmed != "" {
			wanted[trimmed] = true
		}
	}
	accounts := make([]Account, 0, len(entries))
	failures := make([]Failure, 0)
	seen := map[string]bool{}
	vendorCounts := map[string]int{}
	for _, entry := range entries {
		if !opts.IncludeDisabled && !entry.Usable() {
			continue
		}
		if len(wanted) > 0 && !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
			continue
		}
		raw, filePath, okRaw := hostbridge.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
		if !okRaw && strings.TrimSpace(entry.Path) != "" {
			if data, errRead := os.ReadFile(entry.Path); errRead == nil && len(data) > 0 {
				raw, filePath, okRaw = data, entry.Path, true
			}
		}
		if !okRaw || len(raw) == 0 {
			failures = append(failures, Failure{Entry: entry, Message: "failed to read credential"})
			continue
		}
		credential, vendor, errResolve := p.Resolve(ctx, callbackID, raw, hostbridge.AccountIdentity(entry), nil)
		if errResolve != nil {
			failures = append(failures, Failure{Entry: entry, Message: "failed to parse credential: " + errResolve.Error()})
			continue
		}
		if !core.RealmEnabled(opts.EnabledRealms, vendor.Region()) {
			continue
		}
		uid := credential.FileIDValue()
		if uid != "" {
			if seen[uid] {
				continue
			}
			seen[uid] = true
		}
		if vendorCounts[vendor.ID()] >= core.MaxAccountsPerVendor {
			failures = append(failures, Failure{Entry: entry, Message: fmt.Sprintf("exceeded the limit of %d accounts per provider", core.MaxAccountsPerVendor)})
			continue
		}
		account := Account{Entry: entry, Credential: credential, Vendor: vendor, RawJSON: raw, FilePath: filePath}
		if opts.Filter != nil && !opts.Filter(account) {
			continue
		}
		vendorCounts[vendor.ID()]++
		accounts = append(accounts, account)
	}
	return accounts, failures
}

// CheckVendorLimit validates whether the target provider has reached the single-provider account limit (MaxAccountsPerVendor).
//
// When candidateFileName is non-empty, if the file corresponds to an existing account under the same provider (overwrite/update by name),
// it is allowed; adding a new account when the existing count reaches the limit returns a 400 PluginError.
func (p *Pool) CheckVendorLimit(ctx context.Context, callbackID string, vendor core.Vendor, candidateFileName string) error {
	if p == nil || vendor == nil {
		return nil
	}
	loaded, _ := p.Load(ctx, callbackID, Options{IncludeDisabled: true})
	candidateID := strings.TrimSuffix(strings.TrimSpace(candidateFileName), ".json")
	count := 0
	for _, account := range loaded {
		if account.Vendor.ID() != vendor.ID() {
			continue
		}
		if candidateID != "" && (account.Credential.FileIDValue() == candidateID ||
			strings.TrimSuffix(strings.TrimSpace(account.Entry.Name), ".json") == candidateID ||
			strings.TrimSuffix(strings.TrimSpace(account.Entry.ID), ".json") == candidateID) {
			return nil
		}
		count++
	}
	if count >= core.MaxAccountsPerVendor {
		return core.NewPluginError("account_limit_exceeded",
			fmt.Sprintf("You can add a maximum of %d accounts per provider (%s has reached the limit)", core.MaxAccountsPerVendor, vendor.Name()),
			http.StatusBadRequest)
	}
	return nil
}
