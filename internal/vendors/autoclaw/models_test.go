package autoclaw

// This file locks in the response format parsing for the Global model directory (autoclaw-model-config).
//
// Actual upstream returns a **top-level models array** (without code/msg/data envelope):
//
//	{"models":[{"id":"zai_auto",...,"input":["image","text"]}, ...]}
//
// The old implementation only recognized data.models: JSON parsing succeeded but there were 0 models,
// so the host eliminated this credential as "no available models", resulting in an empty model list in the console,
// and only one log line: "probe returned 0 model(s)". This locks in both formats and "empty list must error".

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modelConfigTopLevelFixture is the structure of the actual upstream response (values based on real ones).
const modelConfigTopLevelFixture = `{"models":[
 {"id":"zai_auto","name":"Auto","reasoning":true,"input":["image","text"],
  "contextWindow":1048576,"maxTokens":131072,
  "tooltip":"Automatically picks the best model for your task."},
 {"id":"zai_auto-fast","name":"Auto Fast","reasoning":true,"input":["text"],
  "contextWindow":1048576,"maxTokens":131072},
 {"id":"zaicoding_glm-5.3","name":"GLM-5.3","reasoning":true,"input":["text"],
  "contextWindow":1048576,"maxTokens":307200,"tooltip":"GLM-5.3 coding"}]}`

// modelConfigEnvelopeFixture is the old envelope format.
const modelConfigEnvelopeFixture = `{"code":0,"msg":"SUCCESS","data":{"models":[
 {"id":"legacy-model","name":"Legacy","contextWindow":200000,"maxTokens":8192}]}}`

// serveModels spins up a stub serving only ModelsConfigPath and asserts that the auth header is present.
func serveModels(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ModelsConfigPath {
			t.Errorf("path = %q, want %q", r.URL.Path, ModelsConfigPath)
		}
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "tok-abc") {
			t.Errorf("Authorization = %q, want token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestFetchModelsParsesTopLevelModels(t *testing.T) {
	useLoopbackHost(t)
	baseURL := serveModels(t, modelConfigTopLevelFixture)

	models, errFetch := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errFetch != nil {
		t.Fatalf("fetch models: %v", errFetch)
	}
	if len(models) != 3 {
		t.Fatalf("models = %d, want 3 (top-level models not recognized)", len(models))
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	auto, okAuto := byID["zai_auto"]
	if !okAuto {
		t.Fatalf("missing zai_auto, got %v", byID)
	}
	if auto.Name != "Auto" || auto.ContextWindow != 1048576 || auto.MaxOutputTokens != 131072 || !auto.IsReasoning {
		t.Fatalf("zai_auto metadata = %+v", auto)
	}
	if !auto.SupportsImages || !auto.SupportsTools {
		t.Fatalf("zai_auto capabilities = %+v, want images+tools", auto)
	}
	if glm := byID["zaicoding_glm-5.3"]; glm.SupportsImages {
		t.Fatalf("zaicoding_glm-5.3 (input=text) must not claim image support")
	}
}

func TestFetchModelsStillParsesEnvelope(t *testing.T) {
	useLoopbackHost(t)
	baseURL := serveModels(t, modelConfigEnvelopeFixture)

	models, errFetch := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errFetch != nil {
		t.Fatalf("fetch models: %v", errFetch)
	}
	if len(models) != 1 || models[0].ID != "legacy-model" {
		t.Fatalf("models = %+v, want legacy-model", models)
	}
}

func TestFetchModelsErrorsOnUnknownShape(t *testing.T) {
	useLoopbackHost(t)
	// Parsing succeeded but without any models: must return an error instead of silently returning an empty list.
	baseURL := serveModels(t, `{"code":0,"msg":"SUCCESS","data":{}}`)

	models, errFetch := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errFetch == nil {
		t.Fatalf("expected error for empty model list, got %+v", models)
	}
	if !strings.Contains(errFetch.Error(), "0 model") {
		t.Fatalf("error = %v, want it to mention 0 model", errFetch)
	}
}

// TestFetchModelsParsesCreditLevel locks in the pass-through of credit tiers.
//
// Upstream creditConsumptionLevel is a text tier (low/medium/high), not a discount multiplier; pass it through
// as-is to the multiplier column. If it's an integer, label it as "Tier N", leave blank if missing (console displays "—",
// cannot write it as free, nor format 3 as 3.00X).
func TestFetchModelsParsesCreditLevel(t *testing.T) {
	baseURL := serveModels(t, `{"models":[
	 {"id":"zai_auto","creditConsumptionLevel":"中"},
	 {"id":"zai_auto-fast","creditConsumptionLevel":"低"},
	 {"id":"numeric","creditConsumptionLevel":3},
	 {"id":"none"}]}`)

	models, errModels := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errModels != nil {
		t.Fatalf("fetch: %v", errModels)
	}
	want := map[string]string{"zai_auto": "中", "zai_auto-fast": "低", "numeric": "Tier 3", "none": ""}
	for _, model := range models {
		if model.Credits != want[model.ID] {
			t.Fatalf("%s: Credits = %q, want %q", model.ID, model.Credits, want[model.ID])
		}
	}

	// Domestic static tables don't have upstream tiers and must be left blank rather than writing "not provided" as 0/free.
	for _, model := range staticCNModels() {
		if model.Credits != "" {
			t.Fatalf("static model %s should not have a multiplier: %q", model.ID, model.Credits)
		}
	}
}

// TestLevelTextToleratesShapes guards against unknown formats: better left blank than stuffing structures into the multiplier column.
func TestLevelTextToleratesShapes(t *testing.T) {
	cases := map[string]string{
		`"高"`:           "高",
		`3`:             "Tier 3", // 整数是档位编号，不能被格式化成伪倍率
		`null`:          "",
		``:              "",
		`{"level":"低"}`: "",
	}
	for raw, want := range cases {
		if got := levelText(json.RawMessage(raw)); got != want {
			t.Fatalf("levelText(%s) = %q, want %q", raw, got, want)
		}
	}
}

// TestAdapterModelCredits locks in the consistency between the multiplier table and the model list.
func TestAdapterModelCredits(t *testing.T) {
	v := &adapter{}
	v.modelsToPluginAPI([]Model{
		{ID: "zai_auto", Credits: "中"},
		{ID: "tier_3", Credits: "Tier 3"},
		{ID: "plain"},
	})
	credits := v.ModelCredits()
	if got := credits["zai_auto"]; got != "中" {
		t.Fatalf("multiplier = %q, want Medium", got)
	}
	if got := credits["tier_3"]; got != "Tier 3" {
		t.Fatalf("integer tier = %q, want Tier 3 (must not become 3.00X)", got)
	}
	if _, exists := credits["plain"]; exists {
		t.Fatal("models without tiers should not enter the table")
	}
}
