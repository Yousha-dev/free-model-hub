package admin

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"free-model-hub/cpasdk/pluginapi"
	"free-model-hub/internal/core"
	"free-model-hub/internal/hostbridge"
	"free-model-hub/internal/logger"
)

// consoleModel is the model entry displayed on the console page.
//
// Deliberately customized instead of directly returning pluginapi.ModelInfo: the latter lacks json tags,
// field names would become Go field names, and the frontend reading by lowercase keys would get undefined.
type consoleModel struct {
	ID              string   `json:"id"`
	ScopeID         string   `json:"scope_id"`
	VendorID        string   `json:"vendor_id"`
	VendorName      string   `json:"vendor_name"`
	Realm           string   `json:"realm"`
	Name            string   `json:"name,omitempty"`
	Alias           string   `json:"alias,omitempty"`
	Description     string   `json:"description,omitempty"`
	ContextLength   int64    `json:"context_length,omitempty"`
	MaxOutputTokens int64    `json:"max_output_tokens,omitempty"`
	Efforts         []string `json:"efforts,omitempty"`
	DefaultEffort   string   `json:"default_effort,omitempty"`
	SupportsImages  bool     `json:"supports_images,omitempty"`
	SupportsTools   bool     `json:"supports_tools,omitempty"`
	Credits         string   `json:"credits,omitempty"`
	Disabled        bool     `json:"disabled,omitempty"`
}

func (s *Service) handleModelsList(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)
	cfg := s.deps.Config()
	manager := s.deps.Model()
	models := make([]consoleModel, 0, 128)

	active := s.hostActiveVendors(ctx, callbackID)

	for _, vendor := range s.registry.Vendors() {
		if !core.RealmEnabled(cfg.EnabledRealms, vendor.Region()) || !active[vendor.ID()] {
			continue
		}
		// For providers with empty cache, initiate a supplementary probe first (non-blocking), then wait once collectively,
		// so that when N providers lack models, the total waiting time remains a single upstream latency rather than N times.
		manager.EnsureModels(vendor)
	}
	manager.WaitProbes(modelProbeWait)

	for _, vendor := range s.registry.Vendors() {
		if !core.RealmEnabled(cfg.EnabledRealms, vendor.Region()) || !active[vendor.ID()] {
			continue
		}
		models = append(models, s.consoleModelsForVendor(manager, vendor)...)
	}

	for _, extra := range manager.ExtraModels() {
		vendorID := extra.OwnedBy
		if vendorID != "" {
			if _, okVendor := s.registry.ByID(vendorID); !okVendor || !active[vendorID] {
				continue
			}
		}
		models = append(models, consoleModel{
			ID:            extra.ID,
			ScopeID:       s.registry.ModelKeyFor(vendorID, extra.ID),
			VendorID:      vendorID,
			VendorName:    s.vendorNameFor(vendorID),
			Realm:         s.vendorRegionFor(vendorID),
			Name:          core.FirstNonEmpty(extra.DisplayName, extra.Name),
			ContextLength: extra.ContextLength,
		})
	}

	for index := range models {
		models[index].Disabled = manager.IsDisabled(models[index].VendorID, models[index].ID)
		models[index].Alias = manager.AliasFor(models[index].VendorID, models[index].ID)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].VendorName != models[j].VendorName {
			return models[i].VendorName < models[j].VendorName
		}
		return models[i].ID < models[j].ID
	})

	payload := map[string]any{"ok": true, "models": models, "count": len(models)}
	if len(models) == 0 {
		payload["error"] = "No local model cache available yet, fetching from upstream; you can also click 'Read Model List' above to pull from upstream."
	} else {
		// Notify the frontend if any provider is still probing in the background, asking it to pull again shortly;
		// otherwise, users might think those missing providers simply have no models.
		if missing := manager.ProbesRunning(); missing > 0 {
			payload["pending_probes"] = missing
		}
	}
	return JSONResponse(http.StatusOK, payload)
}

func (s *Service) consoleModelsForVendor(manager *Catalog, vendor core.Vendor) []consoleModel {
	out := consoleModelsFromRegistered(manager, s.registry, vendor)
	// Prefer using detailed display info provided by the provider when available; however, a blank snapshot (cold start without
	// probing or no available credentials) must not wipe the entire provider from the console, so we only replace when non-empty.
	if provider, okProvider := core.As[core.ConsoleModelProvider](vendor); okProvider {
		if models := provider.ConsoleModels(); len(models) > 0 {
			out = s.consoleModelsFromProvider(vendor, models)
		}
	}
	// Multipliers are persisted along with the model cache and reloaded from here after restart; do not overwrite if the provider already has a multiplier.
	for index := range out {
		if out[index].Credits == "" {
			out[index].Credits = manager.CreditsFor(vendor.ID(), out[index].ID)
		}
	}
	return out
}

// consoleModelsFromRegistered converts the directory-cached official list into console entries.
func consoleModelsFromRegistered(manager *Catalog, registry *core.Registry, vendor core.Vendor) []consoleModel {
	infos := manager.Registered(vendor.ID())
	out := make([]consoleModel, 0, len(infos))
	for _, info := range infos {
		entry := consoleModel{
			ID:              info.ID,
			ScopeID:         registry.ModelKeyFor(vendor.ID(), info.ID),
			VendorID:        vendor.ID(),
			VendorName:      vendor.Name(),
			Realm:           vendor.Region(),
			Name:            core.FirstNonEmpty(info.DisplayName, info.Name),
			Description:     info.Description,
			ContextLength:   info.ContextLength,
			MaxOutputTokens: info.MaxCompletionTokens,
			SupportsImages:  core.ModelSupportsImages(info),
			SupportsTools:   core.ModelSupportsTools(info),
		}
		if info.Thinking != nil {
			entry.Efforts = info.Thinking.Levels
		}
		out = append(out, entry)
	}
	return out
}

func (s *Service) consoleModelsFromProvider(vendor core.Vendor, models []core.ConsoleModel) []consoleModel {
	out := make([]consoleModel, 0, len(models))
	for _, model := range models {
		out = append(out, consoleModel{
			ID:              model.ID,
			ScopeID:         s.registry.ModelKeyFor(vendor.ID(), model.ID),
			VendorID:        vendor.ID(),
			VendorName:      vendor.Name(),
			Realm:           vendor.Region(),
			Name:            model.Name,
			Description:     model.Description,
			ContextLength:   model.ContextLength,
			MaxOutputTokens: model.MaxOutputTokens,
			Efforts:         model.Efforts,
			DefaultEffort:   model.DefaultEffort,
			SupportsImages:  model.SupportsImages,
			SupportsTools:   model.SupportsTools,
			Credits:         model.Credits,
		})
	}
	return out
}

func (s *Service) vendorNameFor(vendorID string) string {
	if vendor, okVendor := s.registry.ByID(vendorID); okVendor {
		return vendor.Name()
	}
	return ""
}

func (s *Service) vendorRegionFor(vendorID string) string {
	if vendor, okVendor := s.registry.ByID(vendorID); okVendor {
		return vendor.Region()
	}
	return ""
}

// ModelToggle batch disables/enables models (exported for root-level compatibility wrapper).
func (s *Service) ModelToggle(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return s.modelToggleHandler(req)
}

// ModelAlias sets/clears aliases (exported for root-level compatibility wrapper).
func (s *Service) ModelAlias(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return s.modelAliasHandler(req)
}

// modelToggleRequest is the request body for batch disabling/enabling models.
type modelToggleRequest struct {
	Models   []string `json:"models"`
	Disabled bool     `json:"disabled"`
}

// modelToggleHandler batch disables/enables models.
func (s *Service) modelToggleHandler(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[modelToggleRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	targets := make([]string, 0, len(body.Models))
	for _, item := range body.Models {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			targets = append(targets, trimmed)
		}
	}
	if len(targets) == 0 {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "Please select the models to operate on first"})
	}
	s.deps.Model().SetDisabled(targets, body.Disabled)
	verb := "enabled"
	if body.Disabled {
		verb = "disabled"
	}
	logger.Info("models %s: %d items (%s)", verb, len(targets), strings.Join(targets, ", "))
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": len(targets), "disabled": body.Disabled,
		"disabled_models": s.deps.State().DisabledModels,
		"message":         "Successfully " + verb + " " + strconv.Itoa(len(targets)) + " models (takes effect immediately)",
	})
}

// modelAliasRequest is the request body for setting/clearing model aliases.
type modelAliasRequest struct {
	Model string `json:"model"`
	Alias string `json:"alias"`
}

// modelAliasHandler sets or clears the alias for a model.
func (s *Service) modelAliasHandler(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[modelAliasRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	vendorID, bare := s.deps.Model().VendorForAliasScope(body.Model)
	if vendorID == "" || strings.TrimSpace(bare) == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "model must be in the format <vendor>:<model ID>",
		})
	}
	alias := strings.TrimSpace(body.Alias)
	if strings.ContainsAny(alias, " \t\r\n") {
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "alias cannot contain whitespace characters",
		})
	}
	s.deps.Model().SetAlias(vendorID, bare, alias)
	return JSONResponse(http.StatusOK, map[string]any{"ok": true})
}
