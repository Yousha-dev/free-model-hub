package thinking

import "testing"

// Only numeric budgets require a fixed threshold conversion. Explicit
// effort values are tested separately at each protocol adapter boundary.
func TestEffortFromBudgetTableOnly(t *testing.T) {
	cases := []struct {
		name   string
		budget int64
		want   string
	}{
		{"negative budget is off", -1, "none"},
		{"zero budget is off", 0, "none"},
		{"small positive budget", 1, "minimal"},
		{"minimal bound", 512, "minimal"},
		{"just past minimal", 513, "low"},
		{"low bound", 1024, "low"},
		{"just past low", 1025, "medium"},
		{"medium bound", 8192, "medium"},
		{"just past medium", 8193, "high"},
		{"high bound", 24576, "high"},
		{"past high is xhigh", 24577, "xhigh"},
		{"huge budget stays xhigh", 999999, "xhigh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffortFromBudget(tc.budget); got != tc.want {
				t.Errorf("EffortFromBudget(%d) = %q, want %q", tc.budget, got, tc.want)
			}
		})
	}
}
