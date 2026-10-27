package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Environment variables controlling the quota history collector.
const (
	quotaHistoryDisableEnv      = "QUOTA_HISTORY_DISABLE"
	quotaHistoryDirEnv          = "QUOTA_HISTORY_DIR"
	quotaHistoryPollIntervalEnv = "QUOTA_HISTORY_POLL_INTERVAL"
)

const (
	quotaHistoryDirName             = "quota-history"
	quotaHistoryStartDelay          = 30 * time.Second
	quotaHistoryTickInterval        = time.Minute
	quotaHistoryDefaultPollInterval = 10 * time.Minute
	// quotaHistoryMinHeaderSpacing is the minimum gap between a sample and a
	// following one taken from passive response headers.
	quotaHistoryMinHeaderSpacing = 5 * time.Minute
	quotaHistoryPlanRefresh      = 24 * time.Hour
	quotaHistoryPruneInterval    = 24 * time.Hour
	quotaHistoryMaxBackoff       = 3 * time.Hour
	// quotaHistoryStateLookback bounds the history read on startup to restore
	// per-credential schedules.
	quotaHistoryStateLookback    = 48 * time.Hour
	quotaHistoryMaxResponseBytes = 1 << 20

	quotaHistorySourcePoll   = "poll"
	quotaHistorySourceHeader = "header"

	quotaHistoryClaudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	quotaHistoryClaudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	quotaHistoryCodexUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
	quotaHistoryClaudeUserAgent  = "claude-cli/2.1.280 (external, cli)"
	quotaHistoryCodexUserAgent   = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
)

// quotaHistoryDoFunc performs one upstream request for a credential and
// returns its status code and body.
type quotaHistoryDoFunc func(auth *coreauth.Auth, req *http.Request) (int, []byte, error)

// quotaHistoryAuthState is the in-memory schedule of one credential.
type quotaHistoryAuthState struct {
	lastSample    time.Time
	failures      int
	nextPollAt    time.Time
	plan          string
	planCheckedAt time.Time
}

// quotaHistoryCollector records Claude and Codex OAuth quota samples from passive
// response headers and, when those are stale, from active usage polls.
type quotaHistoryCollector struct {
	store        *quotaHistoryStore
	pollInterval time.Duration
	tickInterval time.Duration
	nowFunc      func() time.Time
	listAuths    func() []*coreauth.Auth
	resolveToken func(ctx context.Context, auth *coreauth.Auth) (string, error)
	do           quotaHistoryDoFunc

	// The fields below are owned by the goroutine running tick.
	states      map[string]*quotaHistoryAuthState
	stateLoaded bool
	lastPrune   time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

func newQuotaHistoryCollector(h *Handler, store *quotaHistoryStore, pollInterval time.Duration) *quotaHistoryCollector {
	return &quotaHistoryCollector{
		store:        store,
		pollInterval: pollInterval,
		tickInterval: quotaHistoryTickInterval,
		nowFunc:      time.Now,
		listAuths:    h.quotaHistoryAuths,
		resolveToken: func(ctx context.Context, auth *coreauth.Auth) (string, error) {
			return h.resolveTokenForAuth(ctx, auth, "")
		},
		do:     h.quotaHistoryDo,
		states: make(map[string]*quotaHistoryAuthState),
	}
}

// quotaHistoryCollectors maps running collectors to their handler, so the
// Handler struct stays untouched.
var quotaHistoryCollectors = struct {
	sync.Mutex
	byHandler map[*Handler]*quotaHistoryCollector
}{byHandler: make(map[*Handler]*quotaHistoryCollector)}

func quotaHistoryCollectorFor(h *Handler) *quotaHistoryCollector {
	quotaHistoryCollectors.Lock()
	defer quotaHistoryCollectors.Unlock()
	return quotaHistoryCollectors.byHandler[h]
}

// StartQuotaHistory starts recording Claude and Codex OAuth quota samples in the
// background. It returns immediately and is a no-op when the collector already
// runs or QUOTA_HISTORY_DISABLE is set.
func (h *Handler) StartQuotaHistory() {
	if h == nil {
		return
	}
	quotaHistoryCollectors.Lock()
	defer quotaHistoryCollectors.Unlock()
	if _, running := quotaHistoryCollectors.byHandler[h]; running {
		return
	}
	if quotaHistoryEnvFlag(os.Getenv(quotaHistoryDisableEnv)) {
		log.Infof("quota history: disabled by %s", quotaHistoryDisableEnv)
		return
	}
	dir, errDir := h.quotaHistoryDir()
	if errDir != nil {
		log.WithError(errDir).Warn("quota history: disabled")
		return
	}
	pollInterval := quotaHistoryPollIntervalFromEnv()
	collector := newQuotaHistoryCollector(h, newQuotaHistoryStore(dir), pollInterval)
	ctx, cancel := context.WithCancel(context.Background())
	collector.cancel = cancel
	collector.done = make(chan struct{})
	quotaHistoryCollectors.byHandler[h] = collector
	go collector.run(ctx)
	log.Infof("quota history: recording Claude/Codex quota samples in %s (poll interval %s)", dir, pollInterval)
}

// StopQuotaHistory stops the collector started by StartQuotaHistory and waits
// for its goroutine to exit.
func (h *Handler) StopQuotaHistory() {
	if h == nil {
		return
	}
	quotaHistoryCollectors.Lock()
	collector := quotaHistoryCollectors.byHandler[h]
	delete(quotaHistoryCollectors.byHandler, h)
	quotaHistoryCollectors.Unlock()
	if collector == nil || collector.cancel == nil {
		return
	}
	collector.cancel()
	<-collector.done
}

func (h *Handler) quotaHistoryDir() (string, error) {
	authDir := ""
	if cfg := h.configSnapshot(); cfg != nil {
		authDir = cfg.AuthDir
	}
	return resolveQuotaHistoryDir(os.Getenv(quotaHistoryDirEnv), h.configFilePath, authDir)
}

// resolveQuotaHistoryDir returns QUOTA_HISTORY_DIR or <config dir>/quota-history,
// refusing any location inside the auth directory.
func resolveQuotaHistoryDir(override, configFilePath, authDir string) (string, error) {
	dir := strings.TrimSpace(override)
	if dir == "" {
		configFilePath = strings.TrimSpace(configFilePath)
		if configFilePath == "" {
			return "", fmt.Errorf("config file path unknown; set %s", quotaHistoryDirEnv)
		}
		dir = filepath.Join(filepath.Dir(configFilePath), quotaHistoryDirName)
	}
	absDir, errAbs := filepath.Abs(dir)
	if errAbs != nil {
		return "", fmt.Errorf("resolve quota history directory: %w", errAbs)
	}
	resolvedAuthDir, errAuthDir := util.ResolveAuthDir(authDir)
	if errAuthDir != nil {
		return "", fmt.Errorf("resolve auth directory: %w", errAuthDir)
	}
	absAuthDir, errAbsAuth := filepath.Abs(resolvedAuthDir)
	if errAbsAuth != nil {
		return "", fmt.Errorf("resolve auth directory: %w", errAbsAuth)
	}
	if quotaHistoryPathWithin(absDir, absAuthDir) {
		return "", fmt.Errorf("%s is inside the auth directory; set %s to another location", absDir, quotaHistoryDirEnv)
	}
	return absDir, nil
}

func quotaHistoryPathWithin(path, parent string) bool {
	rel, errRel := filepath.Rel(parent, path)
	if errRel != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func quotaHistoryPollIntervalFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(quotaHistoryPollIntervalEnv))
	if raw == "" {
		return quotaHistoryDefaultPollInterval
	}
	interval, errParse := time.ParseDuration(raw)
	if errParse != nil || interval < 0 {
		log.Warnf("quota history: invalid %s=%q, using %s", quotaHistoryPollIntervalEnv, raw, quotaHistoryDefaultPollInterval)
		return quotaHistoryDefaultPollInterval
	}
	return interval
}

func quotaHistoryEnvFlag(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (h *Handler) quotaHistoryAuths() []*coreauth.Auth {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return nil
	}
	return manager.List()
}

// quotaHistoryDo sends a request through the same transport and timeout as the
// management APICall handler.
func (h *Handler) quotaHistoryDo(auth *coreauth.Auth, req *http.Request) (int, []byte, error) {
	// apiCallTransport reads h.cfg without h.mu; this runs outside any request,
	// so hand it a clone taken under the lock instead of the live config.
	snapshot := &Handler{}
	if h != nil {
		h.mu.Lock()
		snapshot.cfg = h.cfg.CloneForRuntime()
		h.mu.Unlock()
	}
	client := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: snapshot.apiCallTransport(auth, ""),
	}
	defer client.CloseIdleConnections()
	resp, errDo := client.Do(req)
	if errDo != nil {
		return 0, nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("quota history: response body close error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, quotaHistoryMaxResponseBytes))
	if errRead != nil {
		return resp.StatusCode, nil, errRead
	}
	return resp.StatusCode, body, nil
}

func (c *quotaHistoryCollector) run(ctx context.Context) {
	defer close(c.done)
	timer := time.NewTimer(quotaHistoryStartDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			c.safeTick(ctx)
			timer.Reset(c.tickInterval)
		}
	}
}

func (c *quotaHistoryCollector) safeTick(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.WithField("panic", recovered).Error("quota history: collector tick panicked")
		}
	}()
	c.tick(ctx)
}

// tick records at most one sample per eligible credential.
func (c *quotaHistoryCollector) tick(ctx context.Context) {
	now := c.nowFunc()
	if !c.stateLoaded {
		c.stateLoaded = true
		c.loadState(now)
	}
	if c.lastPrune.IsZero() || now.Sub(c.lastPrune) >= quotaHistoryPruneInterval {
		c.lastPrune = now
		if errPrune := c.store.Prune(now); errPrune != nil {
			log.WithError(errPrune).Warn("quota history: failed to prune expired samples")
		}
	}
	for _, auth := range c.listAuths() {
		if ctx.Err() != nil {
			return
		}
		if !quotaHistoryEligible(auth) {
			continue
		}
		c.collect(ctx, auth)
	}
}

// loadState restores the last sample time and plan of each credential so a
// restart does not poll every credential at once.
func (c *quotaHistoryCollector) loadState(now time.Time) {
	samples, errRead := c.store.Read(now.Add(-quotaHistoryStateLookback), now)
	if errRead != nil {
		log.WithError(errRead).Warn("quota history: failed to read recent samples")
		return
	}
	for _, sample := range samples {
		state := c.state(sample.AuthID)
		sampledAt := time.UnixMilli(sample.T)
		if !sampledAt.After(state.lastSample) {
			continue
		}
		state.lastSample = sampledAt
		if sample.Plan != "" {
			state.plan = sample.Plan
		}
	}
}

func (c *quotaHistoryCollector) state(authID string) *quotaHistoryAuthState {
	state := c.states[authID]
	if state == nil {
		state = &quotaHistoryAuthState{}
		c.states[authID] = state
	}
	return state
}

func quotaHistoryEligible(auth *coreauth.Auth) bool {
	if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	switch quotaHistoryProvider(auth) {
	case "claude", "codex":
		return auth.AuthKind() == coreauth.AuthKindOAuth
	default:
		return false
	}
}

func quotaHistoryProvider(auth *coreauth.Auth) string {
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func (c *quotaHistoryCollector) collect(ctx context.Context, auth *coreauth.Auth) {
	now := c.nowFunc()
	auth.EnsureIndex()
	provider := quotaHistoryProvider(auth)
	state := c.state(auth.ID)

	if provider == "claude" && c.pollInterval > 0 &&
		(state.planCheckedAt.IsZero() || now.Sub(state.planCheckedAt) >= quotaHistoryPlanRefresh) {
		state.planCheckedAt = now
		c.refreshClaudePlan(ctx, auth, state)
	}

	if c.recordHeaderSample(auth, provider, state) {
		return
	}
	if c.pollInterval <= 0 || now.Before(state.nextPollAt) {
		return
	}
	if !state.lastSample.IsZero() && now.Sub(state.lastSample) < c.pollInterval {
		return
	}
	if errPoll := c.poll(ctx, auth, provider, state, now); errPoll != nil {
		if ctx.Err() != nil {
			return
		}
		c.recordPollFailure(auth, provider, state, now, errPoll)
	}
}

// recordHeaderSample records the passive header snapshot when it is newer than
// the last sample and at least quotaHistoryMinHeaderSpacing after it.
func (c *quotaHistoryCollector) recordHeaderSample(auth *coreauth.Auth, provider string, state *quotaHistoryAuthState) bool {
	observedAt := auth.Quota.ObservedAt
	if observedAt.IsZero() || len(auth.Quota.Signals) == 0 {
		return false
	}
	if !state.lastSample.IsZero() &&
		(!observedAt.After(state.lastSample) || observedAt.Sub(state.lastSample) < quotaHistoryMinHeaderSpacing) {
		return false
	}
	windows, plan := quotaHistoryWindowsFromSignals(provider, auth.Quota.Signals, observedAt)
	if len(windows) == 0 {
		return false
	}
	if plan != "" {
		state.plan = plan
	}
	c.record(auth, provider, state, observedAt, quotaHistorySourceHeader, windows)
	return true
}

func (c *quotaHistoryCollector) poll(ctx context.Context, auth *coreauth.Auth, provider string, state *quotaHistoryAuthState, now time.Time) error {
	token, errToken := c.token(ctx, auth)
	if errToken != nil {
		return errToken
	}
	var windows []quotaHistoryWindow
	switch provider {
	case "claude":
		headers := quotaHistoryClaudeHeaders(token)
		headers["Accept"] = "application/json"
		body, errFetch := c.fetch(ctx, auth, quotaHistoryClaudeUsageURL, headers)
		if errFetch != nil {
			return errFetch
		}
		windows = parseClaudeUsageHistoryWindows(body)
	case "codex":
		body, errFetch := c.fetch(ctx, auth, quotaHistoryCodexUsageURL, quotaHistoryCodexHeaders(token, quotaHistoryCodexAccountID(auth)))
		if errFetch != nil {
			return errFetch
		}
		var plan string
		windows, plan = parseCodexUsageHistory(body, now)
		if plan != "" {
			state.plan = plan
		}
	default:
		return fmt.Errorf("unsupported provider %q", provider)
	}
	if len(windows) == 0 {
		return errors.New("usage response contained no quota windows")
	}
	c.record(auth, provider, state, now, quotaHistorySourcePoll, windows)
	state.failures = 0
	state.nextPollAt = time.Time{}
	return nil
}

// refreshClaudePlan fetches the Claude profile; callers limit it to once per
// quotaHistoryPlanRefresh per credential, whatever the outcome.
func (c *quotaHistoryCollector) refreshClaudePlan(ctx context.Context, auth *coreauth.Auth, state *quotaHistoryAuthState) {
	token, errToken := c.token(ctx, auth)
	if errToken != nil {
		return
	}
	body, errFetch := c.fetch(ctx, auth, quotaHistoryClaudeProfileURL, quotaHistoryClaudeHeaders(token))
	if errFetch != nil {
		if ctx.Err() == nil {
			c.logEntry(auth, "claude").WithError(errFetch).Debug("quota history: claude profile fetch failed")
		}
		return
	}
	if plan := parseClaudeProfilePlan(body); plan != "" {
		state.plan = plan
	}
}

func (c *quotaHistoryCollector) token(ctx context.Context, auth *coreauth.Auth) (string, error) {
	token, errToken := c.resolveToken(ctx, auth)
	if errToken != nil {
		return "", fmt.Errorf("resolve auth token: %w", errToken)
	}
	if token == "" {
		return "", errors.New("auth token not found")
	}
	return token, nil
}

func (c *quotaHistoryCollector) fetch(ctx context.Context, auth *coreauth.Auth, url string, headers map[string]string) ([]byte, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("build request: %w", errRequest)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	status, body, errDo := c.do(auth, req)
	if errDo != nil {
		return nil, fmt.Errorf("request failed: %w", errDo)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("upstream returned status %d", status)
	}
	return body, nil
}

// recordPollFailure backs the credential off exponentially from the poll
// interval, up to quotaHistoryMaxBackoff.
func (c *quotaHistoryCollector) recordPollFailure(auth *coreauth.Auth, provider string, state *quotaHistoryAuthState, now time.Time, errPoll error) {
	state.failures++
	backoff := c.pollInterval
	for i := 1; i < state.failures && backoff < quotaHistoryMaxBackoff; i++ {
		backoff *= 2
	}
	if backoff > quotaHistoryMaxBackoff && c.pollInterval < quotaHistoryMaxBackoff {
		backoff = quotaHistoryMaxBackoff
	}
	state.nextPollAt = now.Add(backoff)
	entry := c.logEntry(auth, provider).WithError(errPoll).WithFields(log.Fields{
		"failures": state.failures,
		"retry_in": backoff.String(),
	})
	if state.failures == 1 {
		entry.Warn("quota history: quota poll failed")
		return
	}
	entry.Debug("quota history: quota poll failed")
}

func (c *quotaHistoryCollector) record(auth *coreauth.Auth, provider string, state *quotaHistoryAuthState, at time.Time, source string, windows []quotaHistoryWindow) {
	// Advance the schedule even if the write fails, so a broken disk does not
	// turn into an upstream poll on every tick.
	state.lastSample = at
	sample := quotaHistorySample{
		T:         at.UnixMilli(),
		AuthID:    auth.ID,
		AuthIndex: auth.Index,
		FileName:  quotaHistoryFileName(auth),
		Provider:  provider,
		Label:     strings.TrimSpace(auth.Label),
		Email:     authEmail(auth),
		Plan:      quotaHistoryPlan(auth, provider, state),
		Src:       source,
		Windows:   windows,
	}
	if errAppend := c.store.Append(sample); errAppend != nil {
		c.logEntry(auth, provider).WithError(errAppend).Warn("quota history: failed to record sample")
	}
}

func (c *quotaHistoryCollector) logEntry(auth *coreauth.Auth, provider string) *log.Entry {
	return log.WithFields(log.Fields{
		"provider":   provider,
		"auth_index": auth.Index,
		"file_name":  quotaHistoryFileName(auth),
	})
}

func quotaHistoryPlan(auth *coreauth.Auth, provider string, state *quotaHistoryAuthState) string {
	if state.plan != "" || provider != "codex" {
		return state.plan
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		if plan, ok := claims["plan_type"].(string); ok {
			return strings.ToLower(strings.TrimSpace(plan))
		}
	}
	return ""
}

func quotaHistoryFileName(auth *coreauth.Auth) string {
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return filepath.Base(name)
	}
	if path := strings.TrimSpace(authAttribute(auth, "path")); path != "" {
		return filepath.Base(path)
	}
	return auth.ID
}

func quotaHistoryClaudeHeaders(token string) map[string]string {
	return map[string]string{
		"User-Agent":     quotaHistoryClaudeUserAgent,
		"Authorization":  "Bearer " + token,
		"Content-Type":   "application/json",
		"anthropic-beta": "oauth-2025-04-20",
	}
}

func quotaHistoryCodexHeaders(token, accountID string) map[string]string {
	headers := map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
		"User-Agent":    quotaHistoryCodexUserAgent,
	}
	if accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	return headers
}

// quotaHistoryCodexAccountID resolves the ChatGPT account id like the
// management UI, then falls back to the account_id the Codex executor sends.
func quotaHistoryCodexAccountID(auth *coreauth.Auth) string {
	for _, key := range []string{"chatgpt_account_id", "chatgptAccountId"} {
		if accountID := stringValue(auth.Metadata, key); accountID != "" {
			return accountID
		}
		if accountID := strings.TrimSpace(authAttribute(auth, key)); accountID != "" {
			return accountID
		}
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		if accountID, ok := claims["chatgpt_account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
			return strings.TrimSpace(accountID)
		}
	}
	return stringValue(auth.Metadata, "account_id")
}
