package core

import (
	"slices"
	"testing"
)

// TestModelInfoDeclaresToolsOnlyWhenSupported locks the declaration of supported_parameters.
//
// Previously all models unconditionally listed tools / tool_choice, even models that did not support
// tool calling allowed clients to render tool toggles—users would check them and send, and the upstream would return 400. Capability declarations must follow
// SupportsTools; basic sampling parameters apply to all chat models and are always retained.
func TestModelInfoDeclaresToolsOnlyWhenSupported(t *testing.T) {
	withTools := ModelInfoToPluginAPI("demo", ModelDescriptor{ID: "tooly", SupportsTools: true})
	withoutTools := ModelInfoToPluginAPI("demo", ModelDescriptor{ID: "plain"})

	for _, param := range []string{"temperature", "top_p", "max_tokens", "stream"} {
		if !slices.Contains(withTools.SupportedParameters, param) {
			t.Fatalf("Model supporting tools missing base parameter %q: %v", param, withTools.SupportedParameters)
		}
		if !slices.Contains(withoutTools.SupportedParameters, param) {
			t.Fatalf("Standard model missing base parameter %q: %v", param, withoutTools.SupportedParameters)
		}
	}
	// Pinned by literal: if constants are renamed or their values changed, expectations here must not change accordingly, otherwise the test becomes meaningless.
	if !slices.Equal(ToolParameters(), []string{"tools", "tool_choice"}) {
		t.Fatalf("ToolParameters() = %#v", ToolParameters())
	}
	for _, param := range []string{"tools", "tool_choice"} {
		if !slices.Contains(withTools.SupportedParameters, param) {
			t.Fatalf("Model supporting tools missing %q: %v", param, withTools.SupportedParameters)
		}
		if slices.Contains(withoutTools.SupportedParameters, param) {
			t.Fatalf("不支持工具的模型不应声明 %q: %v", param, withoutTools.SupportedParameters)
		}
	}

	if !slices.Contains(withTools.SupportedGenerationMethods, "tool_calls") {
		t.Fatalf("Model supporting tools missing tool_calls: %v", withTools.SupportedGenerationMethods)
	}
	if slices.Contains(withoutTools.SupportedGenerationMethods, "tool_calls") {
		t.Fatalf("Model not supporting tools should not declare tool_calls: %v", withoutTools.SupportedGenerationMethods)
	}
}
