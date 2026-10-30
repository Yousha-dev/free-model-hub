package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"free-model-hub/cpasdk/pluginapi"
)

// malformedJSON is a request body that is "non-empty but invalid": it cannot be treated as an empty request to proceed with default values.
const malformedJSON = `{"name":`

func TestDecodeRequestRejectsMalformedJSON(t *testing.T) {
	type request struct {
		Name string `json:"name"`
	}
	if _, errDecode := decodeRequest[request]([]byte(malformedJSON)); errDecode == nil {
		t.Fatal("decodeRequest accepted malformed JSON")
	}
	if _, errEmpty := decodeRequest[request](nil); errEmpty != nil {
		t.Fatalf("empty body should be accepted: %v", errEmpty)
	}
}

// TestDecodeRequestIntoKeepsDefaults locks down that "the request body only overwrites the fields it provides".
func TestDecodeRequestIntoKeepsDefaults(t *testing.T) {
	type request struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
		School      bool `json:"school"`
	}
	base := request{Concurrency: 1, Growth: true, School: true}
	got, errDecode := decodeRequestInto([]byte(`{"concurrency":3}`), base)
	if errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if got != (request{Concurrency: 3, Growth: true, School: true}) {
		t.Fatalf("decodeRequestInto = %+v, want defaults kept", got)
	}
}

func TestBadJSONRequestResponseIs400(t *testing.T) {
	response := BadJSONRequestResponse()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("body is not JSON: %v", errUnmarshal)
	}
	if payload["ok"] != false || payload["error"] != "Failed to parse request body, please check the JSON format" {
		t.Fatalf("body = %s", response.Body)
	}
}

// TestLoginCallbackKeepsAuthStatusContract locks down the error shape of login.
//
// The page (console.html) reads data.status / data.message according to the AuthLoginStatus contract,
// while a generic {ok,error} shape would cause the prompt text to disappear.
func TestLoginCallbackKeepsAuthStatusContract(t *testing.T) {
	response := newService(t).handleLoginCallbackRequest(pluginapi.ManagementRequest{
		Method: http.MethodPost, Body: []byte(malformedJSON),
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("body is not JSON: %v", errUnmarshal)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusError) {
		t.Fatalf("status = %v, want %q", payload["status"], pluginapi.AuthLoginStatusError)
	}
	if message, _ := payload["message"].(string); message == "" {
		t.Fatalf("message missing: %s", response.Body)
	}
}

func TestManagementHandlersRejectMalformedJSON(t *testing.T) {
	service := newService(t)
	request := pluginapi.ManagementRequest{Method: http.MethodPost, Body: []byte(malformedJSON)}
	handlers := map[string]func(pluginapi.ManagementRequest) pluginapi.ManagementResponse{
		"accounts/delete": service.handleAccountDeleteRequest,
		"accounts/add":    service.handleAccountAddRequest,
		"accounts/alias":  service.handleAccountAliasRequest,
		"quotas":          service.handleQuotasRequest,
		"settings":        service.handleSettingsRequest,
		"plans/preview":   service.handlePlansPreview,
		"plans/claim":     service.handlePlansClaim,
		"models/toggle":   service.modelToggleHandler,
		"models/alias":    service.modelAliasHandler,
		"login/callback":  service.handleLoginCallbackRequest,
	}
	for name, handler := range handlers {
		response := handler(request)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want %d (body=%s)",
				name, response.StatusCode, http.StatusBadRequest, response.Body)
		}
	}
}
