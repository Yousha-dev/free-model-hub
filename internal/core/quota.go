package core

// This file contains cross-provider reusable quota display helpers.
//
// While quota response structures vary by provider, the two display formats
// "plan validity" and "ratio clamping" are consistent: since vendors cannot import each other, such shared pure functions must be placed in core.

import (
	"time"

	"free-model-hub/cpasdk/pluginapi"
)

// QuotaExpiryGroup constructs the "plan validity" display group.
//
// Validity is not "reset time": one-time gift packs expire and do not reset, so the date goes
// into Description instead of ResetTime (which would be rendered as "next reset"). Also, do not provide
// RemainingFraction—the host renders it as a progress bar, but validity has no "remaining ratio".
func QuotaExpiryGroup(expiresAt time.Time) *pluginapi.QuotaGroup {
	return &pluginapi.QuotaGroup{DisplayName: "Plan validity", Buckets: []pluginapi.QuotaBucket{{
		Window:      "Validity",
		Description: "Plan expires " + expiresAt.UTC().Format(time.RFC3339),
	}}}
}

// ClampFraction clamps the quota ratio to [0, 1].
func ClampFraction(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}
