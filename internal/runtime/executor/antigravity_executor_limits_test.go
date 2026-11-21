package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// resetAntigravityCooldownState clears the package-level credits state
// between tests. It empties each map in place instead of assigning a fresh
// sync.Map, because credits hint refreshes run on background goroutines that
// may still be writing these maps when a test's cleanup runs. Replacing the
// variable is an unsynchronized write and races with them; Clear is not.
func resetAntigravityCooldownState() {
	antigravityTOSPauseUntil.Clear()
	antigravityShortCooldownByAuth.Clear()
}

type closeSignalReadCloser struct {
	io.ReadCloser
	closed chan<- struct{}
}

func (c *closeSignalReadCloser) Close() error {
	errClose := c.ReadCloser.Close()
	close(c.closed)
	return errClose
}

type fakeAntigravityKVClient struct {
	values       map[string][]byte
	getErr       error
	setErr       error
	setNXErr     error
	delErr       error
	setNXResult  bool
	getCount     int
	setCount     int
	setNXCount   int
	delCount     int
	lastSetTTL   time.Duration
	lastSetNXTTL time.Duration
	lastSetNXKey string
	lastSetKey   string
}

func newFakeAntigravityKVClient() *fakeAntigravityKVClient {
	return &fakeAntigravityKVClient{
		values:      make(map[string][]byte),
		setNXResult: true,
	}
}

func (c *fakeAntigravityKVClient) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	c.getCount++
	if c.getErr != nil {
		return nil, false, c.getErr
	}
	value, ok := c.values[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), value...), true, nil
}

func (c *fakeAntigravityKVClient) KVSet(_ context.Context, key string, value []byte, opts homekv.KVSetOptions) (bool, error) {
	c.setCount++
	c.lastSetKey = key
	c.lastSetTTL = opts.EX
	if c.setErr != nil {
		return false, c.setErr
	}
	c.values[key] = append([]byte(nil), value...)
	return true, nil
}

func (c *fakeAntigravityKVClient) KVSetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	c.setNXCount++
	c.lastSetNXKey = key
	c.lastSetNXTTL = ttl
	if c.setNXErr != nil {
		return false, c.setNXErr
	}
	if _, ok := c.values[key]; ok {
		return false, nil
	}
	if c.setNXResult {
		c.values[key] = append([]byte(nil), value...)
		return true, nil
	}
	return false, nil
}

func (c *fakeAntigravityKVClient) KVDel(_ context.Context, keys ...string) (int64, error) {
	c.delCount++
	if c.delErr != nil {
		return 0, c.delErr
	}
	var deleted int64
	for _, key := range keys {
		if _, ok := c.values[key]; ok {
			delete(c.values, key)
			deleted++
		}
	}
	return deleted, nil
}

func useFakeAntigravityKVClient(t *testing.T, client *fakeAntigravityKVClient, homeMode bool, errClient error) {
	t.Helper()
	previous := currentAntigravityKVClient
	currentAntigravityKVClient = func() (antigravityKVClient, bool, error) {
		return client, homeMode, errClient
	}
	t.Cleanup(func() {
		currentAntigravityKVClient = previous
	})
}

func mustAntigravityJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal value: %v", errMarshal)
	}
	return raw
}

func TestClassifyAntigravity429(t *testing.T) {
	t.Run("quota exhausted", func(t *testing.T) {
		body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`)
		if got := classifyAntigravity429(body); got != antigravity429QuotaExhausted {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429QuotaExhausted)
		}
	})

	t.Run("standard antigravity rate limit with ui message stays rate limited", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"code": 429,
				"message": "You have exhausted your capacity on this model. Your quota will reset after 0s.",
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{
						"@type": "type.googleapis.com/google.rpc.ErrorInfo",
						"reason": "RATE_LIMIT_EXCEEDED",
						"domain": "cloudcode-pa.googleapis.com",
						"metadata": {
							"model": "claude-opus-4-6-thinking",
							"quotaResetDelay": "479.417207ms",
							"quotaResetTimeStamp": "2026-04-20T09:19:49Z",
							"uiMessage": "true"
						}
					},
					{
						"@type": "type.googleapis.com/google.rpc.RetryInfo",
						"retryDelay": "0.479417207s"
					}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429RateLimited {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429RateLimited)
		}
		decision := decideAntigravity429(body)
		if decision.kind != antigravity429DecisionInstantRetrySameAuth {
			t.Fatalf("decideAntigravity429().kind = %q, want %q", decision.kind, antigravity429DecisionInstantRetrySameAuth)
		}
		if decision.retryAfter == nil {
			t.Fatal("decideAntigravity429().retryAfter = nil")
		}
	})

	t.Run("structured rate limit", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED"},
					{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "0.5s"}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429RateLimited {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429RateLimited)
		}
	})

	t.Run("structured quota exhausted", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED"}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429QuotaExhausted {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429QuotaExhausted)
		}
	})

	t.Run("unstructured 429 defaults to soft rate limit", func(t *testing.T) {
		body := []byte(`{"error":{"message":"too many requests"}}`)
		if got := classifyAntigravity429(body); got != antigravity429SoftRateLimit {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429SoftRateLimit)
		}
	})
}

func TestParseRetryDelay_HumanReadableDuration(t *testing.T) {
	body := []byte(`{"error":{"message":"You have exhausted your capacity on this model. Your quota will reset after 1h43m56s."}}`)
	retryAfter, err := helps.ParseRetryDelay(body)
	if err != nil {
		t.Fatalf("helps.ParseRetryDelay() error = %v", err)
	}
	if retryAfter == nil {
		t.Fatal("helps.ParseRetryDelay() returned nil")
	}
	want := time.Hour + 43*time.Minute + 56*time.Second
	if *retryAfter != want {
		t.Fatalf("helps.ParseRetryDelay() = %v, want %v", *retryAfter, want)
	}
}

func TestAntigravityExecute_DoesNotUseRequestRetryForInternalRetries(t *testing.T) {
	resetAntigravityCooldownState()
	t.Cleanup(resetAntigravityCooldownState)

	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 3})
	auth := &cliproxyauth.Auth{
		ID: "auth-transient-429",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gemini-3-pro",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatGemini,
	})
	if err == nil {
		t.Fatalf("Execute() error = nil, want upstream 429")
	}
	if len(resp.Payload) != 0 {
		t.Fatalf("Execute() returned payload %q, want empty payload", resp.Payload)
	}
	if requestCount != 1 {
		t.Fatalf("request count = %d, want 1", requestCount)
	}
}

func TestAntigravityShortCooldownRequiredHomeKV(t *testing.T) {
	resetAntigravityCooldownState()
	t.Cleanup(resetAntigravityCooldownState)
	client := newFakeAntigravityKVClient()
	useFakeAntigravityKVClient(t, client, true, nil)
	auth := &cliproxyauth.Auth{ID: "home-cooldown-auth"}
	now := time.Now()
	duration := 30 * time.Second

	if errMark := markAntigravityShortCooldownRequired(context.Background(), auth, "gemini-3.8-flash-high", now, duration); errMark != nil {
		t.Fatalf("markAntigravityShortCooldownRequired() error = %v", errMark)
	}
	if client.setCount != 1 || client.lastSetTTL != duration+5*time.Second {
		t.Fatalf("KVSet count/ttl = %d/%v, want 1/%v", client.setCount, client.lastSetTTL, duration+5*time.Second)
	}
	antigravityShortCooldownByAuth = sync.Map{}
	inCooldown, remaining, errRead := antigravityIsInShortCooldownRequired(context.Background(), auth, "gemini-3.8-flash-high", now.Add(5*time.Second))
	if errRead != nil {
		t.Fatalf("antigravityIsInShortCooldownRequired() error = %v", errRead)
	}
	if !inCooldown || remaining <= 0 {
		t.Fatalf("cooldown = %v remaining %v, want active Home KV cooldown", inCooldown, remaining)
	}
}

func TestAntigravityShortCooldownRequiredHomeKVFailures(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "home-cooldown-failure-auth"}
	for _, tc := range []struct {
		name   string
		client *fakeAntigravityKVClient
		write  bool
	}{
		{name: "read", client: &fakeAntigravityKVClient{values: make(map[string][]byte), getErr: errors.New("get failed")}},
		{name: "write", client: &fakeAntigravityKVClient{values: make(map[string][]byte), setErr: errors.New("set failed")}, write: true},
		{name: "delete-expired", client: &fakeAntigravityKVClient{
			values: map[string][]byte{
				antigravityShortCooldownKVKey(auth, "gemini-3.8-flash-high"): []byte("1"),
			},
			delErr: errors.New("delete failed"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFakeAntigravityKVClient(t, tc.client, true, nil)
			if tc.write {
				if errMark := markAntigravityShortCooldownRequired(context.Background(), auth, "gemini-3.8-flash-high", time.Now(), time.Second); errMark == nil {
					t.Fatalf("markAntigravityShortCooldownRequired() error = nil, want error")
				}
				return
			}
			if _, _, errRead := antigravityIsInShortCooldownRequired(context.Background(), auth, "gemini-3.8-flash-high", time.Now()); errRead == nil {
				t.Fatalf("antigravityIsInShortCooldownRequired() error = nil, want error")
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
