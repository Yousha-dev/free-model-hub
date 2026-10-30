package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// DescribeClaimError translates reward API error codes into plain language.
func DescribeClaimError(message string) string {
	switch {
	case strings.Contains(message, "APIG.0602"):
		return "Credential (STS security token) has expired and cannot be claimed; please log in again or wait for a keep-alive refresh"
	case strings.Contains(message, "APIG.0301"), strings.Contains(message, "APIG.0303"):
		return "Signature verification failed, credential may be invalid"
	case strings.Contains(message, "4006"):
		return "Today's reward quota has been exhausted"
	case strings.Contains(message, "4004"):
		return "Reward benefit does not exist or has expired"
	case strings.Contains(message, "4003"):
		return "Account has been disabled"
	}
	return message
}

// FetchQuota queries the quota and usage of daily free reward tokens.
//
// Report only the numbers from the reward channel: subscription points are not supported by this plugin, nor should a hardcoded
// "status normal" be used to fake a real quota.
//
// Metric keys must use these **standard keys**: remaining / total / used. The console's "Remaining / Total"
// recognizes only them (internal/auth.Totals); custom keys will be ignored, and the column will always show 0/0.
func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return nil, fmt.Errorf("credential is nil or missing ak")
	}

	balance, errBalance := fetchBenefitBalance(ctx, cred)
	if errBalance != nil {
		return nil, errBalance
	}
	if balance == nil {
		// Upstream returns 4004 benefit not found for accounts "without benefit rights". That is an **account state**
		// rather than a plugin failure (the same account's reward model list can be listed normally, and check-in also returns success).
		// Accurately reporting 0/0 and explaining the reason is better than making the whole column show "-- / --" which makes people think the query failed.
		return noBenefitQuota(), nil
	}

	dailyLeft := balance.DailyTokenLimit - balance.DailyTokensUsed
	if dailyLeft < 0 {
		dailyLeft = 0
	}
	monthlyLeft := balance.MonthlyTokenLimit - balance.MonthlyTokensUsed
	if monthlyLeft < 0 {
		monthlyLeft = 0
	}
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "CodeArts Daily Free Reward Tokens",
			TierName: core.FirstNonEmpty(balance.Channel, "benefit"),
			TierID:   "benefit",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "total", Label: "Daily Free Quota", Value: float64(balance.DailyTokenLimit)},
			{Key: "remaining", Label: "Remaining Today", Value: float64(dailyLeft)},
			{Key: "used", Label: "Used Today", Value: float64(balance.DailyTokensUsed)},
			{Key: "benefit_monthly_total", Label: "Monthly Quota", Value: float64(balance.MonthlyTokenLimit)},
			{Key: "benefit_monthly_remaining", Label: "Remaining This Month", Value: float64(monthlyLeft)},
		},
	}, nil
}

// noBenefitQuota is the quota result when the "account has no benefit rights".
func noBenefitQuota() *pluginapi.QuotaFetchResponse {
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "CodeArts Daily Free Reward Tokens",
			TierName: "Not Activated",
			TierID:   "benefit",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "total", Label: "Daily Free Quota (Upstream benefit rights not activated, no queryable quota)", Value: 0},
		},
	}
}

// benefitBalance is the quota data for the reward channel.
type benefitBalance struct {
	Channel           string
	DailyTokenLimit   int64
	DailyTokensUsed   int64
	MonthlyTokenLimit int64
	MonthlyTokensUsed int64
}

// fetchBenefitBalance queries the reward quota.
//
// Returning (nil, nil) means the upstream explicitly returned "this account has no benefit records"—not an error, but an answer.
func fetchBenefitBalance(ctx context.Context, cred *Credential) (*benefitBalance, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, BenefitHost+EpBenefitBalance, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 20*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts benefit balance failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	var envelope struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Result    struct {
			Channel           string `json:"channel"`
			DailyTokenLimit   int64  `json:"daily_token_limit"`
			DailyTokensUsed   int64  `json:"daily_tokens_used"`
			MonthlyTokenLimit int64  `json:"monthly_token_limit"`
			MonthlyTokensUsed int64  `json:"monthly_tokens_used"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode benefit balance: %w", err)
	}
	// 0000 is the success code; 4004 is "no benefit records", neither of which is an error exit.
	if envelope.ErrorCode == "4004" || strings.Contains(envelope.ErrorMsg, "benefit not found") {
		return nil, nil
	}
	if envelope.ErrorCode != "" && envelope.ErrorCode != "0000" {
		return nil, fmt.Errorf("benefit balance %s: %s", envelope.ErrorCode, DescribeClaimError(envelope.ErrorMsg))
	}
	return &benefitBalance{
		Channel:           envelope.Result.Channel,
		DailyTokenLimit:   envelope.Result.DailyTokenLimit,
		DailyTokensUsed:   envelope.Result.DailyTokensUsed,
		MonthlyTokenLimit: envelope.Result.MonthlyTokenLimit,
		MonthlyTokensUsed: envelope.Result.MonthlyTokensUsed,
	}, nil
}
