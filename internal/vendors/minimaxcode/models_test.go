package minimaxcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

const workspaceConfigBody = `{"models":[
 {"model_id":"MiniMax-M3.1-Flash","display_name":"M3.1-Flash","context_limit":512000,"supported_variants":["thinking"]},
 {"model_id":"MiniMax-M4","display_name":"M4","context_limit":1000000,"supported_variants":["thinking"]},
 {"model_id":"   "}
]}`

// stubWorkspaceHost replaces the host HTTP bridge with a fixed response and records /config hit counts.
// Returns the baseURL override that can be passed to various functions.
func stubWorkspaceHost(t *testing.T, status int, body string) (string, *int32) {
	t.Helper()
	var hits int32
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method string `json:"method"`
			URL    string `json:"url"`
		}
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		parsed, errParse := url.Parse(req.URL)
		if errParse != nil {
			return nil, errParse
		}
		if parsed.Path != EpConfig {
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL)
		}
		atomic.AddInt32(&hits, 1)
		return json.Marshal(map[string]any{
			"StatusCode": status,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       []byte(body),
		})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
	return "https://workspace.test", &hits
}

func TestFetchWorkspaceModels(t *testing.T) {
	base, _ := stubWorkspaceHost(t, http.StatusOK, workspaceConfigBody)
	models, err := fetchWorkspaceModels(context.Background(), &Credential{Region: RegionCN, AccessToken: "t"}, base)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2 non-empty entries", models)
	}
	if models[0].ModelID != "MiniMax-M3.1-Flash" || models[0].ContextLimit != 512000 {
		t.Fatalf("first = %+v", models[0])
	}
	if len(models[0].Variants) != 1 || models[0].Variants[0] != "thinking" {
		t.Fatalf("variants = %+v", models[0].Variants)
	}
}

func TestFetchWorkspaceModelsRejectsUpstreamError(t *testing.T) {
	base, _ := stubWorkspaceHost(t, http.StatusInternalServerError, `{}`)
	if _, err := fetchWorkspaceModels(context.Background(), &Credential{Region: RegionCN, AccessToken: "t"}, base); err == nil {
		t.Fatal("non-200 config must be an error")
	}
}

// TestResolveWorkspaceModel verifies case-insensitive matching, -thinking suffix mapping, and fallback on miss.
func TestResolveWorkspaceModel(t *testing.T) {
	base, _ := stubWorkspaceHost(t, http.StatusOK, workspaceConfigBody)
	cache := &workspaceCache{}
	cred := &Credential{Region: RegionCN, AccessToken: "t", UserID: "u1"}

	bundled, _ := findModel("minimax-m3.1-flash")
	id, variant, ok := resolveWorkspaceModel(context.Background(), cache, cred, base, "minimax-m3.1-flash", bundled)
	if !ok || id != "MiniMax-M3.1-Flash" || variant != "" {
		t.Fatalf("resolve = %q %q %v", id, variant, ok)
	}

	thinking, _ := findModel("minimax-m3.1-flash-thinking")
	id, variant, ok = resolveWorkspaceModel(context.Background(), cache, cred, base, "minimax-m3.1-flash-thinking", thinking)
	if !ok || id != "MiniMax-M3.1-Flash" || variant != "thinking" {
		t.Fatalf("thinking resolve = %q %q %v", id, variant, ok)
	}

	agent, _ := findModel("minimax-agent")
	if _, _, ok = resolveWorkspaceModel(context.Background(), cache, cred, base, "minimax-agent", agent); ok {
		t.Fatal("workspace must not match minimax-agent")
	}
}

// TestWorkspaceCacheTTLAndFailureWindow verifies TTL hits, failure cooldowns, and retention of stale results.
func TestWorkspaceCacheTTLAndFailureWindow(t *testing.T) {
	base, hits := stubWorkspaceHost(t, http.StatusOK, workspaceConfigBody)
	cache := &workspaceCache{}
	cred := &Credential{Region: RegionCN, AccessToken: "t", UserID: "u2"}
	ctx := context.Background()

	if models := cache.models(ctx, cred, base); len(models) != 2 {
		t.Fatalf("first load = %+v", models)
	}
	if models := cache.models(ctx, cred, base); len(models) != 2 {
		t.Fatalf("cached load = %+v", models)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (TTL cache)", got)
	}

	key := workspaceAccountKey(cred, base)
	cache.mu.Lock()
	entry := cache.entries[key]
	entry.fetchedAt = time.Now().Add(-workspaceModelsTTL - time.Second)
	entry.lastFail = time.Now()
	cache.entries[key] = entry
	cache.mu.Unlock()

	if models := cache.models(ctx, cred, base); len(models) != 2 {
		t.Fatalf("stale fallback = %+v", models)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (failure window)", got)
	}
}

// TestPrepareReusesWorkspaceCatalog verifies that handshake and catalog fetching share the same /config:
// prepare does not re-request when the cache is warm; it still performs one handshake when there is no instance cache.
func TestPrepareReusesWorkspaceCatalog(t *testing.T) {
	base, hits := stubWorkspaceHost(t, http.StatusOK, workspaceConfigBody)
	cache := &workspaceCache{}
	cred := &Credential{Region: RegionCN, AccessToken: "t", UserID: "u5"}
	ctx := context.Background()

	if models := cache.models(ctx, cred, base); len(models) != 2 {
		t.Fatalf("priming load = %+v", models)
	}
	if err := prepare(ctx, cred, base, cache); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("upstream /config hits = %d, want 1 (warm cache)", got)
	}

	if err := prepare(ctx, cred, base, nil); err != nil {
		t.Fatalf("prepare without cache: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("upstream /config hits = %d, want 2 (handshake only)", got)
	}
}

// TestModelsForAuthMergesStaticAndWorkspace verifies that the registry uses the built-in catalog as its skeleton,
// preserving -thinking entries and supplementing newly added workspace models.
func TestModelsForAuthMergesStaticAndWorkspace(t *testing.T) {
	base, _ := stubWorkspaceHost(t, http.StatusOK, workspaceConfigBody)
	v := newInstance(core.Env{}, RegionCN).(*adapter)
	v.baseURL = base
	cred := &core.Credential{Native: &Credential{Region: RegionCN, AccessToken: "t", UserID: "u3"}}

	models, err := v.ModelsForAuth(context.Background(), cred)
	if err != nil {
		t.Fatalf("ModelsForAuth: %v", err)
	}
	seen := map[string]int{}
	for _, model := range models {
		seen[model.ID]++
	}
	for _, want := range []string{
		"minimax-agent", "minimax-m3-thinking", "minimax-m3.1-flash",
		"minimax-m3.1-flash-thinking", "minimax-m4", "minimax-m4-thinking",
	} {
		if seen[want] != 1 {
			t.Fatalf("model %q count = %d (models = %+v)", want, seen[want], models)
		}
	}
	// Newly added workspace models must have complete metadata: output limit / tool capability cannot be 0/false.
	for _, model := range models {
		if model.ID != "minimax-m4" {
			continue
		}
		if model.MaxCompletionTokens != core.DefaultMaxOutputTokens || model.ContextLength != 1000000 {
			t.Fatalf("workspace model metadata = %+v", model)
		}
		if !core.ModelSupportsTools(model) {
			t.Fatalf("workspace model must keep tool support: %+v", model)
		}
		return
	}
	t.Fatal("minimax-m4 missing from merged catalog")
}

// TestModelsForAuthFallsBackToStatic verifies fallback to the built-in manifest when upstream is unavailable or credentials cannot be parsed.
func TestModelsForAuthFallsBackToStatic(t *testing.T) {
	base, _ := stubWorkspaceHost(t, http.StatusInternalServerError, `{}`)
	v := newInstance(core.Env{}, RegionCN).(*adapter)
	v.baseURL = base
	cred := &core.Credential{Native: &Credential{Region: RegionCN, AccessToken: "t", UserID: "u4"}}

	models, err := v.ModelsForAuth(context.Background(), cred)
	if err != nil {
		t.Fatalf("ModelsForAuth: %v", err)
	}
	if !hasModelID(models, "minimax-m3-thinking") {
		t.Fatalf("static fallback missing thinking entry: %+v", models)
	}

	v2 := newInstance(core.Env{}, RegionCN).(*adapter)
	models, err = v2.ModelsForAuth(context.Background(), &core.Credential{})
	if err != nil {
		t.Fatalf("ModelsForAuth without native: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("ModelsForAuth must not return an empty catalog")
	}
}

func hasModelID(models []pluginapi.ModelInfo, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}
