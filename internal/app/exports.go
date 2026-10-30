package app

import (
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/admin"
	"free-model-hub/internal/state"
)

// This file only exports pure types/pure functions needed by tests and embedders, without exposing any process-level state.

type Config = pluginConfig
type RegistrationInfo = registration
type LifecycleRequest = lifecycleRequest
type ExecutorRPCRequest = executorRPCRequest
type ExecutorStreamResponse = rpcExecutorStreamResponse

const (
	ProviderKey           = providerKey
	PluginID              = pluginID
	FormatChatCompletions = formatChatCompletions
	ManagementRoutePrefix = managementRoutePrefix
)

func DefaultConfig() Config   { return defaultPluginConfig() }
func DefaultStateDir() string { return defaultStateDir() }

// LoadState loads state from disk (for testing/embedding use).
func (a *App) LoadState(cfg Config) (*state.State, error) { return a.loadState(cfg) }
func FormatSSEChunk(b []byte) []byte                      { return formatSSEChunk(b) }
func RewritePayloadModel(payload []byte, model string) []byte {
	return rewritePayloadModel(payload, model)
}

func (a *App) DecodeConfig(raw []byte) (Config, error) { return a.decodeConfig(raw) }

func (a *App) IsModelDisabled(vendorID, modelID string) bool {
	return a.isModelDisabled(vendorID, modelID)
}

func (a *App) ResolveOutboundModel(vendorID, model string) string {
	return a.resolveOutboundModel(vendorID, model)
}

func (a *App) ApplyModelAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return a.applyModelAliases(vendorID, models)
}

func (a *App) ModelAliasFor(vendorID, modelID string) string {
	return a.models.AliasFor(vendorID, modelID)
}

func (a *App) HandleModelsToggle(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return a.admin.ModelToggle(req)
}

// CacheStaticModels records the official model list of a provider (for testing and read-back use).
func (a *App) CacheStaticModels(vendorID string, models []pluginapi.ModelInfo) {
	a.models.CacheStatic(vendorID, models)
}

// RegisteredModels returns the console list cache of a provider (for testing and read-back use).
func (a *App) RegisteredModels(vendorID string) []pluginapi.ModelInfo {
	return a.models.Registered(vendorID)
}

func (a *App) HandleModelAlias(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return a.admin.ModelAlias(req)
}

func (a *App) BuildStatusPayload(req pluginapi.ManagementRequest) map[string]any {
	return a.admin.StatusPayload(req)
}

func (a *App) ApplyModelCatalog(cfg Config) { a.applyModelCatalog(cfg) }

func (a *App) ManagementRegistration() pluginapi.ManagementRegistrationResponse {
	return a.managementRegistration()
}

func JSONResponse(status int, payload any) pluginapi.ManagementResponse {
	return admin.JSONResponse(status, payload)
}

func HTMLResponse(status int, body []byte) pluginapi.ManagementResponse {
	return admin.HTMLResponse(status, body)
}
