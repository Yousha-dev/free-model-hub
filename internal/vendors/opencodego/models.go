package opencodego

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/ocgo/catalog"
)

const (
	catalogTTL     = 10 * time.Minute
	catalogTimeout = 30 * time.Second
)

// hostClient satisfies catalog.HostClient over the host HTTP bridge.
type hostClient struct{}

func (hostClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	httpReq, errReq := http.NewRequestWithContext(ctx, req.Method, req.URL, strings.NewReader(string(req.Body)))
	if errReq != nil {
		return pluginapi.HTTPResponse{}, errReq
	}
	httpReq.Header = req.Headers.Clone()
	resp, errDo := hostbridge.Client(ctx, catalogTimeout).Do(httpReq)
	if errDo != nil {
		return pluginapi.HTTPResponse{}, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if errRead != nil {
		return pluginapi.HTTPResponse{}, errRead
	}
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, nil
}

// refresh reloads the catalog with apiKey when the snapshot is stale.
func (a *adapter) refresh(ctx context.Context, apiKey string) error {
	a.mu.Lock()
	fresh := time.Since(a.refreshed) < catalogTTL && len(a.catalog.Models()) > 0
	a.mu.Unlock()
	if fresh {
		return nil
	}
	if errRefresh := a.catalog.Refresh(ctx, apiKey); errRefresh != nil {
		return errRefresh
	}
	a.mu.Lock()
	a.refreshed = time.Now()
	a.mu.Unlock()
	return nil
}

func (a *adapter) modelInfos() []pluginapi.ModelInfo {
	records := a.catalog.Models()
	return core.ConvertModels(records, func(rec catalog.ModelRecord) (core.ModelDescriptor, bool) {
		contextWindow := rec.ContextLimit
		if contextWindow == 0 {
			contextWindow = core.DefaultContextWindow(rec.UpstreamID)
		}
		maxOutput := rec.OutputLimit
		if maxOutput == 0 {
			maxOutput = core.DefaultMaxOutputTokens
		}
		var efforts []string
		if rec.Thinking != nil {
			efforts = rec.Thinking.Levels
		}
		supportsImages := false
		for _, mode := range rec.InputModes {
			if strings.EqualFold(mode, "image") {
				supportsImages = true
			}
		}
		return core.ModelDescriptor{
			ID:                 rec.PublicID,
			Name:               rec.DisplayName + " (OpenCode Go)",
			Description:        "OpenCode Go model " + rec.UpstreamID + " via " + string(rec.Protocol),
			ContextLength:      contextWindow,
			MaxOutputTokens:    maxOutput,
			Efforts:            efforts,
			SupportsImages:     supportsImages,
			SupportsTools:      true,
			CanDisableThinking: rec.Thinking == nil || rec.Thinking.ZeroAllowed,
		}, true
	})
}
