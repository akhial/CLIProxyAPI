package auth

import (
	"testing"
	"time"
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
