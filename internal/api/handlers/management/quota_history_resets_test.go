package management

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testClaudeResetGrantsBody = `{"five_hour":null,"cedar_ember":{"eligible":true,"at_limit":false,` +
	`"cooldown_until":null,"grants":[` +
	`{"id":"general-1","label":"General","resets_total":1,"resets_left":1,"ends_at":"2026-12-10T00:00:00Z","clears":["seven_day","five_hour","other"],"usable_now":true},` +
	`{"id":"fable-1","label":"Fable\u0007 reset","resets_total":3,"resets_left":2,"starts_at":"2026-10-01T00:00:00Z","ends_at":"2027-03-08T00:00:00Z","gives_back_weekly":true,"paused":false}]}}`

func TestParseClaudeResetGrants(t *testing.T) {
	now := time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)
	got, errParse := parseClaudeResetGrants([]byte(testClaudeResetGrantsBody), now)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if !got.Eligible || got.CheckedAt != now.UnixMilli() || got.CooldownUntil != 0 || len(got.Grants) != 2 {
		t.Fatalf("grants = %+v", got)
	}
	general, fable := got.Grants[0], got.Grants[1]
	if general.ID != "general-1" || general.GivesBackWeekly || general.ResetsLeft != 1 ||
		len(general.Clears) != 2 || general.Clears[0] != "five_hour" || general.Clears[1] != "seven_day" ||
		general.EndsAt != time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC).UnixMilli() || general.StartsAt != 0 {
		t.Fatalf("general grant = %+v", general)
	}
	if fable.ID != "fable-1" || !fable.GivesBackWeekly || fable.ResetsTotal != 3 || fable.ResetsLeft != 2 ||
		fable.Label != "Fable reset" || len(fable.Clears) != 0 ||
		fable.StartsAt != time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("fable grant = %+v", fable)
	}
}

func TestParseClaudeResetGrantsRejectsMalformedBlocks(t *testing.T) {
	grant := `{"id":"g-1","resets_total":2,"resets_left":1}`
	for name, body := range map[string]string{
		"not json":        `nope`,
		"block string":    `{"cedar_ember":"x"}`,
		"no eligibility":  `{"cedar_ember":{"grants":[]}}`,
		"grants object":   `{"cedar_ember":{"eligible":true,"grants":{}}}`,
		"duplicate id":    `{"cedar_ember":{"eligible":true,"grants":[` + grant + `,` + grant + `]}}`,
		"bad cooldown":    `{"cedar_ember":{"eligible":true,"cooldown_until":"bad"}}`,
		"left over total": `{"cedar_ember":{"eligible":true,"grants":[{"id":"g-1","resets_total":1,"resets_left":2}]}}`,
		"bad id":          `{"cedar_ember":{"eligible":true,"grants":[{"id":"G 1","resets_total":1,"resets_left":1}]}}`,
		"bad flag":        `{"cedar_ember":{"eligible":true,"grants":[{"id":"g-1","resets_total":1,"resets_left":1,"gives_back_weekly":"yes"}]}}`,
		"fractional":      `{"cedar_ember":{"eligible":true,"grants":[{"id":"g-1","resets_total":1.5,"resets_left":1}]}}`,
	} {
		if _, errParse := parseClaudeResetGrants([]byte(body), time.Now()); errParse == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
	for _, body := range []string{`{"five_hour":null}`, `{"cedar_ember":null}`} {
		got, errParse := parseClaudeResetGrants([]byte(body), time.Now())
		if errParse != nil || got.Eligible || got.IneligibleReason != "no_program" || got.Grants == nil || len(got.Grants) != 0 {
			t.Fatalf("%s = %+v, %v", body, got, errParse)
		}
	}
	got, errParse := parseClaudeResetGrants([]byte(`{"cedar_ember":{"eligible":false,"ineligible_reason":"tier"}}`), time.Now())
	if errParse != nil || got.Eligible || got.IneligibleReason != "tier" || got.Grants == nil || len(got.Grants) != 0 {
		t.Fatalf("ineligible = %+v, %v", got, errParse)
	}
}

func TestQuotaHistoryCollector_ReadsResetGrantsOnceAndAfterAClaim(t *testing.T) {
	auth := testQuotaHistoryClaudeAuth()
	collector, clock, doer := newTestQuotaHistoryCollector(t, 10*time.Minute, auth)
	ctx := context.Background()
	reads := func() int { return len(doer.requestsTo(quotaHistoryClaudeResetGrantsURL)) }

	collector.tick(ctx)
	if reads() != 1 {
		t.Fatalf("reads after first tick = %d, want 1", reads())
	}
	req := doer.requestsTo(quotaHistoryClaudeResetGrantsURL)[0]
	if req.Header.Get("Authorization") != "Bearer claude-token" || req.Header.Get("User-Agent") != quotaHistoryClaudeUserAgent {
		t.Fatalf("reset grant request headers = %v", req.Header)
	}
	record, ok, stale := collector.resets.Get(auth.ID)
	if !ok || stale || len(record.Grants) != 2 || record.CheckedAt != clock.Now().UnixMilli() {
		t.Fatalf("saved record = %+v ok=%v stale=%v", record, ok, stale)
	}

	for range 3 {
		clock.Advance(24 * time.Hour)
		collector.tick(ctx)
	}
	if reads() != 1 {
		t.Fatalf("reads after three days = %d, want 1", reads())
	}

	// A restarted collector finds the saved record and does not read again.
	restarted, _, restartedDoer := newTestQuotaHistoryCollector(t, 10*time.Minute, auth)
	restarted.resets = newQuotaHistoryResetStore(collector.store.dir)
	restarted.nowFunc = clock.Now
	restarted.tick(ctx)
	if got := len(restartedDoer.requestsTo(quotaHistoryClaudeResetGrantsURL)); got != 0 {
		t.Fatalf("reads after restart = %d, want 0", got)
	}

	// A claim makes the record stale until the next tick reads it again.
	doer.set(quotaHistoryClaudeResetGrantsURL, fakeQuotaHistoryResponse{status: http.StatusOK,
		body: `{"cedar_ember":{"eligible":true,"grants":[{"id":"fable-1","resets_total":3,"resets_left":1,"gives_back_weekly":true}]}}`})
	collector.resets.Invalidate(auth.ID)
	if _, _, stale := collector.resets.Get(auth.ID); !stale {
		t.Fatal("record not stale after invalidation")
	}
	clock.Advance(time.Minute)
	collector.tick(ctx)
	record, _, stale = collector.resets.Get(auth.ID)
	if reads() != 2 || stale || len(record.Grants) != 1 || record.Grants[0].ResetsLeft != 1 {
		t.Fatalf("after claim: reads=%d record=%+v stale=%v", reads(), record, stale)
	}
	saved, errRead := os.ReadFile(filepath.Join(collector.store.dir, quotaHistoryResetGrantsFileName))
	if errRead != nil {
		t.Fatalf("read saved file: %v", errRead)
	}
	reloaded := newQuotaHistoryResetStore(collector.store.dir)
	if got, ok, _ := reloaded.Get(auth.ID); !ok || len(got.Grants) != 1 {
		t.Fatalf("reloaded record = %+v ok=%v (file %s)", got, ok, saved)
	}
}

func TestQuotaHistoryCollector_RetriesFailedResetGrantRead(t *testing.T) {
	auth := testQuotaHistoryClaudeAuth()
	collector, clock, doer := newTestQuotaHistoryCollector(t, 10*time.Minute, auth)
	ctx := context.Background()
	reads := func() int { return len(doer.requestsTo(quotaHistoryClaudeResetGrantsURL)) }
	doer.set(quotaHistoryClaudeResetGrantsURL, fakeQuotaHistoryResponse{status: http.StatusTooManyRequests})

	collector.tick(ctx)
	clock.Advance(quotaHistoryResetGrantsRetry - time.Minute)
	collector.tick(ctx)
	if reads() != 1 {
		t.Fatalf("reads before retry delay = %d, want 1", reads())
	}
	doer.set(quotaHistoryClaudeResetGrantsURL, fakeQuotaHistoryResponse{status: http.StatusOK, body: testClaudeResetGrantsBody})
	clock.Advance(time.Minute)
	collector.tick(ctx)
	if _, ok, _ := collector.resets.Get(auth.ID); reads() != 2 || !ok {
		t.Fatalf("reads after retry delay = %d, saved = %v", reads(), ok)
	}
}

func TestQuotaHistoryCollector_SkipsResetGrantsWithoutPolling(t *testing.T) {
	auth := testQuotaHistoryClaudeAuth()
	collector, _, doer := newTestQuotaHistoryCollector(t, 0, auth)
	collector.tick(context.Background())
	if got := len(doer.requestsTo(quotaHistoryClaudeResetGrantsURL)); got != 0 {
		t.Fatalf("reads with polling off = %d, want 0", got)
	}
}

func TestIsClaudeResetClaim(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://api.anthropic.com/api/organizations/11111111-2222-3333-4444-555555555555/reset_rate_limits": true,
		"https://API.anthropic.com/api/organizations/org/reset_rate_limits/":                                 true,
		"https://api.anthropic.com/api/oauth/usage?cedar_ember=1":                                            false,
		"https://example.com/api/organizations/org/reset_rate_limits":                                        false,
		"https://api.anthropic.com/api/organizations/org/reset_rate_limits/extra":                            false,
	} {
		parsed, errParse := url.Parse(raw)
		if errParse != nil {
			t.Fatal(errParse)
		}
		if got := isClaudeResetClaim(http.MethodPost, parsed); got != want {
			t.Errorf("POST %s = %v, want %v", raw, got, want)
		}
	}
	parsed, _ := url.Parse("https://api.anthropic.com/api/organizations/org/reset_rate_limits")
	if isClaudeResetClaim(http.MethodGet, parsed) {
		t.Error("GET counted as a claim")
	}
}

func TestAddQuotaHistoryResetGrants(t *testing.T) {
	store := newQuotaHistoryResetStore(t.TempDir())
	if errPut := store.Put("a", claudeResetGrants{CheckedAt: 1, Eligible: true, Grants: []claudeResetGrant{{ID: "g"}}}); errPut != nil {
		t.Fatal(errPut)
	}
	store.Invalidate("a")
	accounts := []quotaHistoryAccount{{AuthID: "a"}, {AuthID: "b"}}
	addQuotaHistoryResetGrants(accounts, store)
	if got := accounts[0].ResetGrants; got == nil || !got.Stale || got.CheckedAt != 1 || len(got.Grants) != 1 {
		t.Fatalf("account a = %+v", got)
	}
	if accounts[1].ResetGrants != nil {
		t.Fatalf("account b = %+v, want none", accounts[1].ResetGrants)
	}
}
