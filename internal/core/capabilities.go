package core

// This file defines the "optional capability interfaces" and shared helpers.
//
// Design principles (Go-style interface segregation):
//   - Vendor retains only the capabilities that every provider must have;
//   - Login/refresh/quota/check-in/task/config/storage merging capabilities are each a small interface;
//   - Callers use type assertions to fetch capabilities as needed, and unsupported providers **need not write anything**;
//   - Simulating Java's default methods with "empty implementation + constant false" is prohibited—that makes
//     "unsupported" and "forgot to implement" indistinguishable.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"free-model-hub/cpasdk/pluginapi"
)

// Meta is the identity information of a vendor adapter (embedded into the adapter to replace the old BaseVendor).
//
// Field names differ from interface method names (VendorID/ID, Prefix/ModelPrefix): Go does not allow
// fields and methods with the same name to coexist.
type Meta struct {
	VendorID     string
	VendorName   string
	VendorRegion string
	// Prefix is the ID prefix when registering models, empty means bare name.
	Prefix string
}

// ID returns the vendor instance identifier (also the credential file name prefix).
func (m *Meta) ID() string { return m.VendorID }

// Name returns the display name for the management console.
func (m *Meta) Name() string { return m.VendorName }

// Region returns the region identifier (cn / global; empty if no region concept).
func (m *Meta) Region() string { return m.VendorRegion }

// ModelPrefix returns the model registration prefix.
func (m *Meta) ModelPrefix() string { return m.Prefix }

// ModelsProvider is implemented by providers where "available models returned by credential" differs from the static list.
//
// When not implemented, callers fall back to StaticModels (for the vast majority of providers, both share the same source).
type ModelsProvider interface {
	ModelsForAuth(ctx context.Context, cred *Credential) ([]pluginapi.ModelInfo, error)
}

// LoginProvider is implemented by providers that offer multi-step login (device code / OAuth).
//
// **Implementing it indicates login support**: the console displays the "Add Account" entry accordingly.
// Pure API key providers simply do not implement it, eliminating the need to return empty methods for "unsupported".
type LoginProvider interface {
	LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error)
	LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error)
}

// LoginSessionOwner is implemented by providers whose "session table is self-held and not registered in public LoginSessions".
//
// When the root layer routes login requests by state, it first checks the public registry; if not found, it asks each provider
// individually "is this session yours". The Trae family stores sessions in its own SessionStore (callback monitoring and
// instance isolation require this) without registering them in the public table; lacking this self-verification layer,
// its polling would be rejected by the routing layer as "session does not exist"—causing login to fail halfway with "login session does not exist or has expired".
type LoginSessionOwner interface {
	OwnsLoginSession(sessionID string) bool
}

// CallbackSubmitResult is the processing result of "pasting a localhost callback link".
//
// It intentionally avoids reusing pluginapi.AuthLoginPollResponse: that structure is a response to the host contract,
// and lacks a place for the "upstream login page that the user still needs to click open" (CodeArts' callback carries it).
type CallbackSubmitResult struct {
	// Status is the login status after this submission (success / still waiting / failure).
	Status pluginapi.AuthLoginStatus
	// Message is the progress or error description shown to the user.
	Message string
	// Auth is the credential record produced upon successful login.
	Auth pluginapi.AuthData
	// RedirectURL is the upstream login page that the user still needs to open in a new tab (dedicated to CodeArts).
	RedirectURL string
}

// CallbackSubmitter is implemented by providers where "upstream recognizes only localhost callbacks".
//
// After successful upstream authorization, the browser redirects to 127.0.0.1. When the plugin and browser are not on the same machine
// (remote host, Docker, WSL), this callback can never arrive, and login will remain stuck waiting. Such providers hand
// the callback link as-is to the user: the console displays the local callback address, and the user pastes back the link where
// the browser stopped. The plugin parses it locally to complete the login, rather than blindly waiting for an HTTP request that will never arrive.
type CallbackSubmitter interface {
	// LocalCallbackHint returns the localhost callback address that the page should display (for user recognition).
	LocalCallbackHint() string
	// SubmitCallback parses the callback link pasted by the user and advances the current login session.
	SubmitCallback(ctx context.Context, state, rawURL string) (*CallbackSubmitResult, error)
}

// SMSLoginProvider is implemented by providers that support "phone number + SMS verification code" login.
//
// Two-step interaction differs from the LoginProvider's "initiate/poll" model: the verification code requires the user to read it
// from their phone and enter it back, and the console renders a dedicated two-step form accordingly (send code -> enter code to log in).
// Implementing it means the console "Add Account" entry displays the phone number form.
type SMSLoginProvider interface {
	// SendLoginCode sends the SMS verification code and returns the associated key for the current login session
	// (subsequent VerifyLoginCode must pass it back; AutoClaw uses deviceId).
	SendLoginCode(ctx context.Context, phone string) (sessionKey string, err error)
	// VerifyLoginCode completes the login using the verification code, returning the persisted credential JSON and display label.
	VerifyLoginCode(ctx context.Context, sessionKey, phone, code string) (storageJSON []byte, label string, err error)
}

// Refresher is implemented by providers that need to validate/refresh credentials.
//
// When not implemented, the caller considers it "validated and no write-back needed" (refreshed=false).
type Refresher interface {
	Refresh(ctx context.Context, cred *Credential) (updated *Credential, refreshed bool, err error)
}

// QuotaProvider is implemented by providers whose upstream offers quota queries.
type QuotaProvider interface {
	Quota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error)
}

// AuthModeReporter is implemented by providers where "the credential itself is sensitive material".
//
// Currently only apikey-type providers need this: the page uses it for masked display.
type AuthModeReporter interface {
	AuthMode() string
}

// StorageMerger is implemented by providers that need to merge new credentials back into the original file after a refresh.
type StorageMerger interface {
	MergeStorageJSON(original []byte, credential *Credential) ([]byte, error)
}

// Configurable is implemented by instances that need to read provider-specific configurations.
//
// The root layer only parses global keys, leaving the remaining raw keys to be interpreted by each provider;
// raw is key -> raw string, and implementations must tolerate missing keys and nil maps.
type Configurable interface {
	Configure(settings Settings, raw map[string]string)
}

// CreditsProvider is implemented by providers that can supply model billing multipliers.
//
// Returns "official model ID -> display text" (e.g., "0.08X", "Free"). There is no multiplier
// field in the host contract, so the console can only obtain it through this capability; if not implemented, it is considered "unknown" and the page displays "—".
//
// Made into a separate narrow interface instead of stuffing it into ConsoleModelProvider: multipliers and "what the model list looks like"
// are two separate things; providers only need to supply their unique dimension of information, while the rest of the details are derived from the Catalog.
type CreditsProvider interface {
	ModelCredits() map[string]string
}

// FormatRate normalizes various upstream raw multiplier strings (such as "x0.71 credits", "x0.05", "0.5")
// into a uniform "0.50X" style that always retains two decimal places; non-numeric text (such as "Free", "Subscription") is kept as is;
// Negative numbers and NaN/Inf are considered "unknown" and return an empty string (upstream commonly uses them to indicate missing data).
func FormatRate(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Strip redundant units like credits / credit
	lower := strings.ToLower(s)
	clean := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(lower, "credits"), "credit"))
	clean = strings.TrimPrefix(clean, "x")
	clean = strings.TrimSuffix(clean, "x")
	clean = strings.TrimSpace(clean)
	if num, err := strconv.ParseFloat(clean, 64); err == nil {
		// Negative multipliers do not exist; NaN/Inf are placeholders written by upstream for missing data, which would display
		// as a visible fake multiplier if shown. Both cases are treated uniformly as "unknown".
		if math.IsNaN(num) || math.IsInf(num, 0) || num < 0 {
			return ""
		}
		return fmt.Sprintf("%.2fX", num)
	}
	return s
}

// ConsoleModel is the model display detail provided by the provider (used by the console model page).
type ConsoleModel struct {
	ID              string
	Name            string
	Description     string
	ContextLength   int64
	MaxOutputTokens int64
	Efforts         []string
	DefaultEffort   string
	SupportsImages  bool
	SupportsTools   bool
	Credits         string
}

// ConsoleModelsFromInfo converts the registry list into console display entries, supplementing multipliers by official model ID.
//
// Providers that only supply a multiplier table can use this to complete integration: detail fields come entirely from the registry list without storing
// a separate set of model metadata, thereby avoiding discrepancies between two places over time.
func ConsoleModelsFromInfo(models []pluginapi.ModelInfo, credits map[string]string) []ConsoleModel {
	out := make([]ConsoleModel, 0, len(models))
	for _, model := range models {
		entry := ConsoleModel{
			ID:              model.ID,
			Name:            FirstNonEmpty(model.DisplayName, model.Name),
			Description:     model.Description,
			ContextLength:   model.ContextLength,
			MaxOutputTokens: model.MaxCompletionTokens,
			SupportsImages:  ModelSupportsImages(model),
			SupportsTools:   ModelSupportsTools(model),
			Credits:         credits[model.ID],
		}
		if model.Thinking != nil {
			entry.Efforts = model.Thinking.Levels
			if len(model.Thinking.Levels) > 0 {
				entry.DefaultEffort = model.Thinking.Levels[0]
			}
		}
		out = append(out, entry)
	}
	return out
}

// ModelCacheLoader is implemented by providers that need to warm up their own model catalogs.
//
// Caches in state.json are distinguished by VendorID; Manager passes the corresponding entry to
// the provider at startup, preventing the root layer from directly depending on a specific provider package.
type ModelCacheLoader interface {
	LoadModelCache(cached []CachedModel)
}

// ConsoleModelProvider is implemented by providers that need to supplement display details for the console.
//
// When not implemented, the console directly uses the registry list cached in the Catalog (model ID, context, output limit,
// capabilities, etc., can all be derived from pluginapi.ModelInfo), just without fields outside the host contract such as multipliers.
type ConsoleModelProvider interface {
	ConsoleModels() []ConsoleModel
}

// As asserts the provider into an optional capability interface.
//
// Usage: if quota, ok := core.As[core.QuotaProvider](vendor); ok { ... }
func As[T any](vendor Vendor) (T, bool) {
	value, okCapability := any(vendor).(T)
	return value, okCapability
}

// ModelsFor returns models available for a credential: optional implementation takes precedence, otherwise falls back to StaticModels.
//
// Returns an empty list when cred is empty (the host will not register models for empty credentials).
func ModelsFor(vendor Vendor, ctx context.Context, cred *Credential) ([]pluginapi.ModelInfo, error) {
	if provider, okProvider := As[ModelsProvider](vendor); okProvider {
		return provider.ModelsForAuth(ctx, cred)
	}
	if cred == nil {
		return nil, nil
	}
	return vendor.StaticModels(ctx)
}

// RefreshVendor validates/refreshes credentials: a vendor without a Refresher is considered already validated.
func RefreshVendor(vendor Vendor, ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if refresher, okRefresher := As[Refresher](vendor); okRefresher {
		return refresher.Refresh(ctx, cred)
	}
	return cred, false, nil
}

// EstimateTokens estimates tokens by byte count (conservative, tends to overestimate).
func EstimateTokens(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	tokens := len(payload) / 3
	if tokens < 1 {
		return 1
	}
	return tokens
}

// QuotaUnsupported is a unified error for "upstream has no quota interface".
func QuotaUnsupported(vendor Vendor) error {
	return NewPluginError("quota_unsupported",
		vendor.Name()+" upstream does not provide a quota query interface (quota is subject to the subscription page)", http.StatusNotImplemented)
}

// LoginUnsupported is a unified error for "upstream has no login flow".
func LoginUnsupported(vendor Vendor) error {
	return NewPluginError("login_unsupported",
		vendor.Name()+" does not support login: please use the 'Add API Key' method", http.StatusNotImplemented)
}

// ConfigFieldsProvider is implemented by vendors that need to declare YAML configuration fields.
type ConfigFieldsProvider interface {
	ConfigFields() []pluginapi.ConfigField
}

// ── Plan Claiming ────────────────────────────────────────────────────────────────
//
// Some vendors' "plan claiming" requires promotional plans issued by the upstream, and forcibly requires a CAPTCHA token
// (Alibaba Cloud stealth verification for ZCode). Since a solver is not suitable to be embedded into the plugin process, the contract only requires the
// vendor to provide **SDK initialization parameters**. The management page renders the control in the user's browser, and after passing, the user
// passes the token back to the plugin for forwarding.
//
// Three methods combined into one interface: they describe the same manual claim link, and are always implemented together
// and called together (the preview page needs SDK parameters to render the control, and the claim needs the preview results to select a plan).
// Splitting them into three small interfaces would only force the call site to write type assertions twice.

// LoginVariant is an entry point that the console needs to render when "the same vendor has multiple OAuth identity sources".
//
// The ID will be put into metadata["oauth_variant"] of LoginStart, to be interpreted by the vendor itself
// (AutoClaw is zai / google); CallbackHint is the localhost callback address where this variant will pause in the browser,
// and the page uses this to prompt the user which link to paste back.
type LoginVariant struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	CallbackHint string `json:"callback_hint,omitempty"`
}

// LoginVariantProvider is implemented by vendors with "multiple login variants".
//
// Implementation means the console should render these variants as buttons; if not implemented, it is still treated as a single login entry.
type LoginVariantProvider interface {
	LoginVariants() []LoginVariant
}

// LoginCaptchaProvider is implemented by vendors where "CAPTCHA verification must be passed in the browser before login".
//
// Difference between login and plan claiming: plan claiming gets a token and immediately claims; getting a token for login is only a prerequisite
// for obtaining the authorization URL, followed by "browser login -> callback link pasting -> code exchange". Therefore, it is a separate capability,
// rather than reusing PlanClaimer (which would force vendors to implement irrelevant PreviewPlans/ClaimPlan).
type LoginCaptchaProvider interface {
	// LoginCaptchaConfig returns control initialization parameters (currently Alibaba Cloud scene ID/region/prefix).
	LoginCaptchaConfig(ctx context.Context) (CaptchaConfig, error)
}

// CaptchaConfig is the initialization parameter for the CAPTCHA SDK.
//
// The frontend renders the control based on this, and passes back VerifyParam as is after getting the token; an empty Region indicates that the upstream
// did not issue a region, in which case the Verify-Region header is omitted.
type CaptchaConfig struct {
	Enabled bool   `json:"enabled"`
	SceneID string `json:"scene_id"`
	Region  string `json:"region"`
	Prefix  string `json:"prefix"`
}

// PlanGrant is a quota granted by a plan (usually token quota for a model).
type PlanGrant struct {
	Model  string  `json:"model"`
	Units  float64 `json:"units"`
	Period string  `json:"period,omitempty"`
}

// PlanInfo is a claimable plan.
type PlanInfo struct {
	PlanID      string      `json:"plan_id"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Priority    int         `json:"priority,omitempty"`
	Grants      []PlanGrant `json:"grants,omitempty"`
	StartsAt    int64       `json:"starts_at,omitempty"`
	EndsAt      int64       `json:"ends_at,omitempty"`
}

// ClaimPlanRequest is a plan claim request.
//
// An empty PlanID indicates that the vendor should pick the unclaimed plan with the highest priority; VerifyParam is the token obtained
// by the user after completing CAPTCHA verification in the browser.
type ClaimPlanRequest struct {
	PlanID       string `json:"plan_id"`
	VerifyParam  string `json:"verify_param"`
	VerifyRegion string `json:"verify_region,omitempty"`
}

// ClaimPlanResponse is the result of a successful claim.
type ClaimPlanResponse struct {
	PlanID   string `json:"plan_id"`
	PlanName string `json:"plan_name,omitempty"`
	StartsAt int64  `json:"starts_at,omitempty"`
	EndsAt   int64  `json:"ends_at,omitempty"`
}

// PlanClaimer is implemented by vendors offering promotional plans (currently only ZCode's Coding Plan).
//
// When not implemented, the management page does not display the claim entry - claims are not allowed for vendors without the concept of plans.
type PlanClaimer interface {
	// CaptchaConfig provides the initialization parameters for the CAPTCHA control.
	CaptchaConfig(ctx context.Context) (CaptchaConfig, error)
	// PreviewPlans returns the plans currently claimable by the account (already excluding those already held).
	// An empty slice indicates none are claimable, and the page disables the button to "Claimed" based on this.
	PreviewPlans(ctx context.Context, cred *Credential) ([]PlanInfo, error)
	// ClaimPlan claims the specified plan; when req.PlanID is empty, the provider selects one itself.
	ClaimPlan(ctx context.Context, cred *Credential, req ClaimPlanRequest) (ClaimPlanResponse, error)
}

// ActivityProbeResult is the result of a read-only activity probe.
//
// Activities reuses PlanInfo: activities deployed by the upstream are isomorphic to plans (same plan_id, same
// grant details); defining a separate set of fields would only add unnecessary field-by-field mapping. It **does not mean it is claimable**—
// whether it is already held or within the claim window is determined by the provider; this faithfully returns the current upstream deployment.
// Warning explains why this result might be incomplete (e.g., active reporting failure), an empty string indicates a complete trace;
// Warning may also be non-empty when Activities are present.
type ActivityProbeResult struct {
	Activities []PlanInfo `json:"activities"`
	Warning    string     `json:"warning,omitempty"`
}

// ActivityProbe is a read-only activity probing capability, separated from the plan claiming capability.
// Implementations may perform necessary side-effect-free qualification synchronization (such as DAU events) before probing, but must not claim.
type ActivityProbe interface {
	ProbeActivity(ctx context.Context, cred *Credential) (ActivityProbeResult, error)
}

// KeylessProvider is implemented by vendors that serve models without any
// account (OpenCode Zen free lane, Kilo free pool). The app writes the
// returned credential once so the host can route to the vendor; deleting or
// disabling that account in the console opts out for good.
type KeylessProvider interface {
	KeylessCredentialJSON() []byte
}

// KeyInfoProvider is implemented by API-key vendors that can tell the user
// where to create a key; the console shows the link next to the key form.
type KeyInfoProvider interface {
	KeyURL() string
	KeyNote() string
}
