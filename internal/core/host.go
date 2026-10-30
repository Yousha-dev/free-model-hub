package core

// This file defines "host ports": capabilities required by the provider layer
// but which can only be provided by the root layer (ABI adaptation layer).
//
// Why it is needed: internal/vendors/* must remain agnostic of the host (cannot import package main),
// but they do need three things: list host credentials, retrieve credential JSON by auth_index, and read global configuration.
// The dependency direction is guaranteed by the root layer injecting implementations in init (same style as internal/hostbridge.Configure).
//
// Before injection, the port is nil, and all wrapper functions return zero values instead of panicking:
// both unit tests and "plugin not yet registered" scenarios will take this path.

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrHostNotConfigured indicates that the host port has not been injected yet.
var ErrHostNotConfigured = errors.New("core: host port is not configured")

// HostAuth is an entry in the host credential list (neutral form, fields aligned with host.auth.list).
type HostAuth struct {
	ID          string    `json:"id"`
	AuthIndex   string    `json:"auth_index"`
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Label       string    `json:"label"`
	Status      string    `json:"status"`
	Disabled    bool      `json:"disabled"`
	Unavailable bool      `json:"unavailable"`
	RuntimeOnly bool      `json:"runtime_only"`
	Path        string    `json:"path,omitempty"`
	ModTime     time.Time `json:"modtime,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// Usable reports whether the credential can be used for the plugin's own operations (model probing, quota query,
// check-in, keep-alive, console display).
//
// Look only at Disabled: it is explicitly disabled by the user and must be respected. Unavailable is a temporary status
// of the host **routing layer** (cooling down applied after upstream rate limiting or timeout), meaning "temporarily do not route user chat traffic
// to this credential", which does not mean the credential does not exist or is unreadable. Conflating the two causes a single rate limit to make
// the entire provider disappear from model lists and quota queries — manifesting in the UI as "models gone" and "remaining/total quota
// stuck loading forever", while the credential file itself remains intact. This is an incident that has actually occurred.
func (a HostAuth) Usable() bool { return !a.Disabled }

// Settings is a provider-agnostic global configuration snapshot (read-only).
//
// Only include fields that "multiple providers need to inspect": provider-specific configurations are parsed by individual packages
// from raw keys themselves (see Configurable) and do not go here.
type Settings struct {
	// EnabledRealms are the enabled realms (cn / global), empty means all enabled.
	EnabledRealms []string
	// StateDir is the plugin state directory (root for local models).
	StateDir string
	// MachineSalt is the device fingerprint salt for this deployment (empty means not configured by the provider).
	MachineSalt string
	// LogLevel is the logging level.
	LogLevel string
}

// RealmEnabled reports whether a realm is configured as enabled.
func (s Settings) RealmEnabled(region string) bool {
	return RealmEnabled(s.EnabledRealms, region)
}

// RealmEnabled determines whether a realm is in the enabled list; an empty realm or empty list is considered enabled.
func RealmEnabled(enabled []string, region string) bool {
	if strings.TrimSpace(region) == "" || len(enabled) == 0 {
		return true
	}
	for _, realm := range enabled {
		if realm == region {
			return true
		}
	}
	return false
}

// Host is the set of host capabilities that the root layer must provide.
type Host interface {
	// ListAuths lists all credentials under this plugin.
	ListAuths(ctx context.Context) ([]HostAuth, error)
	// AuthJSON retrieves the credential JSON by auth_index.
	AuthJSON(ctx context.Context, authIndex string) ([]byte, bool)
	// Settings returns the current global configuration snapshot.
	Settings() Settings
}
