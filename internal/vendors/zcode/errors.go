package zcode

import (
	"encoding/json"
	"net/http"
	"strings"

	"free-model-hub/internal/core"
)

// Upstream error classification and mapping.
//
// **Do not define** the provider's own error types here: classification results uniformly use the shared
// *core.Failure (which implements core.UpstreamError), translated by core.ToPluginError
// into host status codes. Custom provider types that do not implement this interface will be downgraded to "502 Retryable",
// failing to propagate even a 401 credential invalidation, preventing the host from disabling bad accounts.
// Therefore, this file does only one thing: translate ZCode's upstream responses into core.FailureKind.

// Upstream business codes (measured from zcode.z.ai and api.z.ai).
const (
	codeSuccess         = 0
	codeParameterError  = 3001 // Missing required parameters such as X-Device-Mid
	codeSessionExpired  = 3004 // OAuth polling session expired
	codeModelNotAllowed = 3006 // The account plan does not include this model
	codeCaptchaFailed   = 3007 // Security/captcha verification failed
	codeConcurrent3008  = 3008 // Start Plan concurrent request limit
	codeConcurrent3009  = 3009 // Start Plan model concurrency limit
	codeConcurrent3010  = 3010 // Start Plan model concurrency limit
	codeDailyQuota      = 1005 // Daily quota exhausted (one of the criteria for quota exhaustion)
	codeMaxTokens       = 1210 // max_tokens exceeds [1,131072]
	codeUnusualActivity = 3012 // Risk control "unusual activity"

	// quotaBucketExhaustedPhrase is the text marker for model_usage bucket exhaustion (measured).
	quotaBucketExhaustedPhrase = "exceed quota limit"
)

// Classify translates upstream responses into shared upstream error types.
//
// status passed as 0 indicates "no error signal at the HTTP layer" (upstream wraps business errors with 200); in this case
// HTTPStatus is derived from Kind to avoid passing 200 as an error HTTP code to the host.
//
// Classification order is priority: captcha challenge must precede authentication determination—upstream uses 403 for both
// "credential invalidation" and "CAPTCHA verification", evaluating 403 first would misidentify accounts just needing a captcha as invalid.
func Classify(status int, body string) *core.Failure {
	var payload struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := core.FirstNonEmpty(payload.Msg, payload.Error.Message, textPreview(body))
	lower := strings.ToLower(body)
	lowerMsg := strings.ToLower(msg)

	switch {
	// 1) Captcha challenge (Plan channel): 403 + captcha text, or 400 + code=3007
	case isCaptchaChallenge(status, payload.Code, lower):
		return &core.Failure{
			Kind:   core.FailureKindWAFBlock,
			Status: status,
			Msg:    "Triggered upstream captcha verification. This captcha is Alibaba Cloud invisible verification (no images to solve), this plugin has not built-in browser solver—please use API Key channel for chat, and claim promotional plans in the official client",
		}

	// 2) Risk control: 405 + 3012 unusual activity (triggered by high-frequency requests, a temporary limit)
	case isRiskControl(status, payload.Code, lower):
		return &core.Failure{
			Kind:   core.FailureKindWAFBlock,
			Status: status,
			Msg:    "Triggered upstream risk control limit (3012 unusual activity)",
		}

	// 3) Authentication failure: credential invalid, host should disable this account
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &core.Failure{
			Kind:   core.FailureKindSessionDead,
			Status: status,
			Msg:    core.FirstNonEmpty(msg, "Credential invalid"),
		}

	// 3.5) Model bucket exhaustion: 1005 + "exceed quota limit" is the model_usage bucket's
	//      **single-model quota within the account** being exhausted (e.g., promotional package GLM-5.3 is used up while Flash still has
	//      remaining balance). Avoid by (account, model) rather than cooling down the entire account—otherwise models with remaining balance
	//      in the same account would be unfairly penalized and blocked.
	//      402 is an account-level payment default signal, with higher priority than the single-model bucket (see branch 4); only match phrases
	//      on msg to avoid irrelevant content in the body causing false matches.
	case payload.Code == codeDailyQuota && status != http.StatusPaymentRequired &&
		strings.Contains(lowerMsg, quotaBucketExhaustedPhrase):
		return &core.Failure{
			Kind:   core.FailureKindModelBlocked,
			Status: pickStatus(status, http.StatusBadRequest),
			Msg:    core.FirstNonEmpty(msg, "The quota for this model has been exhausted"),
		}

	// 4) Quota exhausted
	case status == http.StatusPaymentRequired || promptExhausted(lower, payload.Code):
		return &core.Failure{
			Kind:   core.FailureKindHardCredit,
			Status: pickStatus(status, http.StatusPaymentRequired),
			Msg:    core.FirstNonEmpty(msg, "Quota exhausted"),
		}

	// 5) Rate limit / concurrency limit (3008/3009/3010). Upstream occasionally wraps concurrency limit as 500 +
	//    OpenAI-format error ("model concurrency limit exceeded"), caught by message text,
	//    Avoid falling into the 5xx branch and being counted as an upstream failure for circuit breaking. Only match exact phrases to avoid irrelevant
	//    misjudgments as soft rate limits when "concurrency limit" appears in the body.
	case status == http.StatusTooManyRequests || payload.Code == codeConcurrent3008 || payload.Code == codeConcurrent3009 || payload.Code == codeConcurrent3010 ||
		strings.Contains(lower, "concurrency limit exceeded"):
		return &core.Failure{
			Kind:   core.FailureKindSoftRate,
			Status: pickStatus(status, http.StatusTooManyRequests),
			Msg:    core.FirstNonEmpty(msg, "Upstream rate limit exceeded"),
		}

	// 6) This backend does not support this model (avoidance by account x model)
	case payload.Code == codeModelNotAllowed:
		return &core.Failure{
			Kind:   core.FailureKindModelBlocked,
			Status: pickStatus(status, http.StatusBadRequest),
			Msg:    core.FirstNonEmpty(msg, "This account plan does not include this model"),
		}

	// 7) Invalid parameters (max_tokens out of bounds, etc.): switching accounts is meaningless
	case payload.Code == codeMaxTokens || payload.Code == codeParameterError:
		return &core.Failure{
			Kind:   core.FailureKindBadParams,
			Status: status,
			Msg:    core.FirstNonEmpty(msg, "Request parameters rejected by upstream"),
		}

	// 8) Upstream 5xx
	case status >= 500:
		return &core.Failure{
			Kind:   core.FailureKindServer,
			Status: status,
			Msg:    msg,
		}

	default:
		return &core.Failure{
			Kind:   core.FailureKindClient,
			Status: status,
			Msg:    msg,
		}
	}
}

// pickStatus uses a fallback status code when there is no HTTP-level error signal (status=0).
//
// Upstream HTTP status codes are usually correct and should be passed through preferentially; however, business errors may be carried
// by HTTP 200, in which case they must be replaced with semantically correct codes—passing 200 through as an error HTTP code prevents the host
// from determining whether to cool down or disable the account.
func pickStatus(status, fallback int) int {
	if status >= 400 {
		return status
	}
	return fallback
}

// isCaptchaChallenge determines whether the response is a captcha challenge.
//
// Upstream has two forms: 403 + captcha/verify text (or challenge header), and 400 + code=3007.
func isCaptchaChallenge(status, code int, lowerBody string) bool {
	if code == codeCaptchaFailed {
		return true
	}
	if status != http.StatusForbidden && status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(lowerBody, "captcha") || strings.Contains(lowerBody, "verify")
}

// isRiskControl determines whether the response is a risk control block (3012 unusual activity).
func isRiskControl(status, code int, lowerBody string) bool {
	if code == codeUnusualActivity {
		return true
	}
	return status == http.StatusMethodNotAllowed && strings.Contains(lowerBody, "unusual activity")
}

// promptExhausted determines whether the body expresses "quota exhausted".
//
// Upstream text varies across tenants/regions, so both Chinese and English are compared.
func promptExhausted(lowerBody string, code int) bool {
	if code == codeDailyQuota {
		return true
	}
	for _, marker := range []string{
		"insufficient", "quota", "balance", "exhaust",
		"额度", "余额不足", "积分不足",
	} {
		if strings.Contains(lowerBody, marker) {
			return true
		}
	}
	return false
}

// textPreview truncates the body for error messages to avoid stuffing the entire page response into logs.
func textPreview(text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 300 {
		return trimmed[:300]
	}
	return trimmed
}
