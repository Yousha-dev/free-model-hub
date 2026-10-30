package trae

import (
	"encoding/json"
	"testing"
)

// TestConsumptionRateParsesVariants locks down the parsing of consumption rates.
//
// The upstream hides the rate in display_contact_config.consumption_rate, with keys
// oscillating between snake_case and camelCase; enable=false means it does not participate in billing
// (equivalent to "no rate"), and rate=0 is a valid free-tier limit that must be preserved.
func TestConsumptionRateParsesVariants(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"snake", `{"consumption_rate":{"enable":true,"data":{"rate":0.94}}}`, "0.94"},
		{"camel", `{"consumptionRate":{"Enable":true,"Data":{"Rate":"0.7"}}}`, "0.7"},
		{"zero is legal", `{"consumption_rate":{"data":{"rate":0}}}`, "0"},
		{"disabled at rate", `{"consumption_rate":{"enable":false,"data":{"rate":0.9}}}`, ""},
		{"disabled at data", `{"consumption_rate":{"data":{"enable":false,"rate":0.9}}}`, ""},
		{"missing", `{}`, ""},
		{"null", ``, ""},
	}
	for _, tc := range cases {
		if got := consumptionRate(json.RawMessage(tc.raw)); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAdapterModelCredits locks down the consistency between the rate table and the manifest.
func TestAdapterModelCredits(t *testing.T) {
	v := &adapter{}
	v.modelsToPluginAPI([]Model{
		{ID: "glm-5.3", Credits: "0.94"},
		{ID: "plain"},
	})
	credits := v.ModelCredits()
	if got := credits["glm-5.3"]; got != "0.94X" {
		t.Fatalf("rate = %q, want 0.94X", got)
	}
	if _, exists := credits["plain"]; exists {
		t.Fatal("models without a rate should not be in the table")
	}
}

// TestCatalogEntriesParsesCamelCaseOuter locks down end-to-end parsing of outer key variants.
//
// The new generation of upstream responses changes display_contact_config / extra_config to camelCase.
// Recognizing only snake_case would not error out, but would silently turn rates and tool capabilities into "unknown", so
// here we assert all the way from the raw JSON to the output of catalogEntries.
func TestCatalogEntriesParsesCamelCaseOuter(t *testing.T) {
	const body = `{"config_info_list":[
	 {"config_name":"glm-5.3","config_switch":true,"usage":"chat_completion",
	  "context_window_tokens":{"dev":200000},
	  "display_config":{"display_name":"GLM-5.3","model_capability":"reasoning_model"},
	  "extraConfig":"{\"native_function_call\":true}",
	  "displayContactConfig":{"consumptionRate":{"enable":true,"data":{"rate":0.94}}},
	  "model_detail_list":[{"model_name":"glm-5.3__dev","max_tokens":32000}]},
	 {"config_name":"legacy","config_switch":true,"usage":"chat_completion",
	  "display_config":{"display_name":"Legacy"},
	  "extra_config":"{\"native_function_call\":false}",
	  "display_contact_config":{"consumption_rate":{"data":{"rate":0}}},
	  "model_detail_list":[{"model_name":"legacy__dev","max_tokens":8192}]}]}`

	var parsed catalogResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	wireModels, metadata := catalogEntries(parsed.ConfigInfoList)
	if got := wireModels["glm-5.3"].WireName; got != "glm-5.3__dev" {
		t.Fatalf("wire = %q, want glm-5.3__dev", got)
	}
	model := metadata["glm-5.3"]
	if model.Credits != "0.94" {
		t.Fatalf("camelCase outer rate = %q, want 0.94", model.Credits)
	}
	if !model.SupportsTools {
		t.Fatal("extraConfig native_function_call should be true")
	}
	if model.ContextWindow != 200000 || model.MaxTokens != 32000 {
		t.Fatalf("context/output = %d/%d", model.ContextWindow, model.MaxTokens)
	}
	if !model.IsReasoning {
		t.Fatal("reasoning_model should be marked as a reasoning model")
	}

	legacy := metadata["legacy"]
	if legacy.Credits != "0" {
		t.Fatalf("snake_case outer rate=0 should be preserved, got %q", legacy.Credits)
	}
	if legacy.SupportsTools {
		t.Fatal("native_function_call=false should not declare tool capabilities")
	}
}
