package workbuddy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"free-model-hub/internal/core"
)

// This file implements upstream error classification.
//
// Why it is needed: Upstream mixes three methods to express errors: HTTP status codes, business codes, and natural language messages,
// and the same HTTP status code may correspond to completely different actions (e.g., 429 could be either rate-limiting or quota exhaustion).
// Classification results determine: whether to cool down this account, disable this account, avoid only this model, or not penalize the account at all.
//
// The judgment order is semantic and cannot be rearranged (see comments on Classify).

// Error categories used by this provider (mapping one-to-one with core.FailureKind):
//   - none: Indicates success.
//   - hard_credit: Quota exhausted (402 or balance keywords) -> Long cool-down until the next day.
//   - soft_rate: Rate-limited (429) -> Short cool-down with exponential backoff.
//   - session_dead: Session invalid (12153) -> Disabled, requires re-login.
//   - not_found: Occasional upstream 404 -> Fixed short cool-down, does not accumulate circuit breaking.
//   - server: Upstream 5xx -> Counted towards circuit breaking.
//   - content_blocked: Content policy violation -> Do not penalize account, can downgrade and retry.
//   - channel_rejected: Channel verification block (11128) -> Request-level, do not penalize account or rotate, retain upstream description.
//   - bad_params: Invalid parameters -> Do not penalize account, but still rotate and retry.
//   - account_fault: Account-level fault (request illegal / trial not activated) -> Cool-down and rotate.
//   - model_blocked: Model not found on this backend (11102) -> Avoidance per (account, model).
//   - waf_block: Upstream WAF block (403 non-business envelope) -> Account soft cool-down.
//   - prompt_too_long: Context exceeded (11115) -> Request-level, do not penalize account or rotate.
//   - image_invalid: Invalid image -> Request-level, do not penalize account or rotate.
//   - client: Other 4xx / business errors.

// Keyword table.
//
// All lowercase comparison, dual-channel for Chinese and English: upstream returns inconsistent text for different tenants/regions.
var (
	hardMarkers = []string{
		"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
		"quota exceeded", "quota exhaust", "payment required", "credit not enough", "not enough credit",
		"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	}
	softRateMarkers = []string{
		"rate limit", "rate-limiting", "rate-limited", "too many requests", "too many",
		"usage limit", "请求过于频繁", "限流",
	}
	sessionDeadMarkers  = []string{"offline user session not found", "12153"}
	accountFaultMarkers = []string{
		"request illegal", "trial not activated", "trial version is not yet activated",
	}
	contentBlockedMarkers = []string{"blocked by security policy"}
	// channelRejectedMarkers are texts for channel verification failure (11128): indicating the request was not sent by the official client.
	channelRejectedMarkers = []string{"unapproved channel", "illegal api invocation"}
	invalidImageMarkers    = []string{
		"invalid image_url content", "invalid_image_data", "replace the image",
	}
	promptTooLongMarkers = []string{
		`"code":11115`, `"code": 11115`, `"code":"11115"`, "prompt is too long",
	}
)

const (
	badParamsMarkerMsg  = "unmarshal chat params failed"
	badParamsMarkerCode = `"code":11101`
	// modelBlockCode is "model not found on this backend".
	modelBlockCode = "11102"
	// channelRejectedCode is channel verification failure (unofficial client).
	channelRejectedCode = "11128"
)

var (
	cnResetRe = regexp.MustCompile(`将在 (.+?) 重置`)
	enResetRe = regexp.MustCompile(`(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)
	// softRateResetLoc is the timezone used for upstream reset times (fixed UTC+8, independent of system tzdata).
	softRateResetLoc    = time.FixedZone("UTC+8", 8*3600)
	softRateTimeLayout  = "2006-01-02 15:04:05"
	retryAfterSanityCap = 2 * time.Hour
)

// Classify determines the error category based on the status code and response body.
//
// The judgment order is critical, especially for these rules:
//   - 11102 must come first: it could be 400 or 404, and if judged late, it will be swallowed by the 404 branch;
//   - 429 must be earlier than hardMarkers: the body of 429 frequently contains "quota exceeded" / "insufficient quota"
//     Phrasing that straddles billing and rate limiting; if hardMarkers are judged first, rate limiting will be misclassified as quota exhaustion,
//     wasting an account for about 12 hours. True quota exhaustion is caught by 402 or business code 14018;
//   - 11115 (context exceeded) and 11128 (channel verification) must be earlier than 404/5xx/WAF/content policy:
//     They are request-level errors, and penalizing the account is incorrect;
//   - accountFault must be earlier than 429: account banning phrasing may also contain rate-limiting keywords.
func Classify(status int, body string) *core.Failure {
	lower := strings.ToLower(body)

	// 0) Model-level avoidance: only compare independent fields, never perform full-text substring matching--
	// Otherwise, "11102" colliding with the requestId would falsely bypass an available model.
	if isModelBlocked(status, body) {
		return &core.Failure{Kind: core.FailureKindModelBlocked, Status: status, Msg: body}
	}
	// Channel validation failure (11128): The upstream often returns this wrapped in an HTTP 500, but it is a request-level rejection.
	// Treating it as a 5xx causes the host to treat all accounts as server-side faults for cooling down, degrading subsequent retries into
	// 503 auth_unavailable (leaving only numeric codes with readable descriptions lost). Here it is rewritten as 400,
	// and the multi-language body is folded into a readable message (the original is written to logs by the caller for evidence).
	if codeMarker(lower, channelRejectedCode) || core.ContainsAny(lower, channelRejectedMarkers) {
		return &core.Failure{Kind: core.FailureKindChannelRejected, Status: http.StatusBadRequest,
			Code: channelRejectedCode, Msg: channelRejectedMessage(body)}
	}
	// 1) Insufficient balance (explicit 402).
	if status == http.StatusPaymentRequired {
		return &core.Failure{Kind: core.FailureKindHardCredit, Status: status, Msg: body}
	}
	// 2) Session invalid.
	if core.ContainsAny(lower, sessionDeadMarkers) {
		return &core.Failure{Kind: core.FailureKindSessionDead, Status: status, Msg: body}
	}
	// 3) Account-level failure (Note: do not determine by 11140—this code also carries model-level rate limit messages).
	if core.ContainsAny(lower, accountFaultMarkers) {
		return &core.Failure{Kind: core.FailureKindAccountFault, Status: status, Msg: body}
	}
	// 4) 429 + business code 14018 indicates balance is truly exhausted.
	if status == http.StatusTooManyRequests && hasBusinessCode(body, "14018") {
		return &core.Failure{Kind: core.FailureKindHardCredit, Status: status, Msg: body}
	}
	// 5) General rate limit.
	if status == http.StatusTooManyRequests {
		return &core.Failure{Kind: core.FailureKindSoftRate, Status: status, Msg: body}
	}
	// 6) Balance keyword (after 429).
	if core.ContainsAny(lower, hardMarkers) {
		return &core.Failure{Kind: core.FailureKindHardCredit, Status: status, Msg: body}
	}
	// 7) Rate limit keyword.
	if core.ContainsAny(lower, softRateMarkers) {
		return &core.Failure{Kind: core.FailureKindSoftRate, Status: status, Msg: body}
	}
	// 8) Context exceeded (request-level).
	if (status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge) &&
		core.ContainsAny(body, promptTooLongMarkers) {
		return &core.Failure{Kind: core.FailureKindPromptTooLong, Status: status, Msg: body}
	}
	// 9) Occasional 404.
	if status == http.StatusNotFound {
		return &core.Failure{Kind: core.FailureKindNotFound, Status: status, Msg: body}
	}
	// 10) Upstream failure.
	if status >= 500 {
		return &core.Failure{Kind: core.FailureKindServer, Status: status, Msg: body}
	}
	// 11) WAF block: 403 and no business envelope (indicating it is not returned by upstream business).
	if IsWAFBlocked(status, body) {
		return &core.Failure{Kind: core.FailureKindWAFBlock, Status: status, Msg: body}
	}
	// 12) Invalid image.
	if status == http.StatusBadRequest && (codeMarker(lower, "11135") || core.ContainsAny(lower, invalidImageMarkers)) {
		return &core.Failure{Kind: core.FailureKindImageInvalid, Status: status, Msg: body}
	}
	// 13) Content policy block.
	if status >= 400 && core.ContainsAny(lower, contentBlockedMarkers) {
		return &core.Failure{Kind: core.FailureKindContentBlocked, Status: status, Msg: body}
	}
	// 14) Invalid parameter.
	if status >= 400 && (strings.Contains(lower, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode)) {
		return &core.Failure{Kind: core.FailureKindBadParams, Status: status, Msg: body}
	}
	// 15) Other 4xx.
	if status >= 400 {
		return &core.Failure{Kind: core.FailureKindClient, Status: status, Msg: body}
	}
	return &core.Failure{Kind: core.FailureKindNone, Status: status, Msg: body}
}

// isModelBlocked determines whether it is "this backend does not have this model" (11102).
//
// Only compare **isolated fields** (code / errCode / error_code with msg / message),
// traversing the top-level and error sub-object two layers, never doing whole-text substring matching.
func isModelBlocked(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		return false
	}
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(body, modelBlockMsgMarker) {
		// Lightweight precheck short-circuit: No need to parse JSON if the signature string is missing.
		return false
	}
	var fields map[string]any
	if errUnmarshal := json.Unmarshal([]byte(body), &fields); errUnmarshal != nil {
		return false
	}
	if objectHasModelBlock(fields) {
		return true
	}
	if nested, okNested := fields["error"].(map[string]any); okNested {
		return objectHasModelBlock(nested)
	}
	return false
}

const modelBlockMsgMarker = "service info not found"

// objectHasModelBlock checks whether the code/msg fields in a JSON object indicate that the model is unavailable.
func objectHasModelBlock(fields map[string]any) bool {
	for _, key := range []string{"code", "errCode", "error_code"} {
		if value, okValue := fields[key]; okValue {
			if fmt.Sprintf("%v", value) == modelBlockCode {
				return true
			}
		}
	}
	for _, key := range []string{"msg", "message"} {
		if value, okValue := fields[key].(string); okValue &&
			strings.Contains(strings.ToLower(value), modelBlockMsgMarker) {
			return true
		}
	}
	return false
}

// channelRejectedMessage folds the 11128 body into a readable, actionable error message.
//
// Why fold: The upstream body carries three copies of Simplified Chinese, English, and Traditional Chinese for msg / displayMsg / displayTips simultaneously,
// and raw passthrough would bury "why it was blocked and who to align with" in JSON, leaving users with just a bunch of 500s.
// After folding, only three segments remain: upstream English msg (searchable), user-facing Chinese prompt, and requestId.
func channelRejectedMessage(body string) string {
	var payload struct {
		Msg         string            `json:"msg"`
		RequestID   string            `json:"requestId"`
		DisplayMsg  map[string]string `json:"displayMsg"`
		DisplayTips map[string]string `json:"displayTips"`
	}
	text := ""
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal == nil {
		text = strings.TrimSpace(payload.Msg)
		if hint := localizedText(payload.DisplayTips, payload.DisplayMsg); hint != "" {
			if text != "" {
				text += " — "
			}
			text += hint
		}
	}
	if text == "" {
		// The body is not a parsable business envelope (HTML interception page, etc.): Pass through the truncated raw text without swallowing evidence.
		text = core.Truncate(body, 512)
	}
	if text == "" {
		text = "channel verification failed"
	}
	trace := "code=" + channelRejectedCode
	if requestID := strings.TrimSpace(payload.RequestID); requestID != "" {
		trace += " requestId=" + requestID
	}
	return text + " (" + trace + ")"
}

// localizedText gets the first non-empty text according to "Chinese > English > Traditional"; tips take precedence over msg.
func localizedText(tips, msg map[string]string) string {
	for _, source := range []map[string]string{tips, msg} {
		for _, key := range []string{"zh", "en", "zh-hant"} {
			if text := strings.TrimSpace(source[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

// IsWAFBlocked determines if it is a WAF block.
//
// Criterion: 403 and no business envelope marker ("code": or "msg":) in the body.
// Business errors will have these two keys even with a 403.
func IsWAFBlocked(status int, body string) bool {
	if status != http.StatusForbidden {
		return false
	}
	return !strings.Contains(body, `"code":`) && !strings.Contains(body, `"msg":`)
}

// ParseRateReset parses the reset time given by the upstream from the error body.
//
// Chinese text takes precedence (more common in CN domains), falling back to English format. Returns zero value if neither can be retrieved.
func ParseRateReset(body string) time.Time {
	if match := cnResetRe.FindStringSubmatch(body); len(match) == 2 {
		if parsed, errParse := time.ParseInLocation(softRateTimeLayout, strings.TrimSpace(match[1]), softRateResetLoc); errParse == nil {
			return parsed
		}
	}
	if match := enResetRe.FindStringSubmatch(body); len(match) == 2 {
		if parsed, errParse := time.ParseInLocation(softRateTimeLayout, strings.TrimSpace(match[1]), softRateResetLoc); errParse == nil {
			return parsed
		}
	}
	return time.Time{}
}

// ParseRetryAfter parses the upstream-suggested wait time from the response headers.
//
// Candidate headers and semantics: Retry-After (seconds), Retry-After-Ms (milliseconds),
// X-Ratelimit-Reset (epoch seconds, >=12 digits interpreted as milliseconds).
// HTTP-Date formats and values exceeding 2 hours are ignored (upstream occasionally returns absurd values).
func ParseRetryAfter(header http.Header, now time.Time) time.Duration {
	if header == nil {
		return 0
	}
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if seconds, errAtoi := parseInt64(raw); errAtoi == nil && seconds > 0 {
			return clampRetryAfter(time.Duration(seconds) * time.Second)
		}
	}
	if raw := strings.TrimSpace(header.Get("Retry-After-Ms")); raw != "" {
		if millis, errAtoi := parseInt64(raw); errAtoi == nil && millis > 0 {
			return clampRetryAfter(time.Duration(millis) * time.Millisecond)
		}
	}
	if raw := strings.TrimSpace(header.Get("X-Ratelimit-Reset")); raw != "" {
		if value, errAtoi := parseInt64(raw); errAtoi == nil && value > 0 {
			if len(raw) >= 12 {
				value /= 1000
			}
			resetAt := time.Unix(value, 0)
			if resetAt.After(now) {
				return clampRetryAfter(resetAt.Sub(now))
			}
		}
	}
	return 0
}

// clampRetryAfter clamps the wait duration to a reasonable range.
func clampRetryAfter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > retryAfterSanityCap {
		return retryAfterSanityCap
	}
	return d
}

// parseInt64 parses a decimal integer (upstream retry headers sometimes contain decimals, but here we only accept integers).
func parseInt64(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

// codeMarker checks whether a bare business code appears in the body (covering various JSON whitespace/quote patterns).
func codeMarker(lower, code string) bool {
	if strings.Contains(lower, `"code":`+code) ||
		strings.Contains(lower, `"code": `+code) ||
		strings.Contains(lower, `"code":"`+code+`"`) ||
		strings.Contains(lower, `"code": "`+code+`"`) {
		return true
	}
	return strings.Contains(lower, `code=`+code)
}

// hasBusinessCode recursively checks whether the body contains a code field that exactly equals want.
func hasBusinessCode(body, want string) bool {
	var payload any
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal != nil {
		return false
	}
	return scanBusinessCode(payload, want)
}

func scanBusinessCode(value any, want string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if code, okCode := typed["code"]; okCode {
			if fmt.Sprintf("%v", code) == want {
				return true
			}
		}
		for _, nested := range typed {
			if scanBusinessCode(nested, want) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if scanBusinessCode(item, want) {
				return true
			}
		}
	}
	return false
}
