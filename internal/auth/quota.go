package auth

import (
	"strconv"
	"strings"

	"free-model-hub/cpasdk/pluginapi"
)

// Summary generates a single line of quota summary.
func Summary(response *pluginapi.QuotaFetchResponse) string {
	if response == nil {
		return ""
	}
	parts := make([]string, 0, len(response.Summary)+2)
	for _, metric := range response.Summary {
		label := strings.TrimSpace(metric.Label)
		if label == "" {
			label = metric.Key
		}
		parts = append(parts, label+" "+FormatValue(metric.Value))
	}
	for _, group := range response.Groups {
		for _, bucket := range group.Buckets {
			label := strings.TrimSpace(group.DisplayName)
			if label == "" {
				label = bucket.Window
			}
			parts = append(parts, label+" "+bucket.Description)
		}
	}
	return strings.Join(parts, " · ")
}

// FormatValue formats the quota numeric value.
func FormatValue(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// QuotaTotals is the converted result of "remaining / total".
type QuotaTotals struct {
	Remaining int64
	Total     int64
	// Unlimited reports that the remaining quota is unlimited (expressed upstream as -1 / unit=unlimited).
	// When unlimited, Remaining has no meaning, and the presentation layer should display "Infinite" instead of a number.
	Unlimited bool
}

// Totals converts "remaining / total".
//
// Key names are identified by family (standard keys, credit_*, user_quota_* / addon_quota_* / token_plan).
// **New providers must reuse one of the families**: this function only recognizes whitelisted keys; writing other key names will silently
// result in 0/0, causing the page to always display 0 / 0 (which Trae once encountered with remaining_credit).
//
// An account may provide multiple families simultaneously (Qoder's plan quota user_quota_* and addon package
// addon_quota_*). Priority is based on **whether there is actual capacity** rather than the order of appearance: Qoder Free plan's
// userQuota is a valid `{used:0,total:0,remaining:0}`; taking them by order of appearance would lock the entire
// account at 0/0 even if there is still quota in the addon package. Fallback to the first appearing family occurs only when
// all families have no capacity—at that point 0/0 is the true semantic meaning (quota exhausted / not activated).
func Totals(response *pluginapi.QuotaFetchResponse) QuotaTotals {
	if response == nil {
		return QuotaTotals{}
	}
	families := make([]quotaFamily, 0, 2)
	position := make(map[string]int, 2)
	for _, metric := range response.Summary {
		entry, known := quotaKeyRoles[metric.Key]
		if !known {
			continue
		}
		if entry.role == roleRemaining && (strings.EqualFold(metric.Unit, "unlimited") || metric.Value < 0) {
			return QuotaTotals{Unlimited: true}
		}
		index, okFamily := position[entry.family]
		if !okFamily {
			index = len(families)
			position[entry.family] = index
			families = append(families, quotaFamily{})
		}
		family := &families[index]
		switch entry.role {
		case roleRemaining:
			family.remaining, family.hasRemaining = int64(metric.Value), true
		case roleTotal:
			family.total, family.hasTotal = int64(metric.Value), true
		default:
			family.used += int64(metric.Value)
		}
	}
	if len(families) == 0 {
		return QuotaTotals{}
	}

	chosen := families[0]
	for _, family := range families {
		if family.hasCapacity() {
			chosen = family
			break
		}
	}
	// Deduce when total is absent but usage is present: allows providers that "only report usage" to also display a ratio.
	// Families that explicitly specify total=0 are not deduced—that means "no such entitlement", not "no numbers given".
	if !chosen.hasTotal && chosen.used > 0 {
		chosen.total = chosen.remaining + chosen.used
	}
	return QuotaTotals{Remaining: chosen.remaining, Total: chosen.total}
}

// quotaFamily is the aggregation of quota keys (remaining / total / used) for the same family.
type quotaFamily struct {
	remaining, total, used int64
	hasRemaining, hasTotal bool
}

// hasCapacity reports whether the family has actual quota (any dimension is positive).
func (f quotaFamily) hasCapacity() bool {
	return (f.hasRemaining && f.remaining > 0) || (f.hasTotal && f.total > 0)
}

// quotaKeyRoles is the single vocabulary of quota keys: key → (family, role).
//
// Adding or removing keys only requires modifying this table. Previously, remaining / total / used each maintained a whitelist plus a
// family name mapping, requiring three sets of knowledge to be synchronized—missing one would silently result in 0/0.
var quotaKeyRoles = map[string]struct {
	family string
	role   int
}{
	"remaining":             {"default", roleRemaining},
	"total":                 {"default", roleTotal},
	"used":                  {"default", roleUsed},
	"credit_remain":         {"credit", roleRemaining},
	"credit_total":          {"credit", roleTotal},
	"credit_used":           {"credit", roleUsed},
	"user_quota_remaining":  {"user_quota", roleRemaining},
	"user_quota_total":      {"user_quota", roleTotal},
	"user_quota_used":       {"user_quota", roleUsed},
	"addon_quota_remaining": {"addon_quota", roleRemaining},
	"addon_quota_total":     {"addon_quota", roleTotal},
	"addon_quota_used":      {"addon_quota", roleUsed},
	// token_plan family: MiniMax Token Plan token balance. When points (default family) and
	// token plan coexist, they form separate families; Totals selects the family by "capacity priority" without overwriting each other.
	"token_remaining": {"token_plan", roleRemaining},
}

// Role of a quota key: at most one key per role in a family.
const (
	roleRemaining = iota
	roleTotal
	roleUsed
)
