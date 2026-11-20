package helps_test

import (
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
	helps "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/gemini"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type summaryRemovingPluginHooks struct {
	t *testing.T
}

func (h *summaryRemovingPluginHooks) NormalizeRequest(_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	h.t.Helper()
	const path = "generationConfig.thinkingConfig.includeThoughts"
	if !ojson.GetBytes(body, path).Bool() {
		h.t.Fatalf("request normalizer did not receive enabled summary: %s", body)
	}
	out, _ := ojson.DeleteBytes(body, path)
	return out
}

func (*summaryRemovingPluginHooks) TranslateRequest(context.Context, sdktranslator.Format, sdktranslator.Format, string, []byte, bool) ([]byte, bool) {
	return nil, false
}

func (*summaryRemovingPluginHooks) NormalizeResponseBefore(context.Context, sdktranslator.Format, sdktranslator.Format, string, []byte, []byte, []byte, bool) []byte {
	return nil
}

func (*summaryRemovingPluginHooks) TranslateResponse(context.Context, sdktranslator.Format, sdktranslator.Format, string, []byte, []byte, []byte, bool) ([]byte, bool) {
	return nil, false
}

func (*summaryRemovingPluginHooks) NormalizeResponseAfter(context.Context, sdktranslator.Format, sdktranslator.Format, string, []byte, []byte, []byte, bool) []byte {
	return nil
}

func TestApplyThinkingWithSourcePayloadPreservesNormalizerSummaryRemoval(t *testing.T) {
	hooks := &summaryRemovingPluginHooks{t: t}
	sdktranslator.SetPluginHooks(hooks)
	t.Cleanup(func() { sdktranslator.SetPluginHooks(nil) })

	source := []byte(`{"model":"gemini-3.6-flash","reasoning":{"effort":"high","summary":"auto"},"input":"hi"}`)
	translated := sdktranslator.TranslateRequest(
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatGemini,
		"gemini-3.6-flash",
		source,
		false,
	)
	const summaryPath = "generationConfig.thinkingConfig.includeThoughts"
	if ojson.GetBytes(translated, summaryPath).Exists() {
		t.Fatalf("request normalizer did not remove summary: %s", translated)
	}

	out, err := helps.ApplyThinkingWithSourcePayload(
		translated,
		source,
		source,
		"gemini-3.6-flash",
		sdktranslator.FormatOpenAIResponse.String(),
		sdktranslator.FormatGemini.String(),
		"gemini",
	)
	if err != nil {
		t.Fatalf("ApplyThinkingWithSourcePayload() error = %v", err)
	}
	if ojson.GetBytes(out, summaryPath).Exists() {
		t.Fatalf("executor restored summary removed by request normalizer: %s", out)
	}
}

func TestApplyThinkingWithSourcePayloadPreservesOriginalOnlySummary(t *testing.T) {
	currentSource := []byte(`{"model":"gemini-3.6-flash","input":"hi"}`)
	originalSource := []byte(`{"model":"gemini-3.6-flash","reasoning":{"summary":null},"input":"hi"}`)
	body := []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`)

	out, err := helps.ApplyThinkingWithSourcePayload(
		body,
		currentSource,
		originalSource,
		"gemini-3.6-flash",
		sdktranslator.FormatOpenAIResponse.String(),
		sdktranslator.FormatGemini.String(),
		"gemini",
	)
	if err != nil {
		t.Fatalf("ApplyThinkingWithSourcePayload() error = %v", err)
	}
	if include := ojson.GetBytes(out, "generationConfig.thinkingConfig.includeThoughts"); !include.Exists() || include.Bool() {
		t.Fatalf("original disabled summary was not preserved: %s", out)
	}
}
