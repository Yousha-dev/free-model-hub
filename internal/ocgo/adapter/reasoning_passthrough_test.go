package adapters

import (
	"encoding/json"
	"fmt"
	"testing"

	"free-model-hub/cpasdk/pluginapi"

	"free-model-hub/internal/ocgo/adapter/chatcompletions"
	"free-model-hub/internal/ocgo/adapter/messages"
	"free-model-hub/internal/ocgo/adapter/responses"
	"free-model-hub/internal/ocgo/errclass"
)

// A declared effort is an opaque upstream value, not a local capability
// decision. Exercise native and cross-protocol requests, streaming and
// non-streaming, against absent and contradictory capability metadata.
func TestReasoningEffortVerbatim(t *testing.T) {
	builders := []struct {
		name  string
		build func(string, string, []byte, *pluginapi.ThinkingSupport) ([]byte, *errclass.Error)
		field string
	}{
		{"chat-completions", chatcompletions.BuildRequest, "reasoning_effort"},
		{"responses", responses.BuildRequest, "reasoning"},
		{"messages", messages.BuildRequest, "output_config"},
	}
	profiles := []struct {
		name   string
		stream bool
		caps   *pluginapi.ThinkingSupport
	}{
		{"non-stream/no-metadata", false, nil},
		{"stream/no-metadata", true, nil},
		{"non-stream/low-only", false, &pluginapi.ThinkingSupport{Min: 1024, Max: 2048, Levels: []string{"low"}}},
		{"stream/low-only", true, &pluginapi.ThinkingSupport{Min: 1024, Max: 2048, Levels: []string{"low"}}},
	}
	for _, builder := range builders {
		for _, source := range []string{"openai", "openai-response", "claude"} {
			for _, effort := range []string{"xhigh", "max", "none", "auto", "ultra", " Custom-Tier ", " "} {
				for _, profile := range profiles {
					t.Run(fmt.Sprintf("%s/%s/%q/%s", builder.name, source, effort, profile.name), func(t *testing.T) {
						body := effortRequest(t, source, effort, profile.stream)
						out, eErr := builder.build("upstream-model", source, body, profile.caps)
						if eErr != nil {
							t.Fatalf("plugin rejected effort %q: %v", effort, eErr)
						}
						var request map[string]any
						if err := json.Unmarshal(out, &request); err != nil {
							t.Fatalf("invalid upstream JSON: %v", err)
						}
						got := request[builder.field]
						if builder.field != "reasoning_effort" {
							config, _ := got.(map[string]any)
							got = config["effort"]
						}
						if got != effort {
							t.Fatalf("upstream effort = %q, want exact client value %q", got, effort)
						}
						if request["model"] != "upstream-model" {
							t.Fatalf("model rewrite lost: %v", request["model"])
						}
						if source != "claude" && builder.name == "messages" {
							if _, inferred := request["thinking"]; inferred {
								t.Fatal("declared effort must not invent a thinking budget")
							}
						}
					})
				}
			}
		}
	}
}

func effortRequest(t *testing.T, source, effort string, stream bool) []byte {
	t.Helper()
	request := map[string]any{"model": "client-model", "stream": stream}
	switch source {
	case "openai":
		request["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
		request["reasoning_effort"] = effort
	case "openai-response":
		request["input"] = "hi"
		request["reasoning"] = map[string]any{"effort": effort}
	case "claude":
		request["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
		request["max_tokens"] = 4096
		request["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 8192}
		request["output_config"] = map[string]any{"effort": effort}
	default:
		t.Fatalf("unknown fixture format %q", source)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return body
}
