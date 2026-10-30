package admin

import (
	"net/http"
	"strings"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/state"
	"free-model-hub/internal/tasks"
)

type settingsUpdateRequest struct {
	PromptMode     *string `json:"prompt_mode"`
	ModelNameTag   *string `json:"model_name_tag"`
	KeepaliveHours []int   `json:"keepalive_hours"`
}

func (s *Service) handleSettingsRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[settingsUpdateRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	promptMode, errPrompt := normalizePromptMode(body.PromptMode)
	if errPrompt != nil {
		return ErrorResponse(errPrompt)
	}
	modelNameTag, errTag := normalizeModelNameTagPointer(body.ModelNameTag)
	if errTag != nil {
		return ErrorResponse(errTag)
	}
	changed := map[string]any{}
	// registryDirty indicates that this request changed the active value of the external model ID: the console uses this to immediately
	// trigger a host configuration reload (using the same channel as aliases/disabling) to rebuild the model registry.
	registryDirty := false
	s.deps.MutateState(func(current *state.State) {
		if body.PromptMode != nil {
			// An empty string indicates revoking the page override, letting the YAML configuration decide again.
			current.PromptMode = promptMode
			changed["prompt_mode"] = promptMode
		}
		if body.ModelNameTag != nil {
			// The channel identifier changed the external model ID: before the host model registry is rebuilt, /v1/models
			// still shows the old name. Setting RestartPending shares the same semantics as manual alias/disable—the console
			// uses this to prompt "Pending Activation" and trigger syncModelConfig self-healing. Comparing **effective values**:
			// The registry is not rebuilt when the page override matches the YAML configuration.
			before := EffectiveModelNameTag(s.deps.Config(), current.ModelNameTag)
			current.ModelNameTag = modelNameTag
			if EffectiveModelNameTag(s.deps.Config(), modelNameTag) != before {
				current.RestartPending = true
				registryDirty = true
			}
			changed[ModelNameTagKey] = modelNameTag
		}
		if body.KeepaliveHours != nil {
			if current.TaskSchedules == nil {
				current.TaskSchedules = map[string][]int{}
			}
			current.TaskSchedules[core.TaskKindKeepalive] = tasks.NormalizeHours(body.KeepaliveHours)
			changed["keepalive_hours"] = current.TaskSchedules[core.TaskKindKeepalive]
		}
	})
	if errReconfigure := s.deps.Reconfigure(); errReconfigure != nil {
		return ErrorResponse(errReconfigure)
	}
	s.tasks.Poke()
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": changed, "settings": s.settingsView(),
		"model_registry_dirty": registryDirty,
	})
}

// normalizePromptMode validates the prompt filtering mode submitted by the page.
//
// Prompt filtering is a global capability with a single mode: an empty string revokes the page override and reverts to the YAML
// configuration, and any other value must fall within the range recognized by ParsePromptMode.
func normalizePromptMode(input *string) (string, error) {
	if input == nil {
		return "", nil
	}
	mode, errMode := ParsePromptMode(*input)
	if errMode != nil {
		return "", core.NewPluginError("prompt_mode_invalid", errMode.Error(), http.StatusBadRequest)
	}
	return mode, nil
}

// normalizeModelNameTagPointer normalizes the channel identifier mode submitted by the page.
// An empty string indicates revoking the page override, letting the YAML configuration decide again; others must fall within valid values.
func normalizeModelNameTagPointer(input *string) (string, error) {
	if input == nil {
		return "", nil
	}
	trimmed := strings.ToLower(strings.TrimSpace(*input))
	if trimmed == "" {
		// An empty string indicates revoking the page override, letting the YAML configuration decide again.
		return "", nil
	}
	tag, errTag := NormalizeModelNameTag(trimmed)
	if errTag != nil {
		// Same shape as prompt_mode: invalid input returns 400, not 500.
		return "", core.NewPluginError("model_name_tag_invalid", errTag.Error(), http.StatusBadRequest)
	}
	return tag, nil
}

// settingsView is the settings payload for /status: keeping-alive on the hour + current status of prompt filtering.
func (s *Service) settingsView() map[string]any {
	settings := map[string]any{"keepalive_hours": s.tasks.KeepaliveHours()}
	settings[PromptModeKey] = EffectivePromptMode(s.deps.Config(), s.deps.State().PromptMode)
	settings[ModelNameTagKey] = EffectiveModelNameTag(s.deps.Config(), s.deps.State().ModelNameTag)
	return settings
}
