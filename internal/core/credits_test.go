package core

import "testing"

// TestFormatRateTreatsPlaceholdersAsUnknown locks the boundary between "unknown" and "free".
//
// Upstream occasionally writes missing data as -1 / NaN / Inf; once these values enter the console, users would think they are
// the real multiplier. 0 must be kept -- it represents limited-time free, which is a valid value.
func TestFormatRateTreatsPlaceholdersAsUnknown(t *testing.T) {
	cases := map[string]string{
		"x0.94 credits": "0.94X",
		"0":             "0.00X",
		"x0.05":         "0.05X",
		"免费":            "免费",
		"-1":            "",
		"NaN":           "",
		"-Inf":          "",
		"":              "",
	}
	for raw, want := range cases {
		if got := FormatRate(raw); got != want {
			t.Fatalf("FormatRate(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestCreditTableReplaceAndSnapshot verifies full-table replacement and empty-table semantics.
func TestCreditTableReplaceAndSnapshot(t *testing.T) {
	var table CreditTable
	if got := table.Snapshot(); got != nil {
		t.Fatalf("Zero value should return nil, got %v", got)
	}
	table.Replace(map[string]string{"glm-5": "0.70X", "": "x", "glm-6": "  "})
	got := table.Snapshot()
	if len(got) != 1 || got["glm-5"] != "0.70X" {
		t.Fatalf("Snapshot = %v, want only glm-5", got)
	}
	got["glm-5"] = "tampered"
	if table.Snapshot()["glm-5"] != "0.70X" {
		t.Fatal("Snapshot must return a copy")
	}
	table.Replace(nil)
	if got := table.Snapshot(); got != nil {
		t.Fatalf("Replacing with empty table should return nil, got %v", got)
	}
}

// TestFillCreditsNormalizesAndReplaces locks the shared table-entry standard for all six providers:
// Original text is uniformly passed through FormatRate, empty IDs / empty text are not written to the table, and empty batches clear the previous round.
func TestFillCreditsNormalizesAndReplaces(t *testing.T) {
	type item struct {
		ID  string
		Raw string
	}
	var table CreditTable
	FillCredits(&table, []item{
		{"a", "x0.5 credits"},
		{" b ", "1"},
		{"", "0.9"},
		{"c", ""},
		{"d", "免费"},
		{"e", "-1"},
	}, func(m item) string { return m.ID }, func(m item) string { return m.Raw })

	want := map[string]string{"a": "0.50X", "b": "1.00X", "d": "免费"}
	got := table.Snapshot()
	if len(got) != len(want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	for id, label := range want {
		if got[id] != label {
			t.Fatalf("snapshot[%s] = %q, want %q", id, got[id], label)
		}
	}

	FillCredits(&table, nil, func(m item) string { return m.ID }, func(m item) string { return m.Raw })
	if table.Snapshot() != nil {
		t.Fatal("empty batch must clear the table")
	}
}
