package admin

import "testing"

func TestDisplayedAccountStatusClearsStaleErrorAfterSuccessfulQuota(t *testing.T) {
	if got := displayedAccountStatus("error", true); got != "active" {
		t.Fatalf("status = %q, want active", got)
	}
}

func TestDisplayedAccountStatusPreservesErrorWhenQuotaFails(t *testing.T) {
	if got := displayedAccountStatus("error", false); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
}

func TestDisplayedAccountStatusPreservesOtherStatuses(t *testing.T) {
	for _, status := range []string{"active", "disabled", "unavailable", ""} {
		if got := displayedAccountStatus(status, true); got != status {
			t.Fatalf("status %q became %q", status, got)
		}
	}
}
