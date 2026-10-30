package zcode

import (
	"testing"
)

func TestParseConfigModels(t *testing.T) {
	body := []byte(`{"code":0,"data":{"builtinModels":[
		{"modelId":"GLM-5.3","name":"GLM-5.3","contextWindow":1000000,"maxCompletionTokens":128000,
		 "reasoning":{"levels":{"low":{},"high":{}}}},
		{"modelId":"GLM-5.3-Flash","name":"GLM-5.3-Flash","contextWindow":1000000,"maxCompletionTokens":128000,
		 "capabilities":{"vision":true}}
	]}}`)
	models := parseConfigModels(body)
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if models[0].ID != "GLM-5.3" || !models[0].IsReasoning {
		t.Fatalf("unexpected first model: %+v", models[0])
	}
	if !models[1].SupportsImages {
		t.Fatalf("GLM-5.3-Flash should support images: %+v", models[1])
	}
	if models[0].ContextWindow != 1000000 || models[0].MaxTokens != 128000 {
		t.Fatalf("Metadata not parsed: %+v", models[0])
	}
}
