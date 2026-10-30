package workbuddy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"free-model-hub/internal/core"
)

func TestConfigureAppliesPromptAndUADefaults(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("custom prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := newRuntime(core.Env{})
	rt.configure(core.Settings{
		EnabledRealms: []string{"cn"},
		MachineSalt:   "salt-1",
		StateDir:      dir,
	}, map[string]string{
		"prompt_mode":                     "custom",
		"prompt_file":                     promptPath,
		"workbuddy_client_version":        "9.9.9",
		"workbuddy_sanitize_fingerprints": "false",
	})

	cfg := *rt.cfg.Load()
	if cfg.clientVersion != "9.9.9" {
		t.Fatalf("client version = %q", cfg.clientVersion)
	}
	if cfg.promptMode != PromptModeCustom || cfg.promptText != "custom prompt" {
		t.Fatalf("prompt = %q/%q", cfg.promptMode, cfg.promptText)
	}
	if cfg.sanitizeFingerprints {
		t.Fatal("sanitize flag not applied")
	}
	if *rt.salt.Load() != "salt-1" {
		t.Fatalf("install salt = %q", *rt.salt.Load())
	}
	if len(cfg.enabledRealms) != 1 || cfg.enabledRealms[0] != "cn" {
		t.Fatalf("realms = %#v", cfg.enabledRealms)
	}

}

// TestWorkBuddyRuntimeIsolated verifies that the prompts, tier tables, and machine salts of the two runtimes do not contaminate each other.
func TestWorkBuddyRuntimeIsolated(t *testing.T) {
	first := newRuntime(core.Env{})
	second := newRuntime(core.Env{})

	first.configure(core.Settings{MachineSalt: "salt-first"}, map[string]string{
		"prompt_mode": "custom",
	})
	second.configure(core.Settings{MachineSalt: "salt-second"}, map[string]string{
		"prompt_mode": "append",
	})
	first.setEffortTables(
		map[string]string{"glm-5.2": "high"},
		map[string][]string{"glm-5.2": {"low", "high"}},
	)
	second.setEffortTables(
		map[string]string{"glm-5.2": "low"},
		map[string][]string{"glm-5.2": {"low"}},
	)

	if got := *first.salt.Load(); got != "salt-first" {
		t.Fatalf("first salt = %q", got)
	}
	if got := *second.salt.Load(); got != "salt-second" {
		t.Fatalf("second salt = %q", got)
	}
	if got := first.prompt.Load().Mode; got != "custom" {
		t.Fatalf("first prompt mode = %q", got)
	}
	if got := second.prompt.Load().Mode; got != "append" {
		t.Fatalf("second prompt mode = %q", got)
	}
	defaults, supported := first.effortTables()
	if defaults["glm-5.2"] != "high" || len(supported["glm-5.2"]) != 2 {
		t.Fatalf("first effort tables = %#v / %#v", defaults, supported)
	}
	defaults, supported = second.effortTables()
	if defaults["glm-5.2"] != "low" || len(supported["glm-5.2"]) != 1 {
		t.Fatalf("second effort tables = %#v / %#v", defaults, supported)
	}
}

// TestDeriveIDStabilityAndIsolation verifies the stability and isolation of device fingerprints.
func TestDeriveIDStabilityAndIsolation(t *testing.T) {
	rt := newRuntime(core.Env{})
	setSalt := func(s string) { value := s; rt.salt.Store(&value) }
	setSalt("salt-a")
	first := rt.deriveID("uid-1", "machine")
	second := rt.deriveID("uid-1", "machine")
	if first != second {
		t.Fatal("deriveID must be stable for the same input")
	}
	if len(first) != 36 {
		t.Fatalf("deriveID length = %d, want 36 (official device id shape)", len(first))
	}
	if rt.deriveID("uid-2", "machine") == first {
		t.Fatal("different accounts must get different fingerprints")
	}
	// Changing machine salt = changing device: fingerprints must drift entirely.
	setSalt("salt-b")
	if rt.deriveID("uid-1", "machine") == first {
		t.Fatal("changing the install salt must change fingerprints")
	}
}

// TestBuildCacheKeyIsolatesAccounts verifies account isolation of cache keys.
//
// uid prefix is a hard isolation factor: reusing the same cache key across accounts causes the upstream to hit
// the wrong account's prefixed cache, leaking the other party's conversation content.
func TestBuildCacheKeyIsolatesAccounts(t *testing.T) {
	keyA := buildCacheKey("account-aaaa1111", "conv-1")
	keyB := buildCacheKey("account-bbbb2222", "conv-1")
	if keyA == keyB {
		t.Fatal("different accounts must produce different cache keys")
	}
	if !strings.HasPrefix(keyA, "wb2a-account-") {
		t.Fatalf("cache key should carry the uid prefix: %q", keyA)
	}
	if buildCacheKey("account-aaaa1111", "conv-1") != keyA {
		t.Fatal("cache key must be deterministic")
	}
}
