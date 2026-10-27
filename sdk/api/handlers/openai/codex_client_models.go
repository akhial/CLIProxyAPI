package openai

import (
	"encoding/json"
	"slices"
	"strings"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	responsesclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	log "github.com/sirupsen/logrus"
)

func filterCodexClientModelsByProvider(response map[string]any, provider string) map[string]any {
	provider = strings.ToLower(strings.TrimSpace(provider))
	var nativeNames map[string]string
	if provider == "codex" {
		var catalog struct {
			Models []struct {
				Slug        string `json:"slug"`
				DisplayName string `json:"display_name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(registry.GetCodexClientModelsJSON(), &catalog); err != nil {
			log.WithError(err).Warn("could not read native Codex model names")
		}
		nativeNames = make(map[string]string, len(catalog.Models))
		for _, model := range catalog.Models {
			name := model.DisplayName
			if suffix, ok := strings.CutPrefix(name, "GPT-"); ok {
				name = "GPT-" + strings.ReplaceAll(suffix, "-", " ")
			}
			nativeNames[model.Slug] = name
		}
	}

	models, _ := response["models"].([]map[string]any)
	filtered := make([]map[string]any, 0, len(models))
	modelRegistry := registry.GetGlobalRegistry()
	for _, model := range models {
		slug, _ := model["slug"].(string)
		if !slices.Contains(modelRegistry.GetModelProviders(slug), provider) {
			continue
		}
		if name := nativeNames[slug]; name != "" {
			model["display_name"] = name
		}
		filtered = append(filtered, model)
	}
	response["models"] = filtered
	return response
}

func (h *OpenAIAPIHandler) codexClientModelsResponse(clientVersion ...string) map[string]any {
	version := ""
	if len(clientVersion) > 0 {
		version = clientVersion[0]
	}
	optimizeMultiAgentV2 := false
	var applyPatchCapabilityForModel codexmodels.ApplyPatchCapabilityForModelFunc
	if h != nil {
		if cfg := h.CurrentConfig(); cfg != nil {
			optimizeMultiAgentV2 = cfg.Client.Codex.OptimizeMultiAgentV2
			if cfg.Client.Codex.EnableApplyPatch {
				applyPatchCapabilityForModel = h.SupportsApplyPatchModel
			}
		}
	}
	var toolSearchCapabilityForModel codexmodels.ToolSearchCapabilityForModelFunc
	if h != nil && responsesclaude.ClientToolSearchEnabled() {
		toolSearchCapabilityForModel = h.SupportsNativeToolSearchModel
	}
	modelRegistry := registry.GetGlobalRegistry()
	return codexmodels.BuildResponseForClientWithToolCapabilities(h.Models(), modelRegistry.GetModelProviders, modelRegistry.GetResponsesWebSearchCapability, applyPatchCapabilityForModel, toolSearchCapabilityForModel, optimizeMultiAgentV2, version)
}

// CodexClientModelsResponse builds a Codex client model response.
func CodexClientModelsResponse(models []map[string]any) map[string]any {
	return codexmodels.BuildResponse(models, nil, false)
}

// CodexClientModelsResponseWithMultiAgentV2 builds a Codex client model response
// and advertises multi-agent v2 for synthesized models when enabled.
func CodexClientModelsResponseWithMultiAgentV2(models []map[string]any, enabled bool) map[string]any {
	return codexmodels.BuildResponse(models, nil, enabled)
}

// CodexClientModelsResponseForClient builds a Codex client model response
// tailored for a specific client version.
func CodexClientModelsResponseForClient(models []map[string]any, clientVersion string, enabled bool) map[string]any {
	return codexmodels.BuildResponseForClient(models, nil, enabled, clientVersion)
}
