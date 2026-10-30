package core

// This file provides the skeleton for merging credential JSON.
//
// Background: Six providers each implement core.StorageMerger (MergeStorageJSON), where
// the skeletons for four of them (codearts / trae / minimaxcode / zcode) are identical —
//
// 	Original file is empty or not a valid object → entirely replace with new credentials
// 	Otherwise → deserialize into a map, overwrite certain fields in place, and then serialize with indentation
//
// Only "which fields to overwrite and whether to overwrite empty values" differ among them. Centralizing the skeleton here allows each provider to retain
// only its own field list; adding new providers in the future avoids copying the empty/corrupted branches again.
//
// The two providers not centralized (their formats are truly special and kept separate):
//   - cline: Key names must follow the original file (snake_case/camelCase), which cannot be determined by the skeleton;
//   - workbuddy: Deep merge (mergeMaps) with a type fallback, rather than "overwriting fields".

import "encoding/json"

// MergeCredentialJSON merges the updated fields into original and returns the indented JSON.
//
// When original is empty or cannot be deserialized into an object, it directly returns fresh(). Otherwise, it decodes
// original into a map, passes it to apply to overwrite fields, and then serializes it.
//
// apply is only responsible for writing fields, not for null checks: "whether to overwrite empty values" is part of each provider's protocol,
// determined by the provider itself (access_token is usually unconditionally overwritten, refresh_token is usually overwritten only when non-empty).
func MergeCredentialJSON(original []byte, fresh func() ([]byte, error), apply func(map[string]any)) ([]byte, error) {
	if len(original) == 0 {
		return fresh()
	}
	var target map[string]any
	if err := json.Unmarshal(original, &target); err != nil || target == nil {
		return fresh()
	}
	apply(target)
	merged, errMarshal := json.MarshalIndent(target, "", "  ")
	if errMarshal != nil {
		return fresh()
	}
	return merged, nil
}
