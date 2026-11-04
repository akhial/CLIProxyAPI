package auth

import (
	"context"
	"fmt"
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
	return &weeklyResetBoost{now: clock.Now, hints: &weeklyResetHintRegistry{}, sessionHints: &sessionLimitHintRegistry{}}, clock
}

// boostedWeight is the weight the boost gives auth when it is the only candidate.
func boostedWeight(boost *weeklyResetBoost, auth *Auth, configured int64) int64 {
	weights := map[string]int64{auth.ID: configured}
	boost.boostAuthWeights(weights, []*Auth{auth})
	return weights[auth.ID]
}

// claudeExemptQuota is a passive Claude snapshot of a plan without the 5-hour limit.
func claudeExemptQuota(resetAt, observedAt time.Time) QuotaState {
	return QuotaState{
		ObservedAt: observedAt,
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.4",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(resetAt.Unix(), 10),
		},
	}
}

// claudeWeeklyQuota is a passive Claude snapshot whose weekly window resets at resetAt.
func claudeWeeklyQuota(resetAt, observedAt time.Time) QuotaState {
	return QuotaState{
		ObservedAt: observedAt,
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Reset": strconv.FormatInt(observedAt.Add(time.Hour).Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(resetAt.Unix(), 10),
		},
	}
}

func weeklyResetTestAuth(id, provider, weight string, quota QuotaState) *Auth {
	return &Auth{ID: id, Provider: provider, Attributes: map[string]string{AttributeWeight: weight}, Quota: quota}
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

func TestWeeklyResetBoostEffectiveWeight_Window(t *testing.T) {
	t.Parallel()

	observedAt := weeklyResetTestBase.Add(-time.Hour)
	cases := []struct {
		name    string
		weight  string
		resetIn *time.Duration
		want    int64
	}{
		{name: "35h59m boosted", weight: "3", resetIn: durationPtr(35*time.Hour + 59*time.Minute), want: WeeklyResetBoostWeight},
		{name: "one second boosted", weight: "1", resetIn: durationPtr(time.Second), want: WeeklyResetBoostWeight},
		{name: "exactly 36h not boosted", weight: "3", resetIn: durationPtr(WeeklyResetBoostWindow), want: 3},
		{name: "36h01m not boosted", weight: "3", resetIn: durationPtr(36*time.Hour + time.Minute), want: 3},
		{name: "reset now not boosted", weight: "3", resetIn: durationPtr(0), want: 3},
		{name: "reset passed not boosted", weight: "3", resetIn: durationPtr(-time.Minute), want: 3},
		{name: "unknown reset not boosted", weight: "3", want: 3},
		{name: "zero weight never boosted", weight: "0", resetIn: durationPtr(time.Hour), want: 0},
		{name: "max weight unchanged", weight: "1000000", resetIn: durationPtr(time.Hour), want: WeeklyResetBoostWeight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boost, clock := newWeeklyResetTestBoost()
			auth := weeklyResetTestAuth("a", "claude", tc.weight, QuotaState{})
			if tc.resetIn != nil {
				auth.Quota = claudeWeeklyQuota(clock.Now().Add(*tc.resetIn), observedAt)
			}
			if got := boostedWeight(boost, auth, authWeight(auth)); got != tc.want {
				t.Fatalf("boosted weight = %d, want %d", got, tc.want)
			}
		})
	}

	var noBoost *weeklyResetBoost
	auth := weeklyResetTestAuth("a", "claude", "3", claudeWeeklyQuota(weeklyResetTestBase.Add(time.Hour), observedAt))
	if got := boostedWeight(noBoost, auth, 3); got != 3 {
		t.Fatalf("nil boost weight = %d, want configured 3", got)
	}
}

func durationPtr(d time.Duration) *time.Duration { return &d }

func TestWeeklyResetFromSignals_Claude(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		signals map[string]string
		want    time.Time
		ok      bool
	}{
		{name: "unix seconds", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(resetAt.Unix(), 10)}, want: resetAt, ok: true},
		{name: "lowercase key", signals: map[string]string{"anthropic-ratelimit-unified-7d-reset": strconv.FormatInt(resetAt.Unix(), 10)}, want: resetAt, ok: true},
		{name: "unix milliseconds", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(resetAt.UnixMilli(), 10)}, want: resetAt, ok: true},
		{name: "rfc3339", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": resetAt.Format(time.RFC3339)}, want: resetAt, ok: true},
		{name: "five hour only", signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": strconv.FormatInt(resetAt.Unix(), 10)}},
		{name: "invalid value", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": "soon"}},
		{name: "empty", signals: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := weeklyResetFromSignals(tc.signals, weeklyResetTestBase)
			if ok != tc.ok || !got.Equal(tc.want) {
				t.Fatalf("weeklyResetFromSignals() = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWeeklyResetFromSignals_Codex(t *testing.T) {
	t.Parallel()

	observedAt := weeklyResetTestBase
	weeklyReset := observedAt.Add(30 * time.Hour)
	fiveHourReset := observedAt.Add(2 * time.Hour)
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
				"X-Codex-Primary-Reset-At":         strconv.FormatInt(fiveHourReset.Unix(), 10),
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       strconv.FormatInt(weeklyReset.Unix(), 10),
			},
			observedAt: observedAt,
			want:       weeklyReset,
			ok:         true,
		},
		{
			name: "secondary weekly reset-after-seconds",
			signals: map[string]string{
				"X-Codex-Primary-Window-Minutes":        "300",
				"X-Codex-Primary-Reset-After-Seconds":   "7200",
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "108000",
			},
			observedAt: observedAt,
			want:       weeklyReset,
			ok:         true,
		},
		{
			name: "primary weekly window",
			signals: map[string]string{
				"X-Codex-Primary-Window-Minutes": "10080",
				"X-Codex-Primary-Reset-At":       strconv.FormatInt(weeklyReset.Unix(), 10),
			},
			observedAt: observedAt,
			want:       weeklyReset,
			ok:         true,
		},
		{
			name: "relative reset without observation time",
			signals: map[string]string{
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "108000",
			},
		},
		{
			name: "five hour window only",
			signals: map[string]string{
				"X-Codex-Primary-Window-Minutes": "300",
				"X-Codex-Primary-Reset-At":       strconv.FormatInt(fiveHourReset.Unix(), 10),
			},
			observedAt: observedAt,
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

func TestWeeklyResetHintRegistry_NewerObservationWins(t *testing.T) {
	t.Parallel()

	registry := &weeklyResetHintRegistry{}
	t0 := weeklyResetTestBase
	registry.set("a", t0.Add(10*time.Hour), t0)
	registry.set("a", t0.Add(20*time.Hour), t0.Add(-time.Minute))
	if resetAt, _, _ := registry.get("a"); !resetAt.Equal(t0.Add(10 * time.Hour)) {
		t.Fatalf("older observation replaced hint: resetAt = %v", resetAt)
	}
	registry.set("a", t0.Add(30*time.Hour), t0.Add(time.Minute))
	resetAt, observedAt, ok := registry.get("a")
	if !ok || !resetAt.Equal(t0.Add(30*time.Hour)) || !observedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("newer observation = %v, %v, %v; want reset %v observed %v", resetAt, observedAt, ok, t0.Add(30*time.Hour), t0.Add(time.Minute))
	}

	registry.set("", t0, t0)
	registry.set("b", time.Time{}, t0)
	if _, _, okEmpty := registry.get(""); okEmpty {
		t.Fatal("registry stored a hint without auth ID")
	}
	if _, _, okZero := registry.get("b"); okZero {
		t.Fatal("registry stored a hint without reset time")
	}

	authID := "weekly-reset-public-" + t.Name()
	SetWeeklyResetHint(authID, t0.Add(5*time.Hour), t0)
	if gotReset, gotObserved, okPublic := WeeklyResetHint(authID); !okPublic || !gotReset.Equal(t0.Add(5*time.Hour)) || !gotObserved.Equal(t0) {
		t.Fatalf("WeeklyResetHint() = %v, %v, %v", gotReset, gotObserved, okPublic)
	}
}

func TestWeeklyResetHintRegistry_EvictsOldestWhenFull(t *testing.T) {
	t.Parallel()

	registry := &weeklyResetHintRegistry{}
	for index := 0; index < maxWeeklyResetHints; index++ {
		registry.set(fmt.Sprintf("auth-%d", index), weeklyResetTestBase, weeklyResetTestBase.Add(time.Duration(index)*time.Second))
	}
	registry.set("auth-new", weeklyResetTestBase, weeklyResetTestBase.Add(time.Hour*24))
	if len(registry.hints) != maxWeeklyResetHints {
		t.Fatalf("hint count = %d, want %d", len(registry.hints), maxWeeklyResetHints)
	}
	if _, _, ok := registry.get("auth-0"); ok {
		t.Fatal("oldest hint was not evicted")
	}
	if _, _, ok := registry.get("auth-new"); !ok {
		t.Fatal("new hint was not stored")
	}
}

func TestWeeklyResetBoost_PrefersMostRecentObservation(t *testing.T) {
	t.Parallel()

	now := weeklyResetTestBase
	soon := now.Add(10 * time.Hour)
	later := now.Add(6 * 24 * time.Hour)
	type hint struct{ resetAt, observedAt time.Time }
	cases := []struct {
		name  string
		quota QuotaState
		hint  *hint
		want  int64
	}{
		{name: "newer hint without boost overrides header", quota: claudeWeeklyQuota(soon, now.Add(-2*time.Hour)), hint: &hint{later, now.Add(-time.Hour)}, want: 1},
		{name: "older hint ignored", quota: claudeWeeklyQuota(soon, now.Add(-2*time.Hour)), hint: &hint{later, now.Add(-3 * time.Hour)}, want: WeeklyResetBoostWeight},
		{name: "newer hint boosts", quota: claudeWeeklyQuota(later, now.Add(-2*time.Hour)), hint: &hint{soon, now.Add(-time.Hour)}, want: WeeklyResetBoostWeight},
		{name: "newer past header reset wins", quota: claudeWeeklyQuota(now.Add(-time.Minute), now.Add(-time.Minute)), hint: &hint{soon, now.Add(-3 * time.Hour)}, want: 1},
		{name: "hint alone boosts idle credential", hint: &hint{soon, now.Add(-time.Hour)}, want: WeeklyResetBoostWeight},
		{name: "header without weekly reset falls back to hint", quota: QuotaState{ObservedAt: now, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": "1"}}, hint: &hint{soon, now.Add(-time.Hour)}, want: WeeklyResetBoostWeight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boost, _ := newWeeklyResetTestBoost()
			auth := weeklyResetTestAuth("a", "claude", "1", tc.quota)
			if tc.hint != nil {
				boost.hints.set(auth.ID, tc.hint.resetAt, tc.hint.observedAt)
			}
			if got := boostedWeight(boost, auth, 1); got != tc.want {
				t.Fatalf("boosted weight = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWeeklyResetWeightedRoundRobinSelectorPick_BoostedAuthTakesAllPicks(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	selector := &WeightedRoundRobinSelector{resetBoost: boost}
	observedAt := clock.Now().Add(-time.Hour)
	auths := []*Auth{
		weeklyResetTestAuth("a", "claude", "1", claudeWeeklyQuota(clock.Now().Add(10*time.Hour), observedAt)),
		weeklyResetTestAuth("b", "claude", "5", claudeWeeklyQuota(clock.Now().Add(6*24*time.Hour), observedAt)),
		weeklyResetTestAuth("c", "claude", "1", QuotaState{}),
		weeklyResetTestAuth("excluded", "claude", "0", claudeWeeklyQuota(clock.Now().Add(time.Hour), observedAt)),
	}
	counts := countWeeklyResetPicks(t, 1000, func() (*Auth, error) {
		return selector.Pick(context.Background(), "claude", "claude-model", cliproxyexecutor.Options{}, auths)
	})
	if counts["a"] != 1000 {
		t.Fatalf("picks = %#v, want all 1000 on the boosted auth", counts)
	}
}

func TestWeeklyResetWeightedRoundRobinSelectorPick_MostUrgentAuthTakesAllPicks(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	selector := &WeightedRoundRobinSelector{resetBoost: boost}
	auths := []*Auth{
		weeklyResetTestAuth("a", "claude", "1", claudeWeeklyQuota(clock.Now().Add(35*time.Hour), clock.Now())),
		weeklyResetTestAuth("b", "codex", "7", QuotaState{}),
		weeklyResetTestAuth("c", "claude", "1", QuotaState{}),
	}
	boost.hints.set("b", clock.Now().Add(12*time.Hour), clock.Now())
	counts := countWeeklyResetPicks(t, 1000, func() (*Auth, error) {
		return selector.Pick(context.Background(), "mixed", "model", cliproxyexecutor.Options{}, auths)
	})
	if counts["b"] != 1000 {
		t.Fatalf("picks = %#v, want all 1000 on b resetting in 12h", counts)
	}
}

func TestWeeklyResetWeightedRoundRobinSelectorPick_TiedAuthsShareEvenly(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	selector := &WeightedRoundRobinSelector{resetBoost: boost}
	resetAt := clock.Now().Add(20 * time.Hour)
	auths := []*Auth{
		weeklyResetTestAuth("a", "claude", "1", claudeWeeklyQuota(resetAt, clock.Now())),
		weeklyResetTestAuth("b", "claude", "4", claudeWeeklyQuota(resetAt, clock.Now())),
		weeklyResetTestAuth("c", "claude", "1", QuotaState{}),
	}
	counts := countWeeklyResetPicks(t, 1000, func() (*Auth, error) {
		return selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
	})
	if counts["a"] != 500 || counts["b"] != 500 || counts["c"] != 0 {
		t.Fatalf("picks = %#v, want a:b=500:500 and c unused", counts)
	}
}

func TestWeeklyResetBoost_SessionLimitedAuthGoesFirstWithinHeadStart(t *testing.T) {
	t.Parallel()

	now := weeklyResetTestBase
	cases := []struct {
		name                  string
		exemptIn, limitedIn   time.Duration
		wantLimited, wantBoth bool
	}{
		{name: "same reset", exemptIn: 26 * time.Hour, limitedIn: 26 * time.Hour, wantLimited: true},
		{name: "limited 50m later", exemptIn: 26 * time.Hour, limitedIn: 26*time.Hour + 50*time.Minute, wantLimited: true},
		{name: "limited exactly the head start later", exemptIn: 26 * time.Hour, limitedIn: 27 * time.Hour, wantBoth: true},
		{name: "limited 70m later", exemptIn: 26 * time.Hour, limitedIn: 27*time.Hour + 10*time.Minute},
		{name: "exempt much later", exemptIn: 35 * time.Hour, limitedIn: 12 * time.Hour, wantLimited: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boost, _ := newWeeklyResetTestBoost()
			exempt := weeklyResetTestAuth("exempt", "claude", "1", claudeExemptQuota(now.Add(tc.exemptIn), now))
			limited := weeklyResetTestAuth("limited", "claude", "1", claudeWeeklyQuota(now.Add(tc.limitedIn), now))
			weights := map[string]int64{"exempt": 1, "limited": 1}
			boost.boostAuthWeights(weights, []*Auth{exempt, limited})
			wantExempt, wantLimited := int64(WeeklyResetBoostWeight), int64(1)
			switch {
			case tc.wantBoth:
				wantLimited = WeeklyResetBoostWeight
			case tc.wantLimited:
				wantExempt, wantLimited = 1, WeeklyResetBoostWeight
			}
			if weights["exempt"] != wantExempt || weights["limited"] != wantLimited {
				t.Fatalf("weights = %#v, want exempt %d limited %d", weights, wantExempt, wantLimited)
			}
		})
	}
}

func TestWeeklyResetBoost_SessionLimitPrefersMostRecentObservation(t *testing.T) {
	t.Parallel()

	now := weeklyResetTestBase
	resetAt := now.Add(26 * time.Hour)
	rank := func(boost *weeklyResetBoost, exempt *Auth) map[string]int64 {
		other := weeklyResetTestAuth("other", "claude", "1", claudeExemptQuota(resetAt, now))
		weights := map[string]int64{exempt.ID: 1, other.ID: 1}
		boost.boostAuthWeights(weights, []*Auth{exempt, other})
		return weights
	}

	// A newer usage read that still lists the 5-hour window beats exempt headers.
	boost, _ := newWeeklyResetTestBoost()
	auth := weeklyResetTestAuth("a", "claude", "1", claudeExemptQuota(resetAt, now.Add(-time.Hour)))
	boost.sessionHints.set("a", true, now)
	if weights := rank(boost, auth); weights["a"] != WeeklyResetBoostWeight || weights["other"] != 1 {
		t.Fatalf("newer session hint: weights = %#v, want a boosted alone", weights)
	}

	// An older hint loses to the headers, and so does a hint at the same instant.
	boost, _ = newWeeklyResetTestBoost()
	boost.sessionHints.set("a", true, now.Add(-2*time.Hour))
	if weights := rank(boost, auth); weights["a"] != WeeklyResetBoostWeight || weights["other"] != WeeklyResetBoostWeight {
		t.Fatalf("older session hint: weights = %#v, want a and other tied", weights)
	}

	// A hint alone marks an idle credential as exempt.
	boost, _ = newWeeklyResetTestBoost()
	idle := weeklyResetTestAuth("idle", "claude", "1", QuotaState{})
	boost.hints.set("idle", resetAt, now)
	boost.sessionHints.set("idle", false, now)
	limited := weeklyResetTestAuth("limited", "claude", "1", claudeWeeklyQuota(resetAt.Add(30*time.Minute), now))
	weights := map[string]int64{"idle": 1, "limited": 1}
	boost.boostAuthWeights(weights, []*Auth{idle, limited})
	if weights["limited"] != WeeklyResetBoostWeight || weights["idle"] != 1 {
		t.Fatalf("idle exempt hint: weights = %#v, want limited boosted", weights)
	}
}

func TestSessionLimitFromSignals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		signals     map[string]string
		limited, ok bool
	}{
		{name: "claude with 5h", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": "1", "Anthropic-Ratelimit-Unified-5h-Utilization": "0.2"}, limited: true, ok: true},
		{name: "claude exempt", signals: map[string]string{"anthropic-ratelimit-unified-7d-utilization": "0.2"}, ok: true},
		{name: "claude 5h only", signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": "1"}},
		{name: "codex 5h primary", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "300", "X-Codex-Secondary-Window-Minutes": "10080"}, limited: true, ok: true},
		{name: "codex weekly only", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "10080"}, ok: true},
		{name: "codex image limit", signals: map[string]string{"X-Codex-Active-Limit": "imagegen_x", "X-Codex-Primary-Window-Minutes": "300"}},
		{name: "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if limited, ok := sessionLimitFromSignals(tc.signals); limited != tc.limited || ok != tc.ok {
				t.Fatalf("sessionLimitFromSignals() = %v, %v; want %v, %v", limited, ok, tc.limited, tc.ok)
			}
		})
	}

	authID := "session-limit-public-" + t.Name()
	SetSessionLimitHint(authID, false, weeklyResetTestBase)
	if limited, observedAt, ok := SessionLimitHint(authID); !ok || limited || !observedAt.Equal(weeklyResetTestBase) {
		t.Fatalf("SessionLimitHint() = %v, %v, %v", limited, observedAt, ok)
	}
}

func TestWeeklyResetWeightedRoundRobinSelectorPick_FallsBackWhenBoostedAuthUnavailable(t *testing.T) {
	t.Parallel()

	model := "claude-model"
	boost, clock := newWeeklyResetTestBoost()
	selector := &WeightedRoundRobinSelector{resetBoost: boost}
	boosted := weeklyResetTestAuth("a", "claude", "1", claudeWeeklyQuota(clock.Now().Add(10*time.Hour), clock.Now()))
	recoverAt := time.Now().Add(time.Hour)
	boosted.ModelStates = map[string]*ModelState{model: {
		Unavailable:    true,
		NextRetryAfter: recoverAt,
		Quota:          QuotaState{Exceeded: true, NextRecoverAt: recoverAt},
	}}
	auths := []*Auth{boosted, weeklyResetTestAuth("b", "claude", "1", QuotaState{}), weeklyResetTestAuth("c", "claude", "1", QuotaState{})}
	pick := func() (*Auth, error) {
		return selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
	}

	if counts := countWeeklyResetPicks(t, 100, pick); counts["a"] != 0 || counts["b"] != 50 || counts["c"] != 50 {
		t.Fatalf("picks with boosted auth cooling down = %#v, want b:c=50:50", counts)
	}

	boosted.ModelStates = nil
	boosted.Disabled = true
	if counts := countWeeklyResetPicks(t, 10, pick); counts["a"] != 0 {
		t.Fatalf("picks with boosted auth disabled = %#v, want a skipped", counts)
	}

	boosted.Disabled = false
	if counts := countWeeklyResetPicks(t, 10, pick); counts["a"] != 10 {
		t.Fatalf("picks after boosted auth recovered = %#v, want all on a", counts)
	}
}

func TestWeeklyResetWeightedRoundRobinSelectorPick_BoostFollowsClock(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	selector := &WeightedRoundRobinSelector{resetBoost: boost}
	resetAt := clock.Now().Add(40 * time.Hour)
	auths := []*Auth{
		weeklyResetTestAuth("a", "claude", "1", claudeWeeklyQuota(resetAt, clock.Now())),
		weeklyResetTestAuth("b", "claude", "1", QuotaState{}),
	}
	pick := func() (*Auth, error) {
		return selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
	}

	if counts := countWeeklyResetPicks(t, 20, pick); counts["a"] != 10 || counts["b"] != 10 {
		t.Fatalf("picks 40h before reset = %#v, want a:b=10:10", counts)
	}
	clock.Advance(5 * time.Hour)
	if counts := countWeeklyResetPicks(t, 20, pick); counts["a"] != 20 {
		t.Fatalf("picks 35h before reset = %#v, want all on a", counts)
	}
	clock.Advance(35*time.Hour + time.Minute)
	if counts := countWeeklyResetPicks(t, 20, pick); counts["a"] != 10 || counts["b"] != 10 {
		t.Fatalf("picks after reset = %#v, want a:b=10:10", counts)
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

func TestSchedulerPick_WeeklyResetBoost(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	authA := weeklyResetTestAuth("a", "claude", "1", claudeWeeklyQuota(clock.Now().Add(10*time.Hour), clock.Now()))
	authB := weeklyResetTestAuth("b", "claude", "1", QuotaState{})
	authC := weeklyResetTestAuth("c", "claude", "0", QuotaState{})
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{resetBoost: boost}, authA, authB, authC)
	boost.hints.set("c", clock.Now().Add(time.Hour), clock.Now())
	pick := func() (*Auth, error) {
		return scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
	}

	if counts := countWeeklyResetPicks(t, 100, pick); counts["a"] != 100 {
		t.Fatalf("scheduler picks = %#v, want all on boosted a", counts)
	}

	// After a's reset the idle credential b is boosted by a poller hint alone.
	clock.Advance(10*time.Hour + time.Minute)
	boost.hints.set("b", clock.Now().Add(20*time.Hour), clock.Now())
	if counts := countWeeklyResetPicks(t, 100, pick); counts["b"] != 100 {
		t.Fatalf("scheduler picks after a reset = %#v, want all on hinted b", counts)
	}

	clock.Advance(20*time.Hour + time.Minute)
	if counts := countWeeklyResetPicks(t, 20, pick); counts["a"] != 10 || counts["b"] != 10 || counts["c"] != 0 {
		t.Fatalf("scheduler picks after both resets = %#v, want a:b=10:10", counts)
	}
}

func TestSchedulerPick_WeeklyResetBoostPrefersMostUrgent(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	later := weeklyResetTestAuth("later", "claude", "1", claudeWeeklyQuota(clock.Now().Add(30*time.Hour), clock.Now()))
	sooner := weeklyResetTestAuth("sooner", "claude", "1", QuotaState{})
	other := weeklyResetTestAuth("other", "claude", "1", QuotaState{})
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{resetBoost: boost}, later, sooner, other)
	boost.hints.set("sooner", clock.Now().Add(12*time.Hour), clock.Now())
	pick := func() (*Auth, error) {
		return scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
	}
	if counts := countWeeklyResetPicks(t, 100, pick); counts["sooner"] != 100 {
		t.Fatalf("scheduler picks = %#v, want all on sooner", counts)
	}

	// Once the most urgent credential cools down, the next most urgent takes over.
	cooling := sooner.Clone()
	cooling.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Hour)}
	cooling.Generation++
	scheduler.upsertAuth(cooling)
	if counts := countWeeklyResetPicks(t, 100, pick); counts["later"] != 100 {
		t.Fatalf("scheduler picks with sooner cooling = %#v, want all on later", counts)
	}
}

func TestSchedulerPick_WeeklyResetBoostSkipsCoolingDownAuth(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	quota := claudeWeeklyQuota(clock.Now().Add(10*time.Hour), clock.Now())
	quota.Exceeded = true
	quota.Reason = "credential_quota"
	quota.NextRecoverAt = time.Now().Add(time.Hour)
	authA := weeklyResetTestAuth("a", "claude", "1", quota)
	authB := weeklyResetTestAuth("b", "claude", "1", QuotaState{})
	authC := weeklyResetTestAuth("c", "claude", "1", QuotaState{})
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{resetBoost: boost}, authA, authB, authC)
	pick := func() (*Auth, error) {
		return scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
	}

	if counts := countWeeklyResetPicks(t, 100, pick); counts["a"] != 0 || counts["b"] != 50 || counts["c"] != 50 {
		t.Fatalf("scheduler picks with boosted auth cooling down = %#v, want b:c=50:50", counts)
	}

	recovered := authA.Clone()
	recovered.Quota = claudeWeeklyQuota(clock.Now().Add(10*time.Hour), clock.Now())
	recovered.Generation++
	scheduler.upsertAuth(recovered)
	if counts := countWeeklyResetPicks(t, 20, pick); counts["a"] != 20 {
		t.Fatalf("scheduler picks after recovery = %#v, want all on a", counts)
	}
}

func TestSchedulerPick_WeeklyResetBoostMixedProviders(t *testing.T) {
	t.Parallel()

	boost, clock := newWeeklyResetTestBoost()
	scheduler := newSchedulerForTest(
		&WeightedRoundRobinSelector{resetBoost: boost},
		weeklyResetTestAuth("claude-a", "claude", "1", QuotaState{}),
		weeklyResetTestAuth("codex-b", "codex", "3", QuotaState{}),
	)
	boost.hints.set("claude-a", clock.Now().Add(12*time.Hour), clock.Now())
	counts := countWeeklyResetPicks(t, 50, func() (*Auth, error) {
		got, _, errPick := scheduler.pickMixed(context.Background(), []string{"claude", "codex"}, "", cliproxyexecutor.Options{}, nil)
		return got, errPick
	})
	if counts["claude-a"] != 50 {
		t.Fatalf("mixed scheduler picks = %#v, want all on boosted claude-a", counts)
	}
}

func TestSchedulerPick_WeeklyResetBoostReadsLatestAuthSnapshot(t *testing.T) {
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
		t.Fatal("scheduler has no weekly-reset boost for the weekly-reset selector")
	}
	if manager.scheduler.strategy != schedulerStrategyWeightedRoundRobin || !manager.useSchedulerFastPath() {
		t.Fatalf("scheduler strategy = %v, fast path = %v; want weighted fast path", manager.scheduler.strategy, manager.useSchedulerFastPath())
	}

	manager.SetSelector(&WeightedRoundRobinSelector{})
	if manager.scheduler.resetBoost != nil {
		t.Fatal("plain weighted selector kept the weekly-reset boost")
	}
	manager.SetSelector(NewWeeklyResetWeightedRoundRobinSelector())
	if manager.scheduler.resetBoost == nil {
		t.Fatal("switching back did not restore the weekly-reset boost")
	}
}

func TestSessionAffinitySelector_WeeklyResetBoostAppliesToNewSessions(t *testing.T) {
	t.Parallel()

	model := "claude-model"
	boost, clock := newWeeklyResetTestBoost()
	selector := NewSessionAffinitySelector(&WeightedRoundRobinSelector{resetBoost: boost})
	defer selector.Stop()
	authA := weeklyResetTestAuth("auth-a", "claude", "1", claudeWeeklyQuota(clock.Now().Add(40*time.Hour), clock.Now()))
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

	// Before the boost window sessions spread evenly; remember one bound to b.
	stickySession := ""
	for index := 0; index < 3; index++ {
		session := fmt.Sprintf("before-%d", index)
		if pickSession(session).ID == authB.ID {
			stickySession = session
		}
	}
	if stickySession == "" {
		t.Fatal("no session bound to auth-b before the boost window")
	}

	clock.Advance(5 * time.Hour)
	for index := 0; index < 20; index++ {
		if got := pickSession(fmt.Sprintf("boosted-%d", index)); got.ID != authA.ID {
			t.Fatalf("new session %d picked %s, want boosted %s", index, got.ID, authA.ID)
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
	if got := pickSession("boosted-0"); got.ID == authA.ID {
		t.Fatalf("session bound to cooling-down boosted auth was not failed over")
	}
	counts := make(map[string]int)
	for index := 0; index < 10; index++ {
		counts[pickSession(fmt.Sprintf("cooldown-%d", index)).ID]++
	}
	if counts[authA.ID] != 0 || counts[authB.ID] == 0 || counts[authC.ID] == 0 {
		t.Fatalf("new sessions during boosted auth cooldown = %#v, want spread over b and c", counts)
	}
}

func TestWeeklyResetFromSignals_IgnoresCodexImageLimit(t *testing.T) {
	signals := map[string]string{
		"X-Codex-Active-Limit":           "imagegen_premium",
		"X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At":       "1793214612",
	}
	if resetAt, ok := weeklyResetFromSignals(signals, time.Unix(1793132609, 0)); ok {
		t.Fatalf("image-limit headers gave a weekly reset %v", resetAt)
	}
}
