package cline

import (
	"testing"

	"free-model-hub/internal/core"
)

func TestConfigureBaseURLPerInstance(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.Configure(core.Settings{}, map[string]string{"cline_base_url": "http://cline.test"})
	if v.baseURL != "http://cline.test" {
		t.Fatalf("baseURL = %q", v.baseURL)
	}
}

// TestModelCreditsFromCostTier locks the console's "cost multiplier" column to fetch the billing tier.
//
// Cline upstream does not provide a numeric multiplier; the most accurate cost information is the free / pass tiers;
// Unknown tiers must not appear as an entry (console displays "—") and must not be disguised as free.
func TestModelCreditsFromCostTier(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.modelsToPluginAPI([]Model{
		{ID: "cline-free/glm-5.3", Name: "GLM", Cost: CostFree},
		{ID: "cline-pass/glm-5.3", Name: "GLM", Cost: CostPass},
		{ID: "cline-unknown/x", Name: "X"},
	})

	credits := v.ModelCredits()
	if got := credits["cline-free/glm-5.3"]; got != "Free" {
		t.Errorf("free tier = %q; want Free", got)
	}
	if got := credits["cline-pass/glm-5.3"]; got != "Subscription" {
		t.Errorf("pass tier = %q; want Subscription", got)
	}
	if _, ok := credits["cline-unknown/x"]; ok {
		t.Error("unknown cost tier must not be reported as a credits value")
	}
}

func TestEntitlementErrorIsModelScoped(t *testing.T) {
	failure := Classify(403, `{"error":{"code":"ENTITLEMENT_ERROR","message":"Error 403: the user is not subscribed to required model plan"}}`)
	if failure.Kind != core.FailureKindModelBlocked || failure.Retryable() {
		t.Fatalf("entitlement error must be a non-retryable model block, got %+v", failure)
	}
	if failure.HTTPStatus() == 401 || failure.HTTPStatus() == 403 {
		t.Fatalf("entitlement error must not look like a credential failure, status %d", failure.HTTPStatus())
	}
	if Classify(401, "unauthorized").Kind != core.FailureKindCredential {
		t.Fatal("a real 401 must stay a credential failure")
	}
}

func TestUnwrapDataEnvelope(t *testing.T) {
	wrapped := []byte(`{"data":{"id":"x","choices":[{"message":{"content":"OK"}}]}}`)
	if got := string(unwrapData(wrapped)); got != `{"id":"x","choices":[{"message":{"content":"OK"}}]}` {
		t.Fatalf("unwrap = %s", got)
	}
	plain := []byte(`{"id":"x","choices":[]}`)
	if got := string(unwrapData(plain)); got != string(plain) {
		t.Fatalf("plain body must pass through, got %s", got)
	}
}
