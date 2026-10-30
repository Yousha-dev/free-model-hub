package workbuddy

import (
	"testing"

	"free-model-hub/internal/core"
)

// This file tests login pending-authorization judgment.
//
// The upstream uses business code 11217 + "login ing..." to indicate "authorization not yet complete" -- this is the normal
// state during polling. Early implementations treated all non-zero business codes as failures, so users who completed
// authorization in the browser would still see "authorization failed", when in fact only the final poll was missing.

// TestIsLoginPendingRecognizesUpstreamCode verifies that 11217 is recognized as pending authorization.
func TestIsLoginPendingRecognizesUpstreamCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"upstream 11217 login ing", &core.Failure{
			Kind: core.FailureKindClient, Status: 200,
			Msg: `11217 11217:login ing... {"code":11217,"msg":"11217:login ing..."}`,
		}, true},
		{"plain pending wording", &core.Failure{Kind: core.FailureKindClient, Status: 200, Msg: "authorization pending"}, true},
		{"waiting wording", &core.Failure{Kind: core.FailureKindClient, Status: 200, Msg: "waiting for user"}, true},
		{"not found means not yet authorized", &core.Failure{Kind: core.FailureKindNotFound, Status: 404, Msg: "not found"}, true},
		// Real failures must not be treated as pending authorization, otherwise polling will continue until timeout.
		{"invalid state is fatal", &core.Failure{Kind: core.FailureKindClient, Status: 400, Msg: "invalid state"}, false},
		{"nil error", nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IsLoginPending(testCase.err); got != testCase.want {
				t.Fatalf("IsLoginPending = %v, want %v", got, testCase.want)
			}
		})
	}
}
