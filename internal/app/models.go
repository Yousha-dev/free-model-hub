package app

// This file is the ABI adaptation layer for model provider capabilities.

import (
	"context"
	"net/http"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

type staticModelRPCRequest struct {
	pluginapi.StaticModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type authModelRPCRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (a *App) handleModelStatic(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := a.models.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

func (a *App) handleModelRegister(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := a.models.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelRegistrationResponse{Provider: providerKey, Models: models})
}

func (a *App) handleModelForAuth(request []byte) ([]byte, error) {
	var rpc authModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	credential, vendor, errResolve := a.resolveVendorCredential(ctx, rpc.HostCallbackID, rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errResolve != nil {
		logger.Debug("model.for_auth: no credential for %s: %v", rpc.AuthID, errResolve)
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	if !a.models.RealmEnabled(vendor.Region()) {
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	models, errModels := core.ModelsFor(vendor, ctx, credential)
	if errModels != nil {
		logger.Debug("model.for_auth: probe models for %s failed: %v", vendor.ID(), errModels)
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	// Also write to the console readback cache (official ID, unfiltered/no alias applied).
	//
	// The host loads each credential file via this RPC, so after plugin startup it no longer relies on
	// static probing timing: the console list will not miss an entire provider just because probing hasn't finished.
	// Empty lists are not cached: providers return empty results on probe failure per contract (allowing the host to purge bad credentials),
	// whereas if overwritten anyway, a transient failure would wipe out the entire provider from the console.
	if !a.models.CacheProbedModels(vendor.ID(), models) {
		logger.Info("model.for_auth: %s returned no models for %s (host will drop this credential)",
			vendor.ID(), rpc.AuthID)
	}
	return core.OKEnvelope(pluginapi.ModelResponse{
		Provider: providerKey,
		Models:   a.models.ApplyAliases(vendor.ID(), a.models.FilterForVendor(vendor.ID(), models)),
	})
}

func (a *App) applyModelCatalog(_ pluginConfig) {
	a.models.ClearRestartPending()
	snapshot := a.SnapshotState()
	a.models.ReloadFromState(snapshot)
	a.models.LoadCachedState(snapshot.Models)
}

func (a *App) refreshModelsFromUpstream(ctx context.Context) (int, error) {
	total, errRefresh := a.models.RefreshFromUpstream(ctx)
	if errRefresh != nil {
		return 0, core.ToPluginError(errRefresh)
	}
	return total, nil
}

func (a *App) resolveOutboundModel(vendorID, model string) string {
	return a.models.ResolveOutboundModel(vendorID, model)
}

func (a *App) applyModelAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return a.models.ApplyAliases(vendorID, models)
}

func (a *App) isModelDisabled(vendorID, modelID string) bool {
	return a.models.IsDisabled(vendorID, modelID)
}
