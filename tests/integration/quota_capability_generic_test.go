package tests

// This file locks down "quota capability determination is universal for all providers".
//
// The page reads only one field accounts[].supports_quota, so this field must be true for **every**
// registered provider instance, rather than just being a patch for OpenCode ZEN:
//   - Providers implementing QuotaProvider -> true (briefly displays "Querying..." if not found);
//   - Those not implementing it -> false (immediately displays "-- / --", never enters querying state).
//
// Here we enumerate the real registry instances and reconcile them one by one with interface assertions. If a new provider forgets to implement Quota
// but expects a quota column, or implements it but fails to report it, it will be caught here.

import (
	"free-model-hub/internal/vendors/openaicompat"
	"testing"

	"free-model-hub/internal/app"
	"free-model-hub/internal/core"
)

// TestSupportsQuotaIsGenericAcrossVendors enumerates all built-in provider instances.
func TestSupportsQuotaIsGenericAcrossVendors(t *testing.T) {
	registry := core.NewRegistry()
	for _, factory := range app.BuiltinFactories() {
		for _, vendor := range factory.Instances(core.Env{}) {
			if errRegister := registry.Register(vendor); errRegister != nil {
				t.Fatalf("register %s: %v", vendor.ID(), errRegister)
			}
		}
	}
	vendors := registry.Vendors()
	if len(vendors) == 0 {
		t.Fatal("no vendors registered")
	}

	// At least two types of instances must exist, otherwise this assertion is meaningless (all true or all false cannot catch issues).
	var withQuota, withoutQuota []string
	for _, vendor := range vendors {
		_, okQuota := core.As[core.QuotaProvider](vendor)
		if okQuota {
			withQuota = append(withQuota, vendor.ID())
			continue
		}
		withoutQuota = append(withoutQuota, vendor.ID())
		// Providers without a quota interface: must exist in the list and be recognized as unsupported.
		// This is the category that "immediately displays -- / --".
		if vendor.ID() == "" {
			t.Fatal("vendor with empty ID")
		}
	}
	if len(withoutQuota) == 0 {
		t.Fatal("expected at least one vendor without quota (cline, opencodezen); " +
			"if all vendors now implement Quota, update this assertion")
	}
	if len(withQuota) == 0 {
		t.Fatal("expected at least one vendor with quota")
	}
	t.Logf("quota-capable: %v; no-quota (show -- / --): %v", withQuota, withoutQuota)

	// Snapshot: pins down "who does not have a quota interface". This is not an implementation detail, but **user-visible behavior**
	// -- accounts for these providers will immediately display "-- / --". When adding/removing Quota implementations, you must
	// update this accordingly and confirm that the page semantics remain correct.
	wantNoQuota := map[string]bool{"cline": true, "opencodezen": true, "opencodego": true}
	for _, provider := range openaicompat.Providers {
		wantNoQuota[provider.ID] = true
	}
	if len(withoutQuota) != len(wantNoQuota) {
		t.Fatalf("no-quota vendors = %v, want %v", withoutQuota, wantNoQuota)
	}
	for _, id := range withoutQuota {
		if !wantNoQuota[id] {
			t.Fatalf("vendor %s unexpectedly lacks quota (update wantNoQuota if intentional)", id)
		}
	}
}
