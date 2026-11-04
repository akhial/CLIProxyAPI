package management

import (
	"math"
	"strconv"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestComputeQuotaHistoryRoutingStats(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at := func(hoursAgo float64) int64 {
		return now.Add(-time.Duration(hoursAgo * float64(time.Hour))).UnixMilli()
	}
	weekU := now.Add(50 * time.Hour).UnixMilli()
	weekP := now.Add(100 * time.Hour).UnixMilli()
	sample := func(id, provider, plan string, hoursAgo float64, windows ...quotaHistoryWindow) quotaHistorySample {
		return quotaHistorySample{T: at(hoursAgo), AuthID: id, Provider: provider, Plan: plan, Windows: windows}
	}
	week := func(used float64, resetAt int64) quotaHistoryWindow {
		return quotaHistoryWindow{ID: quotaHistoryWindowSevenDay, PeriodS: quotaHistoryWeekSeconds, Used: used, ResetAt: resetAt}
	}
	five := func(used float64, resetAt int64) quotaHistoryWindow {
		return quotaHistoryWindow{ID: quotaHistoryWindowFiveHour, PeriodS: quotaHistoryFiveHourSeconds, Used: used, ResetAt: resetAt}
	}
	oldSession, recentSession := at(96), at(10)
	samples := []quotaHistorySample{
		// u: a window 4 days ago allowed 16 % of the week, the latest one 28 % (the
		// resets jitter by milliseconds between samples).
		sample("u", "claude", "max20", 100, five(0, oldSession), week(10, weekU)),
		sample("u", "claude", "max20", 97, five(50, oldSession+37), week(18, weekU)),
		sample("u", "claude", "max20", 15, five(0, recentSession), week(20, weekU)),
		sample("u", "claude", "max20", 11, five(40, recentSession-20), week(31.2, weekU)),
		// p: a Pro whose 5-hour use never grows enough to measure; 50 weekly points in a day.
		sample("p", "claude", "pro", 30, five(1, at(28)), week(10, weekP)),
		sample("p", "claude", "pro", 12, five(10, at(8)), week(30, weekP)),
		sample("p", "claude", "pro", 1, five(20, at(-2)), week(60, weekP)),
		// v: an Ultra without a measured window borrows u's share.
		sample("v", "claude", "max20", 2, five(5, at(-1)), week(40, weekU)),
		// w: only an old window (20 %), still the best estimate.
		sample("w", "claude", "max5", 90, five(10, at(87)), week(50, weekP)),
		sample("w", "claude", "max5", 88, five(60, at(87)), week(60, weekP)),
		// A window across the weekly reset does not count.
		sample("x", "claude", "max5", 6, five(0, at(2)), week(90, at(5))),
		sample("x", "claude", "max5", 3, five(50, at(2)), week(2, weekP)),
		// Codex is its own pool.
		sample("c", "codex", "pro", 20, week(10, weekP)),
		sample("c", "codex", "pro", 2, week(22, weekP)),
	}

	stats := computeQuotaHistoryRoutingStats(samples, now)

	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-6 }
	wantShare := map[string]float64{"u": 28, "p": 0, "v": 28, "w": 20, "x": 20, "c": 0}
	for id, want := range wantShare {
		if got := stats[id].SessionSharePercent; !near(got, want) {
			t.Errorf("%s session share = %v, want %v", id, got, want)
		}
	}
	// Claude pool in the last day: u 2 + 11.2 points (10 Pro weeks each), p 20 + 30
	// (1 each), x 2 after its reset (5 each): 1.92 Pro weeks over 24 h.
	claudeBurn := (13.2*10 + 50*1 + 2*5) / 100.0 / 24
	wantDrain := map[string]float64{
		"u": claudeBurn * 100 / 10,
		"v": claudeBurn * 100 / 10,
		"p": claudeBurn * 100 / 1,
		"w": claudeBurn * 100 / 5,
		"c": 12.0 / 100 / 24 * 100,
	}
	for id, want := range wantDrain {
		if got := stats[id].DrainPercentPerHour; !near(got, want) {
			t.Errorf("%s drain = %v, want %v", id, got, want)
		}
	}
}

func TestComputeQuotaHistoryRoutingStats_ShortHistory(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	reset := now.Add(50 * time.Hour).UnixMilli()
	samples := []quotaHistorySample{
		{T: now.Add(-6 * time.Hour).UnixMilli(), AuthID: "a", Provider: "claude", Plan: "max20", Windows: []quotaHistoryWindow{{ID: quotaHistoryWindowSevenDay, Used: 10, ResetAt: reset}}},
		{T: now.Add(-time.Hour).UnixMilli(), AuthID: "a", Provider: "claude", Plan: "max20", Windows: []quotaHistoryWindow{{ID: quotaHistoryWindowSevenDay, Used: 40, ResetAt: reset}}},
	}
	// Six hours of history: 30 points over 6 h, not over a day.
	if got := computeQuotaHistoryRoutingStats(samples, now)["a"].DrainPercentPerHour; math.Abs(got-5) > 1e-9 {
		t.Fatalf("drain = %v, want 5 %%/h", got)
	}
}

func TestAddQuotaHistoryRouting(t *testing.T) {
	now := time.Now()
	authID := "history-routing-" + t.Name()
	auth := &coreauth.Auth{ID: authID, Provider: "claude", Quota: coreauth.QuotaState{
		ObservedAt: now,
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(30*time.Hour).Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.5",
		},
	}}
	coreauth.SetRoutingStats(authID, coreauth.RoutingStats{SessionSharePercent: 27.345, DrainPercentPerHour: 3.861}, now)
	accounts := []quotaHistoryAccount{{AuthID: authID}, {AuthID: "gone"}}
	addQuotaHistoryRouting(accounts, []*coreauth.Auth{auth}, now)

	routing := accounts[0].Routing
	if routing == nil || routing.SessionLimited || routing.Deadline != now.Add(30*time.Hour).Truncate(time.Second).UnixMilli() {
		t.Fatalf("routing = %+v, want the exempt credential's reset as its deadline", routing)
	}
	if routing.SessionShare != 27.35 || routing.Drain != 3.86 {
		t.Fatalf("routing stats = %v, %v; want rounded 27.35, 3.86", routing.SessionShare, routing.Drain)
	}
	if accounts[1].Routing != nil {
		t.Fatalf("a credential no longer loaded got routing %+v", accounts[1].Routing)
	}
}
