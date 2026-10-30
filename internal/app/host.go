package app

// This file is the App's host surface: core.Host implementation, provider configuration dispatch, background/stream lifecycle,
// and management interface dependency injection.

import (
	"context"
	"sync"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/admin"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
	"free-model-hub/internal/state"
)

type hostBridge struct{ app *App }

func (h *hostBridge) ListAuths(ctx context.Context) ([]core.HostAuth, error) {
	return hostbridge.ListAuths(ctx, "", h.app.registry)
}

func (h *hostBridge) AuthJSON(ctx context.Context, authIndex string) ([]byte, bool) {
	return hostbridge.AuthJSON(ctx, "", authIndex)
}

func (h *hostBridge) Settings() core.Settings {
	cfg := h.app.Config()
	return core.Settings{
		EnabledRealms: append([]string(nil), cfg.EnabledRealms...),
		StateDir:      cfg.StateDir,
		MachineSalt:   h.app.machineSalt,
		LogLevel:      cfg.LogLevel,
	}
}

// configureVendors pushes global settings and raw config keys to each provider.
func (a *App) configureVendors(cfg pluginConfig) {
	settings := a.host.Settings()
	raw := make(map[string]string, len(cfg.Raw)+1)
	for key, value := range cfg.Raw {
		raw[key] = value
	}
	// Prompt filtering is a global setting: UI override > YAML > built-in default; providers read the same key.
	raw[admin.PromptModeKey] = admin.EffectivePromptMode(cfg, a.SnapshotState().PromptMode)
	for _, vendor := range a.registry.Vendors() {
		if configurable, okConfigurable := vendor.(core.Configurable); okConfigurable {
			configurable.Configure(settings, raw)
		}
	}
}

const DefaultShutdownWaitTimeout = 10 * time.Second

// StartBackgroundWork starts the background loop (idempotent).
func (a *App) StartBackgroundWork() {
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	if a.backgroundCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.backgroundCancel = cancel
	a.tasks.Start(ctx)
	a.backgroundWG.Add(1)
	go func() {
		defer a.backgroundWG.Done()
		a.provisionKeyless(ctx)
	}()
}

// StopBackgroundWork cancels the background loop and waits for exit.
func (a *App) StopBackgroundWork() {
	a.backgroundMu.Lock()
	cancel := a.backgroundCancel
	a.backgroundCancel = nil
	a.backgroundMu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.tasks.Stop()
	a.backgroundWG.Wait()
}

// BackgroundRunning reports whether the background loop is running.
func (a *App) BackgroundRunning() bool {
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	return a.backgroundCancel != nil
}

// BeginPluginStream registers a streaming forwarding task.
func (a *App) BeginPluginStream() (context.Context, context.CancelFunc, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	a.streamMu.Lock()
	a.nextStreamKey++
	key := a.nextStreamKey
	a.streamCancels[key] = cancel
	a.streamMu.Unlock()
	a.streamWG.Add(1)
	var once sync.Once
	finish := func() {
		once.Do(func() {
			a.streamMu.Lock()
			delete(a.streamCancels, key)
			a.streamMu.Unlock()
			cancel()
			a.streamWG.Done()
		})
	}
	return ctx, cancel, finish
}

// WaitPluginStreams cancels and waits for in-flight streams.
func (a *App) WaitPluginStreams(timeout time.Duration) {
	a.streamMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(a.streamCancels))
	for _, cancel := range a.streamCancels {
		cancels = append(cancels, cancel)
	}
	inflight := len(a.streamCancels)
	a.streamMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if inflight == 0 {
		a.streamWG.Wait()
		return
	}
	if timeout <= 0 {
		timeout = DefaultShutdownWaitTimeout
	}
	done := make(chan struct{})
	go func() {
		a.streamWG.Wait()
		close(done)
	}()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(timeout):
		a.streamMu.Lock()
		remaining := len(a.streamCancels)
		a.streamMu.Unlock()
		logger.Error("shutdown: %d stream task(s) still running after %s", remaining, time.Since(start).Round(time.Millisecond))
	}
}

// adminDeps implements admin.Deps.
type adminDeps struct{ app *App }

func (d adminDeps) Identity() admin.Identity {
	return admin.Identity{ID: pluginID, DisplayName: pluginDisplayName, ProviderKey: providerKey, Version: d.app.version}
}

func (d adminDeps) Config() admin.PluginConfig { return d.app.Config() }
func (d adminDeps) State() state.State         { return d.app.SnapshotState() }
func (d adminDeps) MutateState(fn func(*state.State)) {
	d.app.MutateState(fn)
}
func (d adminDeps) Model() *admin.Catalog { return d.app.models }

func (d adminDeps) RefreshModels(ctx context.Context) (int, error) {
	return d.app.refreshModelsFromUpstream(ctx)
}

func (d adminDeps) Reconfigure() error { return d.app.ApplyConfig(d.app.Config()) }

// SubmitLoginCallback passes the localhost callback link pasted back from the page to the corresponding provider for processing.
func (d adminDeps) SendLoginSMS(ctx context.Context, vendorID, phone string) (string, error) {
	return d.app.SendLoginSMS(ctx, vendorID, phone)
}

func (d adminDeps) VerifyLoginSMS(ctx context.Context, vendorID, sessionKey, phone, code string) (string, string, string, error) {
	return d.app.VerifyLoginSMS(ctx, vendorID, sessionKey, phone, code)
}

func (d adminDeps) SubmitLoginCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	return d.app.SubmitLoginCallback(ctx, state, rawURL)
}

func (d adminDeps) StartLogin(ctx context.Context, vendorID string, metadata map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return d.app.StartLogin(ctx, vendorID, metadata)
}

func (d adminDeps) LoginCaptchaConfig(ctx context.Context, vendorID string) (core.CaptchaConfig, error) {
	return d.app.LoginCaptchaConfig(ctx, vendorID)
}

func (a *App) managementRegistration() pluginapi.ManagementRegistrationResponse {
	return a.admin.Registration()
}

func (a *App) handleManagement(request []byte) ([]byte, error) { return a.admin.Handle(request) }
