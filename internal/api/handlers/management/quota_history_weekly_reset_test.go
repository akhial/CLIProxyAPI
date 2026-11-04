package management

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaHistoryCollector_PublishesQuotaHints(t *testing.T) {
	claude := testQuotaHistoryClaudeAuth()
	claude.ID = "weekly-hint-" + claude.ID
	codex := testQuotaHistoryCodexAuth()
	codex.ID = "weekly-hint-" + codex.ID
	collector, clock, _ := newTestQuotaHistoryCollector(t, 10*time.Minute, claude, codex)
	collector.tick(context.Background())

	wantClaude := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	weekly, observedAt, ok := coreauth.WeeklyQuotaHint(claude.ID)
	if !ok || !weekly.ResetAt.Equal(wantClaude) || weekly.UsedPercent != 16 || !observedAt.Equal(clock.Now()) {
		t.Fatalf("claude hint = %+v, %v, %v; want reset %v used 16 observed %v", weekly, observedAt, ok, wantClaude, clock.Now())
	}
	// The Codex weekly window comes from the 604800s secondary window, not the 5h primary.
	wantCodex := time.Unix(1762000000, 0)
	if weekly, _, ok = coreauth.WeeklyQuotaHint(codex.ID); !ok || !weekly.ResetAt.Equal(wantCodex) || weekly.UsedPercent != 30 {
		t.Fatalf("codex hint = %+v, %v; want reset %v used 30", weekly, ok, wantCodex)
	}
	// Both usage reads list a 5-hour window.
	for _, authID := range []string{claude.ID, codex.ID} {
		if session, observedAt, okLimit := coreauth.SessionQuotaHint(authID); !okLimit || !session.Limited || session.UsedPercent <= 0 || session.ResetAt.IsZero() || !observedAt.Equal(clock.Now()) {
			t.Fatalf("%s session hint = %+v, %v, %v; want limited with use and reset at %v", authID, session, observedAt, okLimit, clock.Now())
		}
	}
}

func TestQuotaHistoryRoutingHints_SessionLimit(t *testing.T) {
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	weekly := quotaHistoryWindow{ID: quotaHistoryWindowSevenDay, PeriodS: quotaHistoryWeekSeconds, Used: 7, ResetAt: at.Add(30 * time.Hour).UnixMilli()}
	fiveHour := quotaHistoryWindow{ID: quotaHistoryWindowFiveHour, PeriodS: quotaHistoryFiveHourSeconds, Used: 3}
	authID := "session-hint-" + t.Name()

	publishQuotaHistoryRoutingHints(authID, at, []quotaHistoryWindow{fiveHour, weekly})
	if session, _, ok := coreauth.SessionQuotaHint(authID); !ok || !session.Limited || session.UsedPercent != 3 {
		t.Fatalf("with five_hour: hint = %+v, %v; want limited, 3%% used", session, ok)
	}
	// An exempt plan drops five_hour; a later sample without it clears the limit.
	publishQuotaHistoryRoutingHints(authID, at.Add(time.Minute), []quotaHistoryWindow{weekly})
	if session, _, ok := coreauth.SessionQuotaHint(authID); !ok || session.Limited {
		t.Fatalf("without five_hour: hint = %+v, %v; want not limited", session, ok)
	}
	// A sample without a weekly window does not describe the plan.
	publishQuotaHistoryRoutingHints(authID, at.Add(2*time.Minute), []quotaHistoryWindow{fiveHour})
	if session, observedAt, _ := coreauth.SessionQuotaHint(authID); session.Limited || !observedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("five_hour alone changed the hint: %+v at %v", session, observedAt)
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

	got, gotObserved, ok := coreauth.WeeklyQuotaHint(auth.ID)
	if !ok || !got.ResetAt.Equal(time.UnixMilli(resetAt.UnixMilli())) || got.UsedPercent != 40 || !gotObserved.Equal(time.UnixMilli(sampledAt.UnixMilli())) {
		t.Fatalf("restored hint = %+v, %v, %v; want reset %v used 40 observed %v", got, gotObserved, ok, resetAt, sampledAt)
	}
}
