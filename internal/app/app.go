package app

// Package app is the composition root and ABI adaptation layer of the plugin.
//
// An App instance holds configuration, state, models, provider registry, login sessions, and background tasks;
// package main holds only the default instance required by the C ABI. Tests can construct multiple Apps and verify isolation.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"free-model-hub/internal/admin"
	"free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/state"
	"free-model-hub/internal/tasks"
)

// DefaultVersion is the version when no distribution version number is injected.
const DefaultVersion = "1.0.1"

// Options are the construction parameters for App.
type Options struct {
	Version   string
	Factories []core.VendorFactory
	// Host is for testing/embedding only; when empty, the internal hostBridge is used.
	Host core.Host
	// Now allows tests to inject a clock; when empty, time.Now is used.
	Now func() time.Time
}

// App is a complete running instance of the plugin.
type App struct {
	version string
	now     func() time.Time

	host     core.Host
	registry *core.Registry
	logins   *auth.SessionRegistry

	cfg    atomic.Pointer[admin.PluginConfig]
	store  *state.Store
	models *admin.Catalog

	machineSalt string

	lifecycleMu sync.Mutex
	registered  bool

	credentialMu    sync.Mutex
	credentialCache map[string]credentialCacheEntry

	backgroundMu     sync.Mutex
	backgroundCancel context.CancelFunc
	backgroundWG     sync.WaitGroup

	streamMu       sync.Mutex
	streamCancels  map[uint64]context.CancelFunc
	streamWG       sync.WaitGroup
	nextStreamKey  uint64
	pluginStreamID uint64

	pool  *auth.Pool
	tasks *tasks.Service
	admin *admin.Service
}

// New constructs an App.
func New(opts Options) (*App, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	version := opts.Version
	if version == "" {
		version = DefaultVersion
	}
	a := &App{
		version:         version,
		now:             now,
		registry:        core.NewRegistry(),
		logins:          auth.NewSessionRegistry(),
		store:           state.NewStore(),
		credentialCache: map[string]credentialCacheEntry{},
		streamCancels:   map[uint64]context.CancelFunc{},
	}
	if opts.Host != nil {
		a.host = opts.Host
	} else {
		a.host = &hostBridge{app: a}
	}
	env := core.Env{Host: a.host, Registry: a.registry, Logins: a.logins}
	for _, factory := range opts.Factories {
		if factory.Instances == nil {
			continue
		}
		for _, vendor := range factory.Instances(env) {
			if errRegister := a.registry.Register(vendor); errRegister != nil {
				return nil, errRegister
			}
		}
	}
	a.models = admin.NewCatalog(a.store, a.registry)
	a.pool = auth.NewPool(a.registry, a.resolveVendorCredential)
	a.tasks = tasks.New(taskDeps{app: a})
	a.admin = admin.New(adminDeps{app: a}, a.registry, a.pool, a.tasks)
	defaults := defaultPluginConfig()
	a.storeConfig(defaults)
	a.models.Configure(admin.ModelSettings{
		EnabledRealms:    defaults.EnabledRealms,
		ExtraModels:      defaults.ExtraModels,
		DisabledModels:   defaults.DisabledModels,
		ContextWindowFor: core.DefaultContextWindow,
	})
	return a, nil
}

// MustNew constructs an App and panics on failure (composition root startup error).
func MustNew(opts Options) *App {
	a, err := New(opts)
	if err != nil {
		panic(err)
	}
	return a
}

// Version returns the external version number.
func (a *App) Version() string { return a.version }

// Registry returns the provider registry.
func (a *App) Registry() *core.Registry { return a.registry }

// Model returns the model manager.
func (a *App) Model() *admin.Catalog { return a.models }

// taskDeps implements tasks.Deps.
type taskDeps struct{ app *App }

func (d taskDeps) Registry() *core.Registry          { return d.app.registry }
func (d taskDeps) Pool() *auth.Pool                  { return d.app.pool }
func (d taskDeps) Config() tasks.Config              { return taskConfig(d.app.Config()) }
func (d taskDeps) State() state.State                { return d.app.SnapshotState() }
func (d taskDeps) MutateState(fn func(*state.State)) { d.app.MutateState(fn) }
func (d taskDeps) Now() time.Time                    { return d.app.now() }

// SaveRefreshedCredential reuses the credential write-back implementation in the composition root (merge → host.auth.save → persist).
func (d taskDeps) SaveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor,
	entry core.HostAuth, original []byte, updated *core.Credential, filePath string) {
	d.app.saveRefreshedCredential(ctx, callbackID, vendor, entry, original, updated, filePath)
}
