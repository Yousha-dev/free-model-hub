package qoder

import (
	"testing"

	"free-model-hub/internal/core"
)

func TestCanonicalModelID(t *testing.T) {
	cases := []struct {
		displayName string
		key         string
		want        string
	}{
		{"DeepSeek-V4-Pro", "dmodel", "deepseek-v4-pro"},
		{"DeepSeek-Flash", "dfmodel", "deepseek-flash"},
		{"Qwen3.8-Max", "qmodel_38max", "qwen3.8-max"},
		{"GLM-5.3-Flash", "gfmodel", "glm-5.3-flash"},
		{"Kimi-K2.8-Preview", "kmodel", "kimi-k2.8-preview"},
		{"DeepSeek V4.1  Flash", "dfmodel", "deepseek-v4.1-flash"},
		{" Sonus ", "smodel", "sonus"},
		{"Auto", "auto", "auto"},
		{"Performance", "performance", "performance"},
		// Fall back to the upstream key when display_name is missing, ensuring an available ID can always be obtained.
		{"", "qfmodel", "qfmodel"},
		{"   ", "QModel_38Max", "qmodel_38max"},
	}
	for _, c := range cases {
		if got := canonicalModelID(c.displayName, c.key); got != c.want {
			t.Errorf("canonicalModelID(%q, %q) = %q; want %q", c.displayName, c.key, got, c.want)
		}
	}
}

// testModels simulates a model/list response from upstream (in production, this only comes from upstream).
func testModels() []Model {
	return []Model{
		{Key: "auto", DisplayName: "Auto"},
		{Key: "performance", DisplayName: "Performance"},
		{Key: "qmodel_38max", DisplayName: "Qwen3.8-Max"},
		{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max"},
		{Key: "kmodel_latest", DisplayName: "Kimi-K3"},
		{Key: "kmodel", DisplayName: "Kimi-K2.8-Preview"},
		{Key: "gmodel", DisplayName: "GLM-5.3"},
		{Key: "free", DisplayName: "Free-Model", PriceFactor: float64Ptr(0)},
		{Key: "gfmodel", DisplayName: "GLM-5.3-Flash"},
		{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro"},
		{Key: "dfmodel", DisplayName: "DeepSeek-Flash"},
		{Key: "mmodel", DisplayName: "MiniMax-M3"},
	}
}

func TestModelIndexReplaceAndLookup(t *testing.T) {
	var index modelIndex
	models := testModels()
	slugByKey := index.replace(models)

	if got := slugByKey["dmodel"]; got != "deepseek-v4-pro" {
		t.Fatalf("slug for dmodel = %q; want deepseek-v4-pro", got)
	}
	if len(slugByKey) != len(models) {
		t.Fatalf("slug count = %d; want %d", len(slugByKey), len(models))
	}

	cases := []struct{ name, want string }{
		// External slug
		{"deepseek-v4-pro", "dmodel"},
		{"DeepSeek-V4-Pro", "dmodel"},
		// Old upstream key remains available (for legacy clients/scripts)
		{"dmodel", "dmodel"},
		{"DModel", "dmodel"},
		{"qmodel_38max", "qmodel_38max"},
		// System tier slug has the same name as the key
		{"auto", "auto"},
		{"Performance", "performance"},
	}
	for _, c := range cases {
		got, ok := index.UpstreamKey(c.name)
		if !ok || got != c.want {
			t.Errorf("UpstreamKey(%q) = (%q, %v); want (%q, true)", c.name, got, ok, c.want)
		}
	}
	if _, ok := index.UpstreamKey("unknown-model"); ok {
		t.Error("unknown model must not resolve")
	}
	if _, ok := index.UpstreamKey("  "); ok {
		t.Error("blank name must not resolve")
	}
}

func TestModelIndexReplaceDropsStaleEntries(t *testing.T) {
	var index modelIndex
	index.replace([]Model{{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro"}})
	if _, ok := index.UpstreamKey("deepseek-v4-pro"); !ok {
		t.Fatal("first batch must be indexed")
	}

	// After upstream replaces a model, the old slug should no longer match.
	index.replace([]Model{{Key: "dfmodel", DisplayName: "DeepSeek-Flash"}})
	if _, ok := index.UpstreamKey("deepseek-v4-pro"); ok {
		t.Error("stale slug must be dropped after replace")
	}
	if key, ok := index.UpstreamKey("deepseek-flash"); !ok || key != "dfmodel" {
		t.Errorf("UpstreamKey(deepseek-flash) = (%q, %v); want (dfmodel, true)", key, ok)
	}
}

func TestModelIndexDuplicateSlugKeepsFirst(t *testing.T) {
	var index modelIndex
	slugByKey := index.replace([]Model{
		{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro"},
		{Key: "dfmodel", DisplayName: "DeepSeek-V4-Pro"},
	})
	if _, dup := slugByKey["dfmodel"]; dup {
		t.Error("duplicate display_name must not be registered a second time")
	}
	if key, _ := index.UpstreamKey("deepseek-v4-pro"); key != "dmodel" {
		t.Errorf("duplicate slug resolves to %q; want dmodel", key)
	}
}

func TestModelIndexSlugBeatsRawKey(t *testing.T) {
	var index modelIndex
	// Construct an anomaly: a model's normalized display_name happens to equal another model's upstream key.
	// The first declared in the list is key="alpha" / display="Alpha", so alpha belongs to it.
	index.replace([]Model{
		{Key: "alpha", DisplayName: "Alpha"},
		{Key: "beta", DisplayName: "alpha"},
	})
	if key, _ := index.UpstreamKey("alpha"); key != "alpha" {
		t.Errorf("UpstreamKey(alpha) = %q; want first declared entry (alpha)", key)
	}
}

// CN and Global versions are two adapter instances, and their indexes do not interfere with each other.
func TestModelIndexIsolatedPerInstance(t *testing.T) {
	var cn, global modelIndex
	cn.replace([]Model{{Key: "mmodel", DisplayName: "MiniMax-M2.7"}})
	global.replace([]Model{{Key: "mmodel", DisplayName: "MiniMax-M3"}})

	if key, _ := cn.UpstreamKey("minimax-m2.7"); key != "mmodel" {
		t.Errorf("cn minimax-m2.7 = %q; want mmodel", key)
	}
	if _, ok := cn.UpstreamKey("minimax-m3"); ok {
		t.Error("cn index must not contain the global model")
	}
	if key, _ := global.UpstreamKey("minimax-m3"); key != "mmodel" {
		t.Errorf("global minimax-m3 = %q; want mmodel", key)
	}
	if _, ok := global.UpstreamKey("minimax-m2.7"); ok {
		t.Error("global index must not contain the cn model")
	}
}

func TestModelsToPluginAPIUsesSlug(t *testing.T) {
	instance := newInstance(core.Env{}, RegionCN)
	adapterInstance, okAdapter := instance.(*adapter)
	if !okAdapter {
		t.Fatalf("newInstance returned %T; want *adapter", instance)
	}

	models := adapterInstance.modelsToPluginAPI(testModels())
	byID := make(map[string]bool, len(models))
	for _, model := range models {
		byID[model.ID] = true
		// The display name remains the upstream display_name (the name seen in the console/client remains unchanged).
		if model.ID == "deepseek-v4-pro" && model.DisplayName != "DeepSeek-V4-Pro" {
			t.Errorf("deepseek-v4-pro DisplayName = %q; want DeepSeek-V4-Pro", model.DisplayName)
		}
	}
	for _, want := range []string{"deepseek-v4-pro", "qwen3.8-max", "glm-5.3", "kimi-k3", "auto"} {
		if !byID[want] {
			t.Errorf("model id %q missing from registration", want)
		}
	}
	for _, unwanted := range []string{"dmodel", "qmodel_38max", "gmodel"} {
		if byID[unwanted] {
			t.Errorf("raw upstream key %q must not be registered as a model id", unwanted)
		}
	}

	// Indexes rebuilt upon registration can be used for outbound resolution.
	if key, _ := adapterInstance.models.UpstreamKey("deepseek-v4-pro"); key != "dmodel" {
		t.Errorf("index after registration = %q; want dmodel", key)
	}
}

// TestModelCreditsFromPriceFactor locks the console's "cost multiplier" data source.
//
// The multiplier only appears in the price_factor of the upstream model/list; the host contract does not have this field,
// so it must be recorded when converting the model, otherwise the console would have to parse the description text.
func TestModelCreditsFromPriceFactor(t *testing.T) {
	instance := newInstance(core.Env{}, RegionCN).(*adapter)
	models := []Model{
		{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro", PriceFactor: float64Ptr(0.42)},
		{Key: "gmodel", DisplayName: "GLM-5.3"},
		{Key: "free", DisplayName: "Free-Model", PriceFactor: float64Ptr(0)},
	}
	instance.modelsToPluginAPI(models)

	credits := instance.ModelCredits()
	if got := credits["deepseek-v4-pro"]; got != "0.42X" {
		t.Errorf("deepseek-v4-pro credits = %q; want 0.42X", got)
	}
	// Explicit 0 is a free-tier limit, keep it as 0.00X.
	if got := credits["free-model"]; got != "0.00X" {
		t.Errorf("显式 0 倍率 = %q; want 0.00X", got)
	}
	// Missing price_factor means upstream did not provide a multiplier, so it cannot be displayed as x0.00.
	if _, ok := credits["glm-5.3"]; ok {
		t.Errorf("model without price_factor must have no credits entry, got %q", credits["glm-5.3"])
	}
	// The key must be the external slug, not the upstream key (the console matches by registration ID).
	if _, ok := credits["dmodel"]; ok {
		t.Error("credits must be keyed by the registered slug, not the upstream key")
	}
}

// float64Ptr makes it easy to construct a model in tests where "upstream explicitly provided a multiplier".
func float64Ptr(value float64) *float64 { return &value }
