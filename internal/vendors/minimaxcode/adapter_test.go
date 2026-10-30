package minimaxcode

import (
	"testing"

	"free-model-hub/internal/core"
)

func TestConfigureBaseURLPerInstance(t *testing.T) {
	vendors := Instances(core.Env{})
	cn := vendors[0].(*adapter)
	global := vendors[1].(*adapter)
	cn.Configure(core.Settings{}, map[string]string{
		"minimaxcode_cn_base_url":     "http://minimax-cn.test",
		"minimaxcode_global_base_url": "http://minimax-global.test",
	})
	global.Configure(core.Settings{}, map[string]string{
		"minimaxcode_cn_base_url":     "http://minimax-cn.test",
		"minimaxcode_global_base_url": "http://minimax-global.test",
	})
	if cn.baseURL != "http://minimax-cn.test" {
		t.Fatalf("cn baseURL = %q", cn.baseURL)
	}
	if global.baseURL != "http://minimax-global.test" {
		t.Fatalf("global baseURL = %q", global.baseURL)
	}
}
