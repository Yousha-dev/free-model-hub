package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"free-model-hub/cpasdk/pluginapi"
	authpkg "free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
)

// This file contains two management endpoints for plan claiming: opening the popup (real-time preview) and submitting the claim.
//
// **Button state is not calculated here**: it hitches a ride on the quota response—quota queries already hit billing/current,
// and taking a quick look at preview can answer "are there any claimable items?", uniformly saved into state
// by handleQuotasRequest (see quota.go). The console refreshes the quota every time it enters the page, so the state automatically stays fresh,
// eliminating the need for extra polling endpoints, caching, or invalidation logic.
//
// The provider side is handled by core.PlanClaimer—this layer does not import any provider packages (enforced by
// tests/architecture_test.go).

// planPreviewRequest is the request body for opening the claim popup.
type planPreviewRequest struct {
	AccountID string `json:"account_id"`
}

// planClaimRequest is the request body for submitting a claim.
type planClaimRequest struct {
	AccountID    string `json:"account_id"`
	PlanID       string `json:"plan_id"`
	VerifyParam  string `json:"verify_param"`
	VerifyRegion string `json:"verify_region,omitempty"`
}

// resolveAccount loads an account based on the account identifier passed from the management page.
//
// The identifier can be an auth id or auth index. Failure reasons are returned as strings (for page prompts),
// and an empty string indicates success. All endpoints that "find capabilities by account" share this location logic.
func (s *Service) resolveAccount(ctx context.Context, callbackID, accountID string) (authpkg.Account, string) {
	identifier := strings.TrimSpace(accountID)
	if identifier == "" {
		return authpkg.Account{}, "Missing account_id"
	}
	loaded, failures := s.pool.Load(ctx, callbackID, authpkg.Options{
		Targets:       []string{identifier},
		EnabledRealms: s.deps.Config().EnabledRealms,
	})
	if len(loaded) == 0 {
		if len(failures) > 0 {
			return authpkg.Account{}, failures[0].Message
		}
		return authpkg.Account{}, "Account does not exist or has been deleted"
	}
	return loaded[0], ""
}

// planTarget is a located claim target: the account's plan capabilities and corresponding credentials.
type planTarget struct {
	Claimer    core.PlanClaimer
	Credential *core.Credential
	AuthID     string
}

// resolvePlanTarget parses the account in the request and retrieves its plan claiming capability.
//
// Preview and claim share this location logic: both follow "find credentials by account ID -> assert capability",
// with only the subsequent action differing. Failure reasons are returned as strings (for page prompts), and an empty string indicates success.
func (s *Service) resolvePlanTarget(ctx context.Context, callbackID, accountID string) (planTarget, string) {
	account, reason := s.resolveAccount(ctx, callbackID, accountID)
	if reason != "" {
		return planTarget{}, reason
	}
	claimer, okClaimer := core.As[core.PlanClaimer](account.Vendor)
	if !okClaimer {
		return planTarget{}, "This provider does not offer plan claiming"
	}
	return planTarget{
		Claimer:    claimer,
		Credential: account.Credential,
		AuthID:     core.FirstNonEmpty(account.Entry.ID, account.Entry.Name),
	}, ""
}

func (s *Service) handleActivitiesPreview(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[planPreviewRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	account, reason := s.resolveAccount(ctx, hostbridge.RequestCallbackID(req), body.AccountID)
	if reason != "" {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": reason})
	}
	probe, okProbe := core.As[core.ActivityProbe](account.Vendor)
	if !okProbe {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": "This provider does not offer campaign detection"})
	}
	result, errProbe := probe.ProbeActivity(ctx, account.Credential)
	if errProbe != nil {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": errProbe.Error()})
	}
	activities := result.Activities
	if activities == nil {
		activities = []core.PlanInfo{}
	}
	payload := map[string]any{"ok": true, "activities": activities}
	if result.Warning != "" {
		payload["warning"] = result.Warning
	}
	return JSONResponse(http.StatusOK, payload)
}

// handlePlansPreview returns the account's **real-time** claimable plans and CAPTCHA control parameters.
//
// It reads no cache: this is the true state at the moment the user opens the popup. Reusing cache would cause a user who just claimed
// to still see the old list.
func (s *Service) handlePlansPreview(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[planPreviewRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	target, reason := s.resolvePlanTarget(ctx, hostbridge.RequestCallbackID(req), body.AccountID)
	if reason != "" {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": reason})
	}

	plans, errPreview := target.Claimer.PreviewPlans(ctx, target.Credential)
	if errPreview != nil {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": errPreview.Error()})
	}
	if plans == nil {
		plans = []core.PlanInfo{}
	}

	payload := map[string]any{"ok": true, "plans": plans}
	if len(plans) > 0 {
		// Return control parameters only when claiming is actually possible: when there is nothing to claim, the page doesn't need to load the captcha script.
		if config, errConfig := target.Claimer.CaptchaConfig(ctx); errConfig == nil {
			payload["captcha"] = config
		}
	}
	return JSONResponse(http.StatusOK, payload)
}

// handlePlansClaim submits the CAPTCHA token and claims the plan.
func (s *Service) handlePlansClaim(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[planClaimRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	target, reason := s.resolvePlanTarget(ctx, hostbridge.RequestCallbackID(req), body.AccountID)
	if reason != "" {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": reason})
	}

	result, errClaim := target.Claimer.ClaimPlan(ctx, target.Credential, core.ClaimPlanRequest{
		PlanID:       strings.TrimSpace(body.PlanID),
		VerifyParam:  strings.TrimSpace(body.VerifyParam),
		VerifyRegion: strings.TrimSpace(body.VerifyRegion),
	})
	if errClaim != nil {
		// Business failures (already claimed/quota exhausted/captcha failed) all carry readable reasons, returned to the page with 200 for pop-up prompts.
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": errClaim.Error()})
	}

	// Both new quota and the "claimed" status go through the same quota pipeline, directly reusing it instead of writing a separate query + persistence.
	s.refreshAccountQuota(req, target.AuthID)

	return JSONResponse(http.StatusOK, map[string]any{
		"ok":        true,
		"message":   "Successfully claimed: " + result.PlanName,
		"plan_id":   result.PlanID,
		"plan_name": result.PlanName,
		"starts_at": result.StartsAt,
		"ends_at":   result.EndsAt,
	})
}

// refreshAccountQuota borrows the quota pipeline to refresh a single account.
//
// Two things need to be done after a successful claim: refresh the balance, and gray out the button to "Claimed". These two things are already
// in the quota pipeline (balance comes from billing/balance, claim status from billing/preview). Reusing it
// is enough—writing a separate query would just duplicate the exact same logic for the third time.
func (s *Service) refreshAccountQuota(req pluginapi.ManagementRequest, authID string) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	body, errMarshal := json.Marshal(accountBatchRequest{AccountIDs: []string{authID}})
	if errMarshal != nil {
		return
	}
	s.handleQuotasRequest(pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    req.Path,
		Headers: req.Headers,
		Body:    body,
	})
}
