// Package antigravity implements thinking configuration for Antigravity API format.
package antigravity

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// Applier applies thinking configuration for Antigravity API format.
type Applier struct{}

var _ thinking.ProviderDocApplier = (*Applier)(nil)

// NewApplier creates a new Antigravity thinking applier.
func NewApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("antigravity", NewApplier())
}

// Apply applies thinking configuration to Antigravity request body.
func (a *Applier) Apply(body []byte, config thinking.ThinkingConfig, modelInfo *registry.ModelInfo) ([]byte, error) {
	if !thinking.IsUserDefinedModel(modelInfo) && modelInfo.Thinking == nil {
		return body, nil
	}
	if config.Mode != thinking.ModeBudget && config.Mode != thinking.ModeLevel && config.Mode != thinking.ModeNone && config.Mode != thinking.ModeAuto {
		return body, nil
	}
	if len(body) == 0 || !ojson.ValidBytes(body) {
		body = []byte("{}")
	}
	doc := ojson.NewDoc(body)
	if errApply := a.ApplyDoc(doc, config, modelInfo); errApply != nil {
		return body, errApply
	}
	return doc.Bytes(), nil
}

// ApplyDoc applies thinking configuration onto doc in place.
func (a *Applier) ApplyDoc(doc *ojson.Doc, config thinking.ThinkingConfig, modelInfo *registry.ModelInfo) error {
	if doc == nil {
		return nil
	}
	if thinking.IsUserDefinedModel(modelInfo) {
		return a.applyCompatibleDoc(doc, config)
	}
	if modelInfo.Thinking == nil {
		return nil
	}
	if config.Mode != thinking.ModeBudget && config.Mode != thinking.ModeLevel && config.Mode != thinking.ModeNone && config.Mode != thinking.ModeAuto {
		return nil
	}

	if config.Mode == thinking.ModeAuto || config.Mode == thinking.ModeBudget {
		return a.applyBudgetFormatDoc(doc, config)
	}

	if len(modelInfo.Thinking.Levels) > 0 {
		return a.applyLevelFormatDoc(doc, config)
	}
	return a.applyBudgetFormatDoc(doc, config)
}

func (a *Applier) applyCompatibleDoc(doc *ojson.Doc, config thinking.ThinkingConfig) error {
	if config.Mode != thinking.ModeBudget && config.Mode != thinking.ModeLevel && config.Mode != thinking.ModeNone && config.Mode != thinking.ModeAuto {
		return nil
	}
	if config.Mode == thinking.ModeAuto {
		return a.applyBudgetFormatDoc(doc, config)
	}
	if config.Mode == thinking.ModeLevel || (config.Mode == thinking.ModeNone && config.Level != "") {
		return a.applyLevelFormatDoc(doc, config)
	}
	return a.applyBudgetFormatDoc(doc, config)
}

func (a *Applier) applyLevelFormatDoc(doc *ojson.Doc, config thinking.ThinkingConfig) error {
	includeThoughts, hasIncludeThoughts := extractAntigravityIncludeThoughtsDoc(doc)
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.thinkingBudget")
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.thinking_budget")
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.thinking_level")
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.includeThoughts")
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.include_thoughts")

	if config.Mode == thinking.ModeNone {
		if config.Budget == 0 && config.Level == "" {
			_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig")
			return nil
		}
		if config.Level != "" {
			_ = doc.Set("request.generationConfig.thinkingConfig.thinkingLevel", string(config.Level))
		}
		if hasIncludeThoughts {
			_ = doc.Set("request.generationConfig.thinkingConfig.includeThoughts", includeThoughts)
		}
		return nil
	}

	if config.Mode != thinking.ModeLevel {
		return nil
	}

	_ = doc.Set("request.generationConfig.thinkingConfig.thinkingLevel", string(config.Level))
	if hasIncludeThoughts {
		_ = doc.Set("request.generationConfig.thinkingConfig.includeThoughts", includeThoughts)
	}
	return nil
}

func (a *Applier) applyBudgetFormatDoc(doc *ojson.Doc, config thinking.ThinkingConfig) error {
	includeThoughts, hasIncludeThoughts := extractAntigravityIncludeThoughtsDoc(doc)
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.thinkingLevel")
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.thinking_level")
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.thinking_budget")
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.includeThoughts")
	_ = util.DeleteJSONDocIfExists(doc, "request.generationConfig.thinkingConfig.include_thoughts")

	budget := config.Budget

	_ = doc.Set("request.generationConfig.thinkingConfig.thinkingBudget", budget)
	if hasIncludeThoughts {
		_ = doc.Set("request.generationConfig.thinkingConfig.includeThoughts", includeThoughts)
	}
	return nil
}

func extractAntigravityIncludeThoughtsDoc(doc *ojson.Doc) (bool, bool) {
	for _, path := range []string{
		"request.generationConfig.thinkingConfig.includeThoughts",
		"request.generationConfig.thinkingConfig.include_thoughts",
	} {
		switch value := doc.Get(path); value.Type {
		case ojson.True:
			return true, true
		case ojson.False:
			return false, true
		}
	}
	return false, false
}
