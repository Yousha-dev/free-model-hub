package admin

// This file locks in the "no total" display for the quota column of the account table.
//
// AutoClaw's credit wallet only returns total_balance, with no concept of total/used; auth.Totals
// thus results in Total=0. The old page unconditionally concatenated remain + " / " + total, displaying as 10000 / 0
// and paired it with a red 0% empty bar, making users think the quota was broken.

import (
	"strings"
	"testing"
)

func TestConsolePageHidesMissingTotal(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		"var hasTotal = quota.total > 0;",
		"Upstream provides only available balance",
		// When there is no total, the total position must be a placeholder rather than the number 0.
		"quota.remain + \" / \" + (hasTotal ? quota.total : \"\u2014\")",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the missing-total guard (%s)", literal)
		}
	}
	// Unconditional concatenation would render "no total" as / 0, which must no longer exist.
	if strings.Contains(page, "quota.remain + \" / \" + quota.total") {
		t.Fatal("console page must not render remain / total unconditionally")
	}
}

// TestConsolePageShowsQuotaFailureReason locks in the display exit for quota failure reasons.
//
// The backend runQuotaBatch has long written failure reasons into cached quota.message, but the page
// when !quota.ok only rendered "-- / --", giving users information indistinguishable from "upstream has no quota interface",
// making it impossible to tell whether to re-authorize or check later. The banner also only reported a failure count.
func TestConsolePageShowsQuotaFailureReason(t *testing.T) {
	page := Page("/plugins/free-model-hub")

	for _, literal := range []string{
		`var reason = quota.message || "Quota query failed";`,
		"remainCell.title = reason;",
		"function shortReason(text)",
		// Banner per-account details: first 5 items + collapse the rest.
		"failures.slice(0, 5).map(function (r)",
		`"...and " + failures.length + " other accounts failed"`,
		// Details rely on multi-line layout; lacking white-space causes them to be squeezed into a single line.
		"white-space: pre-line;",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing quota failure rendering (%s)", literal)
		}
	}
}
