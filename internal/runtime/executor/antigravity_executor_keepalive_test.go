package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// TestAntigravityBuildRequestKeepsConnectionAlive guards the regression where the
// upstream request forced "Connection: close", which discarded every established
// TCP + TLS session and made connection pooling impossible. The native Antigravity
// client omits the Connection header entirely.
func TestAntigravityBuildRequestKeepsConnectionAlive(t *testing.T) {
	e := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)

	for _, stream := range []bool{false, true} {
		name := "unary"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			req, err := e.buildRequest(context.Background(), auth, "token", "gemini-3.6-flash-high", payload, stream, "", antigravityBaseURLDaily)
			if err != nil {
				t.Fatalf("buildRequest error: %v", err)
			}
			if req.Close {
				t.Fatal("Antigravity upstream request must not force Connection: close")
			}
			if v := req.Header.Get("Connection"); v != "" {
				t.Fatalf("Antigravity upstream request must not send a Connection header, got %q", v)
			}
		})
	}
}

// TestAntigravityExecuteStreamReusesUpstreamConnection drives the real executor
// against a local upstream and proves that repeated streaming requests share a
// single pooled TCP connection and never advertise Connection: close.
func TestAntigravityExecuteStreamReusesUpstreamConnection(t *testing.T) {
	var mu sync.Mutex
	remotes := map[string]int{}
	var connectionHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes[r.RemoteAddr]++
		connectionHeaders = append(connectionHeaders, r.Header.Get("Connection"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}}\n\n"))
	}))
	defer server.Close()

	enabledPool := true
	exec := NewAntigravityExecutor(&config.Config{
		RequestRetry: 1,
		Antigravity: config.AntigravityConfig{
			ConnectionPool: config.AntigravityConnectionPoolConfig{
				Enabled: &enabledPool,
			},
		},
	})
	auth := &cliproxyauth.Auth{
		ID:         "antigravity-keepalive-auth",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	const requests = 6
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	for i := 0; i < requests; i++ {
		result, errExecute := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "gemini-3.6-flash-high",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatGemini,
			ResponseFormat:  sdktranslator.FormatGemini,
			Stream:          true,
			OriginalRequest: payload,
		})
		if errExecute != nil {
			t.Fatalf("request %d: ExecuteStream() error = %v", i, errExecute)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("request %d: stream chunk error: %v", i, chunk.Err)
			}
		}
	}

	mu.Lock()
	distinct := len(remotes)
	total := 0
	for _, c := range remotes {
		total += c
	}
	headers := append([]string(nil), connectionHeaders...)
	mu.Unlock()

	if total != requests {
		t.Fatalf("expected %d upstream requests, got %d", requests, total)
	}
	for i, h := range headers {
		if h != "" {
			t.Fatalf("upstream request %d advertised Connection: %q", i, h)
		}
	}
	if distinct != 1 {
		t.Fatalf("expected %d streaming requests to reuse one upstream connection, got %d connections", requests, distinct)
	}
}

// TestAntigravityCountTokensReusesUpstreamConnection covers the second upstream
// request builder, which shares the same connection pool.
func TestAntigravityCountTokensReusesUpstreamConnection(t *testing.T) {
	var mu sync.Mutex
	remotes := map[string]int{}
	var connectionHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes[r.RemoteAddr]++
		connectionHeaders = append(connectionHeaders, r.Header.Get("Connection"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"totalTokens":7}`))
	}))
	defer server.Close()

	enabledCountPool := true
	exec := NewAntigravityExecutor(&config.Config{
		RequestRetry: 1,
		Antigravity: config.AntigravityConfig{
			ConnectionPool: config.AntigravityConnectionPoolConfig{
				Enabled: &enabledCountPool,
			},
		},
	})
	auth := &cliproxyauth.Auth{
		ID:         "antigravity-counttokens-auth",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	const requests = 4
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	for i := 0; i < requests; i++ {
		ctx := cliproxyexecutor.WithUpstreamAttemptTracker(context.Background())
		if _, errCount := exec.CountTokens(ctx, auth, cliproxyexecutor.Request{
			Model:   "gemini-3.6-flash-high",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatGemini,
			ResponseFormat:  sdktranslator.FormatGemini,
			OriginalRequest: payload,
		}); errCount != nil {
			t.Fatalf("request %d: CountTokens() error = %v", i, errCount)
		}
		if !cliproxyexecutor.UpstreamAttempted(ctx) {
			t.Fatalf("request %d: CountTokens() did not mark the upstream attempt", i)
		}
	}

	mu.Lock()
	distinct := len(remotes)
	headers := append([]string(nil), connectionHeaders...)
	mu.Unlock()
	for i, h := range headers {
		if h != "" {
			t.Fatalf("countTokens request %d advertised Connection: %q", i, h)
		}
	}
	if distinct != 1 {
		t.Fatalf("expected %d countTokens requests to reuse one upstream connection, got %d connections", requests, distinct)
	}
}

// TestAntigravityConcurrentGenerationsStayIsolated forces concurrent
// requests from one auth to complete in reverse order and verifies each caller
// receives only its own response body.
func TestAntigravityConcurrentGenerationsStayIsolated(t *testing.T) {
	const sessions = 12
	gates := make(map[string]chan struct{}, sessions)
	markers := make([]string, sessions)
	for i := range sessions {
		markers[i] = fmt.Sprintf("session-%02d", i)
		gates[markers[i]] = make(chan struct{})
	}
	arrived := make(chan string, sessions)
	completed := make(chan string, sessions)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			http.Error(w, errRead.Error(), http.StatusBadRequest)
			return
		}
		marker := ojson.GetBytes(body, "request.contents.0.parts.0.text").String()
		gate, ok := gates[marker]
		if !ok {
			http.Error(w, "unknown session marker", http.StatusBadRequest)
			return
		}
		arrived <- marker
		<-gate
		_, _ = fmt.Fprintf(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]},"finishReason":"STOP"}]}}`, marker)
		completed <- marker
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "antigravity-concurrent-sessions-auth",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	errs := make(chan error, sessions)
	var wg sync.WaitGroup
	wg.Add(sessions)
	for _, marker := range markers {
		go func(marker string) {
			defer wg.Done()
			req := cliproxyexecutor.Request{Model: "gemini-test", Payload: []byte(fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":%q}]}]}`, marker))}
			resp, err := exec.Execute(t.Context(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
			if err != nil {
				errs <- err
				return
			}
			if got := ojson.GetBytes(resp.Payload, "candidates.0.content.parts.0.text").String(); got != marker {
				errs <- fmt.Errorf("%s received response for %q", marker, got)
			}
		}(marker)
	}

	seen := make(map[string]struct{}, sessions)
	for range sessions {
		marker := <-arrived
		seen[marker] = struct{}{}
	}
	if len(seen) != sessions {
		t.Fatalf("only %d/%d session markers reached upstream", len(seen), sessions)
	}
	for i := sessions - 1; i >= 0; i-- {
		close(gates[markers[i]])
		if marker := <-completed; marker != markers[i] {
			t.Fatalf("completion order = %q, want %q", marker, markers[i])
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// TestAntigravityDefaultConfigDisablesPoolingAndClosesConnections verifies that by default
// (no connection-pool configuration, or enabled: false), connection pooling is disabled.
// Every request uses a new connection, and the wire request never emits Connection: close.
func TestAntigravityDefaultConfigDisablesPoolingAndClosesConnections(t *testing.T) {
	var mu sync.Mutex
	remotes := map[string]int{}
	var connectionHeaders []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes[r.RemoteAddr]++
		connectionHeaders = append(connectionHeaders, r.Header.Get("Connection"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"totalTokens":1}`)
	}))
	defer server.Close()

	// Default config has ConnectionPool.Enabled = nil, which defaults to false (disabled)
	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "antigravity-default-short-auth",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	const requests = 4
	for i := 0; i < requests; i++ {
		_, err := exec.CountTokens(t.Context(), auth, cliproxyexecutor.Request{Model: "gemini-test", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
		if err != nil {
			t.Fatal(err)
		}

	}

	mu.Lock()
	distinct := len(remotes)
	headers := append([]string(nil), connectionHeaders...)
	mu.Unlock()

	// With pooling disabled by default, every request opens a fresh connection
	if distinct != requests {
		t.Fatalf("default disabled pool: expected %d distinct connections, got %d", requests, distinct)
	}

	// Must never leak Connection: close header to upstream
	for i, h := range headers {
		if strings.Contains(strings.ToLower(h), "close") {
			t.Fatalf("request %d leaked Connection: close header: %q", i, h)
		}
	}
}
