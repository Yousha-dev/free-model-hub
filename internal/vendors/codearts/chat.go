package codearts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"free-model-hub/internal/hostbridge"
)

type ChatRequest struct {
	Credential *Credential
	Body       []byte
	Stream     bool
	// Benefit uses the daily free quota channel (request header maas_type: benefit).
	//
	// This plugin only serves the free benefit channel: upstream splits daily free quota and subscription points into two channels,
	// with the same model name on both sides, distinguished only by this header. Split here when integrating the points channel.
	Benefit bool
}

// Chat executes a non-streaming chat.
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	resp, errDo := doCodeArtsRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read codearts response: %w", errRead)
	}

	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	return body, nil
}

// ChatStream executes a streaming chat.
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	resp, errDo := doCodeArtsRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, Classify(resp.StatusCode, string(body))
	}

	return resp, nil
}

func doCodeArtsRequest(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	if req.Credential == nil {
		return nil, fmt.Errorf("codearts credential is nil")
	}

	target := strings.TrimRight(baseURL, "/")
	if target == "" {
		target = SnapEngineApiHost
	}

	endpoint := target + EpChatV2

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("create codearts http request: %w", errReq)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	httpReq.Header.Set("Agent-Type", "PromptCenter")
	httpReq.Header.Set("User-Agent", "CodeArtsAgent/5.2.0")
	if req.Benefit {
		// Must be placed before signing: the signature covers this header, and setting it later will make it ineffective,
		// causing the request to be judged as the points channel and billed by points.
		httpReq.Header.Set(HeaderMaasType, MaasBenefit)
	}

	// Sign
	signHuaweiRequest(httpReq, req.Body, req.Credential)

	timeout := 120 * time.Second
	if req.Stream {
		timeout = 300 * time.Second
	}

	client := hostbridge.Client(ctx, timeout)
	resp, errCall := client.Do(httpReq)
	if errCall != nil {
		return nil, fmt.Errorf("codearts request failed: %w", errCall)
	}

	return resp, nil
}

// signHuaweiRequest injects the Huawei Cloud SDK-HMAC-SHA256 signature into the request.
func signHuaweiRequest(req *http.Request, body []byte, cred *Credential) {
	if cred.AccessKeyID == "" || cred.SecretAccessKey == "" {
		return
	}

	xDate := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Sdk-Date", xDate)
	req.Header.Set("Host", req.URL.Host)
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
		req.Header.Set("X-Auth-Token", cred.SecurityToken)
	}

	payloadHash := sha256Hex(body)
	req.Header.Set("X-Sdk-Content-Sha256", payloadHash)

	canonicalHeadersStr, signedHeadersStr := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.RawQuery),
		canonicalHeadersStr,
		signedHeadersStr,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"SDK-HMAC-SHA256",
		xDate,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signature := hmacHex(cred.SecretAccessKey, stringToSign)
	req.Header.Set("Authorization", "SDK-HMAC-SHA256 Access="+cred.AccessKeyID+
		", SignedHeaders="+signedHeadersStr+
		", Signature="+signature)
}

func canonicalHeaders(req *http.Request) (headers, signed string) {
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		v := req.Header.Get(k)
		sb.WriteString(k + ":" + strings.TrimSpace(v) + "\n")
	}
	return sb.String(), strings.Join(keys, ";")
}

func canonicalURI(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	out := strings.Join(segments, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := vals[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacHex(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// Constants and endpoint definitions: Huawei Cloud CodeArts Agent configuration.
// Reference: /Users/jiandan/Workspaces/codearts2api/internal/upstream/const.go

const (
	VendorID   = "codearts"
	VendorName = "CodeArts"

	// Huawei Cloud CodeArts API Gateway
	SnapEngineApiHost = "https://snap-access.cn-north-4.myhuaweicloud.com"
	SnapManagerHost   = SnapEngineApiHost + "/snap-manager"
	PortalHost        = "https://codearts.huaweicloud.com/portal"
	// STSHost temporary AK/SK and token service. **Not** snap-manager: hitting /v1/oauth2/tokens
	// on snap-manager returns 404 APIG.0101 (observed). Token refresh and authorization code exchange both happen here.
	STSHost     = "https://sts.cn-north-4.myhuaweicloud.com"
	BenefitHost = "https://opengw.developer.huaweicloud.com"

	ClientID = "codearts-agent"

	// plugin-name / plugin-version in the auth link: the portal uses these headers to identify the caller,
	// keeping it consistent with the official CodeArts Agent plugin (reversed from huaweicloud.authentication).
	PluginName    = "snap_AIIDE"
	PluginVersion = "5.2.0"

	// Local callback: the portal constructs http://127.0.0.1:<port>/oauth/callback using the port in the auth link.
	//
	// If the plugin and browser are not on the same machine, it won't be received, forcing the user to paste this link back to the console.
	LocalCallbackPort = 7866
	LocalCallbackPath = "/oauth/callback"
	LocalCallbackURL  = "http://127.0.0.1:7866" + LocalCallbackPath

	// Endpoint paths
	EpChatV2        = "/api/v2/chat/completions"
	EpModelBuiltin  = "/v1/model/builtin"
	EpBenefitConfig = "/api/v1/gateway/config"
	// EpBenefitBalance queries the daily / monthly benefit token quota.
	EpBenefitBalance = "/api/v1/user/tokens/balance"

	// HeaderMaasType distinguishes inference channels: with benefit it goes to the daily free quota, without it, it is billed by subscription points.
	HeaderMaasType = "maas_type"
	MaasBenefit    = "benefit"
	EpLoginTicket  = "/v1/login/ticket"
	EpOAuthTokens  = "/v1/oauth2/tokens"
)

// clampTokenFields clamps out-of-bounds output limits in the request body to the upstream hard cap, returning whether it was modified.
//
// The catalog-reported limit can be clamped, but clients (like auto-compact resumption) may carry larger values themselves,
// and the upstream performs platform-level pre-validation on the max_tokens value range, returning
// InferHub.001001005.400 if exceeded. Minimal rewriting is done here: rewrite only if it parses as a number and truly exceeds the limit,
// otherwise pass through unchanged, without touching other fields in the request body.
func clampTokenFields(payload map[string]json.RawMessage) bool {
	changed := false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, okRaw := payload[key]
		if !okRaw {
			continue
		}
		value, okNumber := numericValue(raw)
		if !okNumber || value <= maxOutputTokenCap {
			continue
		}
		payload[key] = json.RawMessage(strconv.Itoa(maxOutputTokenCap))
		changed = true
	}
	return changed
}

// numericValue reads a JSON number (including 131072.0) or numeric string (common in client configurations).
// Booleans, objects, and non-numeric strings are strictly treated as "do not touch".
func numericValue(raw json.RawMessage) (int64, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, false
	}
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return int64(number), true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		return int64(number), true
	}
	return 0, false
}
