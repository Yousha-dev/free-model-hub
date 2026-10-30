package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/state"
)

// TestNormalizePromptMode locks the value set of the global prompt filter mode.
func TestNormalizePromptMode(t *testing.T) {
	valid := map[string]string{
		"":            "",
		"passthrough": PromptModePassThrough,
		" CUSTOM ":    PromptModeCustom,
		"append":      PromptModeAppend,
	}
	for raw, want := range valid {
		got, err := normalizePromptMode(&raw)
		if err != nil || got != want {
			t.Errorf("normalizePromptMode(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}

	bad := "overwrite"
	_, errBad := normalizePromptMode(&bad)
	pluginErr, okErr := errBad.(*core.PluginError)
	if !okErr || pluginErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("unsupported mode should be a 400 plugin error, got %T %v", errBad, errBad)
	}

	if got, err := normalizePromptMode(nil); err != nil || got != "" {
		t.Fatalf("absent field = %q, %v", got, err)
	}
}

// TestSettingsViewReportsEffectivePromptMode verifies that /status returns the effective value (page override takes precedence).
func TestSettingsViewReportsEffectivePromptMode(t *testing.T) {
	deps := newStubDeps(t)
	service := newServiceWithDeps(t, deps)

	if got := settingsText(t, service, PromptModeKey); got != PromptModePassThrough {
		t.Fatalf("默认模式 = %q, want passthrough", got)
	}

	deps.store.Mutate(func(current *state.State) { current.PromptMode = PromptModeAppend })
	if got := settingsText(t, service, PromptModeKey); got != PromptModeAppend {
		t.Fatalf("Page override not applied: %q", got)
	}
}

// settingsText reads a string field from the settings payload of /status.
func settingsText(t *testing.T, service *Service, key string) string {
	t.Helper()
	raw, errMarshal := json.Marshal(service.settingsView())
	if errMarshal != nil {
		t.Fatalf("marshal settings: %v", errMarshal)
	}
	var settings map[string]any
	if errUnmarshal := json.Unmarshal(raw, &settings); errUnmarshal != nil {
		t.Fatalf("unmarshal settings: %v", errUnmarshal)
	}
	value, okValue := settings[key].(string)
	if !okValue {
		t.Fatalf("settings 缺少 %s: %s", key, raw)
	}
	return value
}

// TestSettingsViewReportsEffectiveModelNameTag verifies that /status returns the effective value (page override takes precedence).
func TestSettingsViewReportsEffectiveModelNameTag(t *testing.T) {
	deps := newStubDeps(t)
	service := newServiceWithDeps(t, deps)

	if got := settingsText(t, service, ModelNameTagKey); got != ModelNameTagOff {
		t.Fatalf("默认标识 = %q, want off", got)
	}

	deps.store.Mutate(func(current *state.State) { current.ModelNameTag = ModelNameTagPrefix })
	if got := settingsText(t, service, ModelNameTagKey); got != ModelNameTagPrefix {
		t.Fatalf("Page override not applied: %q", got)
	}
}

// TestNormalizeModelNameTagPointer locks that illegal values submitted by the page must result in 400.
func TestNormalizeModelNameTagPointer(t *testing.T) {
	for raw, want := range map[string]string{
		" Prefix ": ModelNameTagPrefix,
		"suffix":   ModelNameTagSuffix,
		"off":      ModelNameTagOff,
		"":         "",
	} {
		input := raw
		got, err := normalizeModelNameTagPointer(&input)
		if err != nil || got != want {
			t.Errorf("normalizeModelNameTagPointer(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if got, err := normalizeModelNameTagPointer(nil); err != nil || got != "" {
		t.Fatalf("absent field = %q, %v", got, err)
	}
	bad := "inline"
	_, errBad := normalizeModelNameTagPointer(&bad)
	pluginErr, okErr := errBad.(*core.PluginError)
	if !okErr || pluginErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("unsupported tag should be a 400 plugin error, got %T %v", errBad, errBad)
	}
}

// registryDirtyOf reads model_registry_dirty from the /settings response.
func registryDirtyOf(t *testing.T, response pluginapi.ManagementResponse) bool {
	t.Helper()
	var payload struct {
		Dirty bool `json:"model_registry_dirty"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatalf("unmarshal settings response: %v", err)
	}
	return payload.Dirty
}

// TestSettingsRequestMarksRestartPending verifies that modifying this flag shares the same semantics as manual alias:
// sets RestartPending and returns model_registry_dirty, so the console immediately triggers
// syncModelConfig to rebuild the registry; neither appears when the effective value remains unchanged.
func TestSettingsRequestMarksRestartPending(t *testing.T) {
	deps := newStubDeps(t)
	service := newServiceWithDeps(t, deps)

	response := service.handleSettingsRequest(pluginapi.ManagementRequest{
		Method: http.MethodPost, Body: []byte(`{"model_name_tag":"prefix"}`),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}
	if !deps.State().RestartPending {
		t.Fatal("Changing model_name_tag should set RestartPending")
	}
	if !registryDirtyOf(t, response) {
		t.Fatal("Changing model_name_tag should return model_registry_dirty=true")
	}
	if got := settingsText(t, service, ModelNameTagKey); got != ModelNameTagPrefix {
		t.Fatalf("Effective value after save = %q", got)
	}

	// Repeatedly submitting the same effective value should not trigger a registry rebuild again.
	deps.store.Mutate(func(current *state.State) { current.RestartPending = false })
	response = service.handleSettingsRequest(pluginapi.ManagementRequest{
		Method: http.MethodPost, Body: []byte(`{"model_name_tag":"prefix"}`),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}
	if deps.State().RestartPending {
		t.Fatal("Should not set RestartPending when effective value is unchanged")
	}
	if registryDirtyOf(t, response) {
		t.Fatal("Should not return model_registry_dirty=true when effective value is unchanged")
	}
}
