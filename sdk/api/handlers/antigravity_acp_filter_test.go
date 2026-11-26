package handlers

import (
	"net/http"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestFilterAntigravityACPProviders(t *testing.T) {
	t.Run("strips antigravity for non-ACP requests when another provider exists", func(t *testing.T) {
		providers, errMsg := filterAntigravityACPProviders(
			[]string{"antigravity", "gemini"},
			"gemini-3.1-pro",
			coreexecutor.Request{Model: "gemini-3.1-pro", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)},
			coreexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		)
		if errMsg != nil {
			t.Fatalf("unexpected error: %+v", errMsg)
		}
		if len(providers) != 1 || providers[0] != "gemini" {
			t.Fatalf("providers = %v, want [gemini]", providers)
		}
	})

	t.Run("rejects with 400 when antigravity is the sole provider for OpenAI or non-ACP Gemini", func(t *testing.T) {
		_, errMsg := filterAntigravityACPProviders(
			[]string{"antigravity"},
			"gemini-3.1-pro",
			coreexecutor.Request{Model: "gemini-3.1-pro", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)},
			coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI},
		)
		if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
			t.Fatalf("errMsg = %+v, want 400 Bad Request", errMsg)
		}
	})

	t.Run("keeps antigravity for recognized ACP Gemini requests", func(t *testing.T) {
		for _, model := range []string{"gemini-3.1-pro-low", "work-acct/gemini-3.1-pro-low"} {
			providers, errMsg := filterAntigravityACPProviders(
				[]string{"antigravity"},
				model,
				coreexecutor.Request{Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)},
				coreexecutor.Options{
					SourceFormat: sdktranslator.FormatGemini,
					Headers:      http.Header{"X-Acp-Conversation-Id": {"11111111-1111-4111-8111-111111111111"}},
				},
			)
			if errMsg != nil {
				t.Fatalf("model %q unexpected error: %+v", model, errMsg)
			}
			if len(providers) != 1 || providers[0] != "antigravity" {
				t.Fatalf("model %q providers = %v, want [antigravity]", model, providers)
			}
		}
	})
}
