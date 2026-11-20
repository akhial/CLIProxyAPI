package main

import (
	"testing"
)

func TestResolveCodexWebSearchTargetModelNeverUsesClaudeName(t *testing.T) {
	got := resolveCodexWebSearchTargetModel("")
	if got != defaultCodexWebSearchModel {
		t.Fatalf("empty config = %q, want %q", got, defaultCodexWebSearchModel)
	}
	if got := resolveCodexWebSearchTargetModel("gpt-5.5"); got != "gpt-5.5" {
		t.Fatalf("configured = %q", got)
	}
}

func TestResolveXAIWebSearchTargetModelNeverUsesClaudeName(t *testing.T) {
	got := resolveXAIWebSearchTargetModel("")
	if got != defaultXAIWebSearchModel {
		t.Fatalf("empty config = %q, want %q", got, defaultXAIWebSearchModel)
	}
}
