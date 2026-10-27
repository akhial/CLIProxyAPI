package auth

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/credentialweight"
)

const (
	// WeeklyResetBoostWindow is how close the weekly quota reset of a credential must be
	// for the weekly-reset weighted round-robin strategy to boost it.
	WeeklyResetBoostWindow = 36 * time.Hour
	// WeeklyResetBoostWeight is the effective weight of a boosted credential.
	WeeklyResetBoostWeight = credentialweight.Max

	// weeklyQuotaWindowMinutes identifies the weekly Codex window by its advertised length.
	weeklyQuotaWindowMinutes = 7 * 24 * 60
	// maxWeeklyResetHints bounds the hint registry so removed credentials cannot leak entries.
	maxWeeklyResetHints = 4096
)

// NewWeeklyResetWeightedRoundRobinSelector returns a weighted round-robin selector that
// raises every credential whose weekly quota resets within WeeklyResetBoostWindow to
// WeeklyResetBoostWeight, so quota that would expire unused at the reset is spent first.
// Availability checks are unchanged, and credentials with a non-positive configured
// weight stay excluded.
func NewWeeklyResetWeightedRoundRobinSelector() *WeightedRoundRobinSelector {
	return &WeightedRoundRobinSelector{resetBoost: &weeklyResetBoost{}}
}

// weeklyResetBoost computes effective weights for the weekly-reset strategy.
type weeklyResetBoost struct {
	// now and hints default to time.Now and the process-wide registry; tests inject both.
	now   func() time.Time
	hints *weeklyResetHintRegistry
}

// selectorResetBoost returns the weekly-reset boost of a weighted selector, if any.
func selectorResetBoost(selector Selector) *weeklyResetBoost {
	if weighted, ok := selector.(*WeightedRoundRobinSelector); ok && weighted != nil {
		return weighted.resetBoost
	}
	return nil
}

// ReuseWeightedRoundRobinSelector returns the weighted selector inside existing when it
// ranks like want (both plain, or both weekly-reset), so a routing change that keeps the
// strategy keeps its credits; otherwise it returns want.
func ReuseWeightedRoundRobinSelector(existing Selector, want *WeightedRoundRobinSelector) *WeightedRoundRobinSelector {
	if current := UnwrapWeightedRoundRobinSelector(existing); current != nil && want != nil && sameResetBoost(current.resetBoost, want.resetBoost) {
		return current
	}
	return want
}

// sameResetBoost reports whether two selectors' weekly-reset settings route alike.
func sameResetBoost(left, right *weeklyResetBoost) bool {
	return (left == nil) == (right == nil)
}

func (b *weeklyResetBoost) clock() time.Time {
	if b != nil && b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *weeklyResetBoost) registry() *weeklyResetHintRegistry {
	if b != nil && b.hints != nil {
		return b.hints
	}
	return defaultWeeklyResetHints
}

// effectiveWeight returns WeeklyResetBoostWeight when the weekly quota of auth resets
// within WeeklyResetBoostWindow after now, and the configured weight otherwise.
func (b *weeklyResetBoost) effectiveWeight(auth *Auth, configured int64, now time.Time) int64 {
	if b == nil || auth == nil || configured <= 0 || configured >= WeeklyResetBoostWeight {
		return configured
	}
	resetAt, ok := b.weeklyResetAt(auth)
	if !ok {
		return configured
	}
	if remaining := resetAt.Sub(now); remaining > 0 && remaining < WeeklyResetBoostWindow {
		return WeeklyResetBoostWeight
	}
	return configured
}

// boostAuthWeights rewrites the positive weights of auths in place. It is a no-op
// for a selector without the weekly-reset boost.
func (b *weeklyResetBoost) boostAuthWeights(weights map[string]int64, auths []*Auth) {
	if b == nil || len(weights) == 0 {
		return
	}
	now := b.clock()
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if weight, ok := weights[auth.ID]; ok {
			weights[auth.ID] = b.effectiveWeight(auth, weight, now)
		}
	}
}

// weeklyResetAt returns the most recently observed weekly reset instant of auth, taken
// from its passive response-header snapshot or from a registered hint. On equal
// observation times the header snapshot wins.
func (b *weeklyResetBoost) weeklyResetAt(auth *Auth) (time.Time, bool) {
	resetAt, ok := weeklyResetFromSignals(auth.Quota.Signals, auth.Quota.ObservedAt)
	hintResetAt, hintObservedAt, okHint := b.registry().get(auth.ID)
	if okHint && (!ok || hintObservedAt.After(auth.Quota.ObservedAt)) {
		return hintResetAt, true
	}
	return resetAt, ok
}

// weeklyResetFromSignals reads the weekly reset instant from a passive quota snapshot:
// the Claude unified 7d window, or whichever Codex primary/secondary window spans a
// week. Reset-After-Seconds is relative to observedAt.
func weeklyResetFromSignals(signals map[string]string, observedAt time.Time) (time.Time, bool) {
	if len(signals) == 0 {
		return time.Time{}, false
	}
	if resetAt, ok := parseQuotaResetInstant(quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-7d-Reset")); ok {
		return resetAt, true
	}
	// Image-generation responses describe their own limit, not the plan's weekly window.
	if strings.HasPrefix(strings.ToLower(quotaSignalValue(signals, "X-Codex-Active-Limit")), "imagegen") {
		return time.Time{}, false
	}
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		minutes, ok := parseQuotaSignalFloat(quotaSignalValue(signals, prefix+"Window-Minutes"))
		if !ok || math.Round(minutes) != weeklyQuotaWindowMinutes {
			continue
		}
		if resetAt, okReset := parseQuotaResetInstant(quotaSignalValue(signals, prefix+"Reset-At")); okReset {
			return resetAt, true
		}
		if after, okAfter := parseQuotaSignalFloat(quotaSignalValue(signals, prefix+"Reset-After-Seconds")); okAfter && after >= 0 && !observedAt.IsZero() {
			return observedAt.Add(time.Duration(after * float64(time.Second))), true
		}
	}
	return time.Time{}, false
}

// quotaSignalValue looks a header up by canonical name, falling back to a
// case-insensitive match.
func quotaSignalValue(signals map[string]string, name string) string {
	if value, ok := signals[http.CanonicalHeaderKey(name)]; ok {
		return strings.TrimSpace(value)
	}
	for key, value := range signals {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseQuotaSignalFloat(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// parseQuotaResetInstant accepts Unix seconds, Unix milliseconds (by magnitude) or RFC 3339.
func parseQuotaResetInstant(raw string) (time.Time, bool) {
	if value, ok := parseQuotaSignalFloat(raw); ok {
		if value <= 0 {
			return time.Time{}, false
		}
		// Unix seconds stay below 1e11 until the year 5138.
		if value < 1e11 {
			return time.UnixMilli(int64(math.Round(value * 1000))), true
		}
		return time.UnixMilli(int64(math.Round(value))), true
	}
	if parsed, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); errParse == nil {
		return parsed, true
	}
	return time.Time{}, false
}

// weeklyResetHint is a weekly reset instant observed outside the request path.
type weeklyResetHint struct {
	resetAt    time.Time
	observedAt time.Time
}

// weeklyResetHintRegistry stores the latest weekly reset hint per credential.
type weeklyResetHintRegistry struct {
	mu    sync.RWMutex
	hints map[string]weeklyResetHint
}

var defaultWeeklyResetHints = &weeklyResetHintRegistry{}

// SetWeeklyResetHint records when the weekly quota window of a credential resets, as
// observed by a source outside the request path such as a usage poller. The hint
// replaces an existing one only if it was observed at the same time or later; a zero
// observedAt means now. The weekly-reset routing strategy combines it with the
// credential's response-header snapshot, preferring the most recent observation.
func SetWeeklyResetHint(authID string, resetAt, observedAt time.Time) {
	defaultWeeklyResetHints.set(authID, resetAt, observedAt)
}

// WeeklyResetHint returns the weekly reset hint recorded for a credential.
func WeeklyResetHint(authID string) (resetAt, observedAt time.Time, ok bool) {
	return defaultWeeklyResetHints.get(authID)
}

func (r *weeklyResetHintRegistry) set(authID string, resetAt, observedAt time.Time) {
	authID = strings.TrimSpace(authID)
	if r == nil || authID == "" || resetAt.IsZero() {
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hints == nil {
		r.hints = make(map[string]weeklyResetHint)
	}
	existing, exists := r.hints[authID]
	if exists && observedAt.Before(existing.observedAt) {
		return
	}
	if !exists && len(r.hints) >= maxWeeklyResetHints {
		r.evictOldestLocked()
	}
	r.hints[authID] = weeklyResetHint{resetAt: resetAt, observedAt: observedAt}
}

func (r *weeklyResetHintRegistry) get(authID string) (time.Time, time.Time, bool) {
	if r == nil {
		return time.Time{}, time.Time{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	hint, ok := r.hints[strings.TrimSpace(authID)]
	return hint.resetAt, hint.observedAt, ok
}

// evictOldestLocked drops the hint with the oldest observation. Callers hold r.mu.
func (r *weeklyResetHintRegistry) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	for authID, hint := range r.hints {
		if oldestID == "" || hint.observedAt.Before(oldest) {
			oldestID, oldest = authID, hint.observedAt
		}
	}
	delete(r.hints, oldestID)
}

// resetBoost returns the weekly-reset boost of the scheduler's weighted selector, if any.
func (s *authScheduler) resetBoost() *weeklyResetBoost {
	if s == nil {
		return nil
	}
	return selectorResetBoost(s.weightedSelector)
}

// weeklyResetPoolLocked applies the weekly-reset weights to an unpinned weighted scheduler
// pool: auths, ids (sorted) and weights are parallel. A pool without the strategy is
// returned unchanged. Callers hold s.mu.
func (s *authScheduler) weeklyResetPoolLocked(auths []*Auth, ids []string, weights []int64) ([]*Auth, []string, []int64) {
	boost := s.resetBoost()
	if boost == nil || len(auths) == 0 {
		return auths, ids, weights
	}
	vector := make(map[string]int64, len(ids))
	latest := make([]*Auth, 0, len(auths))
	for index, auth := range auths {
		vector[ids[index]] = weights[index]
		// Shards outside a result's target models keep older snapshots, so read the quota
		// observation from the provider's latest one.
		if providerState := s.providers[s.authProviders[auth.ID]]; providerState != nil {
			if newest := providerState.auths[auth.ID]; newest != nil && newest.auth != nil {
				auth = newest.auth
			}
		}
		latest = append(latest, auth)
	}
	boost.boostAuthWeights(vector, latest)
	return weightedPoolFromVector(auths, vector)
}

// reserveWeeklyResetScoped is reserveScoped with the weekly-reset weights applied to an
// unbound pick. A bound pick (authID set) keeps the configured weights.
func (s *WeightedRoundRobinSelector) reserveWeeklyResetScoped(route string, priority int, transportClass string, available []*Auth, authID string) (*Auth, *weightedReservation) {
	if s.resetBoost == nil || authID != "" {
		return s.reserveScoped(route, priority, transportClass, available, authID)
	}
	vector := authWeightVector(available)
	s.resetBoost.boostAuthWeights(vector, available)
	auths, ids, weights := weightedPoolFromVector(available, vector)
	return s.reservePoolScoped(route, priority, transportClass, auths, weights, ids, authID)
}

// weightedPoolFromVector returns the candidates of auths that keep a positive weight in
// vector, with their IDs sorted and the parallel weights reservePoolScoped expects.
func weightedPoolFromVector(auths []*Auth, vector map[string]int64) ([]*Auth, []string, []int64) {
	kept := make([]*Auth, 0, len(vector))
	for _, auth := range auths {
		if auth != nil && vector[auth.ID] > 0 {
			kept = append(kept, auth)
		}
	}
	ids := make([]string, 0, len(vector))
	for authID, weight := range vector {
		if weight > 0 {
			ids = append(ids, authID)
		}
	}
	sort.Strings(ids)
	weights := make([]int64, len(ids))
	for index, authID := range ids {
		weights[index] = vector[authID]
	}
	return kept, ids, weights
}
