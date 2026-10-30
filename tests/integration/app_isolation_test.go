package tests

import (
	"context"
	"encoding/json"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/app"
	"free-model-hub/internal/core"
	"free-model-hub/internal/state"
)

// TestAppsHaveIsolatedRegistryAndState verifies that App instances do not share registries and state.
func TestAppsHaveIsolatedRegistryAndState(t *testing.T) {
	first, errFirst := app.New(app.Options{Version: "a"})
	if errFirst != nil {
		t.Fatalf("new first: %v", errFirst)
	}
	second, errSecond := app.New(app.Options{Version: "b", Factories: app.BuiltinFactories()})
	if errSecond != nil {
		t.Fatalf("new second: %v", errSecond)
	}
	if len(first.Registry().Vendors()) != 0 {
		t.Fatalf("first app registry should be empty")
	}
	if len(second.Registry().Vendors()) == 0 {
		t.Fatalf("second app registry should contain builtins")
	}

	cfgFirst := app.DefaultConfig()
	cfgFirst.StateDir = t.TempDir()
	if errApply := first.ApplyConfig(cfgFirst); errApply != nil {
		t.Fatalf("apply first: %v", errApply)
	}
	cfgSecond := app.DefaultConfig()
	cfgSecond.StateDir = t.TempDir()
	if errApply := second.ApplyConfig(cfgSecond); errApply != nil {
		t.Fatalf("apply second: %v", errApply)
	}
	first.MutateState(func(state *state.State) {
		state.PromptMode = "custom"
	})
	if got := second.SnapshotState().PromptMode; got != "" {
		t.Fatalf("second app saw first app state: %q", got)
	}
}

// TestAppsIsolateLoginSessions verifies that the login sessions of two Apps are invisible to each other.
func TestAppsIsolateLoginSessions(t *testing.T) {
	first, errFirst := app.New(app.Options{Version: "a", Factories: app.BuiltinFactories()})
	if errFirst != nil {
		t.Fatalf("new first: %v", errFirst)
	}
	second, errSecond := app.New(app.Options{Version: "b", Factories: app.BuiltinFactories()})
	if errSecond != nil {
		t.Fatalf("new second: %v", errSecond)
	}
	for _, instance := range []*app.App{first, second} {
		cfg := app.DefaultConfig()
		cfg.StateDir = t.TempDir()
		if errApply := instance.ApplyConfig(cfg); errApply != nil {
			t.Fatalf("apply config: %v", errApply)
		}
	}
	// Register a session on the first App using a real codearts login (constructed purely locally).
	rawStart, errStart := first.HandleMethod(pluginabi.MethodAuthLoginStart, mustMarshal(map[string]any{
		"metadata": map[string]any{"vendor": "codearts"},
	}))
	if errStart != nil {
		t.Fatalf("first app login start: %v", errStart)
	}
	payload := decodeEnvelopeResultForTest(t, rawStart)
	var started struct {
		State string `json:"state"`
	}
	if errUnmarshal := json.Unmarshal(payload, &started); errUnmarshal != nil {
		t.Fatalf("decode login start: %v", errUnmarshal)
	}
	if started.State == "" {
		t.Fatal("login session state is empty")
	}

	// The second App cannot see the session of the first App: polling by state must return session_not_found.
	raw, errPoll := second.HandleMethod(pluginabi.MethodAuthLoginPoll, mustMarshal(map[string]any{"state": started.State}))
	if errPoll == nil {
		t.Fatalf("second app should not own first app session, got %s", raw)
	}
}

// recordingVendor is a provider stub that only records Configure inputs, used to verify
// that each App's provider instance receives its own configuration respectively.
type recordingVendor struct {
	env     core.Env
	region  string
	baseURL string
	ua      string
	prompt  string
}

func (v *recordingVendor) ID() string                                { return "recording-" + v.region }
func (v *recordingVendor) Name() string                              { return "Recording " + v.region }
func (v *recordingVendor) Region() string                            { return v.region }
func (v *recordingVendor) ModelPrefix() string                       { return "" }
func (v *recordingVendor) Match(string, string, map[string]any) bool { return false }
func (v *recordingVendor) Parse([]byte, string) (*core.Credential, error) {
	return nil, nil
}
func (v *recordingVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (v *recordingVendor) Execute(context.Context, *core.Credential, *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *recordingVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	return nil
}
func (v *recordingVendor) Configure(_ core.Settings, raw map[string]string) {
	v.baseURL = raw["vendor_base_url"]
	v.ua = raw["vendor_ua"]
	v.prompt = raw["prompt_mode"]
}

// TestAppsIsolateVendorConfig verifies that provider instances of two Apps each hold their own configuration.
func TestAppsIsolateVendorConfig(t *testing.T) {
	var firstVendor, secondVendor *recordingVendor
	first, errFirst := app.New(app.Options{Version: "a", Factories: []core.VendorFactory{{
		Instances: func(env core.Env) []core.Vendor {
			firstVendor = &recordingVendor{env: env, region: "cn"}
			return []core.Vendor{firstVendor}
		},
	}}})
	if errFirst != nil {
		t.Fatalf("new first: %v", errFirst)
	}
	second, errSecond := app.New(app.Options{Version: "b", Factories: []core.VendorFactory{{
		Instances: func(env core.Env) []core.Vendor {
			secondVendor = &recordingVendor{env: env, region: "cn"}
			return []core.Vendor{secondVendor}
		},
	}}})
	if errSecond != nil {
		t.Fatalf("new second: %v", errSecond)
	}

	firstCfg := app.DefaultConfig()
	firstCfg.StateDir = t.TempDir()
	firstCfg.Raw = map[string]string{
		"vendor_base_url": "http://first.test",
		"vendor_ua":       "UA-First/1.0",
	}
	firstCfg.PromptMode = "custom"
	secondCfg := app.DefaultConfig()
	secondCfg.StateDir = t.TempDir()
	secondCfg.Raw = map[string]string{
		"vendor_base_url": "http://second.test",
		"vendor_ua":       "UA-Second/2.0",
	}
	secondCfg.PromptMode = "append"
	if errApply := first.ApplyConfig(firstCfg); errApply != nil {
		t.Fatalf("apply first: %v", errApply)
	}
	if errApply := second.ApplyConfig(secondCfg); errApply != nil {
		t.Fatalf("apply second: %v", errApply)
	}

	if firstVendor.baseURL != "http://first.test" || firstVendor.ua != "UA-First/1.0" || firstVendor.prompt != "custom" {
		t.Fatalf("first vendor config = %+v", firstVendor)
	}
	if secondVendor.baseURL != "http://second.test" || secondVendor.ua != "UA-Second/2.0" || secondVendor.prompt != "append" {
		t.Fatalf("second vendor config = %+v", secondVendor)
	}
	if firstVendor == secondVendor {
		t.Fatal("apps share the same vendor instance")
	}
}
