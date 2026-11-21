package auth

import (
	"errors"
	"net/http"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityProtocolDispatch(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	const model = "gemini-test-routing"
	reg := registry.GetGlobalRegistry()
	for _, provider := range []string{"antigravity", "gemini"} {
		id := t.Name() + "/" + provider
		m.RegisterExecutor(&forceMappingExecutor{id: provider})
		reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := m.Register(t.Context(), &Auth{ID: id, Provider: provider}); err != nil {
			t.Fatal(err)
		}
	}
	req := cliproxyexecutor.Request{Model: model}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	if _, err := m.Execute(t.Context(), []string{"antigravity", "gemini"}, req, opts); err != nil {
		t.Fatal(err)
	}
	if got := m.executors["gemini"].(*forceMappingExecutor).ExecuteModels(); len(got) != 1 || got[0] != model {
		t.Fatalf("Gemini calls = %v", got)
	}
	if _, err := m.Execute(t.Context(), []string{"antigravity"}, req, opts); err == nil {
		t.Fatal("expected unsupported protocol error")
	} else {
		var rejected *Error
		if !errors.As(err, &rejected) || rejected.HTTPStatus != http.StatusBadRequest || rejected.Code != "unsupported_protocol" {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := m.executors["antigravity"].(*forceMappingExecutor).ExecuteModels(); len(got) != 0 {
		t.Fatalf("Antigravity calls = %v", got)
	}
}

func TestVerifyAntigravityInferenceUsesGemini(t *testing.T) {
	const model = "gemini-test-verification"
	m := NewManager(nil, nil, nil)
	e := &unauthorizedRefreshExecutor{id: "antigravity", streamPayloads: [][]byte{[]byte(`{"candidates":[{"finishReason":"STOP"}]}`)}}
	m.RegisterExecutor(e)
	auth, err := m.Register(t.Context(), &Auth{ID: t.Name(), Provider: "antigravity"})
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	verified, err := m.VerifyInference(t.Context(), auth.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !verified.InferenceVerified || e.lastStreamReq.Model != model || e.lastStreamOpts.SourceFormat != sdktranslator.FormatGemini || e.lastStreamOpts.ResponseFormat != sdktranslator.FormatGemini {
		t.Fatal("verification did not use the registered native Gemini model and protocol")
	}
	if !ojson.GetBytes(e.lastStreamReq.Payload, "contents.0.parts.0.text").Exists() {
		t.Fatal("missing native Gemini prompt")
	}
}
