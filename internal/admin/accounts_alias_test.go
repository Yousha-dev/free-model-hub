package admin

// This file locks down the storage convention for the "account alias" management entry: the key is the account identity (file_id),
// not the nickname/email.
//
// Nicknames are not unique across accounts and change whenever the upstream changes them; using them as keys would cause aliases to be lost after re-login
// or mix up with another account having the same nickname. Two rules are pinned here: both write and clear operations must use file_id,
// and clearing must actually delete the key (leaving no empty string entries).

import (
	"encoding/json"
	"net/http"
	"testing"

	"free-model-hub/cpasdk/pluginapi"
)

func accountAliasRequestBody(t *testing.T, fileID, alias string) pluginapi.ManagementRequest {
	t.Helper()
	body, errMarshal := json.Marshal(map[string]string{"file_id": fileID, "alias": alias})
	if errMarshal != nil {
		t.Fatalf("marshal alias request: %v", errMarshal)
	}
	return pluginapi.ManagementRequest{Method: http.MethodPost, Body: body}
}

func TestAccountAliasStoresTrimmedValue(t *testing.T) {
	deps := newStubDeps(t)
	service := newServiceWithDeps(t, deps)

	response := service.handleAccountAliasRequest(accountAliasRequestBody(t, " workbuddycn-u1 ", " 工作号 "))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, response.Body)
	}
	aliases := deps.store.Snapshot().AccountAliases
	if len(aliases) != 1 || aliases["workbuddycn-u1"] != "工作号" {
		t.Fatalf("aliases = %#v, want key workbuddycn-u1 -> 工作号", aliases)
	}
}

func TestAccountAliasEmptyValueClearsKey(t *testing.T) {
	deps := newStubDeps(t)
	service := newServiceWithDeps(t, deps)

	service.handleAccountAliasRequest(accountAliasRequestBody(t, "workbuddycn-u1", "工作号"))
	response := service.handleAccountAliasRequest(accountAliasRequestBody(t, "workbuddycn-u1", "  "))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("clear status = %d: %s", response.StatusCode, response.Body)
	}
	if aliases := deps.store.Snapshot().AccountAliases; len(aliases) != 0 {
		t.Fatalf("clearing an alias must drop the key, got %#v", aliases)
	}
}

func TestAccountAliasRequiresAccountIdentity(t *testing.T) {
	service := newService(t)
	response := service.handleAccountAliasRequest(accountAliasRequestBody(t, "  ", "工作号"))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", response.StatusCode, http.StatusBadRequest, response.Body)
	}
}
