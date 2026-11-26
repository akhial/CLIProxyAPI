package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type capturedUpstreamRequest struct {
	Path    string
	Headers http.Header
	Body    []byte
}

func newFakeAntigravityStreamServer(t *testing.T, captured *[]capturedUpstreamRequest, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*captured = append(*captured, capturedUpstreamRequest{
			Path:    r.URL.RequestURI(),
			Headers: r.Header.Clone(),
			Body:    body,
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(r.URL.Path, "countTokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"totalTokens":42}`)
			return
		}
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"MARIGOLD\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":2,\"totalTokenCount\":12}}}\n\n")
	}))
}

func testAntigravityACPAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "auth-acp-1",
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": baseURL,
		},
		Metadata: map[string]any{
			"access_token": "tok-acp-test",
			"project_id":   "proj-acp-test",
			"email":        "acp-user@example.com",
			"expired":      "2099-01-01T00:00:00Z",
		},
	}
}

func sampleACPSystemPrompt(osName, convID string) string {
	return "<identity>\nYou are Antigravity.\n</identity>\n<user_information>\nThe USER's OS version is " + osName + ".\nApp Data Directory: /fixture/home/.gemini/antigravity-cli\nConversation ID: " + convID + "\nArtifact Directory Path: /fixture/home/.gemini/antigravity-cli/brain/" + convID + "\n</user_information>"
}

func sampleACPMainPayload(osName, convID, prompt string) []byte {
	doc := ojson.NewDocString(`{
		"contents": [{"role": "user", "parts": [{"text": ""}]}],
		"generationConfig": {
			"maxOutputTokens": 65535,
			"thinkingConfig": {"includeThoughts": true, "thinkingBudget": 1024}
		},
		"systemInstruction": {"role": "user", "parts": [{"text": ""}]},
		"tools": [{
			"functionDeclarations": [
				{
					"name": "manage_task",
					"description": "Manage background tasks.",
					"parametersJsonSchema": {
						"$schema": "https://json-schema.org/draft/2020-12/schema",
						"additionalProperties": false,
						"type": "object",
						"properties": {
							"Action": {
								"type": "string",
								"description": "Action to perform.",
								"enum": ["list", "kill", "status", "send_input"]
							},
							"TaskId": {
								"type": "string",
								"minLength": 1
							}
						},
						"required": ["Action"]
					}
				},
				{
					"name": "generate_image",
					"description": "Generate an image.",
					"parametersJsonSchema": {
						"$schema": "https://json-schema.org/draft/2020-12/schema",
						"additionalProperties": false,
						"type": "object",
						"properties": {
							"ImagePaths": {
								"type": "array",
								"items": {"type": "string"},
								"maxItems": 3
							},
							"Prompt": {
								"type": "string",
								"minLength": 1
							}
						},
						"required": ["Prompt"]
					}
				},
				{
					"name": "noop_tool",
					"description": "Tool with no parameters.",
					"parametersJsonSchema": {
						"$schema": "https://json-schema.org/draft/2020-12/schema",
						"additionalProperties": false,
						"type": "object",
						"properties": {}
					}
				}
			]
		}]
	}`)
	_ = doc.Set("contents.0.parts.0.text", "<USER_REQUEST>\n"+prompt+"\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-26T00:00:00Z.\n</ADDITIONAL_METADATA>")
	_ = doc.Set("systemInstruction.parts.0.text", sampleACPSystemPrompt(osName, convID))
	return doc.Bytes()
}

func sampleACPTitlePayload(prompt string) []byte {
	doc := ojson.NewDocString(`{
		"contents": [{"role": "user", "parts": [{"text": ""}]}],
		"generationConfig": {
			"thinkingConfig": {"includeThoughts": true, "thinkingBudget": -1}
		},
		"systemInstruction": {
			"role": "user",
			"parts": [{"text": "You are a conversation title generator. Respond with ONLY a short conversation title."}]
		}
	}`)
	_ = doc.Set("contents.0.parts.0.text", prompt)
	return doc.Bytes()
}

func drainStream(t *testing.T, res *cliproxyexecutor.StreamResult) {
	t.Helper()
	if res == nil || res.Chunks == nil {
		return
	}
	for chunk := range res.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
}

func TestAntigravityACPStreamMainAndTitleCorrelation(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedUpstreamRequest
	srv := newFakeAntigravityStreamServer(t, &captured, &mu)
	defer srv.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := testAntigravityACPAuth(srv.URL)
	worker := session.CallerScope("worker-acp-stream-1")
	convID := "11111111-1111-4111-8111-111111111111"
	prompt := "Reply with the single word MARIGOLD."

	sdkHeaders := http.Header{
		"User-Agent":        {"google-genai-sdk/1.79.0 gl-go/go1.28"},
		"X-Goog-Api-Client": {"google-genai-sdk/1.79.0 gl-go/go1.28"},
	}

	// 1. Main arrives first (macOS platform in <user_information>).
	mainBody := sampleACPMainPayload("mac", convID, prompt)
	mainStream, errMain := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: mainBody,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: mainBody,
		Headers:         sdkHeaders.Clone(),
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: worker},
	})
	if errMain != nil {
		t.Fatalf("main ExecuteStream error: %v", errMain)
	}
	drainStream(t, mainStream)

	// 2. Title arrives second for the same prompt & worker.
	titleBody := sampleACPTitlePayload(prompt)
	titleStream, errTitle := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: titleBody,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: titleBody,
		Headers:         sdkHeaders.Clone(),
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: worker},
	})
	if errTitle != nil {
		t.Fatalf("title ExecuteStream error: %v", errTitle)
	}
	drainStream(t, titleStream)

	if len(captured) != 2 {
		t.Fatalf("captured %d requests, want 2", len(captured))
	}

	mainReq := captured[0]
	titleReq := captured[1]

	wantUA := misc.AntigravityCLIUserAgent("darwin", "arm64", misc.AntigravityDefaultHostPath)
	if got := mainReq.Headers.Get("User-Agent"); got != wantUA {
		t.Fatalf("main User-Agent = %q, want %q", got, wantUA)
	}
	if got := titleReq.Headers.Get("User-Agent"); got != wantUA {
		t.Fatalf("title User-Agent = %q, want %q (remembered platform)", got, wantUA)
	}
	if got := mainReq.Headers.Get("X-Goog-Api-Client"); got != "" {
		t.Fatalf("main X-Goog-Api-Client = %q, want empty", got)
	}
	if got := titleReq.Headers.Get("X-Goog-Api-Client"); got != "" {
		t.Fatalf("title X-Goog-Api-Client = %q, want empty", got)
	}

	mainParsed := ojson.ParseBytes(mainReq.Body)
	titleParsed := ojson.ParseBytes(titleReq.Body)

	// Shared sessionId across main and title.
	mainSession := mainParsed.Get("request.sessionId").String()
	titleSession := titleParsed.Get("request.sessionId").String()
	if mainSession == "" || mainSession != titleSession {
		t.Fatalf("sessionId mismatch: main=%q title=%q", mainSession, titleSession)
	}

	// Main envelope + labels.
	if got := mainParsed.Get("requestType").String(); got != "agent" {
		t.Fatalf("main requestType = %q, want agent", got)
	}
	if got := mainParsed.Get("request.labels.cascade_id").String(); got != convID {
		t.Fatalf("cascade_id = %q, want %q", got, convID)
	}
	if got := mainParsed.Get("request.labels.root_cascade_id").String(); got != convID {
		t.Fatalf("root_cascade_id = %q, want %q", got, convID)
	}
	trajID := mainParsed.Get("request.labels.trajectory_id").String()
	if trajID == "" {
		t.Fatal("trajectory_id is empty")
	}
	if got := mainParsed.Get("request.labels.last_step_index").String(); got != "0" {
		t.Fatalf("last_step_index = %q, want 0", got)
	}
	if got := mainParsed.Get("request.labels.request_id").String(); got != trajID+"-0" {
		t.Fatalf("labels.request_id = %q, want %q", got, trajID+"-0")
	}
	if got := mainParsed.Get("request.labels.model_enum").String(); got != "MODEL_PLACEHOLDER_M36" {
		t.Fatalf("model_enum = %q, want MODEL_PLACEHOLDER_M36", got)
	}
	if got := mainParsed.Get("request.labels.used_claude").String(); got != "false" {
		t.Fatalf("used_claude = %q, want false", got)
	}
	if got := mainParsed.Get("request.labels.used_non_gemini_model").String(); got != "false" {
		t.Fatalf("used_non_gemini_model = %q, want false", got)
	}
	reqID := mainParsed.Get("requestId").String()
	if !strings.HasPrefix(reqID, "agent/"+convID+"/") || !strings.HasSuffix(reqID, "/"+trajID+"/1") {
		t.Fatalf("main requestId = %q, want agent/%s/<ms>/%s/1", reqID, convID, trajID)
	}

	// maxOutputTokens preserved (not stripped by upstream).
	if got := mainParsed.Get("request.generationConfig.maxOutputTokens").Int(); got != 65535 {
		t.Fatalf("maxOutputTokens = %d, want 65535", got)
	}

	// Tools split into 3 single-declaration entries, proto parameters converted, enums preserved, no placeholders.
	tools := mainParsed.Get("request.tools").Array()
	if len(tools) != 3 {
		t.Fatalf("len(request.tools) = %d, want 3", len(tools))
	}
	manageDecl := tools[0].Get("functionDeclarations.0")
	if manageDecl.Get("parametersJsonSchema").Exists() {
		t.Fatalf("parametersJsonSchema should be converted to parameters: %s", manageDecl.Raw)
	}
	if got := manageDecl.Get("parameters.type").String(); got != "OBJECT" {
		t.Fatalf("manage_task parameters.type = %q, want OBJECT", got)
	}
	if got := manageDecl.Get("parameters.properties.Action.type").String(); got != "STRING" {
		t.Fatalf("Action.type = %q, want STRING", got)
	}
	enumVals := manageDecl.Get("parameters.properties.Action.enum").Array()
	if len(enumVals) != 4 || enumVals[0].String() != "list" || enumVals[3].String() != "send_input" {
		t.Fatalf("Action.enum = %s, want preserved 4-element enum", manageDecl.Get("parameters.properties.Action.enum").Raw)
	}
	if got := manageDecl.Get("parameters.properties.TaskId.minLength"); got.Type != ojson.String || got.String() != "1" {
		t.Fatalf("TaskId.minLength = %s (type %v), want string \"1\"", got.Raw, got.Type)
	}
	imageDecl := tools[1].Get("functionDeclarations.0")
	if got := imageDecl.Get("parameters.properties.ImagePaths.maxItems"); got.Type != ojson.String || got.String() != "3" {
		t.Fatalf("ImagePaths.maxItems = %s (type %v), want string \"3\"", got.Raw, got.Type)
	}
	noopDecl := tools[2].Get("functionDeclarations.0")
	if noopDecl.Get("parameters.properties.reason").Exists() || noopDecl.Get("parameters.properties._placeholder").Exists() {
		t.Fatalf("noop_tool unexpectedly received synthetic placeholder: %s", noopDecl.Raw)
	}

	// Wire minification & canonical key order still active from upstream SerializeAntigravityPayload.
	rawMain := string(mainReq.Body)
	if !strings.HasPrefix(rawMain, `{"project":"proj-acp-test","requestId":"agent/`) {
		t.Fatalf("expected canonical upstream wire key order, got prefix: %.80s", rawMain)
	}

	// Title classified as checkpoint with no labels.
	if got := titleParsed.Get("requestType").String(); got != "checkpoint" {
		t.Fatalf("title requestType = %q, want checkpoint", got)
	}
	if got := titleParsed.Get("requestId").String(); !strings.HasPrefix(got, "checkpoint/") {
		t.Fatalf("title requestId = %q, want checkpoint/<uuid>", got)
	}
	if titleParsed.Get("request.labels").Exists() {
		t.Fatalf("title should not have request.labels, got: %s", titleParsed.Get("request.labels").Raw)
	}
}

func TestAntigravityACPTitleArrivingBeforeMain(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedUpstreamRequest
	srv := newFakeAntigravityStreamServer(t, &captured, &mu)
	defer srv.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := testAntigravityACPAuth(srv.URL)
	worker := session.CallerScope("worker-acp-title-first")
	convID := "22222222-2222-4222-8222-222222222222"
	prompt := "Summarize the architecture."

	titleBody := sampleACPTitlePayload(prompt)
	titleStream, errTitle := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: titleBody,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: titleBody,
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: worker},
	})
	if errTitle != nil {
		t.Fatalf("title ExecuteStream error: %v", errTitle)
	}
	drainStream(t, titleStream)

	mainBody := sampleACPMainPayload("linux", convID, prompt)
	mainStream, errMain := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: mainBody,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: mainBody,
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: worker},
	})
	if errMain != nil {
		t.Fatalf("main ExecuteStream error: %v", errMain)
	}
	drainStream(t, mainStream)

	titleSession := ojson.GetBytesNoCopy(captured[0].Body, "request.sessionId").String()
	mainSession := ojson.GetBytesNoCopy(captured[1].Body, "request.sessionId").String()
	if titleSession == "" || titleSession != mainSession {
		t.Fatalf("expected title-first and main to share sessionId: title=%q main=%q", titleSession, mainSession)
	}
}

func TestAntigravityACPRejectsNonACPAndNonGeminiRequests(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedUpstreamRequest
	srv := newFakeAntigravityStreamServer(t, &captured, &mu)
	defer srv.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := testAntigravityACPAuth(srv.URL)

	cases := []struct {
		name    string
		model   string
		format  sdktranslator.Format
		body    []byte
		headers http.Header
	}{
		{
			name:   "openai chat format",
			model:  "gemini-3.1-pro-low",
			format: sdktranslator.FormatOpenAI,
			body:   []byte(`{"model":"gemini-3.1-pro-low","messages":[{"role":"user","content":"hi"}]}`),
		},
		{
			name:   "openai responses format",
			model:  "gemini-3.1-pro-low",
			format: sdktranslator.FormatOpenAIResponse,
			body:   []byte(`{"model":"gemini-3.1-pro-low","input":"hi"}`),
		},
		{
			name:   "claude format and model",
			model:  "claude-sonnet-4-6",
			format: sdktranslator.FormatClaude,
			body:   []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`),
		},
		{
			name:    "claude model via gemini format with ACP header",
			model:   "claude-sonnet-4-6",
			format:  sdktranslator.FormatGemini,
			body:    []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
			headers: http.Header{"X-Acp-Conversation-Id": {"77777777-7777-4777-8777-777777777777"}},
		},
		{
			name:   "generic non-ACP gemini request",
			model:  "gemini-3.1-pro-low",
			format: sdktranslator.FormatGemini,
			body:   []byte(`{"contents":[{"role":"user","parts":[{"text":"hello world"}]}],"systemInstruction":{"role":"user","parts":[{"text":"You are a helpful assistant."}]}}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: tc.model, Payload: tc.body}
			opts := cliproxyexecutor.Options{
				SourceFormat:    tc.format,
				OriginalRequest: tc.body,
				Headers:         tc.headers,
			}
			if _, errExec := exec.Execute(context.Background(), auth, req, opts); errExec == nil {
				t.Fatalf("Execute unexpectedly succeeded for %s", tc.name)
			}
			opts.Stream = true
			if _, errStream := exec.ExecuteStream(context.Background(), auth, req, opts); errStream == nil {
				t.Fatalf("ExecuteStream unexpectedly succeeded for %s", tc.name)
			}
			opts.Stream = false
			if _, errCount := exec.CountTokens(context.Background(), auth, req, opts); errCount == nil {
				t.Fatalf("CountTokens unexpectedly succeeded for %s", tc.name)
			}
		})
	}

	if len(captured) != 0 {
		t.Fatalf("rejected non-ACP requests still made %d upstream call(s)", len(captured))
	}
}

func TestAntigravityACPPayloadRulesRunLast(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedUpstreamRequest
	srv := newFakeAntigravityStreamServer(t, &captured, &mu)
	defer srv.Close()

	cfg := &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{
						{Name: "gemini-3.1-pro-low", Protocol: "antigravity"},
					},
					Params: map[string]any{
						"labels.cascade_id": "overridden-by-user-rule",
					},
				},
			},
			Filter: []config.PayloadFilterRule{
				{
					Models: []config.PayloadModelRule{
						{Name: "gemini-3.1-pro-low", Protocol: "antigravity"},
					},
					Params: []string{"labels.model_enum"},
				},
			},
		},
	}

	exec := NewAntigravityExecutor(cfg)
	auth := testAntigravityACPAuth(srv.URL)
	convID := "33333333-3333-4333-8333-333333333333"
	body := sampleACPMainPayload("mac", convID, "Test payload ordering")

	stream, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: body,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: body,
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope("worker-rules")},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	drainStream(t, stream)

	parsed := ojson.ParseBytes(captured[0].Body)
	if got := parsed.Get("request.labels.cascade_id").String(); got != "overridden-by-user-rule" {
		t.Fatalf("user payload override did not win: cascade_id = %q", got)
	}
	if parsed.Get("request.labels.model_enum").Exists() {
		t.Fatalf("user payload filter did not remove model_enum: %s", parsed.Get("request.labels").Raw)
	}
}

func TestAntigravityACPExplicitHeadersAndValidation(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedUpstreamRequest
	srv := newFakeAntigravityStreamServer(t, &captured, &mu)
	defer srv.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := testAntigravityACPAuth(srv.URL)
	baseBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

	// Invalid X-ACP-Conversation-ID must fail with 400.
	_, errBad := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: baseBody,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: baseBody,
		Headers:         http.Header{"X-Acp-Conversation-Id": {"not-a-uuid"}},
	})
	if errBad == nil {
		t.Fatal("expected error for invalid X-ACP-Conversation-ID")
	}

	// Valid explicit headers override cascade_id, root_cascade_id, step, and host_path.
	convID := "44444444-4444-4444-8444-444444444444"
	rootID := "55555555-5555-4555-8555-555555555555"
	stream, errOK := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: baseBody,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: baseBody,
		Headers: http.Header{
			"X-Acp-Conversation-Id":      {convID},
			"X-Acp-Root-Conversation-Id": {rootID},
			"X-Acp-Step":                 {"7"},
			"X-Acp-Host":                 {"custom-host/2.0"},
		},
		Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope("worker-explicit")},
	})
	if errOK != nil {
		t.Fatalf("explicit ACP headers ExecuteStream error: %v", errOK)
	}
	drainStream(t, stream)

	parsed := ojson.ParseBytes(captured[0].Body)
	if got := parsed.Get("request.labels.cascade_id").String(); got != convID {
		t.Fatalf("cascade_id = %q, want %q", got, convID)
	}
	if got := parsed.Get("request.labels.root_cascade_id").String(); got != rootID {
		t.Fatalf("root_cascade_id = %q, want %q", got, rootID)
	}
	if got := parsed.Get("request.labels.last_step_index").String(); got != "7" {
		t.Fatalf("last_step_index = %q, want 7", got)
	}
	if got := captured[0].Headers.Get("User-Agent"); !strings.Contains(got, "host_path=custom-host/2.0)") {
		t.Fatalf("User-Agent = %q, want host_path=custom-host/2.0", got)
	}
}

func TestAntigravityACPCountTokens(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedUpstreamRequest
	srv := newFakeAntigravityStreamServer(t, &captured, &mu)
	defer srv.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := testAntigravityACPAuth(srv.URL)
	convID := "66666666-6666-4666-8666-666666666666"
	body := sampleACPMainPayload("mac", convID, "Count my tokens")

	_, err := exec.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: body,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: body,
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope("worker-count")},
	})
	if err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d requests, want 1", len(captured))
	}
	wantUA := misc.AntigravityCLIUserAgent("darwin", "arm64", misc.AntigravityDefaultHostPath)
	if got := captured[0].Headers.Get("User-Agent"); got != wantUA {
		t.Fatalf("CountTokens User-Agent = %q, want %q", got, wantUA)
	}
	parsed := ojson.ParseBytes(captured[0].Body)
	if got := parsed.Get("request.tools.0.functionDeclarations.0.parameters.properties.Action.enum.#").Int(); got != 4 {
		t.Fatalf("CountTokens stripped enum: %s", parsed.Get("request.tools.0").Raw)
	}
}

func TestAntigravityACPMacOSFixtureParity(t *testing.T) {
	fixturePath := strings.TrimSpace(os.Getenv("ACP_FIXTURE_PATH"))
	if fixturePath == "" {
		t.Skip("ACP_FIXTURE_PATH not set")
	}
	rawFixture, errRead := os.ReadFile(fixturePath)
	if errRead != nil {
		t.Fatalf("read fixture %s: %v", fixturePath, errRead)
	}
	doc := ojson.ParseBytes(rawFixture)

	var mu sync.Mutex
	var captured []capturedUpstreamRequest
	srv := newFakeAntigravityStreamServer(t, &captured, &mu)
	defer srv.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := testAntigravityACPAuth(srv.URL)
	worker := session.CallerScope("worker-macos-fixture")

	for _, name := range []string{"developerMain", "developerTitle"} {
		entry := doc.Get("requests." + name)
		body := []byte(entry.Get("body").Raw)
		headers := make(http.Header)
		entry.Get("headers").ForEach(func(k, v ojson.Result) bool {
			headers.Set(k.String(), v.String())
			return true
		})
		stream, errStream := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "gemini-3.1-pro-low",
			Payload: body,
		}, cliproxyexecutor.Options{
			Stream:          true,
			SourceFormat:    sdktranslator.FormatGemini,
			OriginalRequest: body,
			Headers:         headers,
			Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: worker},
		})
		if errStream != nil {
			t.Fatalf("%s ExecuteStream error: %v", name, errStream)
		}
		drainStream(t, stream)
	}

	if len(captured) != 2 {
		t.Fatalf("captured %d requests, want 2", len(captured))
	}
	mainReq, titleReq := captured[0], captured[1]
	wantUA := misc.AntigravityCLIUserAgent("darwin", "arm64", misc.AntigravityDefaultHostPath)
	if got := mainReq.Headers.Get("User-Agent"); got != wantUA {
		t.Fatalf("main UA = %q, want %q", got, wantUA)
	}
	if got := titleReq.Headers.Get("User-Agent"); got != wantUA {
		t.Fatalf("title UA = %q, want %q", got, wantUA)
	}
	if mainReq.Headers.Get("X-Goog-Api-Client") != "" || titleReq.Headers.Get("X-Goog-Api-Client") != "" {
		t.Fatalf("unexpected X-Goog-Api-Client header")
	}

	mainBody := ojson.ParseBytes(mainReq.Body)
	titleBody := ojson.ParseBytes(titleReq.Body)

	if mainBody.Get("request.sessionId").String() != titleBody.Get("request.sessionId").String() {
		t.Fatalf("sessionId mismatch: main=%q title=%q", mainBody.Get("request.sessionId").String(), titleBody.Get("request.sessionId").String())
	}
	if titleBody.Get("requestType").String() != "checkpoint" || !strings.HasPrefix(titleBody.Get("requestId").String(), "checkpoint/") || titleBody.Get("request.labels").Exists() {
		t.Fatalf("title checkpoint mismatch: type=%q id=%q labels=%v", titleBody.Get("requestType").String(), titleBody.Get("requestId").String(), titleBody.Get("request.labels").Exists())
	}
	if got := mainBody.Get("request.labels.cascade_id").String(); got != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("cascade_id = %q", got)
	}
	if got := mainBody.Get("request.labels.model_enum").String(); got != "MODEL_PLACEHOLDER_M36" {
		t.Fatalf("model_enum = %q", got)
	}
	if got := mainBody.Get("request.generationConfig.maxOutputTokens").Int(); got != 65535 {
		t.Fatalf("maxOutputTokens = %d, want 65535", got)
	}

	// Compare every overlapping tool parameter schema against oauthMain verbatim.
	oauthTools := make(map[string]any)
	doc.Get("requests.oauthMain.body.request.tools").ForEach(func(_, tool ojson.Result) bool {
		decl := tool.Get("functionDeclarations.0")
		var params any
		_ = json.Unmarshal([]byte(decl.Get("parameters").Raw), &params)
		oauthTools[decl.Get("name").String()] = params
		return true
	})
	gotTools := make(map[string]any)
	mainBody.Get("request.tools").ForEach(func(_, tool ojson.Result) bool {
		decl := tool.Get("functionDeclarations.0")
		var params any
		_ = json.Unmarshal([]byte(decl.Get("parameters").Raw), &params)
		gotTools[decl.Get("name").String()] = params
		return true
	})
	overlap := 0
	for name, wantParams := range oauthTools {
		gotParams, ok := gotTools[name]
		if !ok {
			continue
		}
		overlap++
		if !reflect.DeepEqual(gotParams, wantParams) {
			t.Errorf("tool %q parameters mismatch:\n  got:  %#v\n  want: %#v", name, gotParams, wantParams)
		}
	}
	if overlap != 9 {
		t.Fatalf("expected 9 overlapping built-in tools, got %d", overlap)
	}
}

func TestAntigravityACPVerifyInferenceShortCircuits(t *testing.T) {
	upstreamCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	mgr := cliproxyauth.NewManager(nil, nil, nil)
	mgr.RegisterExecutor(NewAntigravityExecutor(&config.Config{}))
	auth := &cliproxyauth.Auth{
		ID:         "auth-acp-verify-1",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": srv.URL},
		Metadata: map[string]any{
			"type":          "antigravity",
			"access_token":  "stale-token",
			"refresh_token": "refresh-token",
			"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if _, errReg := mgr.Register(context.Background(), auth); errReg != nil {
		t.Fatalf("Register error: %v", errReg)
	}

	_, errVerify := mgr.VerifyInference(context.Background(), auth.ID, "gemini-3.1-pro-low")
	if errVerify == nil {
		t.Fatal("expected VerifyInference to fail for ACP-only Antigravity credential")
	}
	var authErr *cliproxyauth.Error
	if !errors.As(errVerify, &authErr) || authErr.HTTPStatus != http.StatusBadRequest || authErr.Code != "unsupported_operation" {
		t.Fatalf("VerifyInference error = %v, want 400 unsupported_operation", errVerify)
	}
	if upstreamCalls != 0 {
		t.Fatalf("VerifyInference made %d upstream call(s) before short-circuiting, want 0", upstreamCalls)
	}
}

func TestAntigravityACPMultiTurnToolUse(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedUpstreamRequest
	srv := newFakeAntigravityStreamServer(t, &captured, &mu)
	defer srv.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := testAntigravityACPAuth(srv.URL)
	worker := session.CallerScope("worker-multiturn")
	convID := "88888888-8888-4888-8888-888888888888"
	prompt := "Read README.md"

	turn1Body := sampleACPMainPayload("mac", convID, prompt)
	stream1, err1 := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: turn1Body,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: turn1Body,
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: worker},
	})
	if err1 != nil {
		t.Fatalf("turn 1 error: %v", err1)
	}
	drainStream(t, stream1)

	// Turn 2: appends model functionCall + user functionResponse with result key.
	turn2Doc := ojson.NewDoc(turn1Body)
	_ = turn2Doc.SetRaw("contents.1", `{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"manage_task","args":{"Action":"list"}}}]}`)
	_ = turn2Doc.SetRaw("contents.2", `{"role":"user","parts":[{"functionResponse":{"id":"call-1","name":"manage_task","response":{"result":"no running tasks"}}}]}`)
	turn2Body := turn2Doc.Bytes()

	stream2, err2 := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-low",
		Payload: turn2Body,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: turn2Body,
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: worker},
	})
	if err2 != nil {
		t.Fatalf("turn 2 error: %v", err2)
	}
	drainStream(t, stream2)

	if len(captured) != 2 {
		t.Fatalf("captured %d requests, want 2", len(captured))
	}
	t1 := ojson.ParseBytes(captured[0].Body)
	t2 := ojson.ParseBytes(captured[1].Body)

	if t1.Get("request.sessionId").String() != t2.Get("request.sessionId").String() {
		t.Fatalf("sessionId changed across turns: %q vs %q", t1.Get("request.sessionId").String(), t2.Get("request.sessionId").String())
	}
	if t1.Get("request.labels.cascade_id").String() != t2.Get("request.labels.cascade_id").String() {
		t.Fatalf("cascade_id changed across turns")
	}
	trajID := t1.Get("request.labels.trajectory_id").String()
	if t2.Get("request.labels.trajectory_id").String() != trajID {
		t.Fatalf("trajectory_id changed across turns")
	}
	if got := t2.Get("request.labels.last_step_index").String(); got != "2" {
		t.Fatalf("turn 2 last_step_index = %q, want 2 (1 model turn + 1 tool result)", got)
	}
	if got := t2.Get("request.labels.request_id").String(); got != trajID+"-1" {
		t.Fatalf("turn 2 labels.request_id = %q, want %q", got, trajID+"-1")
	}
	if got := t2.Get("requestId").String(); !strings.HasSuffix(got, "/"+trajID+"/2") {
		t.Fatalf("turn 2 requestId = %q, want suffix /%s/2", got, trajID)
	}
	// Verify functionResponse.response.result was normalized to .output per native wire format.
	if got := t2.Get("request.contents.2.parts.0.functionResponse.response.output").String(); got != "no running tasks" {
		t.Fatalf("turn 2 functionResponse.response.output = %q, want 'no running tasks' (raw=%s)", got, t2.Get("request.contents.2").Raw)
	}
}

func TestAntigravityACPLinuxDefaultsToAMD64(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedUpstreamRequest
	srv := newFakeAntigravityStreamServer(t, &captured, &mu)
	defer srv.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := testAntigravityACPAuth(srv.URL)
	convID := "99999999-9999-4999-8999-999999999999"
	body := sampleACPMainPayload("linux", convID, "Hello from Linux")

	stream, errExec := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-4-neon",
		Payload: body,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: body,
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope("worker-linux-amd64")},
	})
	if errExec != nil {
		t.Fatalf("ExecuteStream error: %v", errExec)
	}
	drainStream(t, stream)

	wantUA := misc.AntigravityCLIUserAgent("linux", "amd64", misc.AntigravityDefaultHostPath)
	if got := captured[0].Headers.Get("User-Agent"); got != wantUA {
		t.Fatalf("Linux User-Agent = %q, want %q", got, wantUA)
	}
}

func TestAntigravityACPOnDemandTokenRefresh(t *testing.T) {
	var mu sync.Mutex
	var tokenCalls int
	var gotAuthHeader string
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case "https://oauth2.googleapis.com/token":
			mu.Lock()
			tokenCalls++
			mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"access_token":"fresh-acp-token","expires_in":3600,"token_type":"Bearer"}`)),
			}, nil
		default:
			mu.Lock()
			gotAuthHeader = req.Header.Get("Authorization")
			mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"OK\"}]},\"finishReason\":\"STOP\"}]}}\n\n")),
			}, nil
		}
	})

	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "auth-acp-ondemand-refresh",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": "https://daily-cloudcode-pa.googleapis.com"},
		Metadata: map[string]any{
			"access_token":  "expired-acp-token",
			"refresh_token": "refresh-acp-token",
			"project_id":    "proj-acp-refresh",
			"email":         "acp-refresh@example.com",
			"expired":       "2020-01-01T00:00:00Z",
		},
	}

	body := sampleACPMainPayload("linux", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "Refresh on demand")
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))
	stream, errExec := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gemini-4-neon",
		Payload: body,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatGemini,
		OriginalRequest: body,
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope("worker-ondemand")},
	})
	if errExec != nil {
		t.Fatalf("ExecuteStream error: %v", errExec)
	}
	drainStream(t, stream)

	mu.Lock()
	defer mu.Unlock()
	if tokenCalls != 1 {
		t.Fatalf("OAuth token refresh calls = %d, want 1", tokenCalls)
	}
	if gotAuthHeader != "Bearer fresh-acp-token" {
		t.Fatalf("upstream Authorization = %q, want Bearer fresh-acp-token", gotAuthHeader)
	}
}

func TestAntigravityACPDefaultConnectionPoolingReusesConnection(t *testing.T) {
	var mu sync.Mutex
	remotes := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		remotes[r.RemoteAddr]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"OK\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
	}))
	defer srv.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := testAntigravityACPAuth(srv.URL)
	auth.ID = "auth-acp-default-pool"
	t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })

	body := sampleACPMainPayload("linux", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "Reuse my connection")
	for i := range 2 {
		stream, errExec := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "gemini-4-neon",
			Payload: body,
		}, cliproxyexecutor.Options{
			Stream:          true,
			SourceFormat:    sdktranslator.FormatGemini,
			OriginalRequest: body,
			Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: session.CallerScope("worker-default-pool")},
		})
		if errExec != nil {
			t.Fatalf("request %d ExecuteStream error: %v", i, errExec)
		}
		drainStream(t, stream)
	}

	mu.Lock()
	distinct := len(remotes)
	mu.Unlock()
	if distinct != 1 {
		t.Fatalf("distinct upstream TCP connections = %d, want 1 with default pooling enabled", distinct)
	}
}
