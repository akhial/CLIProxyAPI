package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// newFableTargetTestBoost returns a weekly-reset boost with the Fable target on and
// isolated hint registries.
func newFableTargetTestBoost() (*weeklyResetBoost, *weeklyResetTestClock) {
	boost, clock := newWeeklyResetTestBoost()
	boost.fableUsage = &quotaHintRegistry[FableUsage]{}
	boost.fableResets = &quotaHintRegistry[bool]{}
	boost.fableResetTarget = true
	return boost, clock
}

// fableTestAuth is a Claude credential without a 5-hour limit whose week resets at
// resetAt with weekly (a 0..1 fraction) used, and whose Fable limit is fablePercent used.
func fableTestAuth(boost *weeklyResetBoost, id string, resetAt time.Time, weekly, fablePercent float64, hasReset bool) *Auth {
	now := boost.clock()
	auth := weeklyResetTestAuth(id, "claude", "1", claudeQuota(resetAt, now, weekly, -1))
	boost.fableUsage.set(id, FableUsage{UsedPercent: fablePercent, ResetAt: resetAt}, now)
	boost.fableResets.set(id, hasReset, now)
	return auth
}

func rankModelWeights(boost *weeklyResetBoost, model string, auths ...*Auth) map[string]int64 {
	weights := make(map[string]int64, len(auths))
	for _, auth := range auths {
		weights[auth.ID] = authWeight(auth)
	}
	boost.rankAuthWeights(weights, auths, model)
	return weights
}

func TestFableTarget_FableGoesToResetWithMostWeekLeft(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	soon := fableTestAuth(boost, "soon", clock.now.Add(24*time.Hour), 0.5, 75, true)
	late := fableTestAuth(boost, "late", clock.now.Add(5*24*time.Hour), 0.2, 10, true)
	none := fableTestAuth(boost, "none", clock.now.Add(6*24*time.Hour), 0.1, 0, false)

	assertWeights(t, rankModelWeights(boost, "claude-fable-5", soon, late, none), map[string]int64{"late": 1})
	// Other models keep the earliest deadline.
	assertWeights(t, rankModelWeights(boost, "claude-opus-4-8", soon, late, none), map[string]int64{"soon": 1})
}

func TestFableTarget_UsedUpTargetHandsOver(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	early := fableTestAuth(boost, "early", clock.now.Add(2*24*time.Hour), 0.3, 20, true)
	full := fableTestAuth(boost, "full", clock.now.Add(5*24*time.Hour), 0.9, 100, true)

	assertWeights(t, rankModelWeights(boost, "claude-fable-5", early, full), map[string]int64{"early": 1})
}

func TestFableTarget_NoResetKeepsDeadlineOrder(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	soon := fableTestAuth(boost, "soon", clock.now.Add(24*time.Hour), 0.5, 10, false)
	late := fableTestAuth(boost, "late", clock.now.Add(5*24*time.Hour), 0.2, 10, false)

	assertWeights(t, rankModelWeights(boost, "claude-fable-5", soon, late), map[string]int64{"soon": 1})
}

func TestFableTarget_OtherModelsKeepOffFableRoom(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	// 70 % of the week used and 40 % of Fable: the remaining Fable needs 30 % of the week,
	// so there is no room left for other models.
	target := fableTestAuth(boost, "target", clock.now.Add(12*time.Hour), 0.7, 40, true)
	other := fableTestAuth(boost, "other", clock.now.Add(3*24*time.Hour), 0.2, 0, false)

	assertWeights(t, rankModelWeights(boost, "claude-opus-4-8", target, other), map[string]int64{"other": 1})
	// Alone, the target still serves other models rather than failing the request.
	assertWeights(t, rankModelWeights(boost, "claude-opus-4-8", target), map[string]int64{"target": 1})
	// Fable still goes to it.
	assertWeights(t, rankModelWeights(boost, "claude-fable-5", target, other), map[string]int64{"target": 1})
}

func TestFableTarget_OtherModelsUseRoomBeyondFable(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	// 40 % used, Fable 40 %: the remaining Fable needs 30 %, leaving 30 % for others.
	target := fableTestAuth(boost, "target", clock.now.Add(12*time.Hour), 0.4, 40, true)
	other := fableTestAuth(boost, "other", clock.now.Add(3*24*time.Hour), 0.2, 0, false)

	assertWeights(t, rankModelWeights(boost, "claude-opus-4-8", target, other), map[string]int64{"target": 1})
}

func TestFableTarget_PassedFableWindowCountsAsFresh(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	auth := fableTestAuth(boost, "a", clock.now.Add(24*time.Hour), 0.9, 100, true)
	if used := boost.fableUsed(auth, clock.now); used != 100 {
		t.Fatalf("fable used = %v, want 100", used)
	}
	clock.Advance(25 * time.Hour)
	if used := boost.fableUsed(auth, clock.now); used != 0 {
		t.Fatalf("fable used after the window ended = %v, want 0", used)
	}
}

func TestFableTarget_OffByDefault(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	boost.fableResetTarget = false
	soon := fableTestAuth(boost, "soon", clock.now.Add(24*time.Hour), 0.5, 10, true)
	late := fableTestAuth(boost, "late", clock.now.Add(5*24*time.Hour), 0.2, 10, true)

	assertWeights(t, rankModelWeights(boost, "claude-fable-5", soon, late), map[string]int64{"soon": 1})
	if NewWeeklyResetWeightedRoundRobinSelector().resetBoost.fableResetTarget {
		t.Fatal("weekly-reset selector has the Fable target on")
	}
	if !NewFableResetWeightedRoundRobinSelector().resetBoost.fableResetTarget {
		t.Fatal("Fable-reset selector has the Fable target off")
	}
}

func TestFableTarget_RebindsSessionsItKeepsOff(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &WeightedRoundRobinSelector{resetBoost: boost},
		TTL:      time.Hour,
	})
	defer selector.Stop()
	pick := func(session, model string, auths ...*Auth) string {
		t.Helper()
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"session_id":"` + session + `"}`)}
		picked, errPick := selector.Pick(context.Background(), "claude", model, opts, auths)
		if errPick != nil {
			t.Fatalf("Pick(%s) error = %v", session, errPick)
		}
		return picked.ID
	}

	// The target has the earliest reset and room beyond its Fable, so other models bind to it.
	target := fableTestAuth(boost, "target", clock.now.Add(12*time.Hour), 0.4, 40, true)
	other := fableTestAuth(boost, "other", clock.now.Add(3*24*time.Hour), 0.2, 0, false)
	if got := pick("opus", "claude-opus-4-8", target, other); got != "target" {
		t.Fatalf("first opus pick = %s, want target", got)
	}
	// Once only its Fable's room is left, the bound session moves.
	target.Quota = claudeQuota(clock.now.Add(12*time.Hour), clock.now.Add(time.Second), 0.7, -1)
	if got := pick("opus", "claude-opus-4-8", target, other); got != "other" {
		t.Fatalf("opus pick after the target filled = %s, want other", got)
	}

	// A Fable session bound before an account became the target moves to it.
	boost.fableResets.set("target", false, clock.now.Add(2*time.Second))
	if got := pick("fable", "claude-fable-5", target, other); got != "target" {
		t.Fatalf("first fable pick = %s, want target (earliest deadline)", got)
	}
	boost.fableResets.set("other", true, clock.now.Add(3*time.Second))
	if got := pick("fable", "claude-fable-5", target, other); got != "other" {
		t.Fatalf("fable pick after other became the target = %s, want other", got)
	}
}

// Behind session affinity, only the one target is kept off: the next account with a
// reset is not excluded as well.
func TestFableTarget_AffinityExcludesOnlyTheTarget(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &WeightedRoundRobinSelector{resetBoost: boost},
		TTL:      time.Hour,
	})
	defer selector.Stop()
	a := fableTestAuth(boost, "a", clock.now.Add(5*24*time.Hour), 0.7, 40, true)
	b := fableTestAuth(boost, "b", clock.now.Add(24*time.Hour), 0.7, 40, true)
	c := fableTestAuth(boost, "c", clock.now.Add(3*24*time.Hour), 0.2, 0, false)

	assertWeights(t, rankModelWeights(boost, "claude-opus-4-8", a, b, c), map[string]int64{"b": 1})
	for _, body := range []string{`{}`, `{"session_id":"opus"}`} {
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(body)}
		picked, errPick := selector.Pick(context.Background(), "claude", "claude-opus-4-8", opts, []*Auth{a, b, c})
		if errPick != nil {
			t.Fatalf("Pick(%s) error = %v", body, errPick)
		}
		if picked.ID != "b" {
			t.Fatalf("Pick(%s) = %s, want b", body, picked.ID)
		}
	}
}

// The manager's legacy path passes no model to built-in selectors and carries the
// route model in the context instead.
func TestFableTarget_UsesRouteModelFromContext(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	selector := &WeightedRoundRobinSelector{resetBoost: boost}
	soon := fableTestAuth(boost, "soon", clock.now.Add(24*time.Hour), 0.5, 10, true)
	late := fableTestAuth(boost, "late", clock.now.Add(5*24*time.Hour), 0.2, 10, true)

	ctx := withWeightedSelectorStateModel(context.Background(), selector, "claude-fable-5")
	picked, errPick := selector.Pick(ctx, "claude", "", cliproxyexecutor.Options{}, []*Auth{soon, late})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if picked.ID != "late" {
		t.Fatalf("Pick() = %s, want the target late", picked.ID)
	}
}

// A Fable session bound to a lower priority account while the target was unavailable
// moves back to the target once it is available again.
func TestFableTarget_RebindsFableSessionAcrossPriorities(t *testing.T) {
	boost, clock := newFableTargetTestBoost()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &WeightedRoundRobinSelector{resetBoost: boost},
		TTL:      time.Hour,
	})
	defer selector.Stop()
	target := fableTestAuth(boost, "target", clock.now.Add(5*24*time.Hour), 0.2, 10, true)
	target.Attributes["priority"] = "10"
	low := fableTestAuth(boost, "low", clock.now.Add(24*time.Hour), 0.2, 10, false)
	pick := func(auths ...*Auth) string {
		t.Helper()
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"session_id":"fable"}`)}
		picked, errPick := selector.Pick(context.Background(), "claude", "claude-fable-5", opts, auths)
		if errPick != nil {
			t.Fatalf("Pick() error = %v", errPick)
		}
		return picked.ID
	}
	if got := pick(low); got != "low" {
		t.Fatalf("pick while the target is unavailable = %s, want low", got)
	}
	if got := pick(target, low); got != "target" {
		t.Fatalf("pick once the target is back = %s, want target", got)
	}
}
