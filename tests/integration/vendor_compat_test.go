package tests

// This file locks down the external contract: vendor instance inventory (ID/name/region) and vendor-specific configuration keys.
//
// These are user-visible red lines (credential file name prefixes, console dropdown items, configuration key semantics),
// and any refactoring should not change them; if they change, this will fail first.

import (
	"testing"

	"free-model-hub/internal/core"
	"free-model-hub/internal/vendors/openaicompat"
)

// withoutOpenAICompat drops the generic OpenAI-compatible providers, which
// are covered by their own package tests, from a registry listing.
func withoutOpenAICompat(vendors []core.Vendor) []core.Vendor {
	generic := map[string]bool{"opencodego": true}
	for _, provider := range openaicompat.Providers {
		generic[provider.ID] = true
	}
	out := make([]core.Vendor, 0, len(vendors))
	for _, vendor := range vendors {
		if !generic[vendor.ID()] {
			out = append(out, vendor)
		}
	}
	return out
}

// TestVendorRegistryCompatibility locks the external identity and order of the 15 vendor instances.
func TestVendorRegistryCompatibility(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	want := []struct {
		id     string
		name   string
		region string
	}{
		{"autoclawcn", "AutoClaw CN", "cn"},
		{"autoclawglobal", "AutoClaw Global", "global"},
		{"cline", "Cline", ""},
		{"codearts", "CodeArts", "cn"},
		{"minimaxcodecn", "MiniMax Code CN", "cn"},
		{"minimaxcodeglobal", "MiniMax Code Global", "global"},
		{"opencodezen", "OpenCode ZEN", ""},
		{"qodercn", "Qoder CN", "cn"},
		{"qoderglobal", "Qoder Global", "global"},
		{"raccoon", "Raccoon", ""},
		{"traecn", "Trae CN", "cn"},
		{"traeglobal", "Trae Global", "global"},
		{"workbuddycn", "WorkBuddy CN", "cn"},
		{"workbuddyglobal", "WorkBuddy Global", "global"},
		{"zcode", "ZCode", "global"},
	}
	vendors := withoutOpenAICompat(testApp.Registry().Vendors())
	if len(vendors) != len(want) {
		t.Fatalf("vendor count = %d, want %d", len(vendors), len(want))
	}
	for index, expected := range want {
		vendor := vendors[index]
		if vendor.ID() != expected.id || vendor.Name() != expected.name || vendor.Region() != expected.region {
			t.Fatalf("vendor[%d] = %q/%q/%q, want %q/%q/%q", index,
				vendor.ID(), vendor.Name(), vendor.Region(), expected.id, expected.name, expected.region)
		}
		if vendor.ModelPrefix() != "" {
			t.Fatalf("vendor %s model prefix = %q, want empty", vendor.ID(), vendor.ModelPrefix())
		}
	}
}

func TestVendorCapabilities(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	type want struct {
		login     bool
		refresh   bool
		quota     bool
		models    bool // 自定义 ModelsForAuth（否则回落 StaticModels）
		merge     bool
		configure bool
		console   bool
		credits   bool
		authMode  string
	}
	cases := map[string]want{
		"autoclawcn":        {credits: true, refresh: true, quota: true, models: true, merge: true, configure: true, authMode: "oauth"},
		"autoclawglobal":    {credits: true, login: true, refresh: true, quota: true, models: true, merge: true, configure: true, authMode: "oauth"},
		"cline":             {credits: true, login: true, refresh: true, merge: true, configure: true, authMode: "oauth"},
		"codearts":          {credits: true, login: true, refresh: true, quota: true, models: true, merge: true},
		"minimaxcodecn":     {login: true, refresh: true, quota: true, models: true, merge: true, configure: true},
		"minimaxcodeglobal": {login: true, refresh: true, quota: true, models: true, merge: true, configure: true},
		"opencodezen":       {credits: true, refresh: true, models: true, merge: true, configure: true, authMode: "apikey"},
		"qodercn":           {credits: true, login: true, refresh: true, quota: true, models: true, merge: true, configure: true},
		"qoderglobal":       {credits: true, login: true, refresh: true, quota: true, models: true, merge: true, configure: true},
		"raccoon":           {credits: true, login: true, refresh: true, quota: true, models: true, merge: true, configure: true},
		"traecn":            {credits: true, login: true, refresh: true, quota: true, merge: true, configure: true},
		"traeglobal":        {credits: true, login: true, refresh: true, quota: true, merge: true, configure: true},
		"workbuddycn":       {credits: true, login: true, refresh: true, quota: true, models: true, merge: true, configure: true, console: true},
		"workbuddyglobal":   {credits: true, login: true, refresh: true, quota: true, models: true, merge: true, configure: true, console: true},
		"zcode":             {login: true, refresh: true, quota: true, merge: true, configure: true, authMode: "oauth"},
	}
	for _, vendor := range withoutOpenAICompat(testApp.Registry().Vendors()) {
		expected, okCase := cases[vendor.ID()]
		if !okCase {
			t.Fatalf("unexpected vendor %q in registry", vendor.ID())
		}
		_, hasLogin := core.As[core.LoginProvider](vendor)
		_, hasRefresh := core.As[core.Refresher](vendor)
		_, hasQuota := core.As[core.QuotaProvider](vendor)
		_, hasModels := core.As[core.ModelsProvider](vendor)
		_, hasMerge := core.As[core.StorageMerger](vendor)
		_, hasConfigure := core.As[core.Configurable](vendor)
		_, hasConsole := core.As[core.ConsoleModelProvider](vendor)
		_, hasCredits := core.As[core.CreditsProvider](vendor)
		authMode := ""
		if reporter, okReporter := core.As[core.AuthModeReporter](vendor); okReporter {
			authMode = reporter.AuthMode()
		}
		got := want{
			login: hasLogin, refresh: hasRefresh, quota: hasQuota,
			models: hasModels, merge: hasMerge, configure: hasConfigure,
			console: hasConsole, credits: hasCredits, authMode: authMode,
		}
		if got != expected {
			t.Fatalf("vendor %s capabilities = %+v, want %+v", vendor.ID(), got, expected)
		}
	}
}
