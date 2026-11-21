package main

import (
	"strings"
)

const (
	// Default Codex model for Claude web_search → Codex Responses (override with codex_model).
	defaultCodexWebSearchModel = "gpt-5.4-mini"
	// Default xAI model for server-side web_search per https://docs.x.ai/developers/tools/web-search
	defaultXAIWebSearchModel = "grok-4.3"
)

// resolveCodexWebSearchTargetModel never forwards the client Claude model to Codex.
func resolveCodexWebSearchTargetModel(configured string) string {
	if m := strings.TrimSpace(configured); m != "" {
		return m
	}
	return defaultCodexWebSearchModel
}

// resolveXAIWebSearchTargetModel never forwards the client Claude model to xAI Responses.
func resolveXAIWebSearchTargetModel(configured string) string {
	if m := strings.TrimSpace(configured); m != "" {
		return m
	}
	return defaultXAIWebSearchModel
}
