package e2e

import (
	"os"
	"testing"
)

// TestMain keeps tests from writing plugin state (machine salt, keyless
// account markers) into the real home directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "free-model-hub-test-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("FREE_MODEL_HUB_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
