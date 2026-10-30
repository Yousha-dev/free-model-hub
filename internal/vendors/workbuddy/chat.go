package workbuddy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"

	_ "embed"
	"regexp"
	"sync"
)

// This file is the CodeBuddy protocol client core: request sending, unified envelope processing, credential refresh,
// device fingerprint derivation, and outbound HTTP client construction.
//
// All outbound requests go through the host HTTP bridge (internal/hostbridge), so the host's proxy,
// account-level proxy, and request logging apply equally to these requests.

const (
	// shortTimeout is the total duration limit for short RPCs (refresh, check-in, balance, model list, task reporting).
	shortTimeout = 120 * time.Second
	// chatHeaderTimeout is the wait limit before the first byte of chat responses.
	chatHeaderTimeout = 120 * time.Second
	// refreshTimeout is the limit for credential refresh requests.
	refreshTimeout = 30 * time.Second
	// apiBodyLimit is the read limit for short RPC response bodies.
	apiBodyLimit = 1 << 20
	// configBodyLimit is the read limit for /v3/config (the response body is significantly larger).
	configBodyLimit = 2 << 20

	// deriveSaltPrefix is the fixed salt prefix for device fingerprint derivation.
	//
	// The fixed prefix ensures the fingerprint is stable across restarts (changing it means a new device); true deployment isolation
	// is provided by installSalt (machine salt): fingerprint spaces for different deployments are mutually isolated.
	deriveSaltPrefix = "wb2a:"

	// authRefreshSource is the value of X-Auth-Refresh-Source for refresh requests.
	authRefreshSource = "plugin"

	// maxRefreshTokenExpiresIn is the reasonable upper limit for expiresIn in refresh responses.
	// Anything beyond this is considered dirty data, preserving the original expiresAt instead of writing an absurd future timestamp.
	maxRefreshTokenExpiresIn = int64(10 * 365 * 24 * 60 * 60)

	// deviceTokenFileTTL is the read cache duration for the device token file.
	deviceTokenFileTTL = 5 * time.Minute
	// deviceTokenFileMaxLen is the maximum length limit for the device token file (exceeding it is considered abnormal).
	deviceTokenFileMaxLen = 1024
)

// Options contains all inputs required to construct a Client.
//
// Intentionally using a struct instead of a long parameter list: fields will continue to grow as upstream changes,
// and positional parameters quickly become unreadable.
type Options struct {
	// Context binds the host callback ID, routing outbound requests into the host's request logs.
	Context context.Context
	// EnabledRealms are the enabled realms (a subset of cn / global).
	EnabledRealms []string
	// PromptMode / PromptText control system prompt processing.
	PromptMode string
	PromptText string
	// OnContentBlocked is the callback when content moderation blocks a request (triggering prompt downgrading).
	OnContentBlocked func()
	// SanitizeFingerprints enables blacklist sanitization for outbound request bodies.
	SanitizeFingerprints bool
	// PassthroughIP determines whether to pass the client IP through to upstream.
	PassthroughIP bool
	// UserAgent explicitly overrides the outbound UA.
	UserAgent string
	// ClientVersion / CLIVersion override the two version segments of the UA.
	ClientVersion string
	CLIVersion    string
	// ClientName overrides the attribution header value.
	ClientName string
	// DeviceToken / DeviceTokenFile are the two-tier fallbacks for the device token.
	DeviceToken     string
	DeviceTokenFile string
	// StateDir is used to store the model list cache.
	StateDir string
}

// Client is the CodeBuddy upstream client.
//
// Construct an instance per request: it is lightweight (just a configuration snapshot + a little cache),
// but must bind a request-level context to allow outbound traffic to enter the host's request logs.
type Client struct {
	rt   *runtime
	opts Options
}

// NewClient constructs an upstream client.
func NewClient(rt *runtime, opts Options) *Client {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	return &Client{rt: rt, opts: opts}
}

// enabledRealm reports whether a realm is configured as enabled.
//
// Accounts on disabled realms never initiate upstream requests: the check is placed at the client layer rather than scattered everywhere,
// ensuring that "disabling global" truly means not sending a single request.
func (c *Client) enabledRealm(region Region) bool {
	if len(c.opts.EnabledRealms) == 0 {
		return true
	}
	for _, item := range c.opts.EnabledRealms {
		if item == string(region) {
			return true
		}
	}
	return false
}

// Three tiers of outbound HTTP clients.

// shortClient returns the short RPC client.
func (c *Client) shortClient() *http.Client {
	return hostbridge.Client(c.opts.Context, shortTimeout)
}

// refreshClient returns the credential refresh client.
func (c *Client) refreshClient() *http.Client {
	return hostbridge.Client(c.opts.Context, refreshTimeout)
}

// chatClient returns the chat client.
//
// Total duration is set to 0: streaming chat duration is unpredictable; time-to-first-byte and idle
// are handled by headerTimeout and in-stream idle monitoring respectively. Using a total timeout would falsely kill long answers.
func (c *Client) chatClient(headerTimeout time.Duration) *http.Client {
	if headerTimeout <= 0 {
		headerTimeout = chatHeaderTimeout
	}
	// The host bridge does not support ResponseHeaderTimeout (determined by the host side),
	// so we use context timeout here to express the upper bound for the first byte: httpx's transport will pass
	// the request context to host.http.do_stream.
	return hostbridge.StreamClient(c.opts.Context, 0)
}

// apiEnvelope is the uniform response envelope from the upstream.
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON initiates a short RPC and unwraps the uniform envelope.
//
// Error handling: HTTP ≥400 goes to Classify; HTTP success but business code non-zero also goes to Classify
// (treating msg as the body). Returns the data portion inside the envelope.
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request %s: %w", req.URL.Path, errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
	if errRead != nil {
		// Half-cut body does not go to Classify: network issues should not be penalized as account issues.
		return nil, fmt.Errorf("read %s response: %w", req.URL.Path, errRead)
	}
	return unwrapEnvelope(resp.StatusCode, string(body), resp.Header)
}

// doJSONWithLimit is the same as doJSON, but uses a larger read limit (required by /v3/config).
func (c *Client) doJSONWithLimit(req *http.Request, limit int64) (json.RawMessage, error) {
	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request %s: %w", req.URL.Path, errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, limit))
	if errRead != nil {
		return nil, fmt.Errorf("read %s response: %w", req.URL.Path, errRead)
	}
	return unwrapEnvelope(resp.StatusCode, string(body), resp.Header)
}

// unwrapEnvelope unwraps "status code + body" into data or a classified error.
func unwrapEnvelope(status int, body string, header http.Header) (json.RawMessage, error) {
	if status >= 400 {
		return nil, enrichClassifyError(status, body, header)
	}
	var envelope apiEnvelope
	if errUnmarshal := json.Unmarshal([]byte(body), &envelope); errUnmarshal != nil {
		// Response is not JSON: indicates the upstream returned unexpected content (HTML error page, etc.).
		return nil, &core.Failure{Kind: core.FailureKindClient, Status: status, Msg: core.Truncate(body, 512)}
	}
	if envelope.Code != 0 {
		return nil, enrichClassifyError(status, body, header)
	}
	return envelope.Data, nil
}

// enrichClassifyError supplements the classification result with the wait information provided by the upstream.
//
// The upstream expresses waiting in two ways: response headers (Retry-After family) and body text ("will reset in X").
// Both are parsed and passed to the caller, which decides the cooldown target time.
func enrichClassifyError(status int, body string, header http.Header) *core.Failure {
	classified := Classify(status, body)
	classified.RetryAfter = ParseRetryAfter(header, time.Now())
	classified.ResetAt = ParseRateReset(body)
	if classified.Kind == core.FailureKindChannelRejected {
		// Channel verification is a newly added policy interception by upstream; the plugin side has no (and should not have) means
		// to forge official client proof. The message returned to the user is folded into plain language, while the full body is kept in logs:
		// requestId is the only evidence for upstream feedback and future protocol difference comparison, and only this copy remains after folding.
		logger.Error("upstream channel verification rejected the request (status=%d): %s",
			status, core.Truncate(body, 1024))
	}
	return classified
}

// refreshTokenResponse is the response of the refresh endpoint.
type refreshTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// DeviceToken resolves the device token with the priority of "account credential > plugin configuration > file".
func (c *Client) DeviceToken(cred *Credential) string {
	if token := cred.DeviceTokenValue(); token != "" {
		return token
	}
	if token := strings.TrimSpace(c.opts.DeviceToken); token != "" {
		return token
	}
	return c.rt.readDeviceTokenFile(c.opts.DeviceTokenFile)
}

// deriveIDWithSalt derives an identifier that is stable across restarts and distinct between accounts.
func deriveIDWithSalt(uid, purpose, salt string) string {
	seed := deriveSaltPrefix + purpose + ":" + uid + ":" + salt
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:18])
}

// buildCacheKey constructs the prompt_cache_key.
//
// This is the single largest cost optimization: hitting the cache for the same long prefix can reduce costs by an order of magnitude.
// The first 8 bits of uid are a **hard isolation factor**—reusing the same cache key across accounts will cause the upstream
// to hit the prefix cache of the wrong account, leaking the other party's conversation content.
func buildCacheKey(uid, conversation string) string {
	uidPrefix := uid
	if len(uidPrefix) > 8 {
		uidPrefix = uidPrefix[:8]
	}
	if uidPrefix == "" {
		uidPrefix = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	return "wb2a-" + uidPrefix + "-" + hex.EncodeToString(sum[:16])
}

// closeBody closes the response body and ignores errors (failure to close the response body does not affect the business result).
func closeBody(body io.Closer) {
	if body != nil {
		_ = body.Close()
	}
}

// This file implements chat invocation: request construction, outbound transmission, error classification, and streaming reads.

// ChatRequest is the input for a chat invocation.
type ChatRequest struct {
	// Credential is the selected account credential.
	Credential *Credential
	// Body is the client's original request body (OpenAI format).
	Body []byte
	// Model is the upstream model name (with realm prefix stripped).
	Model string
	// ClientIP is the client IP (only sent when PassthroughIP is enabled).
	ClientIP string
	// Meta is the session header family.
	Meta ChatMeta
}

// ChatResult is the result of a chat invocation.
type ChatResult struct {
	// Response is the non-streaming aggregated response body (empty during streaming calls).
	Response []byte
	// Stats are the streaming statistics (zero value during non-streaming calls).
	Stats StreamStats
	// Credential is the actual credential used (which may have been refreshed).
	Credential *Credential
}

// ChatStream initiates a streaming chat, returning a readable response body.
//
// The caller is responsible for closing the returned ReadCloser. Upstream enforces streaming (stream in request body is always true),
// making this the sole chat exit point: non-streaming requests are aggregated on this side by Aggregate.
func (c *Client) ChatStream(req ChatRequest) (io.ReadCloser, error) {
	if req.Credential == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := req.Credential.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	// Rewrite pipeline: add stream, normalize fields, inject chain-of-thought, desensitize.
	conversationKey := ConversationKey(req.Body)
	cacheKey := buildCacheKey(req.Credential.UIDValue(), core.FirstNonEmpty(conversationKey, req.Meta.ConversationRequestID))
	defaultEfforts, supportedEfforts := c.rt.effortTables()
	prepared := PrepareBody(req.Body, PrepareOptions{
		Model:            model,
		PromptMode:       c.opts.PromptMode,
		PromptText:       c.opts.PromptText,
		Sanitize:         c.opts.SanitizeFingerprints,
		DefaultEfforts:   defaultEfforts,
		SupportedEfforts: supportedEfforts,
		CacheKey:         cacheKey,
		Global:           region.IsGlobal(),
	})

	endpoints := GetEndpoints(region)
	httpReq, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost,
		endpoints.ChatBase+chatCompletionsPath, strings.NewReader(string(prepared)))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	c.applyChatHeaders(httpReq, req.Credential, req.Meta, req.ClientIP)

	resp, errDo := c.chatClient(chatHeaderTimeout).Do(httpReq)
	if errDo != nil {
		// Transport layer error: not an account issue (does not trigger classification side effects), but retrying with a different account makes sense.
		return nil, transientError(errDo)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
		closeBody(resp.Body)
		errClassified := enrichClassifyError(resp.StatusCode, string(body), resp.Header)
		if errClassified != nil && errClassified.Kind == core.FailureKindContentBlocked && c.opts.OnContentBlocked != nil {
			c.opts.OnContentBlocked()
		}
		return nil, errClassified
	}
	return resp.Body, nil
}

// transientError wraps a transport layer error into a "retryable without penalizing the account" error.
//
// Difference from classification errors: Status is 0, host handles it as 502 and retries with a different account,
// but will not mark the credential as bad because of it (half-baked responses should not penalize the account).
func transientError(err error) *core.Failure {
	if err == nil {
		return nil
	}
	return &core.Failure{Kind: core.FailureKindServer, Status: 0, Msg: err.Error()}
}

// Chat initiates a chat and aggregates it into a non-streaming response.
func (c *Client) Chat(req ChatRequest) (*ChatResult, error) {
	reader, errStream := c.ChatStream(req)
	if errStream != nil {
		return nil, errStream
	}
	defer closeBody(reader)

	aggregated, errAggregate := Aggregate(reader)
	if errAggregate != nil {
		if IsEmptyStreamError(errAggregate) {
			return nil, &core.Failure{Kind: core.FailureKindServer, Status: http.StatusBadGateway,
				Msg: "upstream stream contained no valid data events (upstream_parse)"}
		}
		return nil, errAggregate
	}
	return &ChatResult{Response: aggregated, Credential: req.Credential}, nil
}

// Region is the deployment domain for CodeBuddy.
//
// cn and global are **two independent deployments**: domains, model lists, and reasoning tier tables all differ,
// and **credentials are not shared**—using a cn token against the global domain yields 401 TOKEN_EXPIRE.
type Region string

const (
	// RegionCN is the CN edition (copilot.tencent.com / www.codebuddy.cn).
	RegionCN Region = "cn"
	// RegionGlobal is the Global edition (www.workbuddy.ai).
	RegionGlobal Region = "global"
)

// NormalizeRegion normalizes a string into a valid Region, falling back to cn for unknown values.
func NormalizeRegion(raw string) Region {
	if strings.EqualFold(strings.TrimSpace(raw), string(RegionGlobal)) {
		return RegionGlobal
	}
	return RegionCN
}

// IsGlobal reports whether it is the Global edition.
func (r Region) IsGlobal() bool { return r == RegionGlobal }

// String implements fmt.Stringer.
func (r Region) String() string { return string(r) }

// Provider instance identifier. Also acts as the credential filename prefix (workbuddycn-<uid>.json).
//
// CN and GLOBAL are two independent providers in this plugin: domains, model lists, reasoning tier tables,
// and check-in campaigns all differ, and credentials are not shared. Therefore, they have separate IDs and filename prefixes,
// allowing accounts, models, and auth files to be distinguished by provider.
const (
	// VendorIDCN is the provider identifier for the CN edition.
	VendorIDCN = "workbuddycn"
	// VendorIDGlobal is the provider identifier for the Global edition.
	VendorIDGlobal = "workbuddyglobal"
)

// VendorIDFor returns the provider identifier corresponding to the region.
func VendorIDFor(region Region) string {
	if region.IsGlobal() {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// VendorNameFor returns the display name for the region (for management console and pages use).
func VendorNameFor(region Region) string {
	if region.IsGlobal() {
		return "WorkBuddy Global"
	}
	return "WorkBuddy CN"
}

// Endpoints represents all upstream endpoints for a domain.
//
// Centralizing all URLs into a table and fetching by domain avoids scattered URL concatenation which is a maintenance nightmare,
// and domain switches (cn/global) must occur as a group rather than changing only half.
type Endpoints struct {
	// ChatBase is the chat domain base address (/v2/chat/completions is under this).
	ChatBase string
	// BillingBase is the billing and tracking domain base address (/v2/report, /billing/meter/*).
	BillingBase string
	// WebBase is the Web growth center domain base address (task claim /activity/growth/tasks/<code>/claim).
	WebBase string
	// Origin is the Origin / Referer base address for outbound requests.
	Origin string
	// AcceptLanguage is the default language preference for the domain.
	AcceptLanguage string
	// PlatformLabel is the platform segment in the UA (global must be "WorkBuddy AI", otherwise risk control is triggered).
	PlatformLabel string
}

var (
	endpointsCN = Endpoints{
		// ChatBase aligns with the official 5.8.x product.json endpoint: model requests
		// all route through www.workbuddy.cn starting from Oct 2026 (copilot.tencent.com is merely
		// a fallback constant for the desktop gateway). Billing/growth domains remain unchanged (both domains are currently online).
		ChatBase:       "https://www.workbuddy.cn",
		BillingBase:    "https://www.codebuddy.cn",
		WebBase:        "https://www.workbuddy.cn",
		Origin:         "https://www.codebuddy.cn",
		AcceptLanguage: "zh-CN",
		PlatformLabel:  "WorkBuddy",
	}
	endpointsGlobal = Endpoints{
		ChatBase:       "https://www.workbuddy.ai",
		BillingBase:    "https://www.workbuddy.ai",
		WebBase:        "https://www.workbuddy.ai",
		Origin:         "https://www.workbuddy.ai",
		AcceptLanguage: "en-US",
		PlatformLabel:  "WorkBuddy AI",
	}
)

// GetEndpoints returns the endpoint table corresponding to the region.
func GetEndpoints(region Region) Endpoints {
	if region.IsGlobal() {
		return endpointsGlobal
	}
	return endpointsCN
}

// Conversation domain path.
const (
	// chatCompletionsPath is the only conversation exit.
	//
	// History: global once hit /console/chat/completions first, but due to /console hitting Tencent Cloud WAF
	// body content rules (backticks + printf/whoami and other command execution features resulting in deterministic 403), it was changed to a fixed /v2.
	chatCompletionsPath = "/v2/chat/completions"
	// tokenRefreshPath is the credential refresh endpoint (X-Refresh-Token is only allowed to appear here).
	tokenRefreshPath = "/v2/plugin/auth/token/refresh"
	// v3ConfigPath is the official IDE configuration directory (authoritative source of model capabilities, UA sensitive).
	v3ConfigPath = "/v3/config"
	// enterpriseModelsPathCN is the enterprise model directory (CN fallback path).
	enterpriseModelsPathCN = "/console/enterprises/personal/models"
	// enterpriseModelsPathGlobal is the preferred path for the Global enterprise model directory.
	enterpriseModelsPathGlobal = "/v2/enterprises/personal/models"
)

// Billing domain path.
const (
	billingMeterPath   = "/billing/meter/get-user-resource"
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource"
	// Enterprise usage API: personal resource API returns empty Accounts for enterprise accounts, enterprise quota can only be fetched from here.
	enterpriseUsagePath   = "/billing/meter/get-enterprise-user-usage"
	enterpriseUsagePathV2 = "/v2/billing/meter/get-enterprise-user-usage"
	reportPath            = "/v2/report"
)

// Growth / task domain path (nested under ChatBase).
const ()

// Outbound identity built-in values. Both are allowed to be overridden by configuration.
//
// Version number aligned with official 5.8.3 (UA three-part format taken from daemon.log header dump:
// WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0).
const (
	defaultClientVersion = "5.8.3"
	defaultCLIVersion    = "2.161.0"
	defaultClientName    = "WorkBuddy"
	// DesktopClientVersion is the version used by the desktop fingerprint family (deliberately different from the CLI family).
	// Exported for use by the tasks subpackage to construct desktop fingerprint events.
	DesktopClientVersion = "5.8.3"
	// codeBuddyIDEUA / codeBuddyCLIUA are UA dedicated to model directory probing.
	//
	// /v3/config is UA sensitive: too old will get a streamlined directory or 400 code=12403.
	// Actual tests show both paths have unique models (IDE contains o4-mini/enhance-1.0, CLI contains deepseek family), neither can be omitted.
	codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"
	codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// The following constants are exported for use by package main configuration defaults.
const (
	// DefaultClientVersion is the default version of the WorkBuddy segment in UA.
	DefaultClientVersion = defaultClientVersion
	// DefaultCLIVersion is the default version of the CLI segment in UA.
	DefaultCLIVersion = defaultCLIVersion
	// DefaultClientName is the default value of the ownership header.
	DefaultClientName = defaultClientName
)

// This file constructs four types of outbound request headers.
//
// Why it deserves to be a separate file: upstream risk control is extremely sensitive to request headers -- if the UA platform segment is wrong,
// X-CodeBuddy-Request is missing, or the session header family is incomplete, it will get 403 code=11140 "request illegal",
// and the error message does not point to the real cause at all. Centralized management is the only reliable approach.

// ChatMeta is the session header family associated with a single conversation.
//
// Generated once by the caller **outside the rotation loop**, and reused within the loop: upstream backend aggregates
// the same round of requests by X-Conversation-Request-ID, changing the ID on every retry will split it into multiple sessions.
type ChatMeta struct {
	// ConversationID is the multi-round session ID (can be empty, when empty this header is not sent).
	ConversationID string
	// ConversationRequestID is the round-level aggregation primary key (must be sent).
	ConversationRequestID string
	// MessageID is the single message ID (independent for each).
	MessageID string
}

// applyCommonHeaders writes headers shared by all APIs.
func applyCommonHeaders(req *http.Request, c *Client, cred *Credential) {
	endpoints := GetEndpoints(cred.Realm())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", endpoints.Origin)
	req.Header.Set("Referer", endpoints.Origin+"/")
	req.Header.Set("User-Agent", c.userAgent(cred.Realm()))
	req.Header.Set("Accept-Language", endpoints.AcceptLanguage)
	// This header is one of the basic criteria for upstream to recognize "from official client", missing it will be blocked by risk control.
	req.Header.Set("X-CodeBuddy-Request", "1")

	if uid := cred.UIDValue(); uid != "" {
		req.Header.Set("X-Machine-ID", c.rt.deriveID(uid, "machine"))
		req.Header.Set("X-Session-ID", c.rt.deriveID(uid, "session"))
	}
}

// applyAuthHeaders writes credential-related headers.
//
// When credentials are missing, do not send empty Bearer (which would be treated as "provided invalid credentials"),
// but instead send the upstream-agreed X-No-* marker to let it return a more accurate error.
func applyAuthHeaders(req *http.Request, cred *Credential) {
	if token := cred.AccessTokenValue(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if uid := cred.UIDValue(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
}

// userAgent constructs the three-part UA.
//
// The platform segment in the global domain **must** be "WorkBuddy AI": sending it as "WorkBuddy" will trigger
// Upstream 403 code=11140 "request illegal". The official client does not do any UA randomization,
// so determinism is also maintained here (randomization makes it easier to be flagged by risk control).
func (c *Client) userAgent(region Region) string {
	if override := strings.TrimSpace(c.opts.UserAgent); override != "" {
		return override
	}
	clientVersion := c.opts.ClientVersion
	if clientVersion == "" {
		clientVersion = defaultClientVersion
	}
	cliVersion := c.opts.CLIVersion
	if cliVersion == "" {
		cliVersion = defaultCLIVersion
	}
	platform := GetEndpoints(region).PlatformLabel
	return "WorkBuddy/" + clientVersion + " " + platform + "/" + clientVersion + " CLI/" + cliVersion
}

// applyChatHeaders writes headers specific to chat requests.
func (c *Client) applyChatHeaders(req *http.Request, cred *Credential, meta ChatMeta, clientIP string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	// Official chat requests (CLI direct path) do not carry Origin/Referer/Accept-Language,
	// nor the three device fingerprint headers (X-Device-Token is only used for check-in and auth/state).
	for _, key := range []string{
		"Origin", "Referer", "Accept-Language",
		"X-Device-Token", "X-Machine-ID", "X-Session-ID",
	} {
		req.Header.Del(key)
	}

	req.Header.Set("Accept", "application/json, text/event-stream")

	// Enterprise and domain info: CN and global have different formats, writing them wrong will cause rejection.
	// X-Domain takes the current chat domain host name consistent with the official version (always sent for personal accounts as well).
	if cred.Realm().IsGlobal() {
		req.Header.Set("X-No-Enterprise-Id", "1")
		req.Header.Set("X-Domain", endpointHost(GetEndpoints(RegionGlobal).ChatBase))
	} else {
		if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", enterpriseID)
			req.Header.Set("X-Tenant-Id", enterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		req.Header.Set("X-Domain", endpointHost(GetEndpoints(cred.Realm()).ChatBase))
	}

	applyAttributionHeaders(req, c)
	applyConversationHeaders(req, meta)

	if c.opts.PassthroughIP && strings.TrimSpace(clientIP) != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
		req.Header.Set("X-Real-IP", clientIP)
		req.Header.Set("X-Client-IP", clientIP)
	}
}

// applyAttributionHeaders writes client attribution headers.
//
// Official client always sends: X-Product=SaaS (deploymentType), X-IDE-Type/X-IDE-Name=WorkBuddy,
// X-IDE-Version=app version, X-Agent-Intent=craft, X-Agent-Type=main, X-Private-Data=false.
// When client_name is explicitly configured as "SaaS", restore the old behavior (only send X-Product). Keeping this switch
// is to allow a quick rollback if attribution headers trigger risk control changes.
func applyAttributionHeaders(req *http.Request, c *Client) {
	clientVersion := c.opts.ClientVersion
	if clientVersion == "" {
		clientVersion = defaultClientVersion
	}
	name := strings.TrimSpace(c.opts.ClientName)
	if name == "" {
		name = defaultClientName
	}
	if strings.EqualFold(name, "SaaS") {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", clientVersion)
	req.Header.Set("X-Private-Data", "false")
	req.Header.Set("X-Product", "SaaS")
}

// endpointHost extracts the host name (including port) from the base URL for use by X-Domain.
// The base is a package-level constant and always has a scheme; resolution failure only occurs when constants are modified by mistake, returning an empty string
// to expose the issue explicitly rather than silently falling back to the wrong domain.
func endpointHost(base string) string {
	parsed, errParse := url.Parse(strings.TrimSpace(base))
	if errParse != nil {
		return ""
	}
	return parsed.Host
}

// applyConversationHeaders writes the conversation header family.
//
// Official format (CLI direct path):
//   - X-Conversation-Request-ID: stable per round (UUIDv7-32hex);
//   - X-Conversation-Message-ID / X-Request-ID: new per step (UUIDv7-32hex, both have the same value);
//   - X-Root-Request-ID / X-Parent-Conversation-ID: only present in sub-conversations, not sent for main dialogs;
//   - trace family randomized per request: traceId 32hex, spanId 16hex, parentSpanId 16hex,
//     traceparent/b3/X-B3-*/X-Trace-ID are homological.
//
// This family of headers seems redundant (multiple headers take the same value), but upstream backend uses them for request aggregation and tracing,
// and missing any of them will cause the conversation to be split apart in the backend.
func applyConversationHeaders(req *http.Request, meta ChatMeta) {
	messageID := strings.TrimSpace(meta.MessageID)
	if messageID == "" {
		messageID = NewUUIDv7Hex()
	}
	conversationRequestID := strings.TrimSpace(meta.ConversationRequestID)
	if conversationRequestID == "" {
		conversationRequestID = messageID
	}
	if conversationID := strings.TrimSpace(meta.ConversationID); conversationID != "" {
		req.Header.Set("X-Conversation-ID", conversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", conversationRequestID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)

	// Trace family: fully random and homological to each other (injected by otel in the official version, conversation ID is not reused).
	traceID := core.RandomHex(16)
	spanID := core.RandomHex(8)
	parentSpanID := core.RandomHex(8)
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("b3", traceID+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("X-B3-TraceId", traceID)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("X-Trace-ID", traceID)
}

// applyBillingHeaders writes headers for billing and growth domain requests.
//
// Difference from chat headers: no conversation header family and four attribution headers, UA is a single segment "WorkBuddy/<version>".
func (c *Client) applyBillingHeaders(req *http.Request, cred *Credential) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	// Billing domain uses a single-segment UA; UA is not set when SaaS is explicitly configured (old behavior).
	if name := strings.TrimSpace(c.opts.ClientName); !strings.EqualFold(name, "SaaS") && strings.TrimSpace(c.opts.UserAgent) == "" {
		clientVersion := c.opts.ClientVersion
		if clientVersion == "" {
			clientVersion = defaultClientVersion
		}
		req.Header.Set("User-Agent", "WorkBuddy/"+clientVersion)
	}

	if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}
	if domain := cred.DomainValue(); domain != "" {
		req.Header.Set("X-Domain", domain)
	}
	if token := c.DeviceToken(cred); token != "" {
		req.Header.Set("X-Device-Token", token)
	}
}

// applyDesktopHeaders writes headers for desktop fingerprint family requests.
//
// The desktop family is used for growth task tracking reporting: upstream assigns events to
// "desktop client" based on UA and X-Domain, scored separately from CLI family events.
func (c *Client) applyDesktopHeaders(req *http.Request, cred *Credential) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA())
	req.Header.Set("X-Domain", strings.TrimSuffix(GetEndpoints(cred.Realm()).ChatBase, "/"))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", requestIDFor(cred, c.rt))
}

// desktopUA returns the UA of the desktop fingerprint family.
func desktopUA() string {
	return "WorkBuddy/" + DesktopClientVersion + " WorkBuddy/" + DesktopClientVersion + " CLI/" + defaultCLIVersion
}

// requestIDFor generates a request ID with a nanosecond suffix (desktop family requires it to be different every time).
func requestIDFor(cred *Credential, rt *runtime) string {
	return rt.deriveID(cred.UIDValue(), "req") + newHexID()[:6]
}

// applyV3ConfigHeaders writes headers specific to /v3/config.
//
// UA is the risk control gate here: too old will get a stripped-down catalog, or directly 400 code=12403.
func (c *Client) applyV3ConfigHeaders(req *http.Request, cred *Credential, ua string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Domain", v3ConfigDomain(cred))
}

// v3ConfigDomain determines the X-Domain value for /v3/config.
func v3ConfigDomain(cred *Credential) string {
	domain := strings.TrimSpace(cred.DomainValue())
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimSuffix(domain, "/")
	if domain != "" {
		return domain
	}
	base := GetEndpoints(cred.Realm()).ChatBase
	base = strings.TrimPrefix(base, "https://")
	base = strings.TrimPrefix(base, "http://")
	return strings.TrimSuffix(base, "/")
}

// This file implements the handling of upstream SSE streams: streaming passthrough and non-streaming aggregation.
//
// The upstream returns OpenAI-style SSE (`data: {...}` frames + ending `data: [DONE]`),
// but forwarding it directly to the client as-is is unsafe:
//   - Upstream puts the entire message (rather than delta) in `choices[].message`, and accumulative clients will repeatedly concatenate;
//   - Upstream tool_call chunks each contain function.name, and accumulating clients will concatenate them into "BashBash";
//   - Upstream may drop the id/model fields midway, causing the client to treat it as a new message.
//
// Therefore, **whitelist reconstruction** is adopted: only known fields are kept, and the rest are discarded.

// errEmptyStream indicates that there are no valid data frames in the upstream stream.
//
// Defined separately so callers can distinguish between "upstream empty stream" (upstream defect, should report error)
// and "write failure caused by client disconnection" (normal phenomenon, should not report error).
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError reports whether the error is an upstream empty stream.
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// StreamStats is the statistics of a streaming forward.
type StreamStats struct {
	// Frames is the number of valid forwarded frames.
	Frames int
	// Usage is the usage given by the upstream (may be empty).
	Usage map[string]any
	// Model is the model name in the upstream response.
	Model string
	// Credit is the actual deduction credit given by the upstream this time (may be nil).
	Credit *float64
	// FinishReason is the last finish_reason.
	FinishReason string
	// SawDone indicates whether [DONE] was received.
	SawDone bool
}

// StreamOptions are options for streaming forward.
type StreamOptions struct {
	// Emit is called when a frame of deliverable payload is received. The payload is **raw JSON** (without the `data: ` prefix),
	// because the host will add the prefix and trailing [DONE] for chat-completions.
	Emit func(payload []byte) error
	// Hint attaches a value to the error frame (can be nil).
	Hint func(code string) string
}

// Stream reads the upstream SSE and passes the normalized frames to emit.
//
// Key behaviors:
//   - Stop reading after receiving `[DONE]`, and ignore any subsequent data;
//   - Frames containing an error key are passed through as-is (error frames should not be rewritten by the whitelist);
//   - Reconstruct all other frames via whitelist;
//   - Zero valid data frames throughout -> return errEmptyStream.
func Stream(body io.Reader, opts StreamOptions) (StreamStats, error) {
	var stats StreamStats
	if body == nil {
		return stats, errEmptyStream
	}

	scanner := bufio.NewScanner(body)
	// Upstream single frames can be very long (tool arguments, long text), and the default 64KB will truncate them.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	firstID := ""
	seenToolName := map[int]bool{}
	sawAny := false

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Non-data lines (comments, event:, etc.) are dispatched as-is to maintain the upstream frame structure.
		if !strings.HasPrefix(trimmed, "data:") {
			if errEmit := opts.emit([]byte(line)); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			stats.SawDone = true
			break
		}
		if payload == "" {
			continue
		}

		// Error frame: passed through as-is, optional additional hint. Upstream error messages are more valuable than our reconstruction.
		if isErrorFrame(payload) {
			stats.Frames++
			sawAny = true
			if errEmit := opts.emit([]byte(attachHint(payload, opts.Hint))); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		var frame map[string]any
		if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
			// Unparseable frames are passed through as-is (might be new frame types added by upstream).
			stats.Frames++
			sawAny = true
			if errEmit := opts.emit([]byte(payload)); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		// tool_call name is kept only in the first chunk.
		StripToolCallNames(frame, seenToolName)

		// id continuation: upstream occasionally drops the id in subsequent frames, and the client would treat it as a new message.
		if id, _ := frame["id"].(string); strings.TrimSpace(id) != "" {
			if firstID == "" {
				firstID = id
			}
		} else if firstID != "" {
			frame["id"] = firstID
		}

		collectStats(frame, &stats)
		normalized := normalizeFrame(frame)

		encoded, errEncode := json.Marshal(normalized)
		if errEncode != nil {
			continue
		}
		stats.Frames++
		sawAny = true
		if errEmit := opts.emit(encoded); errEmit != nil {
			return stats, errEmit
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		// Scan error (including upstream stream drop): already forwarded frames are still valid, pass the error to the caller for judgment.
		return stats, fmt.Errorf("read upstream stream: %w", errScan)
	}
	if !sawAny {
		return stats, errEmptyStream
	}
	return stats, nil
}

func (o StreamOptions) emit(payload []byte) error {
	if o.Emit == nil {
		return nil
	}
	return o.Emit(payload)
}

// isErrorFrame determines whether a frame is an error frame.
func isErrorFrame(payload string) bool {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &probe); errUnmarshal != nil {
		return false
	}
	return len(probe.Error) > 0 && string(probe.Error) != "null"
}

// attachHint attaches a hint text to the error frame (append only, do not modify).
func attachHint(payload string, hint func(code string) string) string {
	if hint == nil {
		return payload
	}
	var frame map[string]any
	if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
		return payload
	}
	errorField, okError := frame["error"].(map[string]any)
	if !okError {
		return payload
	}
	if _, okHint := errorField["gateway_hint"]; okHint {
		return payload
	}
	code, _ := errorField["code"].(string)
	text := hint(code)
	if strings.TrimSpace(text) == "" {
		return payload
	}
	errorField["gateway_hint"] = text
	encoded, errEncode := json.Marshal(frame)
	if errEncode != nil {
		return payload
	}
	return string(encoded)
}

// collectStats extracts statistical information from frames.
func collectStats(frame map[string]any, stats *StreamStats) {
	if usage, okUsage := frame["usage"].(map[string]any); okUsage && len(usage) > 0 {
		stats.Usage = usage
		if credit, okCredit := toFloat64(usage["credit"]); okCredit {
			stats.Credit = &credit
		}
	}
	if model, okModel := frame["model"].(string); okModel && strings.TrimSpace(model) != "" {
		stats.Model = model
	}
	choices, okChoices := frame["choices"].([]any)
	if !okChoices || len(choices) == 0 {
		return
	}
	choice, okChoice := choices[0].(map[string]any)
	if !okChoice {
		return
	}
	if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
		stats.FinishReason = reason
	}
}

func toFloat64(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	case json.Number:
		parsed, errParse := typed.Float64()
		return parsed, errParse == nil
	}
	return 0, false
}

// normalizeFrame reconstructs frames using a whitelist.
//
// Top-level retained: id / object / created / model / system_fingerprint / service_tier / choices / usage.
// delta retained: role / content / reasoning_content / refusal / tool_calls /
// function_call (empty placeholders are pruned).
//
// Discard upstream private fields (session_id, trace, etc.): they are useless to the client,
// and would expose upstream internal session identifiers to the downstream.
func normalizeFrame(frame map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if value, okValue := frame[key]; okValue {
			out[key] = value
		}
	}
	if _, okObject := out["object"]; !okObject {
		out["object"] = "chat.completion.chunk"
	}
	if _, okID := out["id"]; !okID {
		out["id"] = "chatcmpl-wb2api"
	}

	if choices, okChoices := frame["choices"].([]any); okChoices {
		out["choices"] = normalizeChoices(choices)
	} else {
		out["choices"] = []any{}
	}

	// Explicitly set null when usage is missing: clients commonly read this key to determine if it has ended.
	if usage, okUsage := frame["usage"]; okUsage {
		out["usage"] = usage
	} else {
		out["usage"] = nil
	}
	return out
}

// normalizeChoices rebuilds choices.
func normalizeChoices(choices []any) []any {
	out := make([]any, 0, len(choices))
	for _, item := range choices {
		choice, okChoice := item.(map[string]any)
		if !okChoice {
			continue
		}
		normalized := map[string]any{}
		if index, okIndex := choice["index"]; okIndex {
			normalized["index"] = index
		}
		if delta, okDelta := choice["delta"].(map[string]any); okDelta {
			normalized["delta"] = normalizeDelta(delta)
		}
		// Normalize empty finish_reason string to null (clients rely on it to determine completion).
		if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
			normalized["finish_reason"] = reason
		} else {
			normalized["finish_reason"] = nil
		}
		out = append(out, normalized)
	}
	return out
}

// normalizeDelta rebuilds delta using a whitelist.
func normalizeDelta(delta map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"role", "content", "reasoning_content", "refusal"} {
		if value, okValue := delta[key]; okValue {
			out[key] = value
		}
	}
	if calls, okCalls := delta["tool_calls"].([]any); okCalls && len(calls) > 0 {
		out["tool_calls"] = calls
	}
	// function_call is a legacy field: an empty object placeholder will be treated as a call by the client and must be stripped.
	if functionCall, okFunctionCall := delta["function_call"].(map[string]any); okFunctionCall && len(functionCall) > 0 {
		out["function_call"] = functionCall
	}
	return out
}

// Aggregate aggregates the upstream SSE stream into a non-streaming response.
//
// The upstream forces streaming, so non-streaming clients must be aggregated on our gateway side.
func Aggregate(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, errEmptyStream
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var (
		id           string
		model        string
		created      int64
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    []any
		usage        map[string]any
		finishReason string
		sawDone      bool
		validEvents  int
		gotContent   bool
	)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		if payload == "" {
			continue
		}
		var frame map[string]any
		if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
			continue
		}
		if isErrorFrame(payload) {
			return nil, fmt.Errorf("upstream error: %s", core.Truncate(payload, 512))
		}
		validEvents++

		if got := core.StringField(frame, "id"); got != "" && id == "" {
			id = got
		}
		if got := core.StringField(frame, "model"); got != "" {
			model = got
		}
		if value, okValue := toFloat64(frame["created"]); okValue {
			created = int64(value)
		}
		if payloadUsage, okUsage := frame["usage"].(map[string]any); okUsage && len(payloadUsage) > 0 {
			usage = payloadUsage
		}

		choices, okChoices := frame["choices"].([]any)
		if !okChoices || len(choices) == 0 {
			continue
		}
		choice, okChoice := choices[0].(map[string]any)
		if !okChoice {
			continue
		}
		if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
			finishReason = reason
		}

		// Some upstream frames put content in message (instead of delta); both need to be handled.
		// gotContent latch prevents the same content from being counted twice by delta and message.
		if delta, okDelta := choice["delta"].(map[string]any); okDelta {
			appendContent(&content, delta["content"])
			appendContent(&reasoning, delta["reasoning_content"])
			if calls, okCalls := delta["tool_calls"].([]any); okCalls && len(calls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, calls)
				gotContent = true
			}
		}
		if message, okMessage := choice["message"].(map[string]any); okMessage && !gotContent {
			if text, okText := message["content"].(string); okText && text != "" {
				content.WriteString(text)
			}
			if text, okText := message["reasoning_content"].(string); okText && text != "" {
				reasoning.WriteString(text)
			}
		}
		if content.Len() > 0 {
			gotContent = true
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, fmt.Errorf("read upstream stream: %w", errScan)
	}
	if validEvents == 0 {
		return nil, errEmptyStream
	}

	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		// When the output is truncated or the stream does not end normally, tool arguments will inevitably be incomplete and are discarded.
		if finishReason == "length" || !sawDone {
			toolCalls = DropTruncatedToolCalls(toolCalls)
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = sortToolCalls(toolCalls)
		}
	}
	if strings.TrimSpace(finishReason) == "" {
		finishReason = "stop"
	}

	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		response["usage"] = ensureUsageTotal(usage)
	}
	return json.Marshal(response)
}

func appendContent(builder *strings.Builder, value any) {
	if text, okText := value.(string); okText && text != "" {
		builder.WriteString(text)
	}
}

// mergeToolCalls merges tool call fragments by index.
//
// Upstream fragment format: the first fragment starts with id/name/arguments, subsequent fragments only contain argument deltas.
// Fragments lacking an index require fault tolerance (continue if id is seen, otherwise start a new sequence number).
func mergeToolCalls(existing []any, incoming []any) []any {
	byIndex := make(map[int]map[string]any, len(existing))
	order := make([]int, 0, len(existing)+len(incoming))
	for _, item := range existing {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		index := core.IntFromAny(call["index"])
		byIndex[index] = call
		order = append(order, index)
	}

	nextIndex := len(order)
	for _, item := range incoming {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		index, okIndex := call["index"]
		var key int
		if okIndex {
			key = core.IntFromAny(index)
		} else {
			// Missing index: continue the latest call (occasional upstream behavior).
			if len(order) > 0 {
				key = order[len(order)-1]
			} else {
				key = nextIndex
				nextIndex++
			}
		}
		target, okTarget := byIndex[key]
		if !okTarget {
			target = map[string]any{"index": key}
			byIndex[key] = target
			order = append(order, key)
		}
		mergeToolCallFields(target, call)
	}

	out := make([]any, 0, len(order))
	for _, key := range order {
		if call, okCall := byIndex[key]; okCall {
			out = append(out, call)
		}
	}
	return out
}

// mergeToolCallFields merges fragment fields into the target call.
func mergeToolCallFields(target, incoming map[string]any) {
	if id, okID := incoming["id"].(string); okID && strings.TrimSpace(id) != "" {
		target["id"] = id
	}
	if callType, okType := incoming["type"].(string); okType && strings.TrimSpace(callType) != "" {
		target["type"] = callType
	}
	incomingFunction, okFunction := incoming["function"].(map[string]any)
	if !okFunction {
		return
	}
	targetFunction, okTargetFunction := target["function"].(map[string]any)
	if !okTargetFunction {
		targetFunction = map[string]any{}
		target["function"] = targetFunction
	}
	if name, okName := incomingFunction["name"].(string); okName && strings.TrimSpace(name) != "" {
		targetFunction["name"] = name
	}
	if arguments, okArgs := incomingFunction["arguments"].(string); okArgs && arguments != "" {
		previous, _ := targetFunction["arguments"].(string)
		targetFunction["arguments"] = previous + arguments
	}
}

// sortToolCalls sorts output by index (clients depend on order).
func sortToolCalls(calls []any) []any {
	out := append([]any(nil), calls...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			left, _ := out[j-1].(map[string]any)
			right, _ := out[j].(map[string]any)
			if core.IntFromAny(left["index"]) <= core.IntFromAny(right["index"]) {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ensureUsageTotal fills in missing total_tokens.
//
// Do not modify the original map: upstream usage may be reused by other paths, modifying in place introduces implicit coupling.
func ensureUsageTotal(usage map[string]any) map[string]any {
	if _, okTotal := usage["total_tokens"]; okTotal {
		return usage
	}
	prompt, okPrompt := toFloat64(usage["prompt_tokens"])
	completion, okCompletion := toFloat64(usage["completion_tokens"])
	if !okPrompt || !okCompletion {
		return usage
	}
	out := make(map[string]any, len(usage)+1)
	for key, value := range usage {
		out[key] = value
	}
	out["total_tokens"] = int64(prompt + completion)
	return out
}

// ChatMetaFromBody derives session headers from the request body.
//
// conversationRequestID is a UUIDv7 generated **once per round** according to official semantics (shared by
// retry loops for the same call, ensured by the caller constructing it outside the retry loop): the content-derived value is not
// a valid v7 form, and replaying across sessions in the same round would reuse the same ID.
func ChatMetaFromBody(body []byte) ChatMeta {
	return ChatMeta{
		ConversationID:        ConversationKey(body),
		ConversationRequestID: NewUUIDv7Hex(),
	}
}

// ==================== Request Body Rewriting and Cleaning Pipeline ====================

// This file implements the "OpenAI Request Body → CodeBuddy Request Body" rewriting pipeline.
//
// **The rewriting order has semantics and cannot be swapped**. Each step corresponds to a pitfall actually encountered:
//
//	1. Force stream true          —— Upstream rejects non-streaming requests
//	2. Normalize max_completion_tokens —— Upstream only recognizes max_tokens, aliases are silently ignored
//	3. Supply default stream_options      —— Without it, usage (including actual billing) cannot be retrieved
//	4. Normalize tool_choice           —— Upstream expects a string for this field, object format directly returns 400
//	5. developer → system         —— Upstream role whitelist does not include developer, matching it results in 400
//	6. Normalize image_url             —— Upstream only accepts object format, strings result in 400
//	7. Tool result reordering              —— Out-of-order messages cause upstream to determine tool pairing failure
//	8. Orphan tool_call pruning        —— Incomplete pairing causes upstream to crash the entire session
//	9. Chain-of-thought injection                 —— DeepSeek series does not return thought chains unless explicitly enabled
//	10. Tier normalization                  —— Invalid tiers result in 400
//	11. reasoning_content backfill    —— Some tenants require assistant messages to have non-empty reasoning
//	12. Fingerprint desensitization                  —— Upstream content moderation is word-by-word matching

// PrepareOptions is the input for the rewriting pipeline.
type PrepareOptions struct {
	// Model is the bare name of the target model. When non-empty, directly rewrites payload["model"].
	Model string
	// PromptMode prompt mode: passthrough / custom / append.
	PromptMode string
	// PromptText is the custom or appended prompt content.
	PromptText string
	// Sanitize enables fingerprint sanitization (Step 12).
	Sanitize bool
	// DefaultEfforts is the map of model name -> default effort level (from the model catalog).
	DefaultEfforts map[string]string
	// SupportedEfforts is the map of model name -> list of supported effort levels (from the model catalog).
	SupportedEfforts map[string][]string
	// CacheKey is prompt_cache_key; injection is skipped when empty.
	CacheKey string
	// Global indicates this is a global domain request (additionally requires a system fallback).
	Global bool
}

// defaultDeepSeekEffort is the fallback default effort when it is missing in DeepSeek models.
const defaultDeepSeekEffort = "high"

// effortRank is the strength ranking of effort levels, used to "find the highest supported effort not exceeding the requested effort".
var effortRank = map[string]int{
	"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// PrepareBody executes the full rewriting pipeline, returning the request body that can be sent directly to the upstream.
//
// When parsing fails, return the input as-is: it is better not to rewrite than to let a conversation be interrupted by a rewriting failure,
// as the upstream's real error message is more diagnostic than a local parsing error.
func PrepareBody(body []byte, opts PrepareOptions) []byte {
	payload := decodeBodyMap(body)
	if payload == nil {
		return body
	}

	// 0) Set the outbound model to a bare name (single-pass rewrite, no secondary serialization needed).
	if trimmedModel := strings.TrimSpace(opts.Model); trimmedModel != "" {
		payload["model"] = trimmedModel
	}

	// 1) Upstream rejects non-streaming.
	payload["stream"] = true

	// 2) max_completion_tokens → max_tokens.
	translateMaxCompletionTokens(payload)

	// 3) Default include_usage to stream_options if missing (usage cannot be obtained without it).
	if _, okOptions := payload["stream_options"]; !okOptions {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	// 4) Normalize tool_choice.
	normalizeToolChoice(payload)

	// 5) developer → system.
	normalizeRoles(payload)

	// 5.5) System prompt processing (passthrough / replace / append).
	applyPromptMode(payload, opts.PromptMode, opts.PromptText)

	// 6) Normalize image_url.
	normalizeImageURL(payload)

	if messages, okMessages := payload["messages"].([]any); okMessages {
		// 7) Tool result rearrangement.
		messages = repackToolResultBlocks(messages)
		// 8) Orphan tool_call trimming.
		messages = cleanupOrphanToolCalls(messages)
		payload["messages"] = messages
	}

	model, _ := payload["model"].(string)
	model = strings.TrimSpace(model)

	// 9) Chain-of-thought injection.
	injectThinking(payload, defaultEffortFor(opts.DefaultEfforts, model))

	// 10) Normalize effort level.
	normalizeReasoningEffort(payload, opts.SupportedEfforts[model])

	// 10.5) reasoning_summary: Inject "auto" when the official version has thinking enabled (thinking enabled or with an effort level)
	// and the caller has not specified it, to complete the protocol form.
	injectReasoningSummary(payload)

	// 11) reasoning_content backfill.
	backfillReasoningContent(payload)

	// Fallback system for the global domain: some upstream paths require the first message to be system.
	if opts.Global {
		ensureConsoleSystem(payload)
	}

	// prompt_cache_key: The single largest cost optimization, must be injected last (preceding steps do not depend on it).
	if strings.TrimSpace(opts.CacheKey) != "" {
		if _, okKey := payload["prompt_cache_key"]; !okKey {
			payload["prompt_cache_key"] = opts.CacheKey
		}
	}

	// 12) Fingerprint sanitization (placed last: preceding steps may introduce text that needs sanitization).
	if opts.Sanitize {
		if messages, okMessages := payload["messages"].([]any); okMessages {
			sanitizeMessages(messages)
		}
	}

	encoded, errEncode := encodeBodyMap(payload)
	if errEncode != nil {
		return body
	}
	return encoded
}

// translateMaxCompletionTokens translates max_completion_tokens to max_tokens.
//
// Background: In the OpenAI specification, max_tokens is deprecated and max_completion_tokens is the new field,
// but the upstream only recognizes max_tokens—passing the alias through will cause it to be ignored, falling back to the default output limit,
// and long tasks will be silently truncated.
//
// Rules:
//   - The alias is **deleted regardless of whether it is translated** (to reduce size and debugging noise);
//   - When an explicit max_tokens is present, it is deleted without translation (explicit takes precedence);
//   - The alias is translated only if it is a positive integer value, written as int64 to avoid scientific notation (1.28e5);
//   - 0/null/negative numbers/non-numeric values are not translated (0/null semantics mean "unset", and negative numbers are invalid values).
func translateMaxCompletionTokens(payload map[string]any) {
	alias, hasAlias := payload["max_completion_tokens"]
	if !hasAlias {
		return
	}
	delete(payload, "max_completion_tokens")

	if _, hasExplicit := payload["max_tokens"]; hasExplicit {
		return
	}
	if value, okValue := positiveInt64(alias); okValue {
		payload["max_tokens"] = value
	}
}

// positiveInt64 converts a JSON number to a positive integer.
func positiveInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed <= 0 || typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case int64:
		if typed <= 0 {
			return 0, false
		}
		return typed, true
	case int:
		if typed <= 0 {
			return 0, false
		}
		return int64(typed), true
	}
	return 0, false
}

// normalizeToolChoice normalizes tool_choice into a format accepted by the upstream.
//
// The upstream expects this field to be a Go string, and passing it as an object causes a 400 code=11101.
func normalizeToolChoice(payload map[string]any) {
	raw, hasChoice := payload["tool_choice"]
	if !hasChoice {
		return
	}
	if text, okText := raw.(string); okText {
		if strings.EqualFold(strings.TrimSpace(text), "none") {
			// "none" means no tools used: remove the selector and the tool definitions themselves.
			delete(payload, "tool_choice")
			delete(payload, "tools")
			delete(payload, "functions")
			return
		}
		return
	}
	object, okObject := raw.(map[string]any)
	if !okObject {
		delete(payload, "tool_choice")
		return
	}
	choiceType, _ := object["type"].(string)
	switch strings.ToLower(strings.TrimSpace(choiceType)) {
	case "none":
		delete(payload, "tool_choice")
		delete(payload, "tools")
		delete(payload, "functions")
	case "auto", "required":
		payload["tool_choice"] = strings.ToLower(strings.TrimSpace(choiceType))
	case "function":
		// Two forms: {"function":{"name":"x"}} and {"name":"x"}.
		name := ""
		if function, okFunction := object["function"].(map[string]any); okFunction {
			name, _ = function["name"].(string)
		}
		if strings.TrimSpace(name) == "" {
			name, _ = object["name"].(string)
		}
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			payload["tool_choice"] = trimmed
			return
		}
		delete(payload, "tool_choice")
	default:
		delete(payload, "tool_choice")
	}
}

// normalizeRoles normalizes developer to system.
//
// The upstream role whitelist does not include developer, matching it results in HTTP 400 code=11-128.
// Normalize only this value, keeping all other roles as-is (no merging, reordering, or deletion).
func normalizeRoles(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, okRole := message["role"].(string)
		if !okRole {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			message["role"] = "system"
		}
	}
}

// normalizeImageURL converts string-form image_url into object-form.
//
// Upstream only accepts objects, strings will return 400 code=11101 cannot unmarshal string into ImageContent.
// Always preserve existing objects as-is (do not fill in default detail/mime_type values: let upstream return real errors
// rather than having them masked by our default values).
func normalizeImageURL(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		parts, okParts := message["content"].([]any)
		if !okParts {
			continue
		}
		for _, part := range parts {
			partMap, okPartMap := part.(map[string]any)
			if !okPartMap {
				continue
			}
			if partType, okType := partMap["type"].(string); !okType || partType != "image_url" {
				continue
			}
			if url, okURL := partMap["image_url"].(string); okURL {
				partMap["image_url"] = map[string]any{"url": url}
			}
		}
	}
}

// ensureConsoleSystem adds a fallback system message when missing in the global scope's first position.
//
// Certain upstream global paths require messages[0] to be system, and behavior is undefined when missing.
func ensureConsoleSystem(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		return
	}
	if first, okFirst := messages[0].(map[string]any); okFirst {
		if role, okRole := first["role"].(string); okRole && strings.EqualFold(strings.TrimSpace(role), "system") {
			return
		}
	}
	payload["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}}, messages...)
}

// defaultEffortFor looks up the model's default effort level.
func defaultEffortFor(table map[string]string, model string) string {
	if len(table) == 0 {
		return ""
	}
	return strings.TrimSpace(table[strings.TrimSpace(model)])
}

// applyPromptMode handles system prompts: custom replaces all system/developer prompts; append adds after the initial consecutive system block.
func applyPromptMode(payload map[string]any, mode, text string) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	text = strings.TrimSpace(text)
	if mode == "" || mode == "passthrough" || text == "" {
		return
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	if mode == "custom" {
		kept := make([]any, 0, len(messages)+1)
		kept = append(kept, map[string]any{"role": "system", "content": text})
		for _, item := range messages {
			msgMap, okMap := item.(map[string]any)
			if !okMap {
				kept = append(kept, item)
				continue
			}
			role, _ := msgMap["role"].(string)
			switch strings.ToLower(strings.TrimSpace(role)) {
			case "system", "developer":
				continue
			}
			kept = append(kept, item)
		}
		payload["messages"] = kept
	} else if mode == "append" {
		insertAt := 0
		for insertAt < len(messages) {
			msgMap, okMap := messages[insertAt].(map[string]any)
			if !okMap {
				break
			}
			role, _ := msgMap["role"].(string)
			switch strings.ToLower(strings.TrimSpace(role)) {
			case "system", "developer":
				insertAt++
				continue
			}
			break
		}
		out := make([]any, 0, len(messages)+1)
		out = append(out, messages[:insertAt]...)
		out = append(out, map[string]any{"role": "system", "content": text})
		out = append(out, messages[insertAt:]...)
		payload["messages"] = out
	}
}

// decodeBodyMap decodes the request body into a map. Returns nil on failure (handled as "unable to parse" by the caller).
func decodeBodyMap(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil
	}
	return payload
}

// encodeBodyMap serializes the map back into the request body.
func encodeBodyMap(payload map[string]any) ([]byte, error) {
	return json.Marshal(payload)
}

// This file handles DeepSeek series chain-of-thought.
//
// Upstream marks deepseek series models with thinkingFormat:"deepseek" and
// requiresReasoningContentOnAssistantMessages, resulting in two hard requirements:
//   - Enabling thinking must explicitly include thinking:{type:"enabled"} **and an effort level**,
//     otherwise the upstream defaults to responding with "no thinking" (no chain-of-thought returned at all);
//   - Assistant messages must include non-empty reasoning, and some tenants validate len(reasoning) > 0,
//     where missing/null/empty strings all result in 400 (blank strings are okay, indicating length rather than content is validated).

// isDeepSeekModel determines whether it is a deepseek series model.
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectThinking injects the chain-of-thought toggle for deepseek series models.
//
//	Input                           Behavior
//	thinking.type == "disabled"    Respected, effort field is removed
//	thinking.type == "enabled"     Fills in default effort (if missing)
//	No thinking / Non-object / Empty type  Ininjects enabled and fills in default effort
func injectThinking(payload map[string]any, defaultEffort string) {
	model, _ := payload["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	thinking, okThinking := payload["thinking"].(map[string]any)
	if !okThinking {
		payload["thinking"] = map[string]any{"type": "enabled"}
		ensureDeepSeekEffort(payload, defaultEffort)
		return
	}
	thinkingType, _ := thinking["type"].(string)
	switch strings.ToLower(strings.TrimSpace(thinkingType)) {
	case "disabled":
		// User explicitly wants to disable: respect it and remove the effort level (having an effort level while disabling thinking is contradictory).
		delete(payload, "reasoning_effort")
		delete(payload, "reasoningEffort")
	case "enabled":
		ensureDeepSeekEffort(payload, defaultEffort)
	default:
		thinking["type"] = "enabled"
		ensureDeepSeekEffort(payload, defaultEffort)
	}
}

// ensureDeepSeekEffort ensures deepseek requests include an effort level.
//
// snake_case is preferred, camelCase is the fallback; default effort is written only when neither is present.
func ensureDeepSeekEffort(payload map[string]any, defaultEffort string) {
	if _, okSnake := payload["reasoning_effort"]; okSnake {
		return
	}
	if _, okCamel := payload["reasoningEffort"]; okCamel {
		return
	}
	effort := strings.TrimSpace(defaultEffort)
	if effort == "" {
		effort = defaultDeepSeekEffort
	}
	payload["reasoning_effort"] = effort
}

// normalizeReasoningEffort downgrades the effort level to the range supported by the model.
//
// Rules (pass through as-is without any guesswork if the support table is empty):
//   - Requested effort is in the support list -> pass through as-is;
//   - Otherwise, take the highest supported effort **not higher than** the requested effort;
//   - If all supported efforts are higher than the requested effort -> take the lowest supported effort (minimal deviation).
//
// Upstream directly returns 400 for invalid effort levels, whereas downgrading to the nearest valid level allows the request to succeed.
func normalizeReasoningEffort(payload map[string]any, supported []string) {
	if len(supported) == 0 {
		return
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		raw, okRaw := payload[key].(string)
		if !okRaw {
			continue
		}
		requested := strings.ToLower(strings.TrimSpace(raw))
		if requested == "" {
			continue
		}
		if containsString(supported, requested) {
			continue
		}
		payload[key] = nearestEffort(supported, effortRank[requested])
	}
}

// nearestEffort finds the effort level closest to the requested level in the support list.
func nearestEffort(supported []string, requestedRank int) string {
	lowest := ""
	lowestRank := 99
	nearest := ""
	nearestRank := -1
	for _, item := range supported {
		rank, okRank := effortRank[item]
		if !okRank {
			continue
		}
		if rank < lowestRank {
			lowestRank = rank
			lowest = item
		}
		// The highest one not exceeding the requested effort.
		if rank <= requestedRank && rank > nearestRank {
			nearestRank = rank
			nearest = item
		}
	}
	if nearest != "" {
		return nearest
	}
	// All supported tiers are higher than the requested tier: take the lowest supported tier (smallest deviation).
	if lowest != "" {
		return lowest
	}
	return supported[0]
}

// injectReasoningSummary injects "auto" when thinking is enabled and the caller has not specified a summary tier.
//
// Aligned with the official injectReasoningSummaryIfNeeded: thinking.type=enabled,
// reasoning_effort or reasoning.effort presence is considered thinking enabled;
// Do not overwrite if the body already contains reasoning_summary (including explicit disable).
func injectReasoningSummary(payload map[string]any) {
	if _, okSummary := payload["reasoning_summary"]; okSummary {
		return
	}
	if !reasoningGateOpen(payload) {
		return
	}
	payload["reasoning_summary"] = "auto"
}

// backfillReasoningContent backfills the reasoning field for assistant messages.
//
// Gating: the request enabled thinking, or the history already contains traces of the chain of thought.
// Process assistant messages only:
//   - reasoning_content is already a string -> do not overwrite;
//   - otherwise if reasoning is a string -> copy its value;
//   - otherwise backfill with an empty string.
//
// Simultaneously mirror to ensure the reasoning field is non-empty: missing/null/empty string is normalized to a single space
// (actual tests show some tenants validate len(reasoning) > 0, blank strings can pass).
func backfillReasoningContent(payload map[string]any) {
	if !reasoningGateOpen(payload) {
		return
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}

		if _, okString := message["reasoning_content"].(string); !okString {
			if reasoning, okReasoning := message["reasoning"].(string); okReasoning {
				message["reasoning_content"] = reasoning
			} else {
				message["reasoning_content"] = ""
			}
		}

		// Mirror field: non-empty takes priority, if both are absent backfill a space.
		reasoning, _ := message["reasoning"].(string)
		if strings.TrimSpace(reasoning) == "" {
			if filled, okFilled := message["reasoning_content"].(string); okFilled && strings.TrimSpace(filled) != "" {
				message["reasoning"] = filled
			} else {
				message["reasoning"] = " "
			}
		}
	}
}

// reasoningGateOpen determines whether the chain of thought field needs to be backfilled.
//
// Must backfill when thinking is enabled (upstream will validate assistant messages in history together);
// Must also backfill when traces of the chain of thought have already appeared in history, otherwise subsequent rounds will suddenly lack the field.
func reasoningGateOpen(payload map[string]any) bool {
	if thinking, okThinking := payload["thinking"].(map[string]any); okThinking {
		if thinkingType, okType := thinking["type"].(string); okType &&
			strings.EqualFold(strings.TrimSpace(thinkingType), "enabled") {
			return true
		}
	}
	messages, _ := payload["messages"].([]any)
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		if reasoning, okReasoning := message["reasoning"].(string); okReasoning && strings.TrimSpace(reasoning) != "" {
			return true
		}
		if _, okContent := message["reasoning_content"]; okContent {
			return true
		}
	}
	return false
}

// This file handles the pairing issues of tool_calls and tool results.
//
// Upstream validates tool pairing very strictly: calls and results must be **complete on both sides**, otherwise it returns
// 11148 "tool calls and tool results do not match" and permanently blocks the entire session.
//
// Two sources of corruption in reality:
//   - Clients (such as Codex) insert non-tool messages between two parallel call results;
//   - Clients send incomplete history of "calls without results" or "results without calls".

// repackToolResultBlocks moves non-tool messages inserted between tool_calls and their results to after the entire group.
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// Only adjusts the order without changing the content. Returns the original slice when there are no inserted messages (zero changes, zero allocations).
func repackToolResultBlocks(messages []any) []any {
	if len(messages) < 3 {
		return messages
	}
	out := make([]any, 0, len(messages))
	changed := false

	for index := 0; index < len(messages); index++ {
		message, okMessage := messages[index].(map[string]any)
		if !okMessage {
			out = append(out, messages[index])
			continue
		}
		if !hasToolCalls(message) {
			out = append(out, messages[index])
			continue
		}

		// Collect this group: the call message itself + immediately following tool results (other messages may be sandwiched in between).
		group := []any{messages[index]}
		var displaced []any
		cursor := index + 1
		for ; cursor < len(messages); cursor++ {
			next, okNext := messages[cursor].(map[string]any)
			if !okNext {
				break
			}
			role, _ := next["role"].(string)
			role = strings.ToLower(strings.TrimSpace(role))
			if role == "tool" {
				group = append(group, messages[cursor])
				continue
			}
			// Encountering the next assistant with tool_calls must return control to the outer loop:
			// Otherwise that batch of results will never be reordered (this is exactly how they were missed in real sessions).
			if hasToolCalls(next) {
				break
			}
			displaced = append(displaced, messages[cursor])
		}
		if len(displaced) > 0 {
			changed = true
			out = append(out, group...)
			out = append(out, displaced...)
		} else {
			out = append(out, group...)
		}
		index = cursor - 1
	}
	if !changed {
		return messages
	}
	return out
}

// cleanupOrphanToolCalls trims tool calls and results with incomplete pairing.
//
// Rule: Only the intersection of calls and results is retained.
//   - assistant.tool_calls are symmetrically trimmed by intersection; if trimmed to empty, delete the key;
//   - role=tool is retained only if the corresponding call is retained, otherwise deleted entirely.
//
// The historical incorrect approach was "retain the whole batch only if all are present within the batch, otherwise delete the entire tool_calls key",
// which would leave "assistant without tool_calls + orphan tool", exactly hitting upstream's 11148.
func cleanupOrphanToolCalls(messages []any) []any {
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}

	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "assistant":
			for _, id := range toolCallIDs(message) {
				callIDs[id] = true
			}
		case "tool":
			if id, okID := message["tool_call_id"].(string); okID && strings.TrimSpace(id) != "" {
				resultIDs[strings.TrimSpace(id)] = true
			}
		}
	}
	if len(callIDs) == 0 && len(resultIDs) == 0 {
		return messages
	}

	keep := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}

	out := make([]any, 0, len(messages))
	changed := false
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			out = append(out, item)
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "assistant":
			if !hasToolCalls(message) {
				out = append(out, message)
				continue
			}
			filtered := filterToolCalls(message, keep)
			if len(filtered) == 0 {
				delete(message, "tool_calls")
				changed = true
			} else {
				message["tool_calls"] = filtered
			}
			out = append(out, message)
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if !keep[strings.TrimSpace(id)] {
				changed = true
				continue
			}
			out = append(out, message)
		default:
			out = append(out, message)
		}
	}
	if !changed {
		return messages
	}
	return out
}

// toolCallIDs extracts all tool_call IDs from the assistant message.
func toolCallIDs(message map[string]any) []string {
	calls, okCalls := message["tool_calls"].([]any)
	if !okCalls {
		return nil
	}
	out := make([]string, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		if id, okID := call["id"].(string); okID && strings.TrimSpace(id) != "" {
			out = append(out, strings.TrimSpace(id))
		}
	}
	return out
}

// hasToolCalls reports whether the message has non-empty tool_calls.
func hasToolCalls(message map[string]any) bool {
	calls, okCalls := message["tool_calls"].([]any)
	return okCalls && len(calls) > 0
}

// filterToolCalls trims tool_calls according to the keep set.
func filterToolCalls(message map[string]any, keep map[string]bool) []any {
	calls, _ := message["tool_calls"].([]any)
	out := make([]any, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		id, _ := call["id"].(string)
		if keep[strings.TrimSpace(id)] {
			out = append(out, call)
		}
	}
	return out
}

// IsTruncatedArguments determines whether the tool call arguments are incomplete JSON.
//
// An empty string is valid (tool without arguments); non-empty but unparsable indicates the response was truncated.
func IsTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var value any
	return json.Unmarshal([]byte(trimmed), &value) != nil
}

// DropTruncatedToolCalls discards tool calls with incomplete arguments.
//
// Called when finish_reason is length (output truncated) or stream does not end normally:
// Tool arguments in that case are inevitably incomplete JSON, passing to the client will only cause parsing errors.
// Those that can be parsed but have wrong types (scalars/arrays) are kept, leaving them to client-side schema validation.
func DropTruncatedToolCalls(calls []any) []any {
	out := make([]any, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			out = append(out, item)
			continue
		}
		function, okFunction := call["function"].(map[string]any)
		if !okFunction {
			out = append(out, call)
			continue
		}
		arguments, _ := function["arguments"].(string)
		if IsTruncatedArguments(arguments) {
			continue
		}
		out = append(out, call)
	}
	return out
}

// StripToolCallNames ensures that each tool_call's function.name appears only in the first chunk.
//
// Accumulating clients do name += delta.name; if every chunk has a name, it results in
// "BashBashBash"; while overriding clients (name ?? state.name) are not cleared by empty values.
// Therefore, "keep the first chunk, delete the key in subsequent ones" satisfies both types of clients.
func StripToolCallNames(frame map[string]any, seen map[int]bool) {
	choices, okChoices := frame["choices"].([]any)
	if !okChoices {
		return
	}
	for _, item := range choices {
		choice, okChoice := item.(map[string]any)
		if !okChoice {
			continue
		}
		delta, okDelta := choice["delta"].(map[string]any)
		if !okDelta {
			continue
		}
		calls, okCalls := delta["tool_calls"].([]any)
		if !okCalls {
			continue
		}
		for _, callItem := range calls {
			call, okCall := callItem.(map[string]any)
			if !okCall {
				continue
			}
			index := core.IntFromAny(call["index"])
			function, okFunction := call["function"].(map[string]any)
			if !okFunction {
				continue
			}
			if seen[index] {
				delete(function, "name")
				continue
			}
			if _, okName := function["name"]; okName {
				seen[index] = true
			}
		}
	}
}

// This file implements fingerprint sanitization for outbound request bodies.
//
// Background: Clients (like Claude Code CLIs) inject several fixed template sentences into the system prompt,
// and upstream content moderation performs **verbatim exact matching** (not semantic moderation), so changing a single character can bypass it.
//
// The strategy has two layers:
//   - Key-value/header style fingerprints: Strip entirely (they are injection artifacts themselves, containing no user semantics);
//   - Semantic-carrying template sentences: Minimal rewriting (changing a word), keeping semantics unchanged.
//
// The rules table is loaded (embedded) from sanitize_rules.json in the same directory as catalog,
// making it easy to audit "which literals were actually modified" and preventing Go source code from containing many critical strings prone to accidental modification.

//go:embed assets/sanitize_rules.json
var sanitizeRulesRaw []byte

// sanitizeRules represents the sanitization rules.
type sanitizeRules struct {
	// Features are feature pre-check strings: any match triggers sanitization (zero overhead for regular requests).
	Features []string `json:"features"`
	// Rewrites are [original, rewrite] pairs.
	Rewrites [][2]string `json:"rewrites"`
	// Regexes are regexes used for whole-segment stripping (accessed by name).
	Regexes map[string]string `json:"regexes"`
}

var (
	sanitizeOnce    sync.Once
	sanitizeRuleSet sanitizeRules
	sanitizeRegexes map[string]*regexp.Regexp
)

func loadSanitizeRules() (sanitizeRules, map[string]*regexp.Regexp) {
	sanitizeOnce.Do(func() {
		var rules sanitizeRules
		if errUnmarshal := json.Unmarshal(sanitizeRulesRaw, &rules); errUnmarshal != nil {
			// Embedded resource corruption is a build-time issue: if rules are missing, degrade to "no sanitization"
			// rather than causing plugin startup to fail (sanitization is a bonus, not a requirement).
			rules = sanitizeRules{}
		}
		compiled := map[string]*regexp.Regexp{}
		for name, pattern := range rules.Regexes {
			if strings.TrimSpace(pattern) == "" {
				continue
			}
			expr, errCompile := regexp.Compile(pattern)
			if errCompile != nil {
				// Skip the rule when it is malformed, instead of panicking.
				continue
			}
			compiled[name] = expr
		}
		sanitizeRuleSet = rules
		sanitizeRegexes = compiled
	})
	return sanitizeRuleSet, sanitizeRegexes
}

// sanitizeMessages sanitizes all text in the request body.
//
// Scope of sanitization: content (string or text in parts), reasoning/reasoning_content,
// tool_call arguments (which are stringified JSON, processed as text). **Image parts are untouched**.
//
// The return value reports whether any changes were made.
func sanitizeMessages(messages []any) bool {
	rules, regexes := loadSanitizeRules()
	changed := false
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		// Note: **Cannot** continue here when content is missing.
		// Tool call message content is usually null; earlier versions therefore skipped when content was missing,
		// causing arguments in tool_calls to be completely unsanitized—blocked strings written into tool arguments
		// (file names, commands, written content) would leak out unmodified.
		if content, okContent := message["content"]; okContent {
			if updated, didChange := sanitizeContent(content, rules, regexes); didChange {
				message["content"] = updated
				changed = true
			}
		}
		for _, key := range []string{"reasoning", "reasoning_content"} {
			if text, okText := message[key].(string); okText {
				if updated := sanitizeText(text, rules, regexes); updated != text {
					message[key] = updated
					changed = true
				}
			}
		}
		if calls, okCalls := message["tool_calls"].([]any); okCalls {
			if sanitizeToolCalls(calls, rules, regexes) {
				changed = true
			}
		}
	}
	return changed
}

// sanitizeContent sanitizes message content (string or parts array).
func sanitizeContent(content any, rules sanitizeRules, regexes map[string]*regexp.Regexp) (any, bool) {
	switch typed := content.(type) {
	case string:
		updated := sanitizeText(typed, rules, regexes)
		return updated, updated != typed
	case []any:
		changed := false
		for _, item := range typed {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			// Image parts untouched: sanitizing content outside of text fields would corrupt images.
			if partType, okType := part["type"].(string); okType && partType == "image_url" {
				continue
			}
			text, okText := part["text"].(string)
			if !okText {
				continue
			}
			if updated := sanitizeText(text, rules, regexes); updated != text {
				part["text"] = updated
				changed = true
			}
		}
		return typed, changed
	}
	return content, false
}

// sanitizeToolCalls sanitizes tool call arguments and names.
func sanitizeToolCalls(calls []any, rules sanitizeRules, regexes map[string]*regexp.Regexp) bool {
	changed := false
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		function, okFunction := call["function"].(map[string]any)
		if !okFunction {
			continue
		}
		if arguments, okArgs := function["arguments"].(string); okArgs {
			if updated := sanitizeText(arguments, rules, regexes); updated != arguments {
				function["arguments"] = updated
				changed = true
			}
		}
	}
	return changed
}

// hasFingerprint feature pre-check: quickly determine if text needs sanitization.
//
// Use strings.Contains for the fast path: the vast majority of requests do not match,
// directly skipping all subsequent regexes and rewrites (zero allocations).
func hasFingerprint(text string, rules sanitizeRules) bool {
	for _, feature := range rules.Features {
		if strings.Contains(text, feature) {
			return true
		}
	}
	return false
}

// sanitizeText performs complete desensitization on a piece of text.
//
// Execution order:
//  1. Feature pre-check (returns original string immediately if no match);
//  2. Rewrite table (semantic-carrying template sentences, word-by-word replacement);
//  3. Strip entire header key-value segment;
//  4. Loop to clean cc_ key-value segments (until stable);
//  5. Abbreviate bare header key names.
func sanitizeText(text string, rules sanitizeRules, regexes map[string]*regexp.Regexp) string {
	if text == "" || !hasFingerprint(text, rules) {
		return text
	}
	for _, pair := range rules.Rewrites {
		if len(pair) != 2 || pair[0] == "" {
			continue
		}
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	if expr := regexes["sanitizeHdrRe"]; expr != nil {
		text = expr.ReplaceAllString(text, "")
	}
	if expr := regexes["sanitizeKvRe"]; expr != nil {
		// Loop to clean until stable: stripping the prefix of one rule may expose a match for the next.
		for previous := ""; previous != text; {
			previous = text
			text = expr.ReplaceAllString(text, "")
		}
	}
	if expr := regexes["sanitizeBareHdrRe"]; expr != nil {
		text = expr.ReplaceAllStringFunc(text, func(match string) string {
			// Abbreviate bare key names to short forms, maintaining readability while no longer being the original literals.
			if len(match) > 4 {
				return match[:len(match)-3] + "hdr"
			}
			return match
		})
	}
	return strings.TrimSpace(text)
}
