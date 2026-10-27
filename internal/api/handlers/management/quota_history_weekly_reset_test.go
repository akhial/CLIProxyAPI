package management

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaHistoryCollector_PublishesWeeklyResetHint(t *testing.T) {
	claude := testQuotaHistoryClaudeAuth()
	claude.ID = "weekly-hint-" + claude.ID
	codex := testQuotaHistoryCodexAuth()
	codex.ID = "weekly-hint-" + codex.ID
	collector, clock, _ := newTestQuotaHistoryCollector(t, 10*time.Minute, claude, codex)
	collector.tick(context.Background())

	wantClaude := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	resetAt, observedAt, ok := coreauth.WeeklyResetHint(claude.ID)
	if !ok || !resetAt.Equal(wantClaude) || !observedAt.Equal(clock.Now()) {
		t.Fatalf("claude hint = %v, %v, %v; want reset %v observed %v", resetAt, observedAt, ok, wantClaude, clock.Now())
	}
	// The Codex weekly window comes from the 604800s secondary window, not the 5h primary.
	wantCodex := time.Unix(1762000000, 0)
	if resetAt, _, ok = coreauth.WeeklyResetHint(codex.ID); !ok || !resetAt.Equal(wantCodex) {
		t.Fatalf("codex hint = %v, %v; want reset %v", resetAt, ok, wantCodex)
	}
}

func TestQuotaHistoryCollector_RestoresWeeklyResetHintFromStore(t *testing.T) {
	auth := testQuotaHistoryClaudeAuth()
	auth.ID = "weekly-hint-restore-" + auth.ID
	collector, clock, _ := newTestQuotaHistoryCollector(t, 10*time.Minute)
	sampledAt := clock.Now().Add(-time.Hour)
	resetAt := clock.Now().Add(20 * time.Hour)
	if errAppend := collector.store.Append(quotaHistorySample{
		T:        sampledAt.UnixMilli(),
		AuthID:   auth.ID,
		Provider: "claude",
		Src:      quotaHistorySourcePoll,
		Windows: []quotaHistoryWindow{
			{ID: quotaHistoryWindowFiveHour, PeriodS: quotaHistoryFiveHourSeconds, Used: 10, ResetAt: clock.Now().Add(time.Hour).UnixMilli()},
			{ID: quotaHistoryWindowSevenDay, PeriodS: quotaHistoryWeekSeconds, Used: 40, ResetAt: resetAt.UnixMilli()},
		},
	}); errAppend != nil {
		t.Fatal(errAppend)
	}

	collector.tick(context.Background())

	gotReset, gotObserved, ok := coreauth.WeeklyResetHint(auth.ID)
	if !ok || !gotReset.Equal(time.UnixMilli(resetAt.UnixMilli())) || !gotObserved.Equal(time.UnixMilli(sampledAt.UnixMilli())) {
		t.Fatalf("restored hint = %v, %v, %v; want reset %v observed %v", gotReset, gotObserved, ok, resetAt, sampledAt)
	}
}
