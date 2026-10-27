package management

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	testClaudeUsageBody   = `{"five_hour":{"utilization":42.5,"resets_at":"2026-10-27T15:00:00Z"},"seven_day":{"utilization":16,"resets_at":"2026-11-01T09:00:00Z"}}`
	testClaudeProfileBody = `{"account":{"has_claude_max":true,"has_claude_pro":false}}`
	testCodexUsageBody    = `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_at":1762000000}}}`
)

type fakeQuotaHistoryClock struct{ now time.Time }

func (c *fakeQuotaHistoryClock) Now() time.Time          { return c.now }
func (c *fakeQuotaHistoryClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type fakeQuotaHistoryResponse struct {
	status int
	body   string
	err    error
}

type fakeQuotaHistoryDoer struct {
	mu        sync.Mutex
	requests  []*http.Request
	responses map[string]fakeQuotaHistoryResponse
}

func (d *fakeQuotaHistoryDoer) Do(_ *coreauth.Auth, req *http.Request) (int, []byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, req)
	response, ok := d.responses[req.URL.String()]
	if !ok {
		return http.StatusNotFound, nil, nil
	}
	if response.err != nil {
		return 0, nil, response.err
	}
	return response.status, []byte(response.body), nil
}

func (d *fakeQuotaHistoryDoer) set(url string, response fakeQuotaHistoryResponse) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.responses[url] = response
}

func (d *fakeQuotaHistoryDoer) requestsTo(url string) []*http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	var matched []*http.Request
	for _, req := range d.requests {
		if req.URL.String() == url {
			matched = append(matched, req)
		}
	}
	return matched
}

func (d *fakeQuotaHistoryDoer) total() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

func newTestQuotaHistoryCollector(t *testing.T, pollInterval time.Duration, auths ...*coreauth.Auth) (*quotaHistoryCollector, *fakeQuotaHistoryClock, *fakeQuotaHistoryDoer) {
	t.Helper()
	clock := &fakeQuotaHistoryClock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	doer := &fakeQuotaHistoryDoer{responses: map[string]fakeQuotaHistoryResponse{
		quotaHistoryClaudeUsageURL:   {status: http.StatusOK, body: testClaudeUsageBody},
		quotaHistoryClaudeProfileURL: {status: http.StatusOK, body: testClaudeProfileBody},
		quotaHistoryCodexUsageURL:    {status: http.StatusOK, body: testCodexUsageBody},
	}}
	// A nil handler still resolves Claude/Codex tokens from metadata, like APICall.
	collector := newQuotaHistoryCollector(nil, newQuotaHistoryStore(t.TempDir()), pollInterval)
	collector.nowFunc = clock.Now
	collector.listAuths = func() []*coreauth.Auth { return auths }
	collector.do = doer.Do
	return collector, clock, doer
}

func testQuotaHistoryClaudeAuth() *coreauth.Auth {
	return &coreauth.Auth{
		ID:       "claude-a@x.com.json",
		Provider: "claude",
		FileName: "claude-a@x.com.json",
		Label:    "claude-a",
		Metadata: map[string]any{"type": "claude", "access_token": "claude-token", "email": "a@x.com"},
	}
}

func testQuotaHistoryCodexAuth() *coreauth.Auth {
	claims := `{"email":"b@x.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct-123","chatgpt_plan_type":"team"}}`
	idToken := "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
	return &coreauth.Auth{
		ID:       "codex-b.json",
		Provider: "codex",
		FileName: filepath.Join("auths", "codex-b.json"),
		Metadata: map[string]any{"type": "codex", "access_token": "codex-token", "email": "b@x.com", "id_token": idToken},
	}
}

func readTestQuotaHistorySamples(t *testing.T, collector *quotaHistoryCollector) []quotaHistorySample {
	t.Helper()
	now := collector.nowFunc()
	samples, errRead := collector.store.Read(now.Add(-72*time.Hour), now.Add(72*time.Hour))
	if errRead != nil {
		t.Fatalf("Read: %v", errRead)
	}
	return samples
}

func TestQuotaHistoryCollector_PollSchedule(t *testing.T) {
	collector, clock, doer := newTestQuotaHistoryCollector(t, 10*time.Minute, testQuotaHistoryClaudeAuth(), testQuotaHistoryCodexAuth())
	ctx := context.Background()
	start := clock.Now()

	collector.tick(ctx)
	if got := len(doer.requestsTo(quotaHistoryClaudeUsageURL)); got != 1 {
		t.Fatalf("claude usage requests = %d, want 1", got)
	}
	if got := len(doer.requestsTo(quotaHistoryClaudeProfileURL)); got != 1 {
		t.Fatalf("claude profile requests = %d, want 1", got)
	}
	if got := len(doer.requestsTo(quotaHistoryCodexUsageURL)); got != 1 {
		t.Fatalf("codex usage requests = %d, want 1", got)
	}

	usageReq := doer.requestsTo(quotaHistoryClaudeUsageURL)[0]
	for key, want := range map[string]string{
		"Authorization":  "Bearer claude-token",
		"User-Agent":     quotaHistoryClaudeUserAgent,
		"Anthropic-Beta": "oauth-2025-04-20",
		"Content-Type":   "application/json",
		"Accept":         "application/json",
	} {
		if got := usageReq.Header.Get(key); got != want {
			t.Errorf("claude usage header %s = %q, want %q", key, got, want)
		}
	}
	if got := doer.requestsTo(quotaHistoryClaudeProfileURL)[0].Header.Get("Accept"); got != "" {
		t.Errorf("claude profile Accept = %q, want unset like the management UI", got)
	}
	codexReq := doer.requestsTo(quotaHistoryCodexUsageURL)[0]
	for key, want := range map[string]string{
		"Authorization":      "Bearer codex-token",
		"User-Agent":         quotaHistoryCodexUserAgent,
		"Chatgpt-Account-Id": "acct-123",
	} {
		if got := codexReq.Header.Get(key); got != want {
			t.Errorf("codex usage header %s = %q, want %q", key, got, want)
		}
	}

	samples := readTestQuotaHistorySamples(t, collector)
	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want 2", samples)
	}
	byProvider := map[string]quotaHistorySample{}
	for _, sample := range samples {
		byProvider[sample.Provider] = sample
	}
	claude := byProvider["claude"]
	if claude.Src != "poll" || claude.Plan != "max" || claude.FileName != "claude-a@x.com.json" ||
		claude.Email != "a@x.com" || claude.Label != "claude-a" || claude.AuthIndex == "" || claude.T != start.UnixMilli() || len(claude.Windows) != 2 {
		t.Fatalf("claude sample = %+v", claude)
	}
	codex := byProvider["codex"]
	if codex.Src != "poll" || codex.Plan != "pro" || codex.FileName != "codex-b.json" || len(codex.Windows) != 2 ||
		codex.Windows[0].ResetAt != start.Add(time.Hour).UnixMilli() {
		t.Fatalf("codex sample = %+v", codex)
	}

	clock.Advance(5 * time.Minute)
	collector.tick(ctx)
	if got := doer.total(); got != 3 {
		t.Fatalf("requests after 5 minutes = %d, want 3 (no new polls)", got)
	}

	clock.Advance(5 * time.Minute)
	collector.tick(ctx)
	if got := len(doer.requestsTo(quotaHistoryClaudeUsageURL)); got != 2 {
		t.Fatalf("claude usage requests after poll interval = %d, want 2", got)
	}
	if got := len(doer.requestsTo(quotaHistoryClaudeProfileURL)); got != 1 {
		t.Fatalf("claude profile requests = %d, want 1 (once per day)", got)
	}

	clock.Advance(24 * time.Hour)
	collector.tick(ctx)
	if got := len(doer.requestsTo(quotaHistoryClaudeProfileURL)); got != 2 {
		t.Fatalf("claude profile requests after a day = %d, want 2", got)
	}
}

func TestQuotaHistoryCollector_PassiveHeadersBeforePolling(t *testing.T) {
	auth := testQuotaHistoryCodexAuth()
	collector, clock, doer := newTestQuotaHistoryCollector(t, 10*time.Minute, auth)
	ctx := context.Background()
	observed := clock.Now().Add(-time.Minute)
	auth.Quota.ObservedAt = observed
	auth.Quota.Signals = map[string]string{
		"X-Codex-Plan-Type":                "plus",
		"X-Codex-Primary-Used-Percent":     "20",
		"X-Codex-Primary-Window-Minutes":   "300",
		"X-Codex-Primary-Reset-At":         "1761600000",
		"X-Codex-Secondary-Used-Percent":   "40",
		"X-Codex-Secondary-Window-Minutes": "10080",
		"X-Codex-Secondary-Reset-At":       "1762000000",
	}

	collector.tick(ctx)
	if got := doer.total(); got != 0 {
		t.Fatalf("requests = %d, want 0 while passive headers are fresh", got)
	}

	// An observation less than 5 minutes after the last sample is skipped, and
	// the poll interval has not elapsed either.
	clock.Advance(3 * time.Minute)
	auth.Quota.ObservedAt = observed.Add(3 * time.Minute)
	collector.tick(ctx)

	clock.Advance(3 * time.Minute)
	auth.Quota.ObservedAt = observed.Add(6 * time.Minute)
	collector.tick(ctx)
	if got := doer.total(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}

	// No newer observation: poll once the interval since the last sample elapses.
	clock.Advance(8 * time.Minute)
	collector.tick(ctx)
	if got := doer.total(); got != 0 {
		t.Fatalf("requests = %d, want 0 before the poll interval", got)
	}
	clock.Advance(time.Minute)
	collector.tick(ctx)
	if got := len(doer.requestsTo(quotaHistoryCodexUsageURL)); got != 1 {
		t.Fatalf("codex usage requests = %d, want 1", got)
	}

	samples := readTestQuotaHistorySamples(t, collector)
	if len(samples) != 3 {
		t.Fatalf("samples = %+v, want 3", samples)
	}
	wantSources := []string{"header", "header", "poll"}
	wantTimes := []int64{observed.UnixMilli(), observed.Add(6 * time.Minute).UnixMilli(), clock.Now().UnixMilli()}
	for i, sample := range samples {
		if sample.Src != wantSources[i] || sample.T != wantTimes[i] {
			t.Errorf("sample %d src=%s t=%d, want src=%s t=%d", i, sample.Src, sample.T, wantSources[i], wantTimes[i])
		}
	}
	if samples[0].Plan != "plus" || samples[0].Windows[0].Used != 20 || samples[0].Windows[1].ID != "seven_day" {
		t.Fatalf("header sample = %+v", samples[0])
	}
}

func TestQuotaHistoryCollector_BackoffOnFailure(t *testing.T) {
	collector, clock, doer := newTestQuotaHistoryCollector(t, 10*time.Minute, testQuotaHistoryCodexAuth())
	ctx := context.Background()
	doer.set(quotaHistoryCodexUsageURL, fakeQuotaHistoryResponse{status: http.StatusUnauthorized, body: `{"error":"expired"}`})

	// Expected poll attempts (minutes after start): 0, 10 (+10), 30 (+20), 70 (+40).
	attemptsAt := map[int]bool{0: true, 10: true, 30: true, 70: true}
	attempts := 0
	for minute := 0; minute <= 75; minute++ {
		if minute == 70 {
			doer.set(quotaHistoryCodexUsageURL, fakeQuotaHistoryResponse{status: http.StatusOK, body: testCodexUsageBody})
		}
		collector.tick(ctx)
		if attemptsAt[minute] {
			attempts++
		}
		if got := len(doer.requestsTo(quotaHistoryCodexUsageURL)); got != attempts {
			t.Fatalf("minute %d: requests = %d, want %d", minute, got, attempts)
		}
		clock.Advance(time.Minute)
	}
	samples := readTestQuotaHistorySamples(t, collector)
	if len(samples) != 1 || samples[0].Src != "poll" {
		t.Fatalf("samples = %+v, want only the successful poll", samples)
	}
	state := collector.states["codex-b.json"]
	if state.failures != 0 || !state.nextPollAt.IsZero() {
		t.Fatalf("state after success = %+v, want reset backoff", state)
	}
}

func TestQuotaHistoryCollector_TransportErrorAndEmptyPayloadBackOff(t *testing.T) {
	collector, clock, doer := newTestQuotaHistoryCollector(t, 10*time.Minute, testQuotaHistoryCodexAuth(), testQuotaHistoryClaudeAuth())
	ctx := context.Background()
	doer.set(quotaHistoryCodexUsageURL, fakeQuotaHistoryResponse{err: errors.New("dial tcp: connection refused")})
	doer.set(quotaHistoryClaudeUsageURL, fakeQuotaHistoryResponse{status: http.StatusOK, body: `{"extra_usage":{"utilization":5}}`})

	collector.tick(ctx)
	clock.Advance(time.Minute)
	collector.tick(ctx)
	if got := len(doer.requestsTo(quotaHistoryCodexUsageURL)); got != 1 {
		t.Fatalf("codex requests = %d, want 1", got)
	}
	if got := len(doer.requestsTo(quotaHistoryClaudeUsageURL)); got != 1 {
		t.Fatalf("claude requests = %d, want 1", got)
	}
	if samples := readTestQuotaHistorySamples(t, collector); len(samples) != 0 {
		t.Fatalf("samples = %+v, want none", samples)
	}
}

func TestQuotaHistoryCollector_PollingDisabledKeepsPassive(t *testing.T) {
	auth := testQuotaHistoryClaudeAuth()
	auth.Quota.ObservedAt = time.Date(2026, 10, 27, 11, 59, 0, 0, time.UTC)
	auth.Quota.Signals = map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.25",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1761580800",
	}
	collector, clock, doer := newTestQuotaHistoryCollector(t, 0, auth, testQuotaHistoryCodexAuth())
	for i := 0; i < 30; i++ {
		collector.tick(context.Background())
		clock.Advance(time.Minute)
	}
	if got := doer.total(); got != 0 {
		t.Fatalf("requests = %d, want 0 with polling disabled", got)
	}
	samples := readTestQuotaHistorySamples(t, collector)
	if len(samples) != 1 || samples[0].Src != "header" || samples[0].Windows[0].Used != 25 {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestQuotaHistoryCollector_SkipsIneligibleCredentials(t *testing.T) {
	disabled := testQuotaHistoryClaudeAuth()
	disabled.ID, disabled.Disabled = "disabled", true
	statusDisabled := testQuotaHistoryCodexAuth()
	statusDisabled.ID, statusDisabled.Status = "status-disabled", coreauth.StatusDisabled
	apiKey := &coreauth.Auth{ID: "claude-key", Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-xyz"}}
	gemini := &coreauth.Auth{ID: "gemini", Provider: "gemini", Metadata: map[string]any{"access_token": "g"}}
	collector, _, doer := newTestQuotaHistoryCollector(t, 10*time.Minute, disabled, statusDisabled, apiKey, gemini, nil)
	collector.tick(context.Background())
	if got := doer.total(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestQuotaHistoryCollector_RestoresScheduleFromStore(t *testing.T) {
	collector, clock, doer := newTestQuotaHistoryCollector(t, 10*time.Minute, testQuotaHistoryCodexAuth())
	if errAppend := collector.store.Append(quotaHistorySample{
		T:        clock.Now().Add(-3 * time.Minute).UnixMilli(),
		AuthID:   "codex-b.json",
		Provider: "codex",
		Plan:     "enterprise",
		Src:      "poll",
		Windows:  []quotaHistoryWindow{{ID: "seven_day", PeriodS: 604800, Used: 1}},
	}); errAppend != nil {
		t.Fatal(errAppend)
	}
	collector.tick(context.Background())
	if got := doer.total(); got != 0 {
		t.Fatalf("requests = %d, want 0 right after a restart", got)
	}
	if plan := collector.states["codex-b.json"].plan; plan != "enterprise" {
		t.Fatalf("restored plan = %q", plan)
	}
	clock.Advance(7 * time.Minute)
	collector.tick(context.Background())
	if got := doer.total(); got != 1 {
		t.Fatalf("requests = %d, want 1 once the interval since the stored sample elapsed", got)
	}
}

func TestQuotaHistoryCollector_CodexPlanFallsBackToIDToken(t *testing.T) {
	collector, _, doer := newTestQuotaHistoryCollector(t, 10*time.Minute, testQuotaHistoryCodexAuth())
	doer.set(quotaHistoryCodexUsageURL, fakeQuotaHistoryResponse{status: http.StatusOK, body: `{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":604800}}}`})
	collector.tick(context.Background())
	samples := readTestQuotaHistorySamples(t, collector)
	if len(samples) != 1 || samples[0].Plan != "team" {
		t.Fatalf("samples = %+v, want plan from id_token", samples)
	}
}

func TestResolveQuotaHistoryDir(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	authDir := filepath.Join(root, "auths")

	dir, errDir := resolveQuotaHistoryDir("", configPath, authDir)
	if errDir != nil || dir != filepath.Join(root, "quota-history") {
		t.Fatalf("default dir = %q, %v", dir, errDir)
	}
	override := filepath.Join(root, "elsewhere")
	if dir, errDir = resolveQuotaHistoryDir("  "+override+"  ", configPath, authDir); errDir != nil || dir != override {
		t.Fatalf("override dir = %q, %v", dir, errDir)
	}
	if _, errDir = resolveQuotaHistoryDir(filepath.Join(authDir, "history"), configPath, authDir); errDir == nil {
		t.Fatal("expected a directory inside the auth dir to be rejected")
	}
	if _, errDir = resolveQuotaHistoryDir("", configPath, root); errDir == nil {
		t.Fatal("expected the default dir to be rejected when the auth dir is the config dir")
	}
	if dir, errDir = resolveQuotaHistoryDir("", configPath, filepath.Join(root, "quota")); errDir != nil || dir != filepath.Join(root, "quota-history") {
		t.Fatalf("sibling auth dir with a shared prefix: %q, %v", dir, errDir)
	}
	if _, errDir = resolveQuotaHistoryDir("", "", authDir); errDir == nil {
		t.Fatal("expected an error without a config path or override")
	}
}

func TestQuotaHistoryPollIntervalFromEnv(t *testing.T) {
	tests := map[string]time.Duration{
		"":       quotaHistoryDefaultPollInterval,
		"0":      0,
		"15m":    15 * time.Minute,
		"-1m":    quotaHistoryDefaultPollInterval,
		"often":  quotaHistoryDefaultPollInterval,
		" 90s  ": 90 * time.Second,
	}
	for raw, want := range tests {
		t.Setenv(quotaHistoryPollIntervalEnv, raw)
		if got := quotaHistoryPollIntervalFromEnv(); got != want {
			t.Errorf("%s=%q: interval = %s, want %s", quotaHistoryPollIntervalEnv, raw, got, want)
		}
	}
}

func TestHandlerStartStopQuotaHistory(t *testing.T) {
	root := t.TempDir()
	h := &Handler{configFilePath: filepath.Join(root, "config.yaml")}

	t.Setenv(quotaHistoryDisableEnv, "1")
	h.StartQuotaHistory()
	if quotaHistoryCollectorFor(h) != nil {
		t.Fatal("collector started although QUOTA_HISTORY_DISABLE=1")
	}

	t.Setenv(quotaHistoryDisableEnv, "")
	t.Setenv(quotaHistoryDirEnv, filepath.Join(root, "history"))
	t.Setenv(quotaHistoryPollIntervalEnv, "0")
	h.StartQuotaHistory()
	collector := quotaHistoryCollectorFor(h)
	if collector == nil {
		t.Fatal("collector not started")
	}
	if collector.store.dir != filepath.Join(root, "history") || collector.pollInterval != 0 {
		t.Fatalf("collector dir=%q poll=%s", collector.store.dir, collector.pollInterval)
	}
	h.StartQuotaHistory()
	if quotaHistoryCollectorFor(h) != collector {
		t.Fatal("second start replaced the running collector")
	}

	h.StopQuotaHistory()
	if quotaHistoryCollectorFor(h) != nil {
		t.Fatal("collector still registered after stop")
	}
	select {
	case <-collector.done:
	default:
		t.Fatal("collector goroutine still running after stop")
	}
	h.StopQuotaHistory()
	(*Handler)(nil).StartQuotaHistory()
	(*Handler)(nil).StopQuotaHistory()
}

func TestHandlerQuotaHistoryDoUsesConfigSnapshot(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h := &Handler{cfg: &config.Config{}}
	req, errRequest := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	req.Header.Set("Authorization", "Bearer token")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Concurrent config writes must not race the background request.
		h.SetConfig(&config.Config{})
	}()
	status, body, errDo := h.quotaHistoryDo(testQuotaHistoryClaudeAuth(), req)
	wg.Wait()
	if errDo != nil || status != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("status=%d body=%s err=%v", status, body, errDo)
	}
}
