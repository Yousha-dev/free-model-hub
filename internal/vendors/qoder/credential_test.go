package qoder

import (
	"testing"
)

// This file locks down the parsing rules for device identity.
//
// Background (issue #13 / #10): The upstream performs **strict validation**
// on cosy-machinetoken + cosy-machinetype. 2026-10-23 head-by-head isolation test (same Global account):
//
//	Do not send machine headers          → 2 VIEW_DETAILS, no CLAIM_BENEFIT
//	Official component pairing (Windows real device) → CLAIM_BENEFIT appears (claimable)
//	Change 1 character in the same token → Activity disappears
//	Truncate 1 bit / append 1 bit to the same token → Activity disappears
//	Device A token + Device B type       → Activity disappears
//	Random/derived 88-char token + type  → Activity disappears
//	Official component in container (isVm=true) → Activity disappears
//
// CN version does the exact reverse: claimable without machine headers, but filtered out when a derived token is sent.
// Conclusion: The plugin must not derive identity on its own, but only forward the true values provided by the user.

// TestNativeRequiresTokenAndType covers: the criterion for usable identity is that both headers appear as a pair.
func TestNativeRequiresTokenAndType(t *testing.T) {
	cases := []struct {
		name     string
		identity MachineIdentity
		want     bool
	}{
		{"both", MachineIdentity{Token: "tok", Type: "ty"}, true},
		{"token only", MachineIdentity{Token: "tok"}, false},
		{"type only", MachineIdentity{Type: "ty"}, false},
		{"empty", MachineIdentity{}, false},
	}
	for _, testCase := range cases {
		if got := testCase.identity.Native(); got != testCase.want {
			t.Errorf("%s: Native() = %v, expected %v", testCase.name, got, testCase.want)
		}
	}
}
