package openaicompat

import (
	"encoding/json"
	"net/http"
	"testing"

	"free-model-hub/internal/core"
)

func TestParseModelsFiltersKiloFreePool(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"kilo-auto/free","isFree":true,"context_length":256000},
		{"id":"anthropic/claude-opus","isFree":false},
		{"id":"nvidia/nemotron:free"}
	]}`)
	models, err := parseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	got := filterModels(models, FilterFlagged, false)
	if len(got) != 2 || got[0].ID != "kilo-auto/free" || got[1].ID != "nvidia/nemotron:free" {
		t.Fatalf("unexpected free set: %+v", got)
	}
	if got[0].ContextLength != 256000 {
		t.Fatalf("context length not parsed: %+v", got[0])
	}
}

func TestOpenRouterZeroPriceFilterAndPaidToggle(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"meta/llama:free","pricing":{"prompt":"0","completion":"0"}},
		{"id":"openai/gpt","pricing":{"prompt":"0.000002","completion":"0.00001"}},
		{"id":"vendor/zero","pricing":{"prompt":"0","completion":"0"}}
	]}`)
	models, err := parseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := filterModels(models, FilterZeroPrice, false); len(got) != 2 {
		t.Fatalf("free-only filter kept %d models: %+v", len(got), got)
	}
	if got := filterModels(models, FilterZeroPrice, true); len(got) != 3 {
		t.Fatalf("paid toggle kept %d models", len(got))
	}
}

func TestParseModelsAcceptsBareArrayCatalog(t *testing.T) {
	body := []byte(`[{"id":"openai/gpt-4.1","name":"GPT-4.1","limits":{"max_input_tokens":1048576,"max_output_tokens":32768},"supported_input_modalities":["text","image"]}]`)
	models, err := parseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ContextLength != 1048576 || models[0].MaxOutput != 32768 || !models[0].SupportsImages {
		t.Fatalf("unexpected parse: %+v", models)
	}
}

func TestRewriteChatBodyStripsNamespaceAndPinsStream(t *testing.T) {
	a := &adapter{provider: Provider{ID: "openrouter"}}
	out, err := rewriteChatBody([]byte(`{"model":"openrouter/meta/llama:free","stream":true,"stream_options":{"include_usage":true},"messages":[]}`), "", a.UpstreamModelID, false)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "meta/llama:free" || body["stream"] != false {
		t.Fatalf("unexpected body: %s", out)
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatalf("stream_options must be dropped for non-stream calls: %s", out)
	}
}

func TestCredentialParsing(t *testing.T) {
	if _, err := parseCredential([]byte(`{"anonymous":true}`), false); err == nil {
		t.Fatal("keyed providers must reject anonymous credentials")
	}
	cred, err := parseCredential([]byte(`{"anonymous":true}`), true)
	if err != nil || !cred.Anonymous {
		t.Fatalf("keyless provider should accept anonymous credential: %v %+v", err, cred)
	}
	cred, err = parseCredential([]byte(`{"api_key":" sk-test-1234567 "}`), false)
	if err != nil || cred.APIKey != "sk-test-1234567" {
		t.Fatalf("api key not parsed: %v %+v", err, cred)
	}
}

func TestProviderIDsAreUniqueAndUseHTTPS(t *testing.T) {
	seen := map[string]bool{}
	for _, provider := range Providers {
		if seen[provider.ID] {
			t.Fatalf("duplicate provider %s", provider.ID)
		}
		seen[provider.ID] = true
		if len(provider.BaseURL) < 8 || provider.BaseURL[:8] != "https://" {
			t.Fatalf("%s must use https", provider.ID)
		}
	}
}

func TestOpenRouterSendsAgentIdentityAndHonorsOverrides(t *testing.T) {
	var provider Provider
	for _, candidate := range Providers {
		if candidate.ID == "openrouter" {
			provider = candidate
		}
	}
	a := newAdapter(core.Env{}, provider)
	a.Configure(core.Settings{}, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
	a.applyAppHeaders(req)
	if req.Header.Get("HTTP-Referer") != "https://cline.bot" || req.Header.Get("X-Title") != "Cline" {
		t.Fatalf("default identity not applied: %v", req.Header)
	}
	a.Configure(core.Settings{}, map[string]string{"openrouter_app_url": "https://kilocode.ai", "openrouter_app_title": "Kilo Code"})
	req, _ = http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
	a.applyAppHeaders(req)
	if req.Header.Get("HTTP-Referer") != "https://kilocode.ai" || req.Header.Get("X-Title") != "Kilo Code" {
		t.Fatalf("override not applied: %v", req.Header)
	}
	groq := newAdapter(core.Env{}, Provider{ID: "groq"})
	groq.Configure(core.Settings{}, nil)
	req, _ = http.NewRequest(http.MethodPost, "https://api.groq.com/openai/v1/chat/completions", nil)
	groq.applyAppHeaders(req)
	if req.Header.Get("X-Title") != "" {
		t.Fatalf("providers without an identity must not send one: %v", req.Header)
	}
}

func TestZaiFlashFilterKeepsOnlyFreeModels(t *testing.T) {
	models := []upstreamModel{{ID: "glm-4.7-flash"}, {ID: "GLM-4.5-Flash"}, {ID: "glm-4-flash-250414"}, {ID: "glm-4.7-flashx"}, {ID: "glm-5.3"}}
	got := filterModels(models, FilterFlash, false)
	if len(got) != 3 {
		t.Fatalf("flash filter kept %+v", got)
	}
	for _, model := range got {
		if model.ID == "glm-4.7-flashx" || model.ID == "glm-5.3" {
			t.Fatalf("paid model kept: %s", model.ID)
		}
	}
}
