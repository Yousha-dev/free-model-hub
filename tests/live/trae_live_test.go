//go:build live

package live

// Trae Live Probe: Runs when TRAE_LIVE=1 and TRAE_LIVE_CREDS are set.
// Sequentially verifies token refresh, model catalog, non-streaming chat, streaming chat, check-in, and quota.
// Prints only truncated responses, without printing credential contents.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/vendors/trae"
)

// configureTraeHost sets up the direct host bridge.
//
// Supports both host.http.do (non-streaming) and host.http.do_stream (SSE) paths:
// The streaming path buffers the upstream response entirely and delivers it via stream_read at once—the local probe verifies
// protocol correctness, while incremental delivery is handled by the host.
func configureTraeHost(t *testing.T) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies = map[string][]byte{}
		nextID int
	)
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method   string              `json:"method"`
			URL      string              `json:"url"`
			Headers  map[string][]string `json:"headers"`
			Body     []byte              `json:"body"`
			StreamID string              `json:"stream_id"`
		}
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}

		switch method {
		case pluginabi.MethodHostHTTPDo:
			status, header, body, errDo := doUpstream(req.Method, req.URL, req.Headers, req.Body)
			if errDo != nil {
				return nil, errDo
			}
			return json.Marshal(map[string]any{"StatusCode": status, "Headers": header, "Body": body})
		case pluginabi.MethodHostHTTPDoStream:
			status, header, body, errDo := doUpstream(req.Method, req.URL, req.Headers, req.Body)
			if errDo != nil {
				return nil, errDo
			}
			mu.Lock()
			nextID++
			streamID := fmt.Sprintf("probe-%d", nextID)
			bodies[streamID] = body
			mu.Unlock()
			return json.Marshal(map[string]any{"status_code": status, "headers": header, "stream_id": streamID})
		case pluginabi.MethodHostHTTPStreamRead:
			// Host contract: when done=true, the payload in the same response is discarded,
			// so data is sent first (done=false), and the next empty read sets done=true.
			mu.Lock()
			body, okBody := bodies[req.StreamID]
			if okBody {
				delete(bodies, req.StreamID)
			}
			mu.Unlock()
			if okBody {
				return json.Marshal(map[string]any{"payload": body, "done": false})
			}
			return json.Marshal(map[string]any{"done": true})
		case pluginabi.MethodHostHTTPStreamClose:
			return json.Marshal(map[string]any{})
		default:
			return nil, fmt.Errorf("trae probe host: unsupported method %s", method)
		}
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

func doUpstream(method, url string, headers map[string][]string, body []byte) (int, http.Header, []byte, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	httpReq, errNew := http.NewRequest(method, url, reader)
	if errNew != nil {
		return 0, nil, nil, errNew
	}
	for key, values := range headers {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	client := &http.Client{Timeout: 180 * time.Second}
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return 0, nil, nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return 0, nil, nil, errRead
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

// traeLiveCredential reads credentials and selects a profile based on the vendor.
//
// The access_token in the file may have expired and be empty, so run RefreshCredential first,
// keeping it consistent with the plugin's runtime refresh chain.
func traeLiveCredential(t *testing.T) (*trae.Credential, trae.Config) {
	t.Helper()
	if os.Getenv("TRAE_LIVE") == "" {
		t.Skip("TRAE_LIVE not set, skipping live probe")
	}
	path := os.Getenv("TRAE_LIVE_CREDS")
	if path == "" {
		t.Skip("TRAE_LIVE_CREDS not set")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read creds: %v", errRead)
	}

	cfg := trae.ConfigFor(trae.RegionCN)
	if strings.Contains(strings.ToLower(path), "traeglobal") {
		cfg = trae.ConfigFor(trae.RegionGlobal)
	}
	cred, errParse := trae.ParseCredentialFor(raw, cfg)
	if errParse != nil {
		t.Fatalf("parse creds: %v", errParse)
	}
	configureTraeHost(t)

	if cred.RefreshToken != "" {
		refreshed, changed, errRefresh := trae.RefreshCredential(context.Background(), cred, cfg)
		if errRefresh != nil {
			t.Fatalf("RefreshCredential: %v", errRefresh)
		}
		if changed {
			t.Logf("token refreshed, expires_at=%d", refreshed.ExpiresAt)
			cred = refreshed
		}
	}
	if cred.AccessToken == "" {
		t.Fatalf("credential has no available access_token")
	}
	return cred, cfg
}

func traeChatPayload(model string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": "用一句话介绍你自己"},
		},
		"stream": false,
	})
	return payload
}

func TestTraeLiveRefreshAndQuota(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	quota, errQuota := trae.FetchQuota(context.Background(), cred, cfg)
	if errQuota != nil {
		t.Fatalf("FetchQuota: %v", errQuota)
	}
	t.Logf("quota: %+v", quota.Summary)
}

func TestTraeLiveCheckin(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	result, errCheckin := trae.Checkin(context.Background(), cred, cfg)
	if errCheckin != nil {
		t.Fatalf("Checkin: %v", errCheckin)
	}
	t.Logf("check-in: already=%v credit=%d msg=%s", result.Already, result.Credit, result.Message)
}

func TestTraeLiveChat(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	body, errChat := trae.Chat(context.Background(), "", trae.ChatRequest{
		Credential: cred,
		Profile:    cfg,
		Body:       traeChatPayload("glm-5.2"),
		Stream:     false,
	})
	if errChat != nil {
		t.Fatalf("Chat: %v", errChat)
	}
	t.Logf("non-streaming response: %s", truncateProbe(string(body), 1500))
}

func TestTraeLiveChatStream(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	var (
		builder strings.Builder
		chunks  int
	)
	started := time.Now()
	errStream := trae.ChatStream(context.Background(), "", trae.ChatRequest{
		Credential: cred,
		Profile:    cfg,
		Body:       traeChatPayload("glm-5.2"),
		Stream:     true,
	}, func(chunk []byte) error {
		chunks++
		builder.Write(chunk)
		return nil
	})
	if errStream != nil {
		t.Fatalf("ChatStream: %v", errStream)
	}
	t.Logf("elapsed %s, chunks=%d", time.Since(started), chunks)
	t.Logf("streaming response: %s", truncateProbe(builder.String(), 1500))
	if chunks == 0 {
		t.Fatalf("streaming produced no chunks")
	}
}

func TestTraeLiveModels(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	models, errModels := trae.FetchModels(context.Background(), cred, cfg)
	if errModels != nil {
		t.Fatalf("FetchModels: %v", errModels)
	}
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.ID)
	}
	t.Logf("catalog model count=%d: %s", len(models), truncateProbe(strings.Join(names, ","), 800))
}
