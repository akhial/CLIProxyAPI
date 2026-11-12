package auth

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var weeklyResetTestBase = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

type weeklyResetTestClock struct{ now time.Time }

func (c *weeklyResetTestClock) Now() time.Time          { return c.now }
func (c *weeklyResetTestClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// newWeeklyResetTestBoost returns a boost with isolated hint registries and a manual clock.
func newWeeklyResetTestBoost() (*weeklyResetBoost, *weeklyResetTestClock) {
	clock := &weeklyResetTestClock{now: weeklyResetTestBase}
	return &weeklyResetBoost{
		now:          clock.Now,
		weeklyHints:  &quotaHintRegistry[WeeklyQuota]{},
		sessionHints: &quotaHintRegistry[SessionQuota]{},
		stats:        &quotaHintRegistry[RoutingStats]{},
	}, clock
}

// claudeExemptQuota is a passive Claude snapshot of a plan without the 5-hour limit.
func claudeExemptQuota(resetAt, observedAt time.Time) QuotaState {
	return claudeQuota(resetAt, observedAt, 0.4, -1)
}

// claudeWeeklyQuota is a passive Claude snapshot of a session-limited plan whose weekly
// window resets at resetAt, with nothing used.
func claudeWeeklyQuota(resetAt, observedAt time.Time) QuotaState {
	return claudeQuota(resetAt, observedAt, 0, 0)
}

// claudeQuota is a passive Claude snapshot with weekly and 5-hour utilization fractions;
// a negative fiveHour leaves the 5-hour headers out (an exempt plan). The 5-hour window
// ends an hour after observedAt.
func claudeQuota(resetAt, observedAt time.Time, weekly, fiveHour float64) QuotaState {
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(weekly, 'f', -1, 64),
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(resetAt.Unix(), 10),
	}
	if fiveHour >= 0 {
		signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = strconv.FormatFloat(fiveHour, 'f', -1, 64)
		signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(observedAt.Add(time.Hour).Unix(), 10)
	}
	return QuotaState{ObservedAt: observedAt, Signals: signals}
}

func weeklyResetTestAuth(id, provider, weight string, quota QuotaState) *Auth {
	return &Auth{ID: id, Provider: provider, Attributes: map[string]string{AttributeWeight: weight}, Quota: quota}
}

// rankWeights runs the ranking over auths with their configured weights.
func rankWeights(boost *weeklyResetBoost, auths ...*Auth) map[string]int64 {
	weights := make(map[string]int64, len(auths))
	for _, auth := range auths {
		weights[auth.ID] = authWeight(auth)
	}
	boost.rankAuthWeights(weights, auths, "")
	return weights
}

// assertWeights fails unless got holds exactly the want weights.
func assertWeights(t *testing.T, got, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("weights = %#v, want %#v", got, want)
	}
	for authID, weight := range want {
		if got[authID] != weight {
			t.Fatalf("weights = %#v, want %#v", got, want)
		}
	}
}

func countWeeklyResetPicks(t *testing.T, picks int, pick func() (*Auth, error)) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for index := 0; index < picks; index++ {
		got, errPick := pick()
		if errPick != nil {
			t.Fatalf("pick #%d error = %v", index, errPick)
		}
		counts[got.ID]++
	}
	return counts
}

func TestWeeklyResetRank_EarliestResetFirst(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	now := clock.Now()
	auths := []*Auth{
		weeklyResetTestAuth("in-35h", "claude", "1", claudeExemptQuota(now.Add(35*time.Hour), now)),
		weeklyResetTestAuth("in-12h", "claude", "1", claudeExemptQuota(now.Add(12*time.Hour), now)),
		weeklyResetTestAuth("in-5d", "claude", "1", claudeExemptQuota(now.Add(5*24*time.Hour), now)),
		weeklyResetTestAuth("unknown", "claude", "1", QuotaState{}),
		weeklyResetTestAuth("excluded", "claude", "0", claudeExemptQuota(now.Add(time.Hour), now)),
	}
	assertWeights(t, rankWeights(boost, auths...), map[string]int64{"in-12h": 1})
	// Without the soonest, the next soonest; a reset days away still beats an unknown one.
	assertWeights(t, rankWeights(boost, auths[0], auths[2], auths[3]), map[string]int64{"in-35h": 1})
	assertWeights(t, rankWeights(boost, auths[2], auths[3]), map[string]int64{"in-5d": 1})
	// Nothing known: configured weights apply.
	assertWeights(t, rankWeights(boost, auths[3], weeklyResetTestAuth("other", "claude", "3", QuotaState{})), map[string]int64{"unknown": 1, "other": 3})
}

func TestWeeklyResetRank_TiesShareByConfiguredWeight(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	selector := &WeightedRoundRobinSelector{resetBoost: boost}
	resetAt := clock.Now().Add(20 * time.Hour)
	auths := []*Auth{
		weeklyResetTestAuth("a", "claude", "1", claudeExemptQuota(resetAt, clock.Now())),
		weeklyResetTestAuth("b", "claude", "3", claudeExemptQuota(resetAt, clock.Now())),
		weeklyResetTestAuth("c", "claude", "1", claudeExemptQuota(resetAt.Add(time.Hour), clock.Now())),
	}
	counts := countWeeklyResetPicks(t, 100, func() (*Auth, error) {
		return selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
	})
	if counts["a"] != 25 || counts["b"] != 75 || counts["c"] != 0 {
		t.Fatalf("picks = %#v, want a:b=25:75 and c unused", counts)
	}
}

func TestWeeklyResetRank_SessionLimitedFirstWithinTieBand(t *testing.T) {
	t.Parallel()

	now := weeklyResetTestBase
	cases := []struct {
		name                string
		exemptIn, limitedIn time.Duration
		wantLimited         bool
	}{
		{name: "same reset", exemptIn: 26 * time.Hour, limitedIn: 26 * time.Hour, wantLimited: true},
		{name: "limited 50m later", exemptIn: 26 * time.Hour, limitedIn: 26*time.Hour + 50*time.Minute, wantLimited: true},
		{name: "limited exactly the band later", exemptIn: 26 * time.Hour, limitedIn: 27 * time.Hour, wantLimited: true},
		{name: "limited 70m later", exemptIn: 26 * time.Hour, limitedIn: 27*time.Hour + 10*time.Minute},
		{name: "limited sooner", exemptIn: 35 * time.Hour, limitedIn: 12 * time.Hour, wantLimited: true},
		{name: "limited much later", exemptIn: 12 * time.Hour, limitedIn: 35 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boost, _ := newWeeklyResetTestBoost()
			exempt := weeklyResetTestAuth("exempt", "claude", "1", claudeExemptQuota(now.Add(tc.exemptIn), now))
			limited := weeklyResetTestAuth("limited", "claude", "1", claudeWeeklyQuota(now.Add(tc.limitedIn), now))
			want := map[string]int64{"exempt": 1}
			if tc.wantLimited {
				want = map[string]int64{"limited": 1}
			}
			assertWeights(t, rankWeights(boost, exempt, limited), want)
		})
	}
}

// The user's example: a session-limited credential 34 h from its reset with 20 % left
// must not take the traffic from an exempt one resetting in an hour with 59 % left.
func TestWeeklyResetRank_ExemptAboutToResetBeatsRoomyLimited(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	now := clock.Now()
	limited := weeklyResetTestAuth("limited", "claude", "1", claudeQuota(now.Add(34*time.Hour), now, 0.8, 0.1))
	exempt := weeklyResetTestAuth("exempt", "claude", "1", claudeQuota(now.Add(time.Hour), now, 0.41, -1))
	boost.stats.set("limited", RoutingStats{SessionSharePercent: 28, DrainPercentPerHour: 3.8}, now)
	assertWeights(t, rankWeights(boost, limited, exempt), map[string]int64{"exempt": 1})
}

// Under heavy traffic a session-limited credential has to start before an exempt one
// resetting earlier, or its 5-hour windows run out of time; under light traffic the
// windows never bind and the earlier reset goes first.
func TestWeeklyResetRank_CapDelayFollowsTraffic(t *testing.T) {
	t.Parallel()

	now := weeklyResetTestBase
	exempt := weeklyResetTestAuth("exempt", "claude", "1", claudeQuota(now.Add(20*time.Hour), now, 0, -1))
	limited := weeklyResetTestAuth("limited", "claude", "1", claudeQuota(now.Add(22*time.Hour), now, 0, 0))
	cases := []struct {
		name  string
		drain float64
		want  string
	}{
		{name: "heavy traffic", drain: 10, want: "limited"},
		{name: "light traffic", drain: 1, want: "exempt"},
		{name: "drain unknown", drain: 0, want: "exempt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boost, _ := newWeeklyResetTestBoost()
			boost.stats.set("limited", RoutingStats{SessionSharePercent: 28, DrainPercentPerHour: tc.drain}, now)
			assertWeights(t, rankWeights(boost, exempt, limited), map[string]int64{tc.want: 1})
		})
	}

	// The cap delay at heavy traffic: windows of 28 % at 10 %/h end at 16.6 h instead of 10 h.
	boost, _ := newWeeklyResetTestBoost()
	boost.stats.set("limited", RoutingStats{SessionSharePercent: 28, DrainPercentPerHour: 10}, now)
	limitedFresh := weeklyResetTestAuth("limited", "claude", "1", QuotaState{})
	boost.weeklyHints.set("limited", WeeklyQuota{ResetAt: now.Add(22 * time.Hour), UsedPercent: 0}, now)
	boost.sessionHints.set("limited", SessionQuota{Limited: true, UsedPercent: -1}, now)
	want := now.Add(22*time.Hour - 6*time.Hour - 36*time.Minute)
	if got := boost.rank(limitedFresh, now).deadline; got.Sub(want).Abs() > time.Second {
		t.Fatalf("deadline = %v, want reset minus 6h36m", got.Sub(now))
	}
	// Half of the current window already used leaves 14 % in it, ending in an hour:
	// 10 % in the first hour, then 28 % from 1 h, 6 h, 11 h and the last 6 % at 16 h.
	boost.sessionHints.set("limited", SessionQuota{Limited: true, UsedPercent: 50, ResetAt: now.Add(time.Hour)}, now.Add(time.Second))
	if got := boost.rank(limitedFresh, now).deadline; got.Sub(want).Abs() > time.Second {
		t.Fatalf("deadline with a half-used window = %v, want reset minus 6h36m", got.Sub(now))
	}
}

func TestHoursToSpend(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                          string
		left, drain, share, room, end float64
		want                          float64
	}{
		{name: "fits in the current window", left: 20, drain: 10, share: 28, room: 28, end: 5, want: 2},
		{name: "drain binds, not the windows", left: 50, drain: 2, share: 28, room: 28, end: 5, want: 25},
		{name: "four windows", left: 100, drain: 10, share: 28, room: 28, end: 5, want: 16.6},
		{name: "spent current window", left: 10, drain: 10, share: 28, room: 0, end: 2, want: 3},
		{name: "unlimited drain", left: 100, drain: math.Inf(1), share: 28, room: 28, end: 5, want: 15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hoursToSpend(tc.left, tc.drain, tc.share, tc.room, tc.end); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("hoursToSpend() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWeeklyResetRank_PassedResetMovesToNextWeek(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	now := clock.Now()
	// Observed before its reset an hour ago: the next window ends 7 days after it.
	stale := weeklyResetTestAuth("stale", "claude", "1", claudeQuota(now.Add(-time.Hour), now.Add(-2*time.Hour), 0.9, -1))
	other := weeklyResetTestAuth("other", "claude", "1", claudeExemptQuota(now.Add(3*24*time.Hour), now))
	if got := boost.rank(stale, now).deadline; !got.Equal(now.Add(-time.Hour + weeklyQuotaPeriod)) {
		t.Fatalf("stale deadline = %v, want a week after the passed reset", got.Sub(now))
	}
	assertWeights(t, rankWeights(boost, stale, other), map[string]int64{"other": 1})
}

func TestWeeklyResetRank_PrefersMostRecentObservation(t *testing.T) {
	t.Parallel()

	now := weeklyResetTestBase
	soon := now.Add(10 * time.Hour)
	later := now.Add(6 * 24 * time.Hour)
	reference := func() *Auth {
		return weeklyResetTestAuth("ref", "claude", "1", claudeExemptQuota(now.Add(3*24*time.Hour), now))
	}
	type hint struct{ resetAt, observedAt time.Time }
	cases := []struct {
		name    string
		quota   QuotaState
		hint    *hint
		wantRef bool
	}{
		{name: "newer later hint overrides header", quota: claudeExemptQuota(soon, now.Add(-2*time.Hour)), hint: &hint{later, now.Add(-time.Hour)}, wantRef: true},
		{name: "older hint ignored", quota: claudeExemptQuota(soon, now.Add(-2*time.Hour)), hint: &hint{later, now.Add(-3 * time.Hour)}},
		{name: "newer sooner hint wins", quota: claudeExemptQuota(later, now.Add(-2*time.Hour)), hint: &hint{soon, now.Add(-time.Hour)}},
		{name: "hint alone ranks an idle credential", hint: &hint{soon, now.Add(-time.Hour)}},
		{name: "header without weekly window falls back to hint", quota: QuotaState{ObservedAt: now, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": "1"}}, hint: &hint{soon, now.Add(-time.Hour)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boost, _ := newWeeklyResetTestBoost()
			auth := weeklyResetTestAuth("a", "claude", "1", tc.quota)
			if tc.hint != nil {
				boost.weeklyHints.set(auth.ID, WeeklyQuota{ResetAt: tc.hint.resetAt, UsedPercent: -1}, tc.hint.observedAt)
			}
			want := map[string]int64{"a": 1}
			if tc.wantRef {
				want = map[string]int64{"ref": 1}
			}
			assertWeights(t, rankWeights(boost, auth, reference()), want)
		})
	}
}

func TestWeeklyResetRank_SessionLimitPrefersMostRecentObservation(t *testing.T) {
	t.Parallel()

	now := weeklyResetTestBase
	resetAt := now.Add(26 * time.Hour)
	other := func() *Auth { return weeklyResetTestAuth("other", "claude", "1", claudeExemptQuota(resetAt, now)) }

	// A newer usage read that still lists the 5-hour window beats exempt headers.
	boost, _ := newWeeklyResetTestBoost()
	auth := weeklyResetTestAuth("a", "claude", "1", claudeExemptQuota(resetAt, now.Add(-time.Hour)))
	boost.sessionHints.set("a", SessionQuota{Limited: true, UsedPercent: -1}, now)
	assertWeights(t, rankWeights(boost, auth, other()), map[string]int64{"a": 1})

	// An older hint loses to the headers, and so does a hint at the same instant.
	boost, _ = newWeeklyResetTestBoost()
	boost.sessionHints.set("a", SessionQuota{Limited: true, UsedPercent: -1}, now.Add(-2*time.Hour))
	assertWeights(t, rankWeights(boost, auth, other()), map[string]int64{"a": 1, "other": 1})

	// A hint alone marks an idle credential as exempt; unknown counts as limited.
	boost, _ = newWeeklyResetTestBoost()
	idle := weeklyResetTestAuth("idle", "claude", "1", QuotaState{})
	boost.weeklyHints.set("idle", WeeklyQuota{ResetAt: resetAt, UsedPercent: -1}, now)
	boost.sessionHints.set("idle", SessionQuota{UsedPercent: -1}, now)
	unknown := weeklyResetTestAuth("unknown", "codex", "1", QuotaState{})
	boost.weeklyHints.set("unknown", WeeklyQuota{ResetAt: resetAt.Add(30 * time.Minute), UsedPercent: -1}, now)
	assertWeights(t, rankWeights(boost, idle, unknown), map[string]int64{"unknown": 1})
}

func TestWeeklyQuotaFromSignals(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	observedAt := weeklyResetTestBase
	cases := []struct {
		name    string
		signals map[string]string
		want    WeeklyQuota
		ok      bool
	}{
		{name: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(resetAt.Unix(), 10), "Anthropic-Ratelimit-Unified-7d-Utilization": "0.25"}, want: WeeklyQuota{ResetAt: resetAt, UsedPercent: 25}, ok: true},
		{name: "claude without utilization", signals: map[string]string{"anthropic-ratelimit-unified-7d-reset": strconv.FormatInt(resetAt.UnixMilli(), 10)}, want: WeeklyQuota{ResetAt: resetAt, UsedPercent: -1}, ok: true},
		{name: "claude rfc3339", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": resetAt.Format(time.RFC3339)}, want: WeeklyQuota{ResetAt: resetAt, UsedPercent: -1}, ok: true},
		{name: "codex secondary weekly", signals: map[string]string{
			"X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Used-Percent": "50",
			"X-Codex-Secondary-Window-Minutes": "10080", "X-Codex-Secondary-Used-Percent": "30", "X-Codex-Secondary-Reset-After-Seconds": "3600",
		}, want: WeeklyQuota{ResetAt: observedAt.Add(time.Hour), UsedPercent: 30}, ok: true},
		{name: "codex image limit", signals: map[string]string{"X-Codex-Active-Limit": "imagegen_premium", "X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-At": "1793214612"}},
		{name: "five hour only", signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": "1"}},
		{name: "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := weeklyQuotaFromSignals(tc.signals, observedAt)
			if ok != tc.ok || !got.ResetAt.Equal(tc.want.ResetAt) || (tc.ok && got.UsedPercent != tc.want.UsedPercent) {
				t.Fatalf("weeklyQuotaFromSignals() = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSessionQuotaFromSignals(t *testing.T) {
	t.Parallel()

	observedAt := weeklyResetTestBase
	fiveReset := observedAt.Add(2 * time.Hour)
	cases := []struct {
		name    string
		signals map[string]string
		want    SessionQuota
		ok      bool
	}{
		{name: "claude with 5h", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": "1", "Anthropic-Ratelimit-Unified-5h-Utilization": "0.2", "Anthropic-Ratelimit-Unified-5h-Reset": strconv.FormatInt(fiveReset.Unix(), 10)}, want: SessionQuota{Limited: true, UsedPercent: 20, ResetAt: fiveReset}, ok: true},
		{name: "claude exempt", signals: map[string]string{"anthropic-ratelimit-unified-7d-utilization": "0.2"}, want: SessionQuota{UsedPercent: -1}, ok: true},
		{name: "claude 5h only", signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": "1"}, want: SessionQuota{UsedPercent: -1}},
		{name: "codex 5h primary", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Used-Percent": "12", "X-Codex-Primary-Reset-After-Seconds": "7200", "X-Codex-Secondary-Window-Minutes": "10080"}, want: SessionQuota{Limited: true, UsedPercent: 12, ResetAt: fiveReset}, ok: true},
		{name: "codex weekly only", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "10080"}, want: SessionQuota{UsedPercent: -1}, ok: true},
		{name: "codex image limit", signals: map[string]string{"X-Codex-Active-Limit": "imagegen_x", "X-Codex-Primary-Window-Minutes": "300"}, want: SessionQuota{UsedPercent: -1}},
		{name: "empty", want: SessionQuota{UsedPercent: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sessionQuotaFromSignals(tc.signals, observedAt)
			if ok != tc.ok || got.Limited != tc.want.Limited || got.UsedPercent != tc.want.UsedPercent || !got.ResetAt.Equal(tc.want.ResetAt) {
				t.Fatalf("sessionQuotaFromSignals() = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
			if limited, okLimited := sessionLimitFromSignals(tc.signals); limited != tc.want.Limited || okLimited != tc.ok {
				t.Fatalf("sessionLimitFromSignals() = %v, %v", limited, okLimited)
			}
		})
	}
}

func TestWeeklyResetFromSignals_Codex(t *testing.T) {
	t.Parallel()

	observedAt := weeklyResetTestBase
	weeklyReset := observedAt.Add(30 * time.Hour)
	cases := []struct {
		name       string
		signals    map[string]string
		observedAt time.Time
		want       time.Time
		ok         bool
	}{
		{
			name: "secondary weekly reset-at",
			signals: map[string]string{
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       strconv.FormatInt(weeklyReset.Unix(), 10),
			},
			observedAt: observedAt, want: weeklyReset, ok: true,
		},
		{
			name: "relative reset without observation time",
			signals: map[string]string{
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "108000",
			},
		},
		{
			name: "monthly window ignored",
			signals: map[string]string{
				"X-Codex-Secondary-Window-Minutes": "43200",
				"X-Codex-Secondary-Reset-At":       strconv.FormatInt(weeklyReset.Unix(), 10),
			},
			observedAt: observedAt,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := weeklyResetFromSignals(tc.signals, tc.observedAt)
			if ok != tc.ok || !got.Equal(tc.want) {
				t.Fatalf("weeklyResetFromSignals() = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestQuotaHintRegistry_NewerObservationWins(t *testing.T) {
	t.Parallel()

	registry := &quotaHintRegistry[WeeklyQuota]{}
	t0 := weeklyResetTestBase
	registry.set("a", WeeklyQuota{ResetAt: t0.Add(10 * time.Hour)}, t0)
	registry.set("a", WeeklyQuota{ResetAt: t0.Add(20 * time.Hour)}, t0.Add(-time.Minute))
	if got, _, _ := registry.get("a"); !got.ResetAt.Equal(t0.Add(10 * time.Hour)) {
		t.Fatalf("older observation replaced hint: %+v", got)
	}
	registry.set("a", WeeklyQuota{ResetAt: t0.Add(30 * time.Hour)}, t0.Add(time.Minute))
	if got, observedAt, ok := registry.get("a"); !ok || !got.ResetAt.Equal(t0.Add(30*time.Hour)) || !observedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("newer observation = %+v, %v, %v", got, observedAt, ok)
	}
	registry.set("", WeeklyQuota{ResetAt: t0}, t0)
	if _, _, okEmpty := registry.get(""); okEmpty {
		t.Fatal("registry stored a hint without auth ID")
	}

	authID := "weekly-quota-public-" + t.Name()
	SetWeeklyQuotaHint(authID+"-zero", WeeklyQuota{}, t0)
	if _, _, ok := WeeklyQuotaHint(authID + "-zero"); ok {
		t.Fatal("stored a weekly hint without reset time")
	}
	SetWeeklyQuotaHint(authID, WeeklyQuota{ResetAt: t0.Add(5 * time.Hour), UsedPercent: 40}, t0)
	if got, observedAt, ok := WeeklyQuotaHint(authID); !ok || got.UsedPercent != 40 || !observedAt.Equal(t0) {
		t.Fatalf("WeeklyQuotaHint() = %+v, %v, %v", got, observedAt, ok)
	}
	SetSessionQuotaHint(authID, SessionQuota{Limited: true, UsedPercent: 10}, t0)
	if got, _, ok := SessionQuotaHint(authID); !ok || !got.Limited {
		t.Fatalf("SessionQuotaHint() = %+v, %v", got, ok)
	}
	SetRoutingStats(authID, RoutingStats{SessionSharePercent: 28, DrainPercentPerHour: 4}, t0)
	if got, _, ok := RoutingStatsFor(authID); !ok || got.SessionSharePercent != 28 {
		t.Fatalf("RoutingStatsFor() = %+v, %v", got, ok)
	}
}

func TestQuotaHintRegistry_EvictsOldestWhenFull(t *testing.T) {
	t.Parallel()

	registry := &quotaHintRegistry[WeeklyQuota]{}
	for index := 0; index < maxWeeklyResetHints; index++ {
		registry.set(fmt.Sprintf("auth-%d", index), WeeklyQuota{ResetAt: weeklyResetTestBase}, weeklyResetTestBase.Add(time.Duration(index)*time.Second))
	}
	registry.set("auth-new", WeeklyQuota{ResetAt: weeklyResetTestBase}, weeklyResetTestBase.Add(24*time.Hour))
	if len(registry.hints) != maxWeeklyResetHints {
		t.Fatalf("hint count = %d, want %d", len(registry.hints), maxWeeklyResetHints)
	}
	if _, _, ok := registry.get("auth-0"); ok {
		t.Fatal("oldest hint was not evicted")
	}
}

func TestWeeklyResetRouting(t *testing.T) {
	now := time.Now()
	authID := "weekly-reset-routing-" + t.Name()
	auth := weeklyResetTestAuth(authID, "claude", "1", claudeQuota(now.Add(22*time.Hour), now, 0, 0))
	SetRoutingStats(authID, RoutingStats{SessionSharePercent: 28, DrainPercentPerHour: 10}, now)
	deadline, limited := WeeklyResetRouting(auth, now)
	if !limited || !deadline.Before(now.Add(22*time.Hour)) {
		t.Fatalf("WeeklyResetRouting() = %v, %v; want limited with a deadline before the reset", deadline.Sub(now), limited)
	}
	if deadline, _ := WeeklyResetRouting(weeklyResetTestAuth("x", "claude", "1", QuotaState{}), now); !deadline.IsZero() {
		t.Fatalf("unknown reset gave deadline %v", deadline)
	}
}

func TestWeeklyResetWeightedRoundRobinSelectorPick_FallsBackWhenChosenAuthUnavailable(t *testing.T) {
	t.Parallel()

	model := "claude-model"
	boost, clock := newWeeklyResetTestBoost()
	selector := &WeightedRoundRobinSelector{resetBoost: boost}
	first := weeklyResetTestAuth("a", "claude", "1", claudeWeeklyQuota(clock.Now().Add(10*time.Hour), clock.Now()))
	recoverAt := time.Now().Add(time.Hour)
	first.ModelStates = map[string]*ModelState{model: {
		Unavailable:    true,
		NextRetryAfter: recoverAt,
		Quota:          QuotaState{Exceeded: true, NextRecoverAt: recoverAt},
	}}
	second := weeklyResetTestAuth("b", "claude", "1", claudeExemptQuota(clock.Now().Add(30*time.Hour), clock.Now()))
	auths := []*Auth{first, second, weeklyResetTestAuth("c", "claude", "1", claudeExemptQuota(clock.Now().Add(60*time.Hour), clock.Now()))}
	pick := func() (*Auth, error) {
		return selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
	}

	if counts := countWeeklyResetPicks(t, 20, pick); counts["b"] != 20 {
		t.Fatalf("picks with a cooling down = %#v, want all on b", counts)
	}
	first.ModelStates = nil
	if counts := countWeeklyResetPicks(t, 20, pick); counts["a"] != 20 {
		t.Fatalf("picks after a recovered = %#v, want all on a", counts)
	}
}

func TestWeeklyResetWeightedRoundRobinSelectorPick_FollowsClock(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	selector := &WeightedRoundRobinSelector{resetBoost: boost}
	auths := []*Auth{
		weeklyResetTestAuth("a", "claude", "1", claudeExemptQuota(clock.Now().Add(10*time.Hour), clock.Now())),
		weeklyResetTestAuth("b", "claude", "1", claudeExemptQuota(clock.Now().Add(40*time.Hour), clock.Now())),
	}
	pick := func() (*Auth, error) {
		return selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
	}
	if counts := countWeeklyResetPicks(t, 10, pick); counts["a"] != 10 {
		t.Fatalf("picks before a's reset = %#v, want all on a", counts)
	}
	// After a resets its next window ends a week later, so b goes first.
	clock.Advance(10*time.Hour + time.Minute)
	if counts := countWeeklyResetPicks(t, 10, pick); counts["b"] != 10 {
		t.Fatalf("picks after a's reset = %#v, want all on b", counts)
	}
}

func TestWeightedRoundRobinSelectorPick_IgnoresWeeklyResetWithoutBoost(t *testing.T) {
	t.Parallel()

	selector := &WeightedRoundRobinSelector{}
	now := time.Now()
	auths := []*Auth{
		weeklyResetTestAuth("a", "claude", "1", claudeWeeklyQuota(now.Add(time.Hour), now)),
		weeklyResetTestAuth("b", "claude", "1", QuotaState{}),
	}
	counts := countWeeklyResetPicks(t, 20, func() (*Auth, error) {
		return selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
	})
	if counts["a"] != 10 || counts["b"] != 10 {
		t.Fatalf("plain weighted picks = %#v, want a:b=10:10", counts)
	}
}

func TestSchedulerPick_WeeklyResetRanking(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	authA := weeklyResetTestAuth("a", "claude", "1", claudeExemptQuota(clock.Now().Add(10*time.Hour), clock.Now()))
	authB := weeklyResetTestAuth("b", "claude", "1", QuotaState{})
	authC := weeklyResetTestAuth("c", "claude", "0", QuotaState{})
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{resetBoost: boost}, authA, authB, authC)
	boost.weeklyHints.set("c", WeeklyQuota{ResetAt: clock.Now().Add(time.Hour), UsedPercent: -1}, clock.Now())
	pick := func() (*Auth, error) {
		return scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
	}
	if counts := countWeeklyResetPicks(t, 50, pick); counts["a"] != 50 {
		t.Fatalf("scheduler picks = %#v, want all on a", counts)
	}

	// After a's reset, an idle credential ranked by a poller hint alone goes first.
	clock.Advance(10*time.Hour + time.Minute)
	boost.weeklyHints.set("b", WeeklyQuota{ResetAt: clock.Now().Add(20 * time.Hour), UsedPercent: -1}, clock.Now())
	if counts := countWeeklyResetPicks(t, 50, pick); counts["b"] != 50 {
		t.Fatalf("scheduler picks after a's reset = %#v, want all on hinted b", counts)
	}
}

func TestSchedulerPick_WeeklyResetRankingFallsBackOnCooldown(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	later := weeklyResetTestAuth("later", "claude", "1", claudeExemptQuota(clock.Now().Add(30*time.Hour), clock.Now()))
	sooner := weeklyResetTestAuth("sooner", "claude", "1", QuotaState{})
	other := weeklyResetTestAuth("other", "claude", "1", QuotaState{})
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{resetBoost: boost}, later, sooner, other)
	boost.weeklyHints.set("sooner", WeeklyQuota{ResetAt: clock.Now().Add(12 * time.Hour), UsedPercent: -1}, clock.Now())
	pick := func() (*Auth, error) {
		return scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
	}
	if counts := countWeeklyResetPicks(t, 50, pick); counts["sooner"] != 50 {
		t.Fatalf("scheduler picks = %#v, want all on sooner", counts)
	}

	cooling := sooner.Clone()
	cooling.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Hour)}
	cooling.Generation++
	scheduler.upsertAuth(cooling)
	if counts := countWeeklyResetPicks(t, 50, pick); counts["later"] != 50 {
		t.Fatalf("scheduler picks with sooner cooling = %#v, want all on later", counts)
	}
}

func TestSchedulerPick_WeeklyResetRankingMixedProviders(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	scheduler := newSchedulerForTest(
		&WeightedRoundRobinSelector{resetBoost: boost},
		weeklyResetTestAuth("claude-a", "claude", "1", QuotaState{}),
		weeklyResetTestAuth("codex-b", "codex", "3", QuotaState{}),
	)
	boost.weeklyHints.set("claude-a", WeeklyQuota{ResetAt: clock.Now().Add(12 * time.Hour), UsedPercent: -1}, clock.Now())
	counts := countWeeklyResetPicks(t, 50, func() (*Auth, error) {
		got, _, errPick := scheduler.pickMixed(context.Background(), []string{"claude", "codex"}, "", cliproxyexecutor.Options{}, nil)
		return got, errPick
	})
	if counts["claude-a"] != 50 {
		t.Fatalf("mixed scheduler picks = %#v, want all on claude-a", counts)
	}
}

func TestSchedulerPick_WeeklyResetRankingReadsLatestAuthSnapshot(t *testing.T) {
	t.Parallel()

	authA := weeklyResetTestAuth("weekly-latest-a", "claude", "1", QuotaState{})
	authB := weeklyResetTestAuth("weekly-latest-b", "claude", "1", QuotaState{})
	reg := registry.GetGlobalRegistry()
	for _, auth := range []*Auth{authA, authB} {
		reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "weekly-latest-m1"}, {ID: "weekly-latest-m2"}})
	}
	t.Cleanup(func() {
		reg.UnregisterClient(authA.ID)
		reg.UnregisterClient(authB.ID)
	})
	boost, clock := newWeeklyResetTestBoost()
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{resetBoost: boost}, authA, authB)
	pick := func(model string) func() (*Auth, error) {
		return func() (*Auth, error) {
			return scheduler.pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
		}
	}
	countWeeklyResetPicks(t, 2, pick("weekly-latest-m1"))
	countWeeklyResetPicks(t, 2, pick("weekly-latest-m2"))

	// A result for m1 refreshes only the m1 shard; the m2 shard keeps the older snapshot.
	observed := authA.Clone()
	observed.Quota = claudeWeeklyQuota(clock.Now().Add(10*time.Hour), clock.Now())
	observed.Generation++
	observed.UpdatedAt = time.Now()
	scheduler.upsertAuthResult(observed, []string{"weekly-latest-m1"}, false)

	if counts := countWeeklyResetPicks(t, 20, pick("weekly-latest-m2")); counts[authA.ID] != 20 {
		t.Fatalf("m2 picks = %#v, want all on %s from its latest snapshot", counts, authA.ID)
	}
}

func TestManagerWeeklyResetSelectorConfiguresScheduler(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, NewWeeklyResetWeightedRoundRobinSelector(), nil)
	if manager.scheduler == nil || manager.scheduler.resetBoost == nil {
		t.Fatal("scheduler has no weekly-reset ranking for the weekly-reset selector")
	}
	if manager.scheduler.strategy != schedulerStrategyWeightedRoundRobin || !manager.useSchedulerFastPath() {
		t.Fatalf("scheduler strategy = %v, fast path = %v; want weighted fast path", manager.scheduler.strategy, manager.useSchedulerFastPath())
	}
	manager.SetSelector(&WeightedRoundRobinSelector{})
	if manager.scheduler.resetBoost != nil {
		t.Fatal("plain weighted selector kept the weekly-reset ranking")
	}
	manager.SetSelector(NewWeeklyResetWeightedRoundRobinSelector())
	if manager.scheduler.resetBoost == nil {
		t.Fatal("switching back did not restore the weekly-reset ranking")
	}
}

func TestSessionAffinitySelector_WeeklyResetRankingAppliesToNewSessions(t *testing.T) {
	t.Parallel()

	model := "claude-model"
	boost, clock := newWeeklyResetTestBoost()
	selector := NewSessionAffinitySelector(&WeightedRoundRobinSelector{resetBoost: boost})
	defer selector.Stop()
	authA := weeklyResetTestAuth("auth-a", "claude", "1", QuotaState{})
	authB := weeklyResetTestAuth("auth-b", "claude", "1", QuotaState{})
	authC := weeklyResetTestAuth("auth-c", "claude", "1", QuotaState{})
	auths := []*Auth{authA, authB, authC}
	pickSession := func(session string) *Auth {
		t.Helper()
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(fmt.Sprintf(`{"session_id":%q}`, session))}
		picked, errPick := selector.Pick(context.Background(), "claude", model, opts, auths)
		if errPick != nil {
			t.Fatalf("Pick(%s) error = %v", session, errPick)
		}
		return picked
	}

	// With no quota known, sessions spread evenly; remember one bound to b.
	stickySession := ""
	for index := 0; index < 3; index++ {
		session := fmt.Sprintf("before-%d", index)
		if pickSession(session).ID == authB.ID {
			stickySession = session
		}
	}
	if stickySession == "" {
		t.Fatal("no session bound to auth-b before the ranking applied")
	}

	authA.Quota = claudeExemptQuota(clock.Now().Add(10*time.Hour), clock.Now())
	for index := 0; index < 20; index++ {
		if got := pickSession(fmt.Sprintf("ranked-%d", index)); got.ID != authA.ID {
			t.Fatalf("new session %d picked %s, want %s", index, got.ID, authA.ID)
		}
	}
	if got := pickSession(stickySession); got.ID != authB.ID {
		t.Fatalf("existing session moved to %s, want sticky %s", got.ID, authB.ID)
	}

	recoverAt := time.Now().Add(time.Hour)
	authA.ModelStates = map[string]*ModelState{model: {
		Unavailable:    true,
		NextRetryAfter: recoverAt,
		Quota:          QuotaState{Exceeded: true, NextRecoverAt: recoverAt},
	}}
	if got := pickSession("ranked-0"); got.ID == authA.ID {
		t.Fatalf("session bound to cooling-down %s was not failed over", authA.ID)
	}
}
