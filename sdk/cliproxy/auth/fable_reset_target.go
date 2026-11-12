package auth

import (
	"context"
	"strings"
	"time"
)

// FableUsage is an observation of a credential's weekly Fable limit.
type FableUsage struct {
	// UsedPercent is the share of the Fable limit used, 0..100.
	UsedPercent float64
	// ResetAt is when the Fable limit resets, zero when unknown.
	ResetAt time.Time
}

var (
	defaultFableUsageHints = &quotaHintRegistry[FableUsage]{}
	defaultFableResetHints = &quotaHintRegistry[bool]{}
)

// SetFableUsageHint records a credential's weekly Fable limit as observed by a usage
// poller. Replacement and observedAt follow SetWeeklyQuotaHint.
func SetFableUsageHint(authID string, usage FableUsage, observedAt time.Time) {
	defaultFableUsageHints.set(authID, usage, observedAt)
}

// SetFableResetHint records whether a credential has an unused Fable reset it can claim.
func SetFableResetHint(authID string, available bool, observedAt time.Time) {
	defaultFableResetHints.set(authID, available, observedAt)
}

// isFableModel reports whether a request model draws on the Fable limit.
func isFableModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "fable")
}

func (b *weeklyResetBoost) fableUsageRegistry() *quotaHintRegistry[FableUsage] {
	if b != nil && b.fableUsage != nil {
		return b.fableUsage
	}
	return defaultFableUsageHints
}

func (b *weeklyResetBoost) fableResetRegistry() *quotaHintRegistry[bool] {
	if b != nil && b.fableResets != nil {
		return b.fableResets
	}
	return defaultFableResetHints
}

// fableUsed returns the share of auth's Fable limit used at now, 0 when the observed
// window has ended or nothing was observed.
func (b *weeklyResetBoost) fableUsed(auth *Auth, now time.Time) float64 {
	usage, _, ok := b.fableUsageRegistry().get(auth.ID)
	if !ok || (!usage.ResetAt.IsZero() && !usage.ResetAt.After(now)) {
		return 0
	}
	return min(100, max(0, usage.UsedPercent))
}

// weeklyUsedAndReset returns auth's weekly use and reset at now, moving a passed
// window to the next week with nothing used.
func (b *weeklyResetBoost) weeklyUsedAndReset(auth *Auth, now time.Time) (float64, time.Time, bool) {
	weekly, ok := b.weeklyQuota(auth)
	if !ok {
		return 0, time.Time{}, false
	}
	resetAt, used := weekly.ResetAt, weekly.UsedPercent
	if !resetAt.After(now) {
		resetAt = resetAt.Add((now.Sub(resetAt)/weeklyQuotaPeriod + 1) * weeklyQuotaPeriod)
		used = 0
	}
	return max(0, used), resetAt, true
}

// fableTarget picks the one candidate Fable is concentrated on so its Fable reset can be
// claimed: among those with an unused Fable reset and Fable left, the one whose weekly
// limit resets last, leaving the most time to spend what the reset gives back.
func (b *weeklyResetBoost) fableTarget(weights map[string]int64, auths []*Auth, now time.Time) *Auth {
	var target *Auth
	var targetReset time.Time
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if weight, ok := weights[auth.ID]; !ok || weight <= 0 {
			continue
		}
		if available, _, ok := b.fableResetRegistry().get(auth.ID); !ok || !available {
			continue
		}
		if b.fableUsed(auth, now) >= 100 {
			continue
		}
		_, resetAt, ok := b.weeklyUsedAndReset(auth, now)
		if !ok {
			continue
		}
		if target == nil || resetAt.After(targetReset) || (resetAt.Equal(targetReset) && auth.ID < target.ID) {
			target, targetReset = auth, resetAt
		}
	}
	return target
}

// applyFableTarget narrows the candidates for the Fable-reset target. A Fable request
// goes to the target alone. Other models skip the target once its weekly limit has no
// room left beyond what its remaining Fable needs (Fable 100 % is 50 % of the week), as
// long as another candidate remains. It reports whether the choice is final.
func (b *weeklyResetBoost) applyFableTarget(weights map[string]int64, auths []*Auth, model string, now time.Time) bool {
	target := b.fableTarget(weights, auths, now)
	if target == nil {
		return false
	}
	for authID := range b.fableExcluded(target, weights, model, now) {
		delete(weights, authID)
	}
	return isFableModel(model)
}

// fableExcluded returns the candidates in weights that a request for model keeps off
// given the Fable target; see applyFableTarget.
func (b *weeklyResetBoost) fableExcluded(target *Auth, weights map[string]int64, model string, now time.Time) map[string]struct{} {
	excluded := make(map[string]struct{})
	if isFableModel(model) {
		for authID := range weights {
			if authID != target.ID {
				excluded[authID] = struct{}{}
			}
		}
		return excluded
	}
	used, _, _ := b.weeklyUsedAndReset(target, now)
	if used+(100-b.fableUsed(target, now))/2 < 100 {
		return excluded
	}
	for authID, weight := range weights {
		if authID != target.ID && weight > 0 {
			excluded[target.ID] = struct{}{}
			break
		}
	}
	return excluded
}

// fableTargetAppliedKey marks a context whose candidates the Fable target already
// narrowed, so the fallback selector does not narrow them a second time.
type fableTargetAppliedKey struct{}

// withoutFableExcluded drops the credentials the Fable target keeps this request off,
// so a session bound to one of them is rebound instead of reused. The returned context
// tells the fallback selector the target has been applied; recomputing it over the
// narrowed candidates could pick a second target and exclude that one too.
func withoutFableExcluded(ctx context.Context, fallback Selector, provider, model string, auths []*Auth) (context.Context, []*Auth) {
	boost := selectorResetBoost(fallback)
	if boost == nil || !boost.fableResetTarget {
		return ctx, auths
	}
	// The target comes from the highest priority tier, as the fallback picks it. A Fable
	// request keeps off every other tier too, so a session bound to a lower tier while
	// the target was unavailable moves back to it.
	available, errAvailable := getSelectorAvailableAuthsAcrossPriorities(ctx, positiveWeightAuths(auths), provider, model, time.Now())
	if errAvailable != nil || len(available) == 0 {
		return ctx, auths
	}
	ctx = context.WithValue(ctx, fableTargetAppliedKey{}, true)
	now := boost.clock()
	highest := highestPriorityAuths(available)
	target := boost.fableTarget(authWeightVector(highest), highest, now)
	if target == nil {
		return ctx, auths
	}
	scope := highest
	if isFableModel(model) {
		scope = available
	}
	excluded := boost.fableExcluded(target, authWeightVector(scope), model, now)
	if len(excluded) == 0 {
		return ctx, auths
	}
	kept := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if _, drop := excluded[auth.ID]; !drop {
			kept = append(kept, auth)
		}
	}
	return ctx, kept
}

// withoutAppliedFableTarget returns boost, or a copy without the Fable target when ctx
// says the target was already applied to the candidates.
func withoutAppliedFableTarget(ctx context.Context, boost *weeklyResetBoost) *weeklyResetBoost {
	if boost == nil || !boost.fableResetTarget {
		return boost
	}
	if applied, _ := ctx.Value(fableTargetAppliedKey{}).(bool); !applied {
		return boost
	}
	copied := *boost
	copied.fableResetTarget = false
	return &copied
}
