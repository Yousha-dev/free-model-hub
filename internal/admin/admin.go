// Package admin implements the management interfaces for the plugin: route dispatching, response encapsulation, and resource pages.
//
// Domain data is injected via Deps: the management interface belongs to the ABI layer, but should not inversely depend on package main.
// This allows processing logic for accounts, models, quotas, and settings to be separated from the plugin entry point and facilitates unit testing.
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/auth"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/state"
	"free-model-hub/internal/tasks"
)

const (
	// requestTimeout is the upper limit for upstream calls within the management interface.
	requestTimeout = 3 * time.Minute
	// batchConcurrency is the default upper limit for concurrency in batch operations.
	// Quota queries are concurrent per account; a concurrency of 6 prevents slower accounts at the end (such as Trae) from delaying the entire round.
	batchConcurrency = 6
)

// Identity represents the external identity of the plugin.
type Identity struct {
	ID          string
	DisplayName string
	ProviderKey string
	Version     string
}

// Deps represents the core layer capabilities relied upon by the management interface.
type Deps interface {
	Identity() Identity
	Config() PluginConfig
	State() state.State
	MutateState(func(*state.State))
	Model() *Catalog
	RefreshModels(ctx context.Context) (int, error)
	// SendLoginSMS sends an SMS verification code (vendorID locates the instance implementing SMSLoginProvider).
	SendLoginSMS(ctx context.Context, vendorID, phone string) (sessionKey string, err error)
	// VerifyLoginSMS completes login using the SMS verification code and returns the persisted credentials.
	VerifyLoginSMS(ctx context.Context, vendorID, sessionKey, phone, code string) (fileName, label, storageJSON string, err error)
	// SubmitLoginCallback handles the localhost authorization callback link pasted back by the user.
	SubmitLoginCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error)
	// StartLogin initiates a vendor login (called by the console after completing in-page captcha verification).
	StartLogin(ctx context.Context, vendorID string, metadata map[string]any) (*pluginapi.AuthLoginStartResponse, error)
	// LoginCaptchaConfig retrieves the captcha widget parameters prior to login.
	LoginCaptchaConfig(ctx context.Context, vendorID string) (core.CaptchaConfig, error)
	Reconfigure() error
}

// Service is an instance of the management interface.
type Service struct {
	deps     Deps
	registry *core.Registry
	pool     *auth.Pool
	tasks    *tasks.Service

	quotaMu    sync.RWMutex
	quotaCache map[string]quotaResult

	// quotaRunMu protects quotaRun: non-nil indicates that a round of quota queries is running in the background.
	// Page refreshes cancel their own management RPC, but this background round must run to completion (see quota.go).
	quotaRunMu sync.Mutex
	quotaRun   chan struct{}
}

// New constructs a management Service.
func New(deps Deps, registry *core.Registry, pool *auth.Pool, taskService *tasks.Service) *Service {
	return &Service{
		deps: deps, registry: registry, pool: pool, tasks: taskService,
		quotaCache: map[string]quotaResult{},
	}
}

// Registration returns the management routes and resource page declarations.
func (s *Service) Registration() pluginapi.ManagementRegistrationResponse {
	id := s.deps.Identity()
	prefix := "/plugins/" + id.ID
	return pluginapi.ManagementRegistrationResponse{
		Routes: s.managementRoutes(prefix),
		Resources: []pluginapi.ResourceRoute{{
			Path:        "/console",
			Menu:        id.DisplayName,
			Description: "FreeTier account, task, and quota management page.",
		}},
	}
}

func (s *Service) managementRoutes(prefix string) []pluginapi.ManagementRoute {
	routes := []pluginapi.ManagementRoute{
		{Method: "GET", Path: prefix + "/status", Description: "Account, quota, and task status overview."},
		{Method: "GET", Path: prefix + "/vendors", Description: "List enabled vendors (for account addition dropdown and grouped display)."},
		{Method: "POST", Path: prefix + "/quotas", Description: "Batch query account quotas."},
		{Method: "POST", Path: prefix + "/accounts/delete", Description: "Delete credential files for the specified account (the host monitors the auth directory and will automatically unregister the account)."},
		{Method: "POST", Path: prefix + "/accounts/enabled", Description: "Batch enable/disable accounts (writes the disabled flag on credentials without deleting files or modifying tokens)."},
		{Method: "POST", Path: prefix + "/accounts/add", Description: "Add API Key type accounts."},
		{Method: "POST", Path: prefix + "/accounts/alias", Description: "Set/clear custom display alias for an account (alias takes precedence over upstream nickname in display)."},
		{Method: "GET", Path: prefix + "/models", Description: "Read the currently registered model list."},
		{Method: "POST", Path: prefix + "/models/toggle", Description: "Batch disable/enable models."},
		{Method: "POST", Path: prefix + "/models/alias", Description: "Set/clear external alias for models (allows routing merging for identically named models across different providers)."},
		{Method: "POST", Path: prefix + "/models/refresh", Description: "Fetch model list from upstream and cache it."},
		{Method: "GET", Path: prefix + "/logs", Description: "Read plugin logs (incremental)."},
		{Method: "POST", Path: prefix + "/settings", Description: "Update plugin runtime settings (auto check-in, task scheduling, prompts)."},
		{Method: "POST", Path: prefix + "/keepalive", Description: "Manually execute token keep-alive."},
		{Method: "POST", Path: prefix + "/plans/preview", Description: "Query currently claimable plans and captcha widget parameters for the account."},
		{Method: "POST", Path: prefix + "/activities/preview", Description: "Read-only probe of the account's current activity eligibility without executing claims."},
		{Method: "POST", Path: prefix + "/plans/claim", Description: "Submit captcha token and claim the plan."},
		{Method: "POST", Path: prefix + "/login/callback", Description: "Submit the localhost authorization callback link pasted by the user (login fallback under remote deployment)."},
		{Method: "POST", Path: prefix + "/login/start", Description: "Initiate a vendor login (can carry in-page captcha verification token)."},
		{Method: "GET", Path: prefix + "/login/captcha-config", Description: "Read captcha widget parameters prior to login (Aliyun scenario ID/region/prefix)."},
		{Method: "POST", Path: prefix + "/login/sms/send", Description: "Send SMS verification code (for vendors supporting mobile phone verification code login)."},
		{Method: "POST", Path: prefix + "/login/sms/verify", Description: "Complete login with SMS verification code and save the account."},
	}
	return routes
}

// Handle is the main dispatcher for management APIs.
func (s *Service) Handle(request []byte) ([]byte, error) {
	if s.deps == nil {
		return nil, core.NewPluginError("admin_not_configured", "management deps are not configured", http.StatusInternalServerError)
	}
	var rpc pluginapi.ManagementRequest
	if len(request) > 0 {
		if errDecode := json.Unmarshal(request, &rpc); errDecode != nil {
			return nil, core.NewPluginError("invalid_request", "decode request: "+errDecode.Error(), http.StatusBadRequest)
		}
	}
	method := strings.ToUpper(strings.TrimSpace(rpc.Method))
	if method == "" {
		method = http.MethodGet
	}
	id := s.deps.Identity()

	switch {
	case method == http.MethodGet && matchesResourcePath(rpc.Path, id.ID, "/console"):
		return core.OKEnvelope(HTMLResponse(http.StatusOK, []byte(Page("/plugins/"+id.ID))))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/status"):
		return core.OKEnvelope(JSONResponse(http.StatusOK, s.buildStatusPayload(rpc)))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/vendors"):
		return core.OKEnvelope(JSONResponse(http.StatusOK, s.buildVendorsPayload()))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/logs"):
		return core.OKEnvelope(JSONResponse(http.StatusOK, s.buildLogsPayload(rpc)))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/quotas"):
		return core.OKEnvelope(s.handleQuotasRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/accounts/delete"):
		return core.OKEnvelope(s.handleAccountDeleteRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/accounts/enabled"):
		return core.OKEnvelope(s.handleAccountEnabledRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/accounts/add"):
		return core.OKEnvelope(s.handleAccountAddRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/accounts/alias"):
		return core.OKEnvelope(s.handleAccountAliasRequest(rpc))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/models"):
		return core.OKEnvelope(s.handleModelsList(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/models/toggle"):
		return core.OKEnvelope(s.modelToggleHandler(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/models/alias"):
		return core.OKEnvelope(s.modelAliasHandler(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/models/refresh"):
		ctx, cancel := hostbridge.ManagementContext(rpc, requestTimeout)
		defer cancel()
		count, errRefresh := s.deps.RefreshModels(ctx)
		if errRefresh != nil {
			return core.OKEnvelope(ErrorResponse(errRefresh))
		}
		return core.OKEnvelope(JSONResponse(http.StatusOK, map[string]any{"ok": true, "models": count}))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/settings"):
		return core.OKEnvelope(s.handleSettingsRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/keepalive"):
		return core.OKEnvelope(s.handleKeepaliveRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/plans/preview"):
		return core.OKEnvelope(s.handlePlansPreview(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/activities/preview"):
		return core.OKEnvelope(s.handleActivitiesPreview(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/plans/claim"):
		return core.OKEnvelope(s.handlePlansClaim(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/login/callback"):
		return core.OKEnvelope(s.handleLoginCallbackRequest(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/login/start"):
		return core.OKEnvelope(s.handleLoginStartRequest(rpc))
	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/login/captcha-config"):
		return core.OKEnvelope(s.handleLoginCaptchaConfigRequest(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/login/sms/send"):
		return core.OKEnvelope(s.handleLoginSMSSend(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/login/sms/verify"):
		return core.OKEnvelope(s.handleLoginSMSVerify(rpc))
	}
	return core.OKEnvelope(JSONResponse(http.StatusNotFound, map[string]any{
		"error": "not found", "path": rpc.Path, "method": method,
	}))
}

// matchesManagementPath determines whether the path matches a management sub-path.
func matchesManagementPath(path, pluginID, suffix string) bool {
	normalized := normalizeRequestPath(path)
	prefix := "/plugins/" + pluginID
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == prefix+suffix || normalized == "/v0/management"+prefix+suffix
}

func matchesResourcePath(path, pluginID, suffix string) bool {
	normalized := normalizeRequestPath(path)
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == "/v0/resource/plugins/"+pluginID+suffix
}

// managementSuffix returns the sub-path with the prefix removed.
func managementSuffix(path, pluginID string) (string, bool) {
	normalized := normalizeRequestPath(path)
	for _, prefix := range []string{"/plugins/" + pluginID, "/v0/management/plugins/" + pluginID} {
		if strings.HasPrefix(normalized, prefix) {
			return strings.TrimPrefix(normalized, prefix), true
		}
	}
	return "", false
}

func normalizeRequestPath(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

// HTMLResponse constructs an HTML management response.
func HTMLResponse(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type": []string{"text/html; charset=utf-8"},
			// Pages evolve alongside backend field names: browsers are not allowed to submit renamed keys using old pages.
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	}
}

// decodeRequest decodes a management request body. An empty body remains compatible and is treated as no parameters provided (returning zero values);
// a non-empty body that is not valid JSON returns an error, which callers uniformly respond to with 400,
// preventing malformed requests from continuing to execute with default values.
func decodeRequest[T any](raw []byte) (T, error) {
	var body T
	return decodeRequestInto(raw, body)
}

// decodeRequestInto is the same as decodeRequest, but starts with a non-zero default value:
// only fields present in the request body overwrite the default values, while omitted fields retain base.
func decodeRequestInto[T any](raw []byte, base T) (T, error) {
	if len(raw) == 0 {
		return base, nil
	}
	if errUnmarshal := json.Unmarshal(raw, &base); errUnmarshal != nil {
		var zero T
		return zero, errUnmarshal
	}
	return base, nil
}

// BadJSONRequestResponse constructs a uniform response for malformed JSON requests.
//
// login/callback is the sole exception: the page reads status/message according to the AuthLoginStatus contract,
// which constructs its own 400 of the same shape and does not use this wrapper.
func BadJSONRequestResponse() pluginapi.ManagementResponse {
	return JSONResponse(http.StatusBadRequest, map[string]any{
		"ok": false, "error": "Failed to parse request body, please check the JSON format",
	})
}

// JSONResponse constructs a JSON management response.
func JSONResponse(status int, payload any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		raw = []byte(`{"error":"marshal failed"}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

// ErrorResponse converts an internal error into a JSON response with a status code.
func ErrorResponse(err error) pluginapi.ManagementResponse {
	status := http.StatusInternalServerError
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	if pluginErr, okPlugin := err.(*core.PluginError); okPlugin {
		message = pluginErr.Message
		if pluginErr.HTTPStatus > 0 {
			status = pluginErr.HTTPStatus
		}
	}
	return JSONResponse(status, map[string]any{"error": message, "ok": false})
}

// queryString gets the first non-empty query parameter (returns an empty string if missing).
func queryString(query map[string][]string, key string) string {
	if query == nil {
		return ""
	}
	for _, value := range query[key] {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func queryInt64(query map[string][]string, key string, fallback int64) int64 {
	if query == nil {
		return fallback
	}
	values, okValues := query[key]
	if !okValues || len(values) == 0 {
		return fallback
	}
	parsed, errParse := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
	if errParse != nil {
		return fallback
	}
	return parsed
}

// Shared helpers for use by sub-files.

// hostActiveVendors returns the set of providers with credentials.
func (s *Service) hostActiveVendors(ctx context.Context, callbackID string) map[string]bool {
	return hostbridge.ActiveVendorSet(ctx, callbackID, s.registry)
}
