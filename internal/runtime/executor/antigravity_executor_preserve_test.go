package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
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

func TestAntigravityPreservedACPHostSession(t *testing.T) {
	const (
		root  = "3dbc79e2-5446-427f-aa8b-09466feadb26"
		child = "0ac5d42e-97ba-4fcb-a527-8e7ac6e3a5f7"
	)
	ctx := context.Background()
	e := NewAntigravityExecutor(&config.Config{Antigravity: config.AntigravityConfig{PreserveGeminiRequests: true}})
	model := "gemini-4-neon"
	build := func(ctx context.Context, worker, account, conversation, rootID, step, text string, stream bool) []byte {
		t.Helper()
		auth := &cliproxyauth.Auth{ID: account, Metadata: map[string]any{"project_id": "same-project"}}
		opts := cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatGemini,
			Headers:      http.Header{},
			Metadata:     map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope(worker)},
		}
		for name, value := range map[string]string{
			"X-ACP-Conversation-ID": conversation, "X-ACP-Root-Conversation-ID": rootID, "X-ACP-Step": step,
		} {
			if value != "" {
				opts.Headers.Set(name, value)
			}
		}
		doc := ojson.NewDocString(`{"request":{"contents":[{"role":"user","parts":[{"text":""}]}],"labels":{"custom":"preserved"}}}`)
		_ = doc.Set("request.contents.0.parts.0.text", text)
		req, payload, errBuild := e.buildRequestDoc(ctx, auth, "central-token", model, doc, stream, opts, "http://127.0.0.1", "-987")
		if errBuild != nil {
			t.Fatal(errBuild)
		}
		for name := range opts.Headers {
			if req.Header.Get(name) != "" {
				t.Errorf("host header %s was forwarded", name)
			}
		}
		return payload
	}

	first := build(ctx, "worker-a", "account-a", root, root, "0", "first", true)
	next := build(ctx, "worker-a", "account-a", root, root, "1", "next", false)
	for _, path := range []string{"request.sessionId", "request.labels.cascade_id", "request.labels.trajectory_id", "request.labels.root_cascade_id"} {
		a, b := ojson.GetBytes(first, path).String(), ojson.GetBytes(next, path).String()
		if a == "" || a != b {
			t.Errorf("unstable %s across calls: %q vs %q", path, a, b)
		}
	}
	if firstID, nextID := ojson.GetBytes(first, "requestId").String(), ojson.GetBytes(next, "requestId").String(); firstID == "" || firstID == nextID {
		t.Errorf("request IDs must be distinct: %q vs %q", firstID, nextID)
	}
	for path, want := range map[string]string{
		"request.labels.last_step_index": "1",
		"request.labels.custom":          "preserved",
	} {
		if result := ojson.GetBytes(next, path); result.Type != ojson.String || result.String() != want {
			t.Errorf("%s = %s, want %q", path, result.Raw, want)
		}
	}

	for _, other := range [][]byte{
		build(ctx, "worker-b", "account-a", root, "", "", "first", true),
		build(ctx, "worker-a", "account-b", root, "", "", "first", true),
		build(ctx, "worker-a", "account-a", child, root, "0", "first", true),
	} {
		for _, path := range []string{"request.sessionId", "request.labels.cascade_id", "request.labels.trajectory_id"} {
			if value := ojson.GetBytes(other, path).String(); value == "" || value == ojson.GetBytes(first, path).String() {
				t.Errorf("%s must be distinct across sessions, workers and accounts: %q", path, value)
			}
		}
	}
	branch := build(ctx, "worker-a", "account-a", child, root, "0", "branch", true)
	if rootID := ojson.GetBytes(branch, "request.labels.root_cascade_id").String(); rootID != ojson.GetBytes(first, "request.labels.cascade_id").String() {
		t.Errorf("branch root did not resolve to its root cascade: %q", rootID)
	}
	if _, errCascade := uuid.Parse(ojson.GetBytes(first, "request.labels.cascade_id").String()); errCascade != nil {
		t.Fatalf("cascade ID: %v", errCascade)
	}

	unmarked := build(ctx, "worker-a", "account-a", "", "", "", "unmarked", true)
	if ojson.GetBytes(unmarked, "request.sessionId").String() != "-987" || ojson.GetBytes(unmarked, "request.labels.trajectory_id").Exists() {
		t.Errorf("unexpected ACP metadata on an unmarked request: %s", unmarked)
	}
	finalized := helps.WithPayloadFinalizer(ctx, func(body []byte) []byte {
		doc := ojson.NewDoc(body)
		if !doc.Get("request.labels.trajectory_id").Exists() {
			t.Error("trajectory label must be set before payload rules")
		}
		_ = doc.Delete("request.labels")
		_ = doc.Set("request.sessionId", "configured")
		return doc.Bytes()
	})
	body := build(finalized, "worker-a", "account-a", root, root, "0", "first", true)
	if ojson.GetBytes(body, "request.labels").Exists() || ojson.GetBytes(body, "request.sessionId").String() != "configured" {
		t.Errorf("payload rules must run last: %s", body)
	}
}

func TestAntigravityRejectsInvalidACPHostHeaders(t *testing.T) {
	const validID = "3dbc79e2-5446-427f-aa8b-09466feadb26"
	e := NewAntigravityExecutor(&config.Config{Antigravity: config.AntigravityConfig{PreserveGeminiRequests: true}})
	auth := &cliproxyauth.Auth{ID: "account", Metadata: map[string]any{"project_id": "project"}}
	for _, tc := range []struct {
		name         string
		conversation []string
		root         string
		step         string
		caller       string
	}{
		{name: "step without conversation", step: "0", caller: "worker"},
		{name: "duplicate conversation", conversation: []string{validID, validID}, caller: "worker"},
		{name: "malformed conversation", conversation: []string{"a"}, caller: "worker"},
		{name: "malformed root", conversation: []string{validID}, root: "a", caller: "worker"},
		{name: "negative step", conversation: []string{validID}, step: "-1", caller: "worker"},
		{name: "unauthenticated caller", conversation: []string{validID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			for _, conversation := range tc.conversation {
				headers.Add("X-ACP-Conversation-ID", conversation)
			}
			if tc.root != "" {
				headers.Set("X-ACP-Root-Conversation-ID", tc.root)
			}
			if tc.step != "" {
				headers.Set("X-ACP-Step", tc.step)
			}
			opts := cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatGemini,
				Headers:      headers,
				Metadata:     map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope(tc.caller)},
			}
			req, _, errBuild := e.buildRequestDoc(context.Background(), auth, "token", "gemini-4-neon", ojson.NewDocString(`{"request":{}}`), true, opts, "http://127.0.0.1")
			status, ok := errBuild.(interface{ StatusCode() int })
			if req != nil || !ok || status.StatusCode() != http.StatusBadRequest {
				t.Fatalf("expected request-scoped HTTP 400, got %v, %v", req, errBuild)
			}
		})
	}
}

func TestAntigravityStockACPConversation(t *testing.T) {
	const conversation = "d0092ae5-5db7-42a5-989f-cc5e54df24a6"
	const other = "c784c133-5d3d-4197-a75e-c7a67dd2899d"
	const info = "<user_information>\nCommand Working Directory: /fixture/workspace\n" +
		"App Data Directory: /fixture/data\nConversation ID: " + conversation + "\n" +
		"Artifact Directory Path: /fixture/data/brain/" + conversation + "\n</user_information>"
	e := NewAntigravityExecutor(&config.Config{Antigravity: config.AntigravityConfig{PreserveGeminiRequests: true}})
	auth := &cliproxyauth.Auth{ID: "account", Metadata: map[string]any{"project_id": "project"}}
	ctx := context.Background()
	build := func(system, explicit, native, worker string) *ojson.Doc {
		t.Helper()
		doc := ojson.NewDocString(`{"request":{"systemInstruction":{"parts":[{}]},"contents":[{"role":"user","parts":[{}]}]}}`)
		_ = doc.Set("request.systemInstruction.parts.0.text", system)
		_ = doc.Set("request.contents.0.parts.0.text", info)
		if native != "" {
			_ = doc.Set("request.sessionId", native)
		}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Headers: http.Header{},
			Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope(worker)}}
		if explicit != "" {
			opts.Headers.Set("X-ACP-Conversation-ID", explicit)
		}
		_, payload, errBuild := e.buildRequestDoc(ctx, auth, "fixture-token", "gemini-4-neon", doc, true, opts, "http://127.0.0.1", "-987")
		if errBuild != nil {
			t.Fatal(errBuild)
		}
		res := ojson.NewDoc(payload)
		if res.Get("request.systemInstruction.parts.0.text").String() != system || res.Get("request.contents.0.parts.0.text").String() != info {
			t.Error("input text changed")
		}
		if explicit == "" && res.Get("request.labels").Exists() {
			t.Error("discovery should only supply a session ID")
		}
		return res
	}
	id := func(doc *ojson.Doc) string { return doc.Get("request.sessionId").String() }
	fromHeader := id(build("", conversation, "", "worker"))
	fromOther := id(build("", other, "", "worker"))
	for _, tc := range []struct{ name, system, header, native, worker, want string }{
		{"stock prompt", "prefix\n" + info, "", "", "worker", fromHeader},
		{"custom agent", info + "\n<messaging>more</messaging>", "", "", "worker", fromHeader},
		{"title or user text only", "Make a title", "", "", "worker", "-987"},
		{"malformed ID", strings.ReplaceAll(info, conversation, "bad-id"), "", "", "worker", "-987"},
		{"artifact mismatch", strings.Replace(info, "/brain/"+conversation, "/brain/"+other, 1), "", "", "worker", "-987"},
		{"duplicate blocks", info + "\n" + info, "", "", "worker", "-987"},
		{"duplicate conversation lines", strings.Replace(info, "App Data Directory:", "Conversation ID: "+other+"\nApp Data Directory:", 1), "", "", "worker", "-987"},
		{"native session", info, "", "native", "worker", "native"},
		{"host override", info, other, "native", "worker", fromOther},
		{"unscoped caller", info, "", "", "", "-987"},
		{"separate worker", info, "", "", "other-worker", id(build("", conversation, "", "other-worker"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := id(build(tc.system, tc.header, tc.native, tc.worker)); got != tc.want {
				t.Errorf("session = %q, want %q", got, tc.want)
			}
		})
	}
	ctx = helps.WithPayloadFinalizer(ctx, func(body []byte) []byte {
		doc := ojson.NewDoc(body)
		if id(doc) != fromHeader {
			t.Error("payload rules did not receive the discovered session")
		}
		_ = doc.Delete("request.sessionId")
		return doc.Bytes()
	})
	if build(info, "", "", "worker").Get("request.sessionId").Exists() {
		t.Error("session ID was restored after filtering")
	}
}
