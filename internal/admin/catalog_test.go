package admin

import (
	"context"
	"slices"
	"testing"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/state"
)

// fakeVendor is the minimal registerable provider: the model layer only uses identity and region.
type fakeVendor struct {
	core.Meta
}

func (v *fakeVendor) Match(string, string, map[string]any) bool { return false }
func (v *fakeVendor) Parse([]byte, string) (*core.Credential, error) {
	return &core.Credential{}, nil
}
func (v *fakeVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) { return nil, nil }
func (v *fakeVendor) Execute(context.Context, *core.Credential, *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *fakeVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	return nil
}

// ratedVendor adds a billing multiplier on top of fakeVendor, used to verify that multipliers persist with the manifest.
type ratedVendor struct {
	fakeVendor
	credits map[string]string
}

// ModelCredits completes normalization at this layer just like real providers: admin no longer reformats, otherwise
// legal multipliers like "0" would be repeatedly rewritten.
func (v *ratedVendor) ModelCredits() map[string]string {
	out := make(map[string]string, len(v.credits))
	for id, raw := range v.credits {
		if rate := core.FormatRate(raw); rate != "" {
			out[id] = rate
		}
	}
	return out
}

func newTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry()
	for _, vendor := range []core.Vendor{
		&fakeVendor{Meta: core.Meta{VendorID: "workbuddycn", VendorName: "WB CN", VendorRegion: "cn"}},
		&fakeVendor{Meta: core.Meta{VendorID: "workbuddyglobal", VendorName: "WB Global", VendorRegion: "global"}},
		&fakeVendor{Meta: core.Meta{VendorID: "codearts", VendorName: "CodeArts", VendorRegion: "cn"}},
	} {
		if err := registry.Register(vendor); err != nil {
			t.Fatal(err)
		}
	}
	return NewCatalog(store, registry)
}

func TestExpandKeysByVendorAndRegion(t *testing.T) {
	manager := newTestCatalog(t)
	if got := manager.ExpandKeys("glm-5.2"); len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("bare key = %#v", got)
	}
	if got := manager.ExpandKeys("workbuddycn:glm-5.2"); len(got) != 1 || got[0] != "workbuddycn:glm-5.2" {
		t.Fatalf("vendor key = %#v", got)
	}
	keys := manager.ExpandKeys("cn:glm-5.2")
	if len(keys) == 0 {
		t.Fatal("region alias must expand to registered cn vendors")
	}
	for _, key := range keys {
		if key == "workbuddyglobal:glm-5.2" {
			t.Fatalf("cn alias leaked global key: %#v", keys)
		}
	}
}

func TestAliasUniquenessAndResolve(t *testing.T) {
	c := newTestCatalog(t)
	c.SetAlias("workbuddycn", "glm-5.2", "glm-max")
	if got := c.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.2" {
		t.Fatalf("resolve = %q", got)
	}
	c.SetAlias("workbuddycn", "glm-5.3", "glm-max")
	if got := c.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.3" {
		t.Fatalf("rebind resolve = %q", got)
	}
	if c.AliasFor("workbuddycn", "glm-5.2") != "" {
		t.Fatal("old alias should be replaced")
	}
}

// TestResolveOutboundAliasRoundTrip verifies that forward setting and reverse restoration of aliases are symmetric.
func TestResolveOutboundAliasRoundTrip(t *testing.T) {
	c := newTestCatalog(t)
	c.SetAlias("qodercn", "glm-5.3", "qoder-glm")
	if got := c.ResolveOutboundModel("qodercn", "qoder-glm"); got != "glm-5.3" {
		t.Fatalf("alias resolve = %q, want glm-5.3", got)
	}
	// Official IDs without set aliases are returned as-is.
	if got := c.ResolveOutboundModel("qodercn", "deepseek-flash"); got != "deepseek-flash" {
		t.Fatalf("passthrough = %q, want deepseek-flash", got)
	}
}

// TestStaleModelStateIsIgnoredNotDeleted covers stale key semantics after upgrades:
//
// A provider changed its external model ID (Qoder changed from an upstream key to a normalized slug), but the user state still retains
// the old key alias and old key disabled items. They must become invalid for the current model (otherwise requests are mapped back to the old ID and then hit
// the disabled check), but **must not be deleted**—the manifest might temporarily fail to load due to probing failures, and deletion would cause
// user configurations to be permanently lost.
func TestStaleModelStateIsIgnoredNotDeleted(t *testing.T) {
	c := newTestCatalog(t)
	c.SetAlias("qodercn", "dfmodel", "deepseek-flash")
	c.SetAlias("qodercn", "glm-5.3", "qoder-glm")
	c.SetDisabled([]string{"qodercn:dfmodel", "qodercn:glm-5.3"}, true)
	c.CacheStatic("qodercn", []pluginapi.ModelInfo{{ID: "deepseek-flash"}, {ID: "glm-5.3"}})

	// Old IDs are no longer registered: neither aliases nor disabled states affect requests.
	if got := c.ResolveOutboundModel("qodercn", "deepseek-flash"); got != "deepseek-flash" {
		t.Fatalf("stale alias must be ignored, resolve = %q", got)
	}
	if c.IsDisabled("qodercn", "deepseek-flash") {
		t.Fatal("stale disabled key must not disable the current model id")
	}

	// Still registered official IDs are unaffected.
	if got := c.ResolveOutboundModel("qodercn", "qoder-glm"); got != "glm-5.3" {
		t.Fatalf("live alias resolve = %q, want glm-5.3", got)
	}
	if !c.IsDisabled("qodercn", "glm-5.3") {
		t.Fatal("live disabled key must still apply")
	}

	// State is retained as-is: old configurations can automatically take effect again once the manifest recovers.
	if c.AliasFor("qodercn", "dfmodel") != "deepseek-flash" {
		t.Fatal("stale alias must be kept in state, only ignored at resolution")
	}
	for _, key := range c.store.Snapshot().DisabledModels {
		if key == "qodercn:dfmodel" {
			return
		}
	}
	t.Fatal("stale disabled key must be kept in state, only ignored at lookup")
}

// TestFallbackModelListDoesNotUnlockDisabledModels covers the risk of "temporarily incomplete manifests":
//
// An upstream manifest fetch might only return some models (due to probe failure, downgrade, or upstream jitter). Under no circumstances
// should this unlock user-disabled models or discard disabled configurations—a single instance of jitter must not change authentication semantics.
func TestFallbackModelListDoesNotUnlockDisabledModels(t *testing.T) {
	c := newTestCatalog(t)
	c.SetDisabled([]string{"qodercn:kimi-k3"}, true)

	// kimi-k3 is absent from the fallback/incomplete manifest (it cannot be determined to have disappeared based on this).
	c.CacheStatic("qodercn", []pluginapi.ModelInfo{{ID: "deepseek-flash"}})
	if c.IsDisabled("qodercn", "kimi-k3") {
		t.Fatal("a model missing from a partial list must not be treated as registered")
	}

	// Manifest is empty (all probes failed): being extra conservative, the disabled state remains in effect and is not allowed through.
	c.CacheStatic("qodercn", nil)
	if !c.IsDisabled("qodercn", "kimi-k3") {
		t.Fatal("with no model list at all, disabled models must stay disabled")
	}
}

func TestDisabledUnionConfigAndState(t *testing.T) {
	c := newTestCatalog(t)
	c.Configure(ModelSettings{EnabledRealms: []string{"cn", "global"}, DisabledModels: []string{"cn:glm-5.2"}})
	if !c.IsDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config disable must apply")
	}
	if c.IsDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("config disable must be region scoped")
	}
	c.SetDisabled([]string{"workbuddyglobal:glm-5.2"}, true)
	if !c.IsDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("state disable must apply")
	}
	// Page enabling must not bypass disabled items declared in the configuration.
	c.SetDisabled([]string{"cn:glm-5.2"}, false)
	if !c.IsDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config-declared disable must survive page enable")
	}
}

func TestMergeSameNameModels(t *testing.T) {
	merged := MergeSameNameModels([]pluginapi.ModelInfo{
		{ID: "glm-5.2", ContextLength: 1000, SupportedInputModalities: []string{"text"}},
		{ID: "GLM-5.2", ContextLength: 2000, SupportedInputModalities: []string{"image"}},
	})
	if len(merged) != 1 {
		t.Fatalf("merged len = %d", len(merged))
	}
	if merged[0].ContextLength != 2000 {
		t.Fatalf("context = %d", merged[0].ContextLength)
	}
	if len(merged[0].SupportedInputModalities) != 2 {
		t.Fatalf("modalities = %#v", merged[0].SupportedInputModalities)
	}
}

// TestMergeSameNameModelsUnionsToolParameters locks down both directions of capability unioning.
//
// Regression: previously only SupportedGenerationMethods was unioned while SupportedParameters was not,
// merging [no-tool entry, tool-supported entry] would result in a contradictory combination of "methods has tool_calls, params lacks
// tools", causing the client to hide the tool switch accordingly, which actually stripped capabilities from supported models.
func TestMergeSameNameModelsUnionsToolParameters(t *testing.T) {
	toolModel := pluginapi.ModelInfo{
		ID:                         "glm-5.2",
		SupportedGenerationMethods: []string{"tool_calls"},
		SupportedParameters:        []string{"temperature", "tools", "tool_choice"},
	}
	plainModel := pluginapi.ModelInfo{
		ID:                  "GLM-5.2",
		SupportedParameters: []string{"temperature", "top_p"},
	}

	for name, models := range map[string][]pluginapi.ModelInfo{
		"先无工具有工具":  {plainModel, toolModel},
		"先有工具后无工具": {toolModel, plainModel},
	} {
		merged := MergeSameNameModels(models)
		if len(merged) != 1 {
			t.Fatalf("%s: merged len = %d", name, len(merged))
		}
		if !slices.Contains(merged[0].SupportedGenerationMethods, "tool_calls") {
			t.Fatalf("%s: methods = %#v", name, merged[0].SupportedGenerationMethods)
		}
		for _, param := range []string{"tools", "tool_choice"} {
			if !slices.Contains(merged[0].SupportedParameters, param) {
				t.Fatalf("%s: params = %#v, missing %q", name, merged[0].SupportedParameters, param)
			}
		}
	}
}

// TestLoadCachedStateRestoresCapabilities locks down reboot rehydration without losing capabilities.
//
// state.json only stores SupportsImages / SupportsTool, rehydration must go through registration conversion
// to re-derive modalities / methods / parameters, otherwise after a reboot and before the next successful probe round,
// the console would display vision models and tool models as "not supported".
func TestLoadCachedStateRestoresCapabilities(t *testing.T) {
	manager := newTestCatalog(t)
	manager.LoadCachedState([]core.CachedModel{
		{VendorID: "codearts", ID: "Qwen3-VL-235B", ContextLength: 131072, MaxOutputTokens: 8192, SupportsImages: true, SupportsTool: true},
	})

	models := manager.Registered("codearts")
	if len(models) != 1 {
		t.Fatalf("cached models = %d", len(models))
	}
	info := models[0]
	if !slices.Contains(info.SupportedInputModalities, "image") {
		t.Fatalf("modalities = %#v, missing image", info.SupportedInputModalities)
	}
	if !slices.Contains(info.SupportedGenerationMethods, "tool_calls") {
		t.Fatalf("methods = %#v, missing tool_calls", info.SupportedGenerationMethods)
	}
	for _, param := range []string{"tools", "tool_choice"} {
		if !slices.Contains(info.SupportedParameters, param) {
			t.Fatalf("params = %#v, missing %q", info.SupportedParameters, param)
		}
	}
}

// TestLoadCachedStateDropsRemovedVendor locks down "offline providers do not resurrect".
//
// Cache is persisted by vendor_id, and once a provider is removed (e.g. TRAE SOLO merged into the CN version) these entries
// are never read again; if rehydrated anyway, CachedState would write them back to state.json round after round,
// and a batch of non-existent models would persistently linger in the console and disabled manifest.
func TestLoadCachedStateDropsRemovedVendor(t *testing.T) {
	manager := newTestCatalog(t)
	manager.LoadCachedState([]core.CachedModel{
		{VendorID: "traesolo", ID: "glm-5.2"},
		{VendorID: "workbuddycn", ID: "glm-5.3"},
	})

	if got := manager.Registered("traesolo"); len(got) != 0 {
		t.Fatalf("removed vendor restored %d model(s)", len(got))
	}
	if got := manager.Registered("workbuddycn"); len(got) != 1 {
		t.Fatalf("live vendor cached models = %d, want 1", len(got))
	}
	for _, item := range manager.CachedState() {
		if item.VendorID == "traesolo" {
			t.Fatal("removed vendor must not be written back to state.json")
		}
	}
}

// TestCacheProbedModelsKeepsPreviousOnEmpty locks down "probe failure does not clear cache".
//
// Real incident: OpenCode ZEN's model.for_auth returns an empty manifest per contract upon probe failure
// (letting the host weed out bad credentials); if callers write to cache regardless, a single transient 401 would cause the entire provider
// to disappear from the console. Probed writes must go through CacheProbedModels.
func TestCacheProbedModelsKeepsPreviousOnEmpty(t *testing.T) {
	manager := newTestCatalog(t)
	manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.2"}, {ID: "glm-5.3"}})

	if wrote := manager.CacheProbedModels("workbuddycn", nil); wrote {
		t.Fatal("empty probe result must not be written")
	}
	if got := len(manager.Registered("workbuddycn")); got != 2 {
		t.Fatalf("cached models = %d, want the previous 2 kept", got)
	}

	// Non-empty results overwrite as usual.
	if wrote := manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.4"}}); !wrote {
		t.Fatal("non-empty probe result must be written")
	}
	models := manager.Registered("workbuddycn")
	if len(models) != 1 || models[0].ID != "glm-5.4" {
		t.Fatalf("cache = %+v, want glm-5.4", models)
	}
}

// TestCacheProbedModelsPersistsOnlyOnChange verifies that the manifest is persisted in state.json,
// and is not repeatedly written when contents remain unchanged (the host calls model.for_auth for every credential).
func TestCacheProbedModelsPersistsOnlyOnChange(t *testing.T) {
	manager := newTestCatalog(t)
	manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.2"}})

	snapshot := manager.store.Snapshot()
	if len(snapshot.Models) != 1 || snapshot.Models[0].ID != "glm-5.2" {
		t.Fatalf("state models = %+v, want glm-5.2 persisted", snapshot.Models)
	}

	// Sentinel time: it should not be persisted again if the content has not changed, it must remain untouched.
	manager.store.Mutate(func(s *state.State) { s.ModelsFetchedAt = "sentinel" })
	manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.2"}})
	if got := manager.store.Snapshot().ModelsFetchedAt; got != "sentinel" {
		t.Fatalf("identical list must not be re-persisted (fetched_at = %q)", got)
	}

	manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.3"}})
	updated := manager.store.Snapshot()
	if len(updated.Models) != 1 || updated.Models[0].ID != "glm-5.3" {
		t.Fatalf("state models = %+v, want glm-5.3 after a changed probe", updated.Models)
	}
	if updated.ModelsFetchedAt == "sentinel" {
		t.Fatal("a changed list must be persisted")
	}
}

// TestCreditsFollowProbeAndSurviveRestart locks two invariants of the "cost multiplier":
//
//  1. The multiplier and the model list are the product of the same probe and must be persisted together;
//  2. After a plugin restart (new Catalog + LoadCachedState), the multiplier is immediately available,
//     without waiting for a round of upstream probing—otherwise, the console will show a row of "—" after startup.
func TestCreditsFollowProbeAndSurviveRestart(t *testing.T) {
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, errLoad := store.Load(); errLoad != nil {
		t.Fatal(errLoad)
	}
	registry := core.NewRegistry()
	if errRegister := registry.Register(&ratedVendor{
		fakeVendor: fakeVendor{Meta: core.Meta{VendorID: "qodercn", VendorName: "Qoder CN", VendorRegion: "cn"}},
		credits:    map[string]string{"glm-5.3": "x0.42"},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	manager := NewCatalog(store, registry)
	manager.CacheProbedModels("qodercn", []pluginapi.ModelInfo{{ID: "glm-5.3"}})

	snapshot := store.Snapshot()
	if len(snapshot.Models) != 1 || snapshot.Models[0].Credits != "0.42X" {
		t.Fatalf("state models = %+v, want credits persisted with the model", snapshot.Models)
	}

	restored := NewCatalog(store, registry)
	restored.LoadCachedState(snapshot.Models)
	if got := restored.CreditsFor("qodercn", "glm-5.3"); got != "0.42X" {
		t.Fatalf("credits after restart = %q, want 0.42X", got)
	}
	// Unknown models cannot fabricate multipliers out of thin air.
	if got := restored.CreditsFor("qodercn", "unknown-model"); got != "" {
		t.Fatalf("unknown model credits = %q, want empty", got)
	}
}

// TestCreditsChangeTriggersPersist verifies that multiplier changes themselves also trigger persistence.
//
// When the model list remains unchanged and only the multiplier changes (upstream price adjustment), if persistProbed only compares IDs/names,
// it would silently drop this update.
func TestCreditsChangeTriggersPersist(t *testing.T) {
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, errLoad := store.Load(); errLoad != nil {
		t.Fatal(errLoad)
	}
	vendor := &ratedVendor{
		fakeVendor: fakeVendor{Meta: core.Meta{VendorID: "qodercn", VendorName: "Qoder CN", VendorRegion: "cn"}},
		credits:    map[string]string{"glm-5.3": "x0.42"},
	}
	registry := core.NewRegistry()
	if errRegister := registry.Register(vendor); errRegister != nil {
		t.Fatal(errRegister)
	}
	manager := NewCatalog(store, registry)
	manager.CacheProbedModels("qodercn", []pluginapi.ModelInfo{{ID: "glm-5.3"}})

	vendor.credits = map[string]string{"glm-5.3": "x0.80"}
	manager.CacheProbedModels("qodercn", []pluginapi.ModelInfo{{ID: "glm-5.3"}})
	if got := store.Snapshot().Models[0].Credits; got != "0.80X" {
		t.Fatalf("persisted credits = %q, want 0.80X after repricing", got)
	}
}
