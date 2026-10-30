package app

// free-model-hub -- Multi-provider free quota plugin.
//
// This file contains plugin identity, RPC method dispatch, and registration response. C ABI is in cgo.go;
// The provider registry is constructed by App, and the composition root is in vendors.go.

import (
	"encoding/json"
	"fmt"
	"strings"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/logger"
)

const (
	pluginID          = "free-model-hub"
	pluginDisplayName = "Free Model Hub"
	pluginAuthor      = "Yousha-dev (based on jiandanc, xspeed1989 and others; see NOTICE.md)"
	pluginRepository  = "https://github.com/Yousha-dev/free-model-hub"

	providerKey           = core.ProviderKey
	managementRoutePrefix = "/plugins/" + pluginID
	jsonContentType       = "application/json; charset=utf-8"
	htmlContentType       = "text/html; charset=utf-8"
	formatChatCompletions = core.FormatChatCompletions
)

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ResponseBeforeTranslator bool `json:"response_before_translator"`

	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	QuotaProvider         bool     `json:"quota_provider"`
	ManagementAPI         bool     `json:"management_api"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

// HandleMethod is the main entry point for the plugin ABI.
func (a *App) HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister:
		return a.handlePluginRegister(request)
	case pluginabi.MethodPluginReconfigure:
		return a.handlePluginReconfigure(request)
	case pluginabi.MethodPluginQuiesce:
		a.StopBackgroundWork()
		return core.OKEnvelope(struct{}{})

	case pluginabi.MethodModelRegister:
		return a.handleModelRegister(request)
	case pluginabi.MethodModelStatic:
		return a.handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return a.handleModelForAuth(request)

	case pluginabi.MethodAuthIdentifier:
		return core.OKEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodAuthParse:
		return a.handleAuthParse(request)
	case pluginabi.MethodAuthLoginStart:
		return a.handleAuthLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return a.handleAuthLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		return a.handleAuthRefresh(request)

	case pluginabi.MethodExecutorIdentifier:
		return core.OKEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodExecutorExecute:
		return a.handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return a.handleExecutorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return a.handleExecutorCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return a.handleExecutorHTTPRequest(request)
	case pluginabi.MethodResponseNormalizeBefore:
		return a.handleResponseNormalizeBefore(request)

	case pluginabi.MethodQuotaIdentifier:
		return core.OKEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodQuotaDescribe:
		return a.handleQuotaDescribe(request)
	case pluginabi.MethodQuotaFetch:
		return a.handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return a.handleQuotaReset(request)

	case pluginabi.MethodManagementRegister:
		return core.OKEnvelope(a.managementRegistration())
	case pluginabi.MethodManagementHandle:
		return a.handleManagement(request)

	default:
		return core.ErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

func (a *App) handlePluginRegister(request []byte) ([]byte, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()

	cfg, errConfig := a.decodeConfig(configYAMLFrom(request))
	if errConfig != nil {
		return nil, fmt.Errorf("plugin.register: %w", errConfig)
	}
	if errApply := a.ApplyConfig(cfg); errApply != nil {
		return nil, fmt.Errorf("plugin.register: %w", errApply)
	}
	if _, errState := a.loadState(cfg); errState != nil {
		logger.Error("load state failed: %v", errState)
	}
	a.applyModelCatalog(cfg)
	if !a.registered {
		a.registered = true
		a.StartBackgroundWork()
	}
	logger.Info("registered: realms=%v state_dir=%s", cfg.EnabledRealms, cfg.StateDir)
	return core.OKEnvelope(a.Registration())
}

func (a *App) handlePluginReconfigure(request []byte) ([]byte, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()

	cfg, errConfig := a.decodeConfig(configYAMLFrom(request))
	if errConfig != nil {
		return nil, fmt.Errorf("plugin.reconfigure: %w", errConfig)
	}
	previous := a.Config()
	if errApply := a.ApplyConfig(cfg); errApply != nil {
		return nil, fmt.Errorf("plugin.reconfigure: %w", errApply)
	}
	if previous.StateDir != cfg.StateDir {
		if _, errState := a.loadState(cfg); errState != nil {
			logger.Error("reload state failed: %v", errState)
		}
	}
	a.applyModelCatalog(cfg)
	logger.Info("reconfigured: realms=%v state_dir=%s", cfg.EnabledRealms, cfg.StateDir)
	return core.OKEnvelope(a.Registration())
}

func configYAMLFrom(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	var req lifecycleRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil
	}
	return req.ConfigYAML
}

// Registration constructs the registration response.
func (a *App) Registration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginDisplayName,
			Version:          a.version,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			ConfigFields:     a.configFields(),
		},
		Capabilities: registrationCapabilities{
			ResponseBeforeTranslator: true,

			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			QuotaProvider:         true,
			ManagementAPI:         true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeBoth),
			ExecutorInputFormats:  []string{formatChatCompletions},
			ExecutorOutputFormats: []string{formatChatCompletions},
		},
	}
}

// configFields aggregates global fields and provider self-described fields.
func (a *App) configFields() []pluginapi.ConfigField {
	fields := []pluginapi.ConfigField{
		{Name: "enabled_realms", Type: pluginapi.ConfigFieldTypeString, Description: "Enabled realms (comma-separated): cn / global. Default: cn,global."},

		{Name: "extra_models", Type: pluginapi.ConfigFieldTypeString, Description: "Additional model names to register (without prefix), comma-separated."},

		{Name: "disabled_models", Type: pluginapi.ConfigFieldTypeString, Description: "List of disabled models."},

		{Name: "state_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Plugin state directory."},

		{Name: "log_level", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"debug", "info", "error"}, Description: "Plugin log level."},

		{Name: "log_to_file", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Whether to write logs to <state_dir>/logs."},
	}
	for _, vendor := range a.registry.Vendors() {
		if provider, ok := core.As[core.ConfigFieldsProvider](vendor); ok {
			fields = append(fields, provider.ConfigFields()...)
		}
	}
	return fields
}

func decodeRequest(raw []byte, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(raw, target); errUnmarshal != nil {
		return fmt.Errorf("decode request: %w", errUnmarshal)
	}
	return nil
}

func isChatFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai", "chat-completions", "chat_completions", "openai-chat-completions":
		return true
	}
	return false
}
