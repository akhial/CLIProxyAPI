package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internalsignature "github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func testGeminiSignaturePayload() string {
	payload := append([]byte{0x0A}, bytes.Repeat([]byte{0x56}, 48)...)
	return base64.StdEncoding.EncodeToString(payload)
}

// testFakeClaudeSignature returns a base64 string starting with 'E' that passes
// the lightweight hasValidClaudeSignature check but has invalid protobuf content
// (first decoded byte 0x12 is correct, but no valid protobuf field 2 follows),
// so it fails deep validation in strict mode.
func testFakeClaudeSignature() string {
	return base64.StdEncoding.EncodeToString([]byte{0x12, 0xFF, 0xFE, 0xFD})
}

func issue4959GeminiThoughtSignature() string {
	return "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"
}

// issue4959ResponsesModelFirstPayload is the #4959 Responses history: a
// next:function reasoning carrier, then function_call / function_call_output,
// then trailing assistant turns. Trailing model turns are left for a follow-up.
func issue4959ResponsesModelFirstPayload() []byte {
	carrier := "cpa-gemini-responses-carrier-v1:next:function:" + base64.RawStdEncoding.EncodeToString([]byte(issue4959GeminiThoughtSignature()))
	return []byte(`{"model":"gemini-3.7-flash-high","input":[` +
		`{"type":"reasoning","id":"rs_resp_test_detached_before_0","summary":[],"encrypted_content":"` + carrier + `"},` +
		`{"type":"function_call","call_id":"call_bash_1","name":"Bash","arguments":"{\"command\":\"true\"}"},` +
		`{"type":"function_call_output","call_id":"call_bash_1","output":"ok"},` +
		`{"role":"assistant","content":[{"type":"output_text","text":"first"}]},` +
		`{"role":"assistant","content":[{"type":"output_text","text":"second"}]}` +
		`]}`)
}

func contentHasNamedPart(content ojson.Result, partKind, name string) bool {
	for _, part := range content.Get("parts").Array() {
		if part.Get(partKind+".name").String() == name {
			return true
		}
	}
	return false
}

func assertIssue4959LeadingUserContents(t *testing.T, contents []ojson.Result) {
	t.Helper()
	if len(contents) < 3 {
		t.Fatalf("contents too short: %d", len(contents))
	}
	leadingText := contents[0].Get("parts.0.text")
	if contents[0].Get("role").String() != "user" || !leadingText.Exists() || leadingText.String() != "" {
		t.Fatalf("synthetic leading user missing: %s", contents[0].Raw)
	}
	if contents[1].Get("role").String() != "model" || !contentHasNamedPart(contents[1], "functionCall", "Bash") {
		t.Fatalf("function call is not immediately after the synthetic user: %s", contents[1].Raw)
	}
	if !contentHasNamedPart(contents[2], "functionResponse", "Bash") {
		t.Fatalf("function response missing or moved: %s", contents[2].Raw)
	}
}

func testAntigravityAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": baseURL,
		},
		Metadata: map[string]any{
			"access_token": "token-123",
			"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		},
	}
}

func newSignatureDebugHook(t *testing.T) *test.Hook {
	t.Helper()

	previousLevel := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(previousLevel)
	})
	return hook
}

func assertSignatureDebugDoesNotLeak(t *testing.T, hook *test.Hook, forbidden string) {
	t.Helper()

	if forbidden == "" {
		return
	}
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, forbidden) {
			t.Fatalf("debug log leaked signature in message: %q", entry.Message)
		}
		for key, value := range entry.Data {
			if strings.Contains(fmt.Sprint(value), forbidden) {
				t.Fatalf("debug log leaked signature in field %q: %v", key, value)
			}
		}
	}
}

func TestAntigravityStreamDoesNotEmitSyntheticTerminalOnReadError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}}`+"\n")
		flusher.Flush()
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, errHijack := hijacker.Hijack()
		if errHijack == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{
		Antigravity:  config.AntigravityConfig{},
		RequestRetry: 1,
	})
	result, errExecute := executor.ExecuteStream(context.Background(), &cliproxyauth.Auth{
		Metadata: map[string]any{
			"access_token": "token-123",
			"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"project_id":   "project-1",
		},
		Attributes: map[string]string{"base_url": server.URL},
	}, cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatGemini,
		ResponseFormat: sdktranslator.FormatGemini,
		Stream:         true,
	})
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		if finishReason := ojson.GetBytes(chunk.Payload, "candidates.0.finishReason").String(); finishReason == "STOP" {
			t.Fatalf("read error must not emit synthetic STOP: %s", chunk.Payload)
		}
	}
	if streamErr == nil {
		t.Fatal("expected stream read error")
	}
}

func TestAntigravityRequestPathsDoNotFallbackEndpoints(t *testing.T) {
	type upstreamCall struct {
		host string
		path string
	}
	routes := []struct {
		name     string
		kind     string
		model    string
		wantPath string
	}{
		{name: "generate non-stream", kind: "execute", model: "gemini-3.6-flash-high", wantPath: antigravityGeneratePath},
		{name: "aggregated stream", kind: "execute", model: "gemini-3-pro", wantPath: antigravityStreamPath},
		{name: "generate stream", kind: "stream", model: "gemini-3.6-flash-high", wantPath: antigravityStreamPath},
		{name: "count tokens", kind: "count", model: "gemini-3.6-flash-high", wantPath: antigravityCountTokensPath},
	}
	failures := []struct {
		name      string
		transport bool
	}{
		{name: "HTTP 429"},
		{name: "transport error", transport: true},
	}

	for _, route := range routes {
		for _, failure := range failures {
			t.Run(route.name+"/"+failure.name, func(t *testing.T) {
				calls := make([]upstreamCall, 0, 1)
				transportErr := errors.New("daily endpoint unavailable")
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					calls = append(calls, upstreamCall{host: req.URL.Host, path: req.URL.Path})
					if failure.transport {
						return nil, transportErr
					}
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`)),
					}, nil
				}))
				auth := &cliproxyauth.Auth{Metadata: map[string]any{
					"access_token": "token",
					"expired":      time.Now().Add(2 * time.Hour).Format(time.RFC3339),
					"project_id":   "project-1",
				}}
				req := cliproxyexecutor.Request{
					Model:   route.model,
					Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
				}
				opts := cliproxyexecutor.Options{
					SourceFormat:   sdktranslator.FormatGemini,
					ResponseFormat: sdktranslator.FormatGemini,
				}
				executor := NewAntigravityExecutor(&config.Config{})

				var errRequest error
				switch route.kind {
				case "execute":
					_, errRequest = executor.Execute(ctx, auth, req, opts)
				case "stream":
					_, errRequest = executor.ExecuteStream(ctx, auth, req, opts)
				case "count":
					_, errRequest = executor.CountTokens(ctx, auth, req, opts)
				default:
					t.Fatalf("unknown route kind %q", route.kind)
				}
				if errRequest == nil {
					t.Fatal("request error = nil, want original upstream error")
				}
				if failure.transport {
					if !errors.Is(errRequest, transportErr) {
						t.Fatalf("request error = %v, want transport error", errRequest)
					}
				} else {
					status, ok := errRequest.(interface{ StatusCode() int })
					if !ok || status.StatusCode() != http.StatusTooManyRequests {
						t.Fatalf("request error = %v, want status 429", errRequest)
					}
				}
				if len(calls) != 1 {
					t.Fatalf("upstream calls = %v, want exactly one daily endpoint request", calls)
				}
				if got, want := calls[0].host, resolveHost(antigravityBaseURLDaily); got != want {
					t.Fatalf("upstream host = %q, want %q", got, want)
				}
				if got := calls[0].path; got != route.wantPath {
					t.Fatalf("upstream path = %q, want %q", got, route.wantPath)
				}
			})
		}
	}
}

func TestNormalizeAntigravityGeminiFunctionResponseRolesOrdersMixedParallelResponses(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"read","args":{"file":"one"}}},{"functionCall":{"id":"call-2","name":"read","args":{"file":"two"}}}]},{"role":"user","parts":[{"text":"results follow"},{"functionResponse":{"id":"call-2","name":"read","response":{"result":"two"}}},{"text":"continue"},{"functionResponse":{"id":"call-1","name":"read","response":{"result":"one"}}}]}]}}`)
	if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(payload); errValidate == nil {
		t.Fatal("permuted input unexpectedly passed function call pairing validation")
	}
	output := normalizeAntigravityGeminiFunctionResponseRoles(payload)
	if got := ojson.GetBytes(output, "request.contents.1.role").String(); got != "user" {
		t.Fatalf("mixed functionResponse/user content role = %q, want user; output=%s", got, output)
	}
	parts := ojson.GetBytes(output, "request.contents.1.parts").Array()
	if len(parts) != 4 {
		t.Fatalf("mixed response parts = %d, want 4; output=%s", len(parts), output)
	}
	if got := parts[0].Get("functionResponse.id").String(); got != "call-1" {
		t.Fatalf("first functionResponse.id = %q, want call-1; output=%s", got, output)
	}
	if got := parts[1].Get("functionResponse.id").String(); got != "call-2" {
		t.Fatalf("second functionResponse.id = %q, want call-2; output=%s", got, output)
	}
	if got := parts[2].Get("text").String(); got != "results follow" {
		t.Fatalf("first trailing text = %q; output=%s", got, output)
	}
	if got := parts[3].Get("text").String(); got != "continue" {
		t.Fatalf("second trailing text = %q; output=%s", got, output)
	}
	if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(output); errValidate != nil {
		t.Fatalf("normalized mixed parallel responses are invalid: %v; output=%s", errValidate, output)
	}
}

func TestNormalizeAntigravityGeminiFunctionResponseRolesOrdersParallelResponses(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"read","args":{"file":"one"}}},{"functionCall":{"id":"call-2","name":"read","args":{"file":"two"}}}]},{"role":" Model ","parts":[{"functionResponse":{"id":"call-2","name":"read","response":{"result":"two"}}},{"functionResponse":{"id":"call-1","name":"read","response":{"result":"one"}}}]}]}}`)
	output := normalizeAntigravityGeminiFunctionResponseRoles(payload)
	if got := ojson.GetBytes(output, "request.contents.1.role").String(); got != "model" {
		t.Fatalf("functionResponse role = %q, want model; output=%s", got, output)
	}
	if got := ojson.GetBytes(output, "request.contents.1.parts.0.functionResponse.id").String(); got != "call-1" {
		t.Fatalf("first functionResponse.id = %q, want call-1; output=%s", got, output)
	}
	if got := ojson.GetBytes(output, "request.contents.1.parts.1.functionResponse.id").String(); got != "call-2" {
		t.Fatalf("second functionResponse.id = %q, want call-2; output=%s", got, output)
	}
	if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(output); errValidate != nil {
		t.Fatalf("normalized parallel responses are invalid: %v; output=%s", errValidate, output)
	}
}

func TestNormalizeAntigravityGeminiFunctionResponseRolesDoesNotCrossEmptyContentBoundary(t *testing.T) {
	for _, boundary := range []string{
		`{"role":"user","parts":[]}`,
		`{"role":"user"}`,
		`{"role":"user","parts":null}`,
	} {
		payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"read","args":{}}},{"functionCall":{"id":"call-2","name":"read","args":{}}}]},` + boundary + `,{"role":"user","parts":[{"functionResponse":{"id":"call-2","name":"read","response":{"result":"two"}}},{"functionResponse":{"id":"call-1","name":"read","response":{"result":"one"}}}]}]}}`)
		output := normalizeAntigravityGeminiFunctionResponseRoles(payload)
		if got := ojson.GetBytes(output, "request.contents.2.role").String(); got != "model" {
			t.Fatalf("pure functionResponse role = %q, want model; output=%s", got, output)
		}
		if got := ojson.GetBytes(output, "request.contents.2.parts.0.functionResponse.id").String(); got != "call-2" {
			t.Fatalf("response crossed content boundary %s and was reordered: first id=%q; output=%s", boundary, got, output)
		}
		if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(output); errValidate == nil {
			t.Fatalf("responses crossing content boundary %s were accepted: %s", boundary, output)
		}
	}
}

func TestClaudeExecutor_LogsSanitizedClaudeUpstreamSignatures(t *testing.T) {
	hook := newSignatureDebugHook(t)
	rawSignature := "skip_thought_signature_validator"
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "bad", "signature": "` + rawSignature + `"},
					{"type": "text", "text": "hello"},
					{"type": "tool_use", "id": "call_123", "name": "get_weather", "input": {}, "signature": "` + rawSignature + `"}
				]
			}
		]
	}`)

	output := sanitizeClaudeMessagesForClaudeUpstreamWithDebug(context.Background(), body, "claude-sonnet-4-5")
	parts := ojson.GetBytes(output, "messages.0.content").Array()
	if len(parts) != 2 {
		t.Fatalf("content length = %d, want 2 after invalid thinking strip: %s", len(parts), output)
	}
	if parts[1].Get("signature").Exists() {
		t.Fatalf("tool_use signature should be removed before Claude upstream: %s", output)
	}

	found := false
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.DebugLevel {
			continue
		}
		if entry.Data["component"] != "signature_sanitizer" ||
			entry.Data["executor"] != "claude" ||
			entry.Data["action"] != "sanitize_claude_messages" {
			continue
		}
		if entry.Data["dropped_blocks"] != 1 {
			t.Fatalf("dropped_blocks = %v, want 1", entry.Data["dropped_blocks"])
		}
		if entry.Data["dropped_signatures"] != 1 {
			t.Fatalf("dropped_signatures = %v, want 1", entry.Data["dropped_signatures"])
		}
		found = true
	}
	if !found {
		t.Fatal("expected debug log for Claude upstream signature sanitization")
	}
	assertSignatureDebugDoesNotLeak(t, hook, rawSignature)
}
