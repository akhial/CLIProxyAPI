package handlers

import (
	"net/http"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// filterAntigravityACPProviders removes "antigravity" from candidate providers
// unless the inbound request is a recognized official Antigravity ACP Gemini
// request. When other providers (such as direct Gemini, Vertex, or Claude) are
// available, routing falls through to them cleanly; when "antigravity" was the
// only candidate, it returns a 400 error without touching Antigravity credentials.
func filterAntigravityACPProviders(providers []string, model string, req coreexecutor.Request, opts coreexecutor.Options) ([]string, *interfaces.ErrorMessage) {
	hasAntigravity := false
	for _, provider := range providers {
		if strings.EqualFold(strings.TrimSpace(provider), "antigravity") {
			hasAntigravity = true
			break
		}
	}
	if !hasAntigravity {
		return providers, nil
	}
	baseModel := thinking.ParseSuffix(model).ModelName
	if idx := strings.LastIndex(baseModel, "/"); idx >= 0 && idx+1 < len(baseModel) {
		baseModel = baseModel[idx+1:]
	}
	errACP := helps.ValidateAntigravityACPRequest(baseModel, req, opts)
	if errACP == nil {
		return providers, nil
	}
	filtered := slices.DeleteFunc(slices.Clone(providers), func(provider string) bool {
		return strings.EqualFold(strings.TrimSpace(provider), "antigravity")
	})
	if len(filtered) > 0 {
		return filtered, nil
	}
	return nil, &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errACP,
	}
}
