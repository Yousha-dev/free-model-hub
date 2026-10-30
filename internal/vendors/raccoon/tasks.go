package raccoon

// This file implements RedPanda credential refresh, quota query, daily check-in, and account profile.
//
// Endpoint specifications (measured via Agent2API packet capture):
//   - Refresh: POST {auth_base}/refresh body {"refresh_token":…}, response may
//     rotate refresh_token (empty = not rotated, keep old value);
//   - Points: GET {points_base}/balance (available_points and classifications by source);
//   - Check-in: Daily points are automatically credited by the server at 00:00; client-side
//     claim goes through POST {origin}/api/web/desktop/v1/login/points/grant (idempotent,
//     returns granted:false if already claimed).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// apiHTTPTimeout is the limit for JSON API requests.
const apiHTTPTimeout = 30 * time.Second

// decodeAPIData parses RedPanda's uniform {code,message,data} envelope and maps business codes to core.Failure.
func decodeAPIData[T any](status int, raw []byte, what string) (T, error) {
	var zero T
	var envelope struct {
		Code    *int64          `json:"code"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Details string          `json:"details"`
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return zero, Classify(status, string(raw))
	}
	errMsg := core.FirstNonEmpty(envelope.Message, envelope.Msg, envelope.Details)
	if envelope.Code != nil && *envelope.Code != 0 {
		return zero, ClassifyBusiness(status, *envelope.Code, errMsg)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return zero, nil
	}
	if errUnmarshal := json.Unmarshal(envelope.Data, &zero); errUnmarshal != nil {
		return zero, fmt.Errorf("parse %s data: %w", what, errUnmarshal)
	}
	return zero, nil
}

// RefreshToken exchanges a refresh token for a new ticket (POST {auth_base}/refresh).
//
// Returns new access/refresh tokens; empty refresh indicates the upstream did not rotate it.
func RefreshToken(ctx context.Context, authBase string, cred *Credential) (access, refresh string, expiresAt int64, uid string, err error) {
	if strings.TrimSpace(authBase) == "" {
		return "", "", 0, "", fmt.Errorf("upstream auth base URL is not configured")
	}
	refreshToken := cred.RefreshTokenValue()
	if strings.TrimSpace(refreshToken) == "" {
		return "", "", 0, "", &core.Failure{Msg: "This account lacks a refresh_token and cannot be refreshed. Please log in again", Kind: core.FailureKindClient}
	}
	body, errMarshal := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if errMarshal != nil {
		return "", "", 0, "", fmt.Errorf("marshal refresh request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authBase+RefreshPath, strings.NewReader(string(body)))
	if errReq != nil {
		return "", "", 0, "", fmt.Errorf("build refresh request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", 0, "", &core.Failure{Msg: "refresh: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "read refresh response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", "", 0, "", &core.Failure{Status: http.StatusUnauthorized, Msg: "RedPanda refresh token has expired. Please log in to this account again", Kind: core.FailureKindSessionDead}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, "", Classify(resp.StatusCode, string(raw))
	}
	data, errDecode := decodeAPIData[struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    any    `json:"expires_at"`
		UserID       string `json:"user_id"`
		UID          string `json:"uid"`
	}](resp.StatusCode, raw, "refresh")
	if errDecode != nil {
		return "", "", 0, "", errDecode
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "RedPanda renewal response is missing tokens; old credential was not overwritten", Kind: core.FailureKindServer}
	}
	access = strings.TrimSpace(data.AccessToken)
	refresh = strings.TrimSpace(data.RefreshToken)
	if ms := core.GetUnixSeconds(map[string]any{"expires_at": data.ExpiresAt}, "expires_at"); ms > 0 {
		expiresAt = ms
	}
	uid = core.FirstNonEmpty(data.UserID, data.UID)
	return access, refresh, expiresAt, uid, nil
}

// FetchBalance queries the point balance (GET {points_base}/balance).
func FetchBalance(ctx context.Context, pointsBase string, cred *Credential) (available int64, err error) {
	if strings.TrimSpace(pointsBase) == "" {
		return 0, fmt.Errorf("upstream points base URL is not configured")
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, pointsBase+BalancePath, nil)
	if errReq != nil {
		return 0, fmt.Errorf("build balance request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, &core.Failure{Msg: "balance: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return 0, &core.Failure{Status: resp.StatusCode, Msg: "read balance response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, Classify(resp.StatusCode, string(raw))
	}
	data, errDecode := decodeAPIData[struct {
		AvailablePoints any `json:"available_points"`
	}](resp.StatusCode, raw, "balance")
	if errDecode != nil {
		return 0, errDecode
	}
	return int64(core.IntFromAny(data.AvailablePoints)), nil
}

// FetchUserInfo queries account profile (GET {auth_base}/user_info) to supplement uid / nickname.
func FetchUserInfo(ctx context.Context, authBase string, cred *Credential) (uid, nickname string, err error) {
	if strings.TrimSpace(authBase) == "" {
		return "", "", fmt.Errorf("upstream auth base URL is not configured")
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, authBase+UserInfoPath, nil)
	if errReq != nil {
		return "", "", fmt.Errorf("build user info request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", &core.Failure{Msg: "user info: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return "", "", &core.Failure{Status: resp.StatusCode, Msg: "read user info response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", Classify(resp.StatusCode, string(raw))
	}
	data, errDecode := decodeAPIData[struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}](resp.StatusCode, raw, "user info")
	if errDecode != nil {
		return "", "", errDecode
	}
	return strings.TrimSpace(data.ID), strings.TrimSpace(data.Name), nil
}
