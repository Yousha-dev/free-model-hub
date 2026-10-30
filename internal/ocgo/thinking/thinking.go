// Package thinking converts numeric Anthropic budgets when the destination
// protocol requires an effort string. Explicit effort strings never use this
// package: adapters forward them unchanged and let the upstream validate them.
package thinking

// EffortFromBudget uses the pinned SDK v7.2.138 thresholds without checking
// model capabilities. Non-positive budgets represent the off state. The table
// never derives "max"; explicit client levels, including "max", pass through
// unchanged instead of being converted to a budget and back.
func EffortFromBudget(budget int64) string {
	if budget <= 0 {
		return "none"
	}
	return budgetToLevel(budget)
}

func budgetToLevel(budget int64) string {
	switch {
	case budget <= 512:
		return "minimal"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	default:
		return "xhigh"
	}
}
