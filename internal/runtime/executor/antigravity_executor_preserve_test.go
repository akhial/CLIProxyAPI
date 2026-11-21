package executor

import (
	"context"
	"errors"
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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityNativeGeminiRequests(t *testing.T) {
	// Includes an unsigned tool call and constraints on its argument schema.
	const input = `{
		"systemInstruction":{"parts":[{"text":"original instruction"}]},
		"contents":[{"role":"user","parts":[{"text":"one"},{"text":"two"}]},
			{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"value":"x"}}}]}],
		"tools":[{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":{"$schema":"https://json-schema.org/draft/2020-12/schema","additionalProperties":false,"type":"object",
			"properties":{"q":{"type":"string","minLength":1,"enum":["x","y"]}}}}]}],
		"generationConfig":{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"HIGH"}},
		"safetySettings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","threshold":"BLOCK_ONLY_HIGH"}]
	}`
	const expectedRequest = `{
		"systemInstruction":{"parts":[{"text":"original instruction"}]},
		"contents":[{"role":"user","parts":[{"text":"one"},{"text":"two"}]},
			{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"value":"x"}}}]}],
		"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"OBJECT",
			"properties":{"q":{"type":"STRING","minLength":"1","enum":["x","y"]}}}}]}],
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
				cfg := &config.Config{DisableCooling: true}
				want := ojson.NewDocString(expectedRequest)
				if rules {
					models := []config.PayloadModelRule{{Name: "gemini-*", Protocol: "antigravity"}}
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"contents.0.parts.0", "sessionId"}}}
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"generationConfig.maxOutputTokens": 42}}}
					_ = want.Delete("contents.0.parts.0")
					_ = want.Set("generationConfig.maxOutputTokens", 42)
				}
				headers := http.Header{}
				// Exercise SDK headers (replaced with Antigravity CLI UA) and omitted headers independently of stored auth.
				if !rules {
					headers.Set("User-Agent", "google-genai-sdk/1.79.0 gl-go/go1.28")
					headers.Set("X-Goog-Api-Client", "google-genai-sdk/1.79.0 gl-go/go1.28")
				}
				headers.Set("X-Goog-Api-Key", "downstream-only")
				headers.Set("Authorization", "Bearer downstream-only")
				headers.Set("Cookie", "private=worker")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != tc.path || r.Method != http.MethodPost {
						t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
					}
					if gotUA := r.Header.Get("User-Agent"); gotUA != misc.AntigravityGatewayUserAgent || !strings.HasPrefix(gotUA, "antigravity/cli/") {
						t.Errorf("User-Agent = %q, want %q", gotUA, misc.AntigravityGatewayUserAgent)
					}
					if gotAPIClient := r.Header.Get("X-Goog-Api-Client"); gotAPIClient != "" {
						t.Errorf("X-Goog-Api-Client = %q, want omitted", gotAPIClient)
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

	t.Run("preserves native antigravity User-Agent", func(t *testing.T) {
		e := NewAntigravityExecutor(&config.Config{})
		auth := &cliproxyauth.Auth{ID: "native-ua", Metadata: map[string]any{"project_id": "fixture"}}
		nativeUA := "antigravity/cli/1.5.12 (aidev_client; os_type=darwin; arch=arm64; cl=1024904911; auth_method=consumer; datacollection_opt_in=false; host_path=capture-fixture/1)"
		headers := http.Header{}
		headers.Set("User-Agent", nativeUA)
		headers.Set("X-Goog-Api-Client", "should-be-dropped")
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Headers: headers}
		httpReq, _, err := e.buildRequestDoc(context.Background(), auth, "token", "gemini-4-neon", ojson.NewDocString(`{"request":{"contents":[]}}`), true, opts, "http://127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if got := httpReq.Header.Get("User-Agent"); got != nativeUA {
			t.Fatalf("User-Agent = %q, want %q", got, nativeUA)
		}
		if got := httpReq.Header.Get("X-Goog-Api-Client"); got != "" {
			t.Fatalf("X-Goog-Api-Client = %q, want empty", got)
		}
	})
}

func TestAntigravityPreservedACPHostSession(t *testing.T) {
	const (
		root  = "3dbc79e2-5446-427f-aa8b-09466feadb26"
		child = "0ac5d42e-97ba-4fcb-a527-8e7ac6e3a5f7"
	)
	ctx := context.Background()
	e := NewAntigravityExecutor(&config.Config{})
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
	e := NewAntigravityExecutor(&config.Config{})
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
	const info = "<user_information>\nThe USER's OS version is mac.\nCommand Working Directory: /fixture/workspace\n" +
		"App Data Directory: /fixture/data\nConversation ID: " + conversation + "\n" +
		"Artifact Directory Path: /fixture/data/brain/" + conversation + "\n</user_information>"
	e := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "account",
		Metadata: map[string]any{
			"project_id": "project",
			sdkauth.AntigravityPersistedHintsMetaKey: map[string]any{
				"model_enums": map[string]any{
					"gemini-4-neon": "MODEL_PLACEHOLDER_M99",
				},
			},
		},
	}
	ctx := context.Background()
	build := func(system, explicit, native, worker string, initialLabels ...string) (*http.Request, *ojson.Doc) {
		t.Helper()
		doc := ojson.NewDocString(`{"request":{"systemInstruction":{"parts":[{}]},"contents":[{"role":"user","parts":[{}]}]}}`)
		_ = doc.Set("request.systemInstruction.parts.0.text", system)
		_ = doc.Set("request.contents.0.parts.0.text", info)
		if native != "" {
			_ = doc.Set("request.sessionId", native)
		}
		if len(initialLabels) != 0 {
			_ = doc.SetRaw("request.labels", initialLabels[0])
		}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Headers: http.Header{},
			Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope(worker)}}
		if explicit != "" {
			opts.Headers.Set("X-ACP-Conversation-ID", explicit)
		}
		httpReq, payload, errBuild := e.buildRequestDoc(ctx, auth, "fixture-token", "gemini-4-neon", doc, true, opts, "http://127.0.0.1", "-987")
		if errBuild != nil {
			t.Fatal(errBuild)
		}
		res := ojson.NewDoc(payload)
		if res.Get("request.systemInstruction.parts.0.text").String() != system || res.Get("request.contents.0.parts.0.text").String() != info {
			t.Error("input text changed")
		}
		return httpReq, res
	}
	id := func(doc *ojson.Doc) string { return doc.Get("request.sessionId").String() }
	_, host := build("", conversation, "", "worker")
	fromHeader := id(host)
	_, hostOther := build("", other, "", "worker")
	fromOther := id(hostOther)
	_, hostOtherWorker := build("", conversation, "", "other-worker")
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
		{"separate worker", info, "", "", "other-worker", id(hostOtherWorker)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpReq, res := build(tc.system, tc.header, tc.native, tc.worker)
			if got := id(res); got != tc.want {
				t.Errorf("session = %q, want %q", got, tc.want)
			}
			if tc.header != "" {
				return
			}
			labels := res.Get("request.labels")
			if tc.want == "-987" || tc.native != "" {
				if labels.Exists() {
					t.Error("unexpected labels without a discovered conversation")
				}
				return
			}
			// Discovered ACP conversation must match prompt UUID and emit full OAuth-parity labels + requestId.
			if labels.Get("cascade_id").String() != conversation {
				t.Errorf("cascade_id = %q, want %q", labels.Get("cascade_id").String(), conversation)
			}
			if labels.Get("root_cascade_id").String() != conversation {
				t.Errorf("root_cascade_id = %q, want %q", labels.Get("root_cascade_id").String(), conversation)
			}
			trajID := labels.Get("trajectory_id").String()
			if _, errUUID := uuid.Parse(trajID); errUUID != nil || trajID == conversation {
				t.Errorf("trajectory_id = %q, want distinct valid UUID", trajID)
			}
			if got := labels.Get("last_step_index").String(); got != "0" {
				t.Errorf("last_step_index = %q, want 0", got)
			}
			if got := labels.Get("request_id").String(); got != trajID+"-0" {
				t.Errorf("labels.request_id = %q, want %q", got, trajID+"-0")
			}
			for _, flag := range []string{"used_claude", "used_claude_conservative", "used_non_gemini_model"} {
				if got := labels.Get(flag).String(); got != "false" {
					t.Errorf("labels.%s = %q, want false", flag, got)
				}
			}
			if got := labels.Get("model_enum").String(); got != "MODEL_PLACEHOLDER_M99" {
				t.Errorf("labels.model_enum = %q, want MODEL_PLACEHOLDER_M99", got)
			}
			reqID := res.Get("requestId").String()
			reqParts := strings.Split(reqID, "/")
			if len(reqParts) != 5 || reqParts[0] != "agent" || reqParts[1] != conversation || reqParts[3] != trajID {
				t.Errorf("requestId = %q, want agent/<cascade>/<ts>/<traj>/<seq>", reqID)
			}
			if gotUA := httpReq.Header.Get("User-Agent"); !strings.Contains(gotUA, "os_type=darwin; arch=arm64") {
				t.Errorf("User-Agent = %q, want darwin/arm64 platform from user_information", gotUA)
			}
		})
	}
	for _, labels := range []string{`{"cascade_id":"provided","other":"value"}`, `"opaque"`} {
		_, res := build(info, "", "", "worker", labels)
		if got := res.Get("request.labels").Raw; got != labels {
			t.Errorf("existing labels = %s, want %s", got, labels)
		}
	}
	ctx = helps.WithPayloadFinalizer(ctx, func(body []byte) []byte {
		doc := ojson.NewDoc(body)
		if id(doc) != fromHeader {
			t.Error("payload rules did not receive the discovered session")
		}
		_ = doc.Delete("request.sessionId")
		_ = doc.Delete("request.labels")
		return doc.Bytes()
	})
	_, res := build(info, "", "", "worker")
	if res.Get("request.sessionId").Exists() || res.Get("request.labels").Exists() {
		t.Error("correlation fields were restored after filtering")
	}
}

func TestAntigravityACPTitleCheckpointAndSessionCorrelation(t *testing.T) {
	const titlePrompt = "You are a conversation title generator. The message provided is a transcript of a USER's request."
	e := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "acct-title", Metadata: map[string]any{"project_id": "proj-title"}}

	buildTitle := func(ctx context.Context, worker, userPrompt string) (*http.Request, *ojson.Doc) {
		t.Helper()
		doc := ojson.NewDocString(`{"request":{"systemInstruction":{"role":"user","parts":[{}]},"contents":[{"role":"user","parts":[{}]}]}}`)
		_ = doc.Set("request.systemInstruction.parts.0.text", titlePrompt)
		_ = doc.Set("request.contents.0.parts.0.text", userPrompt)
		opts := cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatGemini,
			Headers:      http.Header{"User-Agent": []string{"google-genai-sdk/1.79.0"}, "X-Goog-Api-Client": []string{"google-genai-sdk/1.79.0"}},
			Metadata:     map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope(worker)},
		}
		httpReq, payload, err := e.buildRequestDoc(ctx, auth, "token", "gemini-3.1-flash-lite", doc, true, opts, "http://127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		return httpReq, ojson.NewDoc(payload)
	}

	buildMain := func(ctx context.Context, worker, convID, osName, userPrompt string, extraTurns int) (*http.Request, *ojson.Doc) {
		t.Helper()
		sys := "<user_information>\nThe USER's OS version is " + osName + ".\n" +
			"App Data Directory: /fixture/data\nConversation ID: " + convID + "\n" +
			"Artifact Directory Path: /fixture/data/brain/" + convID + "\n</user_information>"
		doc := ojson.NewDocString(`{"request":{"systemInstruction":{"parts":[{}]},"contents":[{"role":"user","parts":[{}]}]}}`)
		_ = doc.Set("request.systemInstruction.parts.0.text", sys)
		_ = doc.Set("request.contents.0.parts.0.text", "<USER_REQUEST>\n"+userPrompt+"\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nnow\n</ADDITIONAL_METADATA>")
		for i := 0; i < extraTurns; i++ {
			_ = doc.SetRaw(fmt.Sprintf("request.contents.%d", 1+i*2), `{"role":"model","parts":[{"text":"ok"}]}`)
			_ = doc.SetRaw(fmt.Sprintf("request.contents.%d", 2+i*2), `{"role":"user","parts":[{"text":"next"}]}`)
		}
		opts := cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatGemini,
			Headers:      http.Header{"User-Agent": []string{"google-genai-sdk/1.79.0"}},
			Metadata:     map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope(worker)},
		}
		httpReq, payload, err := e.buildRequestDoc(ctx, auth, "token", "gemini-3.1-pro-low", doc, true, opts, "http://127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		return httpReq, ojson.NewDoc(payload)
	}

	ctx := context.Background()

	t.Run("title before main shares sessionId across title and subsequent turns", func(t *testing.T) {
		convID := uuid.NewString()
		worker := "worker-title-first-" + convID
		titleReq, titleDoc := buildTitle(ctx, worker, "Reply with MARIGOLD.")
		if titleDoc.Get("requestType").String() != "checkpoint" {
			t.Fatalf("title requestType = %q, want checkpoint", titleDoc.Get("requestType").String())
		}
		if reqID := titleDoc.Get("requestId").String(); !strings.HasPrefix(reqID, "checkpoint/") {
			t.Fatalf("title requestId = %q, want checkpoint/<uuid>", reqID)
		}
		if titleDoc.Get("request.labels").Exists() {
			t.Fatalf("title must not carry labels, got %s", titleDoc.Get("request.labels").Raw)
		}
		if titleReq.Header.Get("X-Goog-Api-Client") != "" {
			t.Fatalf("title forwarded X-Goog-Api-Client")
		}
		titleSession := titleDoc.Get("request.sessionId").String()
		if !strings.HasPrefix(titleSession, "-") {
			t.Fatalf("title sessionId = %q, want negative int64", titleSession)
		}

		mainReq, mainTurn1 := buildMain(ctx, worker, convID, "mac", "Reply with MARIGOLD.", 0)
		if got := mainTurn1.Get("request.sessionId").String(); got != titleSession {
			t.Fatalf("main turn 1 sessionId = %q, want title session %q", got, titleSession)
		}
		if got := mainTurn1.Get("request.labels.model_enum").String(); got != "MODEL_PLACEHOLDER_M36" {
			t.Fatalf("static model_enum fallback = %q, want MODEL_PLACEHOLDER_M36", got)
		}
		if got := mainTurn1.Get("request.labels.last_step_index").String(); got != "0" {
			t.Fatalf("turn 1 last_step_index = %q, want 0", got)
		}
		if !strings.Contains(mainReq.Header.Get("User-Agent"), "os_type=darwin; arch=arm64") {
			t.Fatalf("main User-Agent = %q, want darwin/arm64", mainReq.Header.Get("User-Agent"))
		}

		_, mainTurn2 := buildMain(ctx, worker, convID, "mac", "Reply with MARIGOLD.", 1)
		if got := mainTurn2.Get("request.sessionId").String(); got != titleSession {
			t.Fatalf("main turn 2 sessionId = %q, want %q", got, titleSession)
		}
		if got := mainTurn2.Get("request.labels.last_step_index").String(); got != "2" {
			t.Fatalf("turn 2 last_step_index = %q, want 2", got)
		}
		if mainTurn1.Get("requestId").String() == mainTurn2.Get("requestId").String() {
			t.Fatalf("turn 1 and turn 2 must have distinct requestIds")
		}

		// Subsequent title from the same Mac worker remembers darwin/arm64 platform.
		nextTitleReq, _ := buildTitle(ctx, worker, "Another prompt.")
		if gotUA := nextTitleReq.Header.Get("User-Agent"); !strings.Contains(gotUA, "os_type=darwin; arch=arm64") {
			t.Fatalf("subsequent title User-Agent = %q, want remembered darwin/arm64", gotUA)
		}
	})

	t.Run("main before title shares sessionId", func(t *testing.T) {
		convID := uuid.NewString()
		worker := "worker-main-first-" + convID
		_, mainDoc := buildMain(ctx, worker, convID, "linux", "Explain goroutines.", 0)
		_, titleDoc := buildTitle(ctx, worker, "Explain goroutines.")
		if titleDoc.Get("request.sessionId").String() != mainDoc.Get("request.sessionId").String() {
			t.Fatalf("title sessionId %q != main sessionId %q", titleDoc.Get("request.sessionId").String(), mainDoc.Get("request.sessionId").String())
		}
	})

	t.Run("overlapping identical prompts keep isolated sessionIds", func(t *testing.T) {
		convA, convB := uuid.NewString(), uuid.NewString()
		worker := "worker-overlap-" + convA
		_, titleA := buildTitle(ctx, worker, "Reply OK.")
		_, titleB := buildTitle(ctx, worker, "Reply OK.")
		if titleA.Get("request.sessionId").String() == titleB.Get("request.sessionId").String() {
			t.Fatalf("concurrent identical titles collided on sessionId %q", titleA.Get("request.sessionId").String())
		}
		_, mainA := buildMain(ctx, worker, convA, "linux", "Reply OK.", 0)
		_, mainB := buildMain(ctx, worker, convB, "linux", "Reply OK.", 0)
		if mainA.Get("request.sessionId").String() != titleA.Get("request.sessionId").String() {
			t.Fatalf("mainA sessionId = %q, want titleA %q", mainA.Get("request.sessionId").String(), titleA.Get("request.sessionId").String())
		}
		if mainB.Get("request.sessionId").String() != titleB.Get("request.sessionId").String() {
			t.Fatalf("mainB sessionId = %q, want titleB %q", mainB.Get("request.sessionId").String(), titleB.Get("request.sessionId").String())
		}
	})

	t.Run("payload rules still run last on checkpoint title requests", func(t *testing.T) {
		finalizedCtx := helps.WithPayloadFinalizer(ctx, func(body []byte) []byte {
			d := ojson.NewDoc(body)
			if d.Get("requestType").String() != "checkpoint" {
				t.Error("expected checkpoint requestType before payload rules")
			}
			_ = d.Set("requestType", "override")
			return d.Bytes()
		})
		_, titleDoc := buildTitle(finalizedCtx, "worker-finalizer-title", "Rule test.")
		if titleDoc.Get("requestType").String() != "override" {
			t.Fatalf("payload rule override did not stick: %q", titleDoc.Get("requestType").String())
		}
	})
}

func TestAntigravityACPToolDeclarations(t *testing.T) {
	const info = "<user_information>\nApp Data Directory: /data\nConversation ID: 93811820-b8a9-4130-b164-0080ad629d08\nArtifact Directory Path: /data/brain/93811820-b8a9-4130-b164-0080ad629d08\n</user_information>"
	const readIn = `{"name":"read","parametersJsonSchema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{"file":{"type":"string","minLength":1}},"required":["file"]}}`
	const readOut = `{"name":"read","parameters":{"properties":{"file":{"minLength":"1","type":"STRING"}},"required":["file"],"type":"OBJECT"}}`
	const list = `{"name":"list","description":"list files","parameters":{"type":"OBJECT"}}`
	const functionsIn = `"functionDeclarations":[` + readIn + `,` + list + `]`
	const functionsOut = `"functionDeclarations":[` + readOut + `,` + list + `]`
	const groupIn = `[{` + functionsIn + `}]`
	const groupOut = `[{` + functionsOut + `}]`
	const splitIn = `[{"functionDeclarations":[` + readIn + `]},{"functionDeclarations":[` + list + `]}]`
	const splitOut = `[{"functionDeclarations":[` + readOut + `]},{"functionDeclarations":[` + list + `]}]`
	e := NewAntigravityExecutor(&config.Config{})
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}
	for _, tc := range []struct{ name, system, tools, want string }{
		{"stock ACP", info, groupIn, splitOut},
		{"already split", info, splitIn, splitOut},
		{"ordinary Gemini client", "Hello", groupIn, groupOut},
		{"group with metadata", info, `[{"other":true,` + functionsIn + `}]`, `[{"other":true,` + functionsOut + `}]`},
		{"mixed tools", info, `[{"googleSearch":{}},{` + functionsIn + `}]`, `[{"googleSearch":{}},{` + functionsOut + `}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := ojson.NewDocString(`{"systemInstruction":{"parts":[{}]}}`)
			_ = input.Set("systemInstruction.parts.0.text", tc.system)
			_ = input.SetRaw("tools", tc.tools)
			req := cliproxyexecutor.Request{Model: "gemini-test", Payload: input.Bytes()}
			ctx := context.Background()
			doc, _, errTranslate := antigravityPayload(req, opts)
			if errTranslate != nil {
				t.Fatal(errTranslate)
			}
			var gotVal, wantVal any
			if err := json.Unmarshal([]byte(doc.Get("request.tools").Raw), &gotVal); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantVal); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotVal, wantVal) {
				t.Fatalf("tools = %s, want %s", doc.Get("request.tools").Raw, tc.want)
			}
			ctx = helps.WithPayloadFinalizer(ctx, func(body []byte) []byte {
				final := ojson.NewDoc(body)
				_ = final.Delete("request.tools.0")
				return final.Bytes()
			})
			auth := &cliproxyauth.Auth{ID: "fixture", Metadata: map[string]any{"project_id": "fixture"}}
			_, body, errBuild := e.buildRequestDoc(ctx, auth, "fixture", req.Model, doc, true, opts, "http://127.0.0.1")
			if errBuild != nil {
				t.Fatal(errBuild)
			}
			if got := ojson.GetBytes(body, "request.tools.#").Int(); got != ojson.Parse(tc.want).Get("#").Int()-1 {
				t.Fatal("configured tool filter must run after grouping and schema conversion")
			}
		})
	}
}

func TestAntigravityACPSchemaConversionEdgeCases(t *testing.T) {
	input := `{
		"tools":[{"functionDeclarations":[{
			"name":"mcp_complex",
			"description":"Complex MCP tool",
			"parametersJsonSchema":{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"$defs":{"Tag":{"type":"string","minLength":2}},
				"type":"object",
				"additionalProperties":false,
				"allOf":[{"properties":{"merged":{"type":"boolean"}},"required":["merged"]}],
				"properties":{
					"tag":{"$ref":"#/$defs/Tag"},
					"opt":{"type":["string","null"],"description":"nullable string"},
					"union":{"oneOf":[{"type":"integer","minimum":1,"maximum":10},{"type":"null"}]},
					"kind":{"const":"fixed"},
					"numEnum":{"type":"integer","enum":[1,2,3]},
					"tags":{"type":"array","maxItems":5},
					"empty":{},
					"nested":{"type":"object","additionalProperties":{"type":"string"},"properties":{"a":{"type":"number"}}}
				},
				"required":["tag","merged","dangling_missing"]
			}
		}]}]
	}`
	doc, _, err := antigravityPayload(cliproxyexecutor.Request{Model: "gemini-4-neon", Payload: []byte(input)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err != nil {
		t.Fatal(err)
	}
	decl := doc.Get("request.tools.0.functionDeclarations.0")
	if decl.Get("parametersJsonSchema").Exists() {
		t.Fatalf("parametersJsonSchema should have been removed: %s", decl.Raw)
	}
	params := decl.Get("parameters")
	if params.Get("type").String() != "OBJECT" {
		t.Fatalf("root type = %q, want OBJECT", params.Get("type").String())
	}
	if params.Get("$schema").Exists() || params.Get("$defs").Exists() || params.Get("additionalProperties").Exists() {
		t.Fatalf("unsupported root keywords remained: %s", params.Raw)
	}
	if got := params.Get("properties.tag.type").String(); got != "STRING" {
		t.Errorf("tag.type = %q, want STRING (inlined $ref)", got)
	}
	if got := params.Get("properties.tag.minLength").String(); got != "2" {
		t.Errorf("tag.minLength = %q, want \"2\"", got)
	}
	if got := params.Get("properties.opt.type").String(); got != "STRING" || !params.Get("properties.opt.nullable").Bool() {
		t.Errorf("opt = %s, want STRING + nullable", params.Get("properties.opt").Raw)
	}
	if got := params.Get("properties.union.type").String(); got != "INTEGER" || !params.Get("properties.union.nullable").Bool() {
		t.Errorf("union = %s, want INTEGER + nullable", params.Get("properties.union").Raw)
	}
	if params.Get("properties.union.minimum").Int() != 1 || params.Get("properties.union.maximum").Int() != 10 {
		t.Errorf("union bounds lost: %s", params.Get("properties.union").Raw)
	}
	if got := params.Get("properties.kind.enum.0").String(); got != "fixed" || params.Get("properties.kind.type").String() != "STRING" {
		t.Errorf("kind = %s, want STRING enum [fixed]", params.Get("properties.kind").Raw)
	}
	if params.Get("properties.numEnum.enum").Exists() {
		t.Errorf("non-STRING enum should be stripped for Cloud Code compatibility: %s", params.Get("properties.numEnum").Raw)
	}
	if params.Get("properties.tags.items.type").String() != "STRING" || params.Get("properties.tags.maxItems").String() != "5" {
		t.Errorf("tags = %s, want items.type=STRING and maxItems=\"5\"", params.Get("properties.tags").Raw)
	}
	if params.Get("properties.empty").Raw != "{}" {
		t.Errorf("empty = %s, want {}", params.Get("properties.empty").Raw)
	}
	if params.Get("properties.nested.additionalProperties").Exists() {
		t.Errorf("nested additionalProperties should be stripped: %s", params.Get("properties.nested").Raw)
	}
	if params.Get("properties.merged.type").String() != "BOOLEAN" {
		t.Errorf("allOf merged property missing: %s", params.Raw)
	}
	if params.Get("required.#").Int() != 2 {
		t.Errorf("required = %s, want [tag, merged] without dangling key", params.Get("required").Raw)
	}
}

func TestAntigravityRejectsUnsupportedRoutesBeforeAuthentication(t *testing.T) {
	e := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: t.Name(), Metadata: map[string]any{
		"refresh_token": "fixture-refresh", "expired": "2000-01-01T00:00:00Z",
	}}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected outbound request for an unsupported route: %s", r.URL.Path)
		return nil, fmt.Errorf("unexpected outbound request")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", nil)
	if _, err := e.HttpRequest(ctx, auth, req); err == nil {
		t.Fatal("raw Antigravity HTTP request accepted")
	}
	for _, tc := range []struct {
		name, model      string
		source, response sdktranslator.Format
	}{
		{"non-Gemini model", "claude-test", sdktranslator.FormatGemini, sdktranslator.FormatGemini},
		{"foreign request", "gemini-test", sdktranslator.FormatClaude, sdktranslator.FormatGemini},
		{"foreign response", "gemini-test", sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse},
		{"missing source", "gemini-test", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: tc.model, Payload: []byte(`{"contents":[]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: tc.source, ResponseFormat: tc.response}
			_, unary := e.Execute(ctx, auth, req, opts)
			_, stream := e.ExecuteStream(ctx, auth, req, opts)
			_, count := e.CountTokens(ctx, auth, req, opts)
			for operation, err := range map[string]error{"generate": unary, "stream": stream, "count": count} {
				var rejected statusErr
				if !errors.As(err, &rejected) || rejected.StatusCode() != http.StatusBadRequest {
					t.Errorf("%s returned %v, want HTTP 400", operation, err)
				}
			}
		})
	}
}
