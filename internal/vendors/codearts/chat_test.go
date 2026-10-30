package codearts

import (
	"encoding/json"
	"testing"
)

// TestClampMaxTokens locks the output upper bound clamping on the request side.
//
// Clamping the directory limit to 65536 on the metadata side only solves half the problem: when the client
// itself brings max_tokens, it will still trigger upstream platform-level validation (65537 and above results in a 400 error for the entire request). This covers two key names, boundary values,
// and inputs that "should not be modified".
func TestClampMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{name: "超限回夹", in: `{"model":"glm-5.2","max_tokens":131072}`, want: maxOutputTokenCap},
		{name: "completion 键同样夹", in: `{"max_completion_tokens":393216}`, want: maxOutputTokenCap},
		{name: "浮点形态也夹", in: `{"max_tokens":131072.0}`, want: maxOutputTokenCap},
		{name: "数字字符串也夹", in: `{"max_tokens":"131072"}`, want: maxOutputTokenCap},
		{name: "两键同时超限", in: `{"max_tokens":200000,"max_completion_tokens":300000}`, want: maxOutputTokenCap},
		{name: "恰好在顶保留", in: `{"max_tokens":65536}`, want: 65536},
		{name: "未超限原样", in: `{"max_tokens":4096}`, want: 4096},
	}
	v := &adapter{}
	for _, tc := range cases {
		out := v.prepareRequestBody([]byte(tc.in))
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("%s: output is not valid JSON: %v (%s)", tc.name, err, out)
		}
		got, ok := payload["max_tokens"]
		if !ok {
			got = payload["max_completion_tokens"]
		}
		num, ok := got.(float64)
		if !ok || int(num) != tc.want {
			t.Fatalf("%s: max_tokens = %v, want %d", tc.name, got, tc.want)
		}
	}
}

// TestClampMaxTokensPassesThroughUntouched verifies that cases which should not be rewritten are passed through as is.
func TestClampMaxTokensPassesThroughUntouched(t *testing.T) {
	for _, body := range []string{
		`not json at all`,
		`{"model":"glm-5.2"}`,
		`{"max_tokens":true}`,
		`{"max_tokens":{}}`,
		`{"max_tokens":null}`,
		`[]`,
		`null`,
		`{"max_tokens":4096}`,
	} {
		if got := string((&adapter{}).prepareRequestBody([]byte(body))); got != body {
			t.Fatalf("body should be returned as-is, got %q", got)
		}
	}
}
