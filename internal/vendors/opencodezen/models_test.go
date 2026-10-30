package opencodezen

import (
	"testing"
)

// sampleModelsDevCatalog is the minimal reduction of the models.dev response (provider "opencode").
const sampleModelsDevCatalog = `{
  "opencode": {
    "models": {
      "glm-5.3-flash-free": {
        "name": "GLM-5.3-Flash Free",
        "description": "Native multimodal GLM model for efficient coding",
        "reasoning": true,
        "tool_call": true,
        "reasoning_options": [
          {"type": "effort", "values": ["low", "high", "max"]},
          {"type": "verbosity", "values": ["brief"]}
        ],
        "modalities": {"input": ["text", "image"], "output": ["text"]},
        "limit": {"context": 200000, "output": 32000}
      },
      "qwen3-coder": {
        "name": "Qwen3 Coder",
        "reasoning": false,
        "tool_call": true,
        "modalities": {"input": ["text"], "output": ["text"]},
        "limit": {"context": 262144, "output": 65536}
      }
    }
  },
  "zenmux": {
    "models": {"unrelated": {"name": "Unrelated", "limit": {"context": 1}}}
  }
}`

func TestParseModelsDevCatalog(t *testing.T) {
	catalog, errParse := parseModelsDevCatalog([]byte(sampleModelsDevCatalog))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	entry, okEntry := catalog["glm-5.3-flash-free"]
	if !okEntry {
		t.Fatalf("zen provider entry missing")
	}
	if entry.Limit.Context != 200000 || entry.Limit.Output != 32000 {
		t.Fatalf("limit = %+v", entry.Limit)
	}
	if len(catalog) != 2 {
		t.Fatalf("catalog should only keep the zen provider, got %d entries", len(catalog))
	}
}

func TestParseModelsDevCatalogMissingProvider(t *testing.T) {
	catalog, errParse := parseModelsDevCatalog([]byte(`{"zenmux": {"models": {}}}`))
	if errParse != nil {
		t.Fatalf("parse should tolerate missing provider: %v", errParse)
	}
	if len(catalog) != 0 {
		t.Fatalf("expected empty catalog, got %d", len(catalog))
	}
}

func TestApplyModelsDevCatalog(t *testing.T) {
	catalog, errParse := parseModelsDevCatalog([]byte(sampleModelsDevCatalog))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	models := []Model{
		// Empty fields are all filled in by the catalog.
		{ID: "glm-5.3-flash-free", Name: "glm-5.3-flash-free"},
		// Metadata already provided by upstream takes precedence; the catalog does not overwrite it.
		{ID: "qwen3-coder", Name: "Qwen3 Coder", ContextWindow: 111, SupportsTools: false},
		// Models not in the catalog remain as they are.
		{ID: "unknown-free", Name: "unknown-free"},
	}
	applyModelsDevCatalog(models, catalog)

	glm := models[0]
	if glm.ContextWindow != 200000 || glm.MaxOutputTokens != 32000 {
		t.Fatalf("glm limits = %d/%d", glm.ContextWindow, glm.MaxOutputTokens)
	}
	if !glm.IsReasoning || !glm.SupportsTools || !glm.SupportsImages {
		t.Fatalf("glm capabilities wrong: reasoning=%v tools=%v images=%v", glm.IsReasoning, glm.SupportsTools, glm.SupportsImages)
	}
	if got, want := glm.Name, "GLM-5.3-Flash Free"; got != want {
		t.Fatalf("glm name = %q, want %q", got, want)
	}
	if glm.Description == "" {
		t.Fatalf("glm description should be filled from catalog")
	}
	wantEfforts := []string{"low", "high", "max"}
	if len(glm.Efforts) != len(wantEfforts) {
		t.Fatalf("glm efforts = %v, want %v", glm.Efforts, wantEfforts)
	}
	for i, want := range wantEfforts {
		if glm.Efforts[i] != want {
			t.Fatalf("glm efforts = %v, want %v", glm.Efforts, wantEfforts)
		}
	}

	qwen := models[1]
	if qwen.ContextWindow != 111 {
		t.Fatalf("qwen context = %d, upstream value should win", qwen.ContextWindow)
	}
	if qwen.IsReasoning {
		t.Fatalf("non-reasoning model must stay non-reasoning")
	}

	unknown := models[2]
	if unknown.ContextWindow != 0 || unknown.MaxOutputTokens != 0 || len(unknown.Efforts) != 0 {
		t.Fatalf("unknown model should stay untouched: %+v", unknown)
	}
}

func TestEffortsFromModelsDevIgnoresNonEffortOptions(t *testing.T) {
	catalog, errParse := parseModelsDevCatalog([]byte(sampleModelsDevCatalog))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	efforts := effortsFromModelsDev(catalog["glm-5.3-flash-free"])
	if len(efforts) != 3 || efforts[0] != "low" {
		t.Fatalf("efforts = %v, verbosity option must be ignored", efforts)
	}
}

// TestApplyModelsDevCatalogMarksFreeOnlyOnZeroPrice locks down the determination source for zero multipliers.
//
// The cost in models.dev is the USD unit price, not a multiplier; only when both input and output unit prices are 0 is it
// a zero-multiplier signal. Entries without a cost block cannot be treated as zero-multiplier—that is just missing data in the catalog.
func TestApplyModelsDevCatalogMarksFreeOnlyOnZeroPrice(t *testing.T) {
	models := []Model{{ID: "free-model"}, {ID: "paid-model"}, {ID: "unknown-model"}}
	catalog := modelsDevCatalog{
		"free-model":    {Cost: &modelsDevCost{}},
		"paid-model":    {Cost: &modelsDevCost{Input: 1.5, Output: 2}},
		"unknown-model": {},
	}
	applyModelsDevCatalog(models, catalog)

	if models[0].Credits != "0.00X" {
		t.Fatalf("Zero unit price should be recorded as 0.00X, got %q", models[0].Credits)
	}
	if models[1].Credits != "" {
		t.Fatalf("Priced models cannot be disguised as multipliers, got %q", models[1].Credits)
	}
	if models[2].Credits != "" {
		t.Fatalf("Missing cost block cannot be treated as free, got %q", models[2].Credits)
	}
}

// TestParseModelsKeepsNonFreeNamedModels locks down "the manifest does not filter by name".
//
// In the official catalog, big-pickle is a zero-price model (or the default model when logged out), but lacks
// the "-free" suffix. Filtering by name would wipe the official default model from the manifest.
func TestParseModelsKeepsNonFreeNamedModels(t *testing.T) {
	body := []byte(`{"object":"list","data":[
		{"id":"big-pickle","object":"model"},
		{"id":"glm-5.3","object":"model"},
		{"id":"deepseek-v4.1-flash-free","object":"model"}
	]}`)
	models, errParse := parseModels(body)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	got := make(map[string]bool, len(models))
	for _, model := range models {
		got[model.ID] = true
	}
	for _, want := range []string{"big-pickle", "glm-5.3", "deepseek-v4.1-flash-free"} {
		if !got[want] {
			t.Fatalf("Model %q was filtered out; the list should retain all models returned by upstream", want)
		}
	}
}

// TestFreeOnlyKeepsZeroPriceModels locks down that the manifest only displays the free tier.
//
// Upstream /v1/models returns dozens of paid models and debug test models at the same time; this provider
// only integrates the free quota, and paid models will inevitably return 402 in the plugin. Determination uses the catalog unit price rather than the name,
// so big-pickle (zero price without the free suffix) must be kept, and test models not in the catalog must be weeded out.
func TestFreeOnlyKeepsZeroPriceModels(t *testing.T) {
	catalog := modelsDevCatalog{
		"big-pickle":               {Cost: &modelsDevCost{}},
		"deepseek-v4.1-flash-free": {Cost: &modelsDevCost{}},
		"glm-5.3":                  {Cost: &modelsDevCost{Input: 1.5, Output: 6}},
	}
	models := []Model{
		{ID: "big-pickle"},
		{ID: "deepseek-v4.1-flash-free"},
		{ID: "glm-5.3"},
		{ID: "test"}, // 上游调试模型，目录里没有
	}
	got := freeOnly(models, catalog)
	kept := make(map[string]bool, len(got))
	for _, model := range got {
		kept[model.ID] = true
	}
	if !kept["big-pickle"] || !kept["deepseek-v4.1-flash-free"] {
		t.Fatalf("Zero-price model was filtered out: %v", kept)
	}
	if kept["glm-5.3"] {
		t.Fatal("Paid models should not appear in the free provider's list")
	}
	if kept["test"] {
		t.Fatal("Models not in the catalog (no price data) should not be displayed")
	}

	// Do not filter when the catalog is unavailable: a momentary catalog glitch should not make all free models disappear.
	if all := freeOnly(models, nil); len(all) != len(models) {
		t.Fatalf("Should not filter when catalog is unavailable, got %d/%d", len(all), len(models))
	}
}

// TestAdapterModelCredits locks down that the multiplier table and manifest share the same source.
func TestAdapterModelCredits(t *testing.T) {
	v := &adapter{}
	v.modelsToPluginAPI([]Model{
		{ID: "deepseek-v4-flash-free", Credits: "0.00X"},
		{ID: "plain"},
	})
	credits := v.ModelCredits()
	if got := credits["deepseek-v4-flash-free"]; got != "0.00X" {
		t.Fatalf("Multiplier = %q, want 0.00X", got)
	}
	if _, exists := credits["plain"]; exists {
		t.Fatal("Models without a multiplier should not enter the table")
	}
}
