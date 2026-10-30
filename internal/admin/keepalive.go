package admin

// This file is the management endpoint adapter for token keep-alive: request decoding + result aggregation.
//
// The implementation of refresh and write-back is in internal/tasks (keepalive.go), sharing the
// same logic with the scheduled keepalive task; this file only handles HTTP semantics (result aggregation and response shape).
//
// The response shape remains unchanged: {ok,total,succeeded,failed,message,results[]}.

import (
	"fmt"
	"net/http"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/hostbridge"
)

func (s *Service) handleKeepaliveRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	// Empty request body / empty account_ids = all accounts; when accounts are checked, the page only passes the selected ones.
	body, errDecode := decodeRequest[accountBatchRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	results := s.tasks.Keepalive(ctx, hostbridge.RequestCallbackID(req), body.AccountIDs, batchConcurrency)
	if len(results) == 0 {
		return JSONResponse(http.StatusOK, map[string]any{
			"ok": true, "message": "No available accounts need refreshing", "succeeded": 0, "failed": 0, "results": []any{},
		})
	}

	succeeded, failed := 0, 0
	firstErrMsg := ""
	for _, result := range results {
		if result.OK {
			succeeded++
			continue
		}
		failed++
		if firstErrMsg == "" {
			firstErrMsg = result.Label + ": " + result.Message
		}
	}
	message := ""
	switch {
	case failed > 0 && succeeded == 0:
		message = fmt.Sprintf("All account refreshes failed (%s)", firstErrMsg)
	case failed > 0:
		message = fmt.Sprintf("Refresh complete: %d succeeded, %d failed (%s)", succeeded, failed, firstErrMsg)
	default:
		message = fmt.Sprintf("All account tokens refreshed successfully (%d accounts updated in total)", succeeded)
	}
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": failed == 0, "total": len(results), "succeeded": succeeded, "failed": failed,
		"message": message, "results": results,
	})
}
