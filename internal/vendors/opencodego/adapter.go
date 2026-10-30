// Package opencodego exposes the OpenCode Go subscription as one vendor.
//
// Ported from xspeed1989/opencode-go-cliproxyapi (MIT): OpenCode Go serves
// each model on one of three upstream protocols (chat completions, Anthropic
// messages, Responses). The host hands this plugin chat-completions, so the
// ported translators in internal/ocgo convert to and from the model's route.
package opencodego

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
	"free-model-hub/internal/ocgo/catalog"
	"free-model-hub/internal/ocgo/config"
)

type adapter struct {
	*core.NativeAdapter[*Credential]

	mu        sync.Mutex
	cfg       config.Config
	catalog   *catalog.Manager
	refreshed time.Time
}

// Instances builds the single OpenCode Go vendor.
func Instances(env core.Env) []core.Vendor {
	cfg := config.Default("")
	a := &adapter{cfg: cfg, catalog: catalog.New(cfg, hostClient{})}
	a.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorID, VendorName: VendorName},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := parseCredential(raw)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			label := native.Label
			if label == "" {
				label = maskKey(native.APIKey)
			}
			return core.CredentialSeed{
				VendorID: VendorID, Label: label, FileID: core.FileIDFromFileName(fileName),
				Token: native.APIKey, AuthMode: "apikey",
			}, native, nil
		},
	})
	return []core.Vendor{a}
}

// Match accepts only files named after this vendor.
func (a *adapter) Match(fileName, _ string, _ map[string]any) bool {
	return core.FileNameMatchesConvention(fileName, VendorID, "opencode-go")
}

// Configure applies the optional base URL override.
func (a *adapter) Configure(_ core.Settings, raw map[string]string) {
	cfg := config.Default(raw["opencodego_base_url"])
	a.mu.Lock()
	defer a.mu.Unlock()
	if cfg.BaseURL == a.cfg.BaseURL {
		return
	}
	next := catalog.New(cfg, hostClient{})
	next.SeedFrom(a.catalog)
	a.cfg, a.catalog, a.refreshed = cfg, next, time.Time{}
}

// ConfigFields declares OpenCode Go options.
func (a *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{{Name: "opencodego_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "OpenCode Go upstream base URL override."}}
}

// StaticModels lists the catalog using any usable OpenCode Go key.
func (a *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	native, ok := core.ProbeNativeCredential(a.Env, VendorID, func(cred *core.Credential) (*Credential, bool) {
		value, okValue := cred.Native.(*Credential)
		return value, okValue
	})
	if !ok || native == nil {
		return nil, fmt.Errorf("no opencodego credential available for model probe")
	}
	if errRefresh := a.refresh(ctx, native.APIKey); errRefresh != nil {
		return nil, errRefresh
	}
	return a.modelInfos(), nil
}

// ModelsForAuth lists the catalog with this credential's own key, so an
// invalid key yields no models and the host drops it.
func (a *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := a.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	a.mu.Lock()
	a.refreshed = time.Time{}
	a.mu.Unlock()
	if errRefresh := a.refresh(ctx, native.APIKey); errRefresh != nil {
		logger.Info("opencodego: model probe failed: %v", errRefresh)
		return nil, nil
	}
	return a.modelInfos(), nil
}

func (a *adapter) lookup(model string) (catalog.ModelRecord, config.Config, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.catalog.Lookup(strings.TrimSpace(model))
	return rec, a.cfg, ok
}

var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
)
