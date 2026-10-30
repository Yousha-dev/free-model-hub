package core

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestMergeCredentialJSON locks down the three branches and field-override semantics of the credential merge skeleton.
//
// This skeleton is shared by codearts, trae, minimaxcode, and zcode. A mistake in any branch
// would cause "refreshed credentials written back to the host" to silently drop fields, so each branch is pinned down.
func TestMergeCredentialJSON(t *testing.T) {
	fresh := func() ([]byte, error) { return []byte(`{"fresh":true}`), nil }
	apply := func(m map[string]any) {
		m["access_token"] = "new"
		if v, ok := m["refresh_token"].(string); ok && v != "" {
			m["refresh_token"] = "rotated"
		} else {
			m["refresh_token"] = "rotated"
		}
	}

	cases := []struct {
		name     string
		original string
		freshErr error
		want     map[string]any
	}{
		{
			name:     "空原文件退回 fresh",
			original: "",
			want:     map[string]any{"fresh": true},
		},
		{
			name:     "损坏的 JSON 退回 fresh",
			original: "{not json",
			want:     map[string]any{"fresh": true},
		},
		{
			name:     "JSON null 退回 fresh（不是对象）",
			original: "null",
			want:     map[string]any{"fresh": true},
		},
		{
			name:     "JSON 数组退回 fresh（不是对象）",
			original: `[1,2]`,
			want:     map[string]any{"fresh": true},
		},
		{
			name:     "正常合并：保留无关字段并覆盖目标字段",
			original: `{"keep":"me","access_token":"old"}`,
			want:     map[string]any{"keep": "me", "access_token": "new", "refresh_token": "rotated"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.original)
			if tc.original == "" {
				raw = nil
			}
			got, err := MergeCredentialJSON(raw, func() ([]byte, error) {
				if tc.freshErr != nil {
					return nil, tc.freshErr
				}
				return fresh()
			}, apply)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var decoded map[string]any
			if errUnmarshal := json.Unmarshal(got, &decoded); errUnmarshal != nil {
				t.Fatalf("result is not a JSON object: %v (%s)", errUnmarshal, got)
			}
			if len(decoded) != len(tc.want) {
				t.Fatalf("field count = %d, want %d (%v)", len(decoded), len(tc.want), decoded)
			}
			for key, wantValue := range tc.want {
				if decoded[key] != wantValue {
					t.Errorf("[%s] = %#v, want %#v", key, decoded[key], wantValue)
				}
			}
		})
	}

	// When fresh returns an error, the error must be propagated rather than silently returning an empty credential.
	sentinel := errors.New("storage json failed")
	if _, err := MergeCredentialJSON(nil, func() ([]byte, error) { return nil, sentinel }, apply); !errors.Is(err, sentinel) {
		t.Fatalf("fresh error = %v, want %v", err, sentinel)
	}
}
