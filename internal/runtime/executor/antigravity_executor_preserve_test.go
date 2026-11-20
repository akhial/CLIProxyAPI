package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityPreserveGeminiRequests(t *testing.T) {
	// Includes an unsigned tool call in history and constraints affected by legacy cleanup.
	const input = `{
		"systemInstruction":{"parts":[{"text":"original instruction"}]},
		"contents":[{"role":"user","parts":[{"text":"one"},{"text":"two"}]},
			{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"value":"x"}}}]}],
		"tools":[{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":{"type":"object",
			"properties":{"q":{"type":"string","minLength":1,"enum":["x","y"]}}}}]}],
		"generationConfig":{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"HIGH"}},
		"safetySettings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","threshold":"BLOCK_ONLY_HIGH"}]
	}`
	const answer = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}}`
	for _, tc := range []struct {
		name, model, path string
	}{
		{"unary", "gemini-4-neon", antigravityGeneratePath},
		{"stream", "gemini-4-neon", antigravityStreamPath},
		{"aggregated stream", "gemini-3-pro", antigravityStreamPath},
		{"count", "gemini-4-neon", antigravityCountTokensPath},
	} {
		for _, rules := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rules=%t", tc.name, rules), func(t *testing.T) {
				ctx := context.Background()
				cfg := &config.Config{DisableCooling: true, Antigravity: config.AntigravityConfig{PreserveGeminiRequests: true, SensitiveWords: []string{"original"}}}
				want := ojson.NewDocString(input)
				if rules {
					models := []config.PayloadModelRule{{Name: "gemini-*", Protocol: "antigravity"}}
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"contents.0.parts.0", "sessionId"}}}
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"generationConfig.maxOutputTokens": 42}}}
					_ = want.Delete("contents.0.parts.0")
					_ = want.Set("generationConfig.maxOutputTokens", 42)
				}
				headers := http.Header{}
				// Exercise absent and present headers independently of gateway host and stored auth.
				if !rules {
					headers.Set("User-Agent", "native-sdk/test")
					headers.Set("X-Goog-Api-Client", "native-sdk/test")
				}
				headers.Set("X-Goog-Api-Key", "downstream-only")
				headers.Set("Authorization", "Bearer downstream-only")
				headers.Set("Cookie", "private=worker")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != tc.path || r.Method != http.MethodPost {
						t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
					}
					for _, key := range []string{"User-Agent", "X-Goog-Api-Client"} {
						if r.Header.Get(key) != headers.Get(key) {
							t.Errorf("%s = %q, want %q", key, r.Header.Get(key), headers.Get(key))
						}
					}
					if r.Header.Get("Authorization") != "Bearer fake-central" || r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("Cookie") != "" {
						t.Error("incorrect credential boundary")
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					doc := ojson.NewDoc(body)
					if tc.name != "count" {
						if doc.Get("project").String() != "fixture" || doc.Get("model").String() != tc.model || doc.Get("requestId").String() == "" {
							t.Error("missing Cloud Code routing fields")
						}
						if !rules && doc.Get("request.sessionId").String() == "" {
							t.Error("missing generated session ID")
						}
						if !rules {
							_ = doc.Delete("request.sessionId")
						}
					}
					var gotValue, wantValue any
					if err := json.Unmarshal([]byte(doc.Get("request").Raw), &gotValue); err != nil {
						t.Error(err)
					}
					if err := json.Unmarshal(want.Bytes(), &wantValue); err != nil {
						t.Error(err)
					}
					if !reflect.DeepEqual(gotValue, wantValue) {
						t.Errorf("upstream request differs from expected payload: %s", doc.Get("request").Raw)
					}
					switch tc.path {
					case antigravityCountTokensPath:
						_, _ = io.WriteString(w, `{"totalTokens":4}`)
					case antigravityStreamPath:
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", answer)
					default:
						_, _ = io.WriteString(w, answer)
					}
				}))
				defer server.Close()
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "antigravity", Attributes: map[string]string{"base_url": server.URL, "user_agent": "configured-other-client", "header:User-Agent": "different-client", "header:X-Goog-Api-Key": "extraneous"}, Metadata: map[string]any{"access_token": "fake-central", "project_id": "fixture", "expired": "2090-01-01T00:00:00Z"}}
				req := cliproxyexecutor.Request{Model: tc.model, Payload: []byte(input)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Headers: headers}
				e := NewAntigravityExecutor(cfg)
				switch tc.name {
				case "stream":
					stream, err := e.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				case "count":
					if _, err := e.CountTokens(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				default:
					if _, err := e.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
