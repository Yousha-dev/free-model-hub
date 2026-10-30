package zcode

import (
	"testing"

	"free-model-hub/internal/core"
)

func TestConfigureBaseURLPerInstance(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.Configure(core.Settings{}, map[string]string{"zcode_base_url": "http://zcode.test"})
	if v.baseURL != "http://zcode.test" {
		t.Fatalf("baseURL = %q", v.baseURL)
	}
}
