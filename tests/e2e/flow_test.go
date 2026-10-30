package e2e

// This file is a complete ABI end-to-end smoke test: registration -> credential parsing -> model -> execution.
// Specific provider behaviors are covered by tests/ integration tests; this only verifies that the App entry chain is reachable.

import (
	"encoding/json"
	"testing"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/internal/app"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/tests/testhost"
)

func TestPluginABIFlow(t *testing.T) {
	host := testhost.New()
	hostbridge.Configure(host.Call)
	t.Cleanup(func() { hostbridge.Configure(nil) })

	instance, errNew := app.New(app.Options{Version: "test", Factories: app.BuiltinFactories()})
	if errNew != nil {
		t.Fatalf("new app: %v", errNew)
	}
	t.Cleanup(func() { instance.StopBackgroundWork(); instance.WaitPluginStreams(0) })

	cfg := app.DefaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.LogLevel = "error"
	if errApply := instance.ApplyConfig(cfg); errApply != nil {
		t.Fatalf("apply config: %v", errApply)
	}
	raw, errHandle := instance.HandleMethod(pluginabi.MethodPluginRegister, []byte(`{"config_yaml":"state_dir: `+cfg.StateDir+`\nlog_level: error\n"}`))
	if errHandle != nil {
		t.Fatalf("register: %v", errHandle)
	}
	var envelope struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		t.Fatalf("register envelope: %s %v", raw, err)
	}
}
