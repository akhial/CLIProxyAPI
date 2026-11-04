package auth

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/credentialweight"
)

const (
	// WeeklyResetBoostWindow is how close the weekly quota reset of a credential must be
	// for the weekly-reset weighted round-robin strategy to treat it as urgent.
	WeeklyResetBoostWindow = 36 * time.Hour
	// WeeklyResetBoostWeight is the effective weight of the most urgent credentials.
	WeeklyResetBoostWeight = credentialweight.Max

	// weeklyQuotaWindowMinutes identifies the weekly Codex window by its advertised length.
	weeklyQuotaWindowMinutes = 7 * 24 * 60
	// sessionWindowMinutes identifies the 5-hour Codex window by its advertised length.
	sessionWindowMinutes = 5 * 60
	// maxWeeklyResetHints bounds the hint registry so removed credentials cannot leak entries.
	maxWeeklyResetHints = 4096
)

// NewWeeklyResetWeightedRoundRobinSelector returns a weighted round-robin selector that
// sends each pick to the first of these tiers that has an available candidate:
//
//  1. the weekly quota resets within WeeklyResetBoostWindow and there is a 5-hour
//     session limit;
//  2. the weekly quota resets within WeeklyResetBoostWindow;
//  3. there is a 5-hour session limit;
//  4. there is no 5-hour session limit.
//
// In tiers 1 and 2 the credential whose weekly quota resets soonest takes every pick, so
// quota that would expire unused at the reset is spent first; credentials tied on that
// instant share evenly at WeeklyResetBoostWeight. In tiers 3 and 4 the configured weights
// apply. Session-limited credentials go first because they can spend only one session's
// worth at a time, while exempt ones empty faster and take the overflow. Only available
// candidates are ranked, so when the chosen ones cool down the next tier takes over.
// Credentials with a non-positive configured weight stay excluded.
func NewWeeklyResetWeightedRoundRobinSelector() *WeightedRoundRobinSelector {
	return &WeightedRoundRobinSelector{resetBoost: &weeklyResetBoost{}}
}

// routingTier orders candidates for the weekly-reset strategy; lower tiers go first.
type routingTier int

const (
	tierUrgentSessionLimited routingTier = iota
	tierUrgentExempt
	tierSessionLimited
	tierExempt
)

// urgent reports whether the tier ranks by weekly reset.
func (t routingTier) urgent() bool { return t <= tierUrgentExempt }

// weeklyResetBoost computes effective weights for the weekly-reset strategy.
type weeklyResetBoost struct {
	// now and the hint registries default to time.Now and the process-wide registries;
	// tests inject them.
	now          func() time.Time
	hints        *weeklyResetHintRegistry
	sessionHints *sessionLimitHintRegistry
}

// selectorResetBoost returns the weekly-reset boost of a weighted selector, if any.
func selectorResetBoost(selector Selector) *weeklyResetBoost {
	if weighted, ok := selector.(*WeightedRoundRobinSelector); ok && weighted != nil {
		return weighted.resetBoost
	}
	return nil
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

func (b *weeklyResetBoost) sessionRegistry() *sessionLimitHintRegistry {
	if b != nil && b.sessionHints != nil {
		return b.sessionHints
	}
	return defaultSessionLimitHints
}

// routingTier returns the tier of auth and, in the urgent tiers, its weekly reset.
func (b *weeklyResetBoost) routingTier(auth *Auth, now time.Time) (routingTier, time.Time) {
	exempt := b.sessionExempt(auth)
	if resetAt, ok := b.urgentResetAt(auth, now); ok {
		if exempt {
			return tierUrgentExempt, resetAt
		}
		return tierUrgentSessionLimited, resetAt
	}
	if exempt {
		return tierExempt, time.Time{}
	}
	return tierSessionLimited, time.Time{}
}

// urgentResetAt returns the weekly reset of auth when it is known and within
// WeeklyResetBoostWindow after now.
func (b *weeklyResetBoost) urgentResetAt(auth *Auth, now time.Time) (time.Time, bool) {
	resetAt, ok := b.weeklyResetAt(auth)
	if !ok {
		return time.Time{}, false
	}
	if remaining := resetAt.Sub(now); remaining <= 0 || remaining >= WeeklyResetBoostWindow {
		return time.Time{}, false
	}
	return resetAt, true
}

// rankAuthWeights rewrites the candidate weights in place so that only the best tier
// present is picked. Candidates are the auths with a positive weight. Those in a worse
// tier are removed. In the urgent tiers only the ones resetting soonest stay, raised to
// WeeklyResetBoostWeight; in the others the configured weights stay. Removed candidates
// keep their smooth weighted credits, as for any transient exclusion. It is a no-op for a
// selector without the weekly-reset strategy.
func (b *weeklyResetBoost) rankAuthWeights(weights map[string]int64, auths []*Auth) {
	if b == nil || len(weights) == 0 {
		return
	}
	now := b.clock()
	best := tierExempt + 1
	var chosen []string
	var earliest time.Time
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if weight, ok := weights[auth.ID]; !ok || weight <= 0 {
			continue
		}
		tier, resetAt := b.routingTier(auth, now)
		switch {
		case tier > best:
		case tier < best || (tier.urgent() && resetAt.Before(earliest)):
			best, chosen, earliest = tier, []string{auth.ID}, resetAt
		case !tier.urgent() || resetAt.Equal(earliest):
			chosen = append(chosen, auth.ID)
		}
	}
	if len(chosen) == 0 {
		return
	}
	keep := make(map[string]struct{}, len(chosen))
	for _, authID := range chosen {
		keep[authID] = struct{}{}
	}
	for authID := range weights {
		if _, ok := keep[authID]; !ok {
			delete(weights, authID)
		}
	}
	if best.urgent() {
		for _, authID := range chosen {
			weights[authID] = WeeklyResetBoostWeight
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

// sessionExempt reports whether auth is known to have no 5-hour session limit, from the
// more recent of its passive response-header snapshot and a registered hint. Unknown
// counts as limited: only a credential seen without the limit is exempt. On equal
// observation times the header snapshot wins.
func (b *weeklyResetBoost) sessionExempt(auth *Auth) bool {
	limited, ok := sessionLimitFromSignals(auth.Quota.Signals)
	hint, hintObservedAt, okHint := b.sessionRegistry().get(auth.ID)
	if okHint && (!ok || hintObservedAt.After(auth.Quota.ObservedAt)) {
		return !hint
	}
	return ok && !limited
}

// sessionLimitFromSignals reads from a passive quota snapshot whether the credential
// has a 5-hour session limit. Claude sends the unified 5h headers only on plans with
// the limit; Codex advertises a 300-minute window. ok is false when the snapshot does
// not describe the plan's windows.
func sessionLimitFromSignals(signals map[string]string) (limited, ok bool) {
	if len(signals) == 0 {
		return false, false
	}
	if quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-7d-Utilization") != "" ||
		quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-7d-Reset") != "" {
		return quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-5h-Utilization") != "" ||
			quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-5h-Reset") != "", true
	}
	if strings.HasPrefix(strings.ToLower(quotaSignalValue(signals, "X-Codex-Active-Limit")), "imagegen") {
		return false, false
	}
	sawWindow := false
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		minutes, okMinutes := parseQuotaSignalFloat(quotaSignalValue(signals, prefix+"Window-Minutes"))
		if !okMinutes {
			continue
		}
		sawWindow = true
		if math.Round(minutes) == sessionWindowMinutes {
			return true, true
		}
	}
	return false, sawWindow
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

// quotaHint is a quota observation made outside the request path, such as by a usage poller.
type quotaHint[V any] struct {
	value      V
	observedAt time.Time
}

// quotaHintRegistry stores the latest hint per credential.
type quotaHintRegistry[V any] struct {
	mu    sync.RWMutex
	hints map[string]quotaHint[V]
}

// weeklyResetHintRegistry stores weekly reset instants; zero instants are ignored.
type weeklyResetHintRegistry struct {
	quotaHintRegistry[time.Time]
}

// sessionLimitHintRegistry stores whether credentials have a 5-hour session limit.
type sessionLimitHintRegistry = quotaHintRegistry[bool]

var (
	defaultWeeklyResetHints  = &weeklyResetHintRegistry{}
	defaultSessionLimitHints = &sessionLimitHintRegistry{}
)

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

// SetSessionLimitHint records whether a credential has a 5-hour session limit, as
// observed outside the request path: a full usage read lists every window the plan has.
// Replacement and observedAt follow SetWeeklyResetHint.
func SetSessionLimitHint(authID string, limited bool, observedAt time.Time) {
	defaultSessionLimitHints.set(authID, limited, observedAt)
}

// SessionLimitHint returns the session-limit hint recorded for a credential.
func SessionLimitHint(authID string) (limited bool, observedAt time.Time, ok bool) {
	return defaultSessionLimitHints.get(authID)
}

func (r *weeklyResetHintRegistry) set(authID string, resetAt, observedAt time.Time) {
	if r == nil || resetAt.IsZero() {
		return
	}
	r.quotaHintRegistry.set(authID, resetAt, observedAt)
}

func (r *quotaHintRegistry[V]) set(authID string, value V, observedAt time.Time) {
	authID = strings.TrimSpace(authID)
	if r == nil || authID == "" {
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hints == nil {
		r.hints = make(map[string]quotaHint[V])
	}
	existing, exists := r.hints[authID]
	if exists && observedAt.Before(existing.observedAt) {
		return
	}
	if !exists && len(r.hints) >= maxWeeklyResetHints {
		r.evictOldestLocked()
	}
	r.hints[authID] = quotaHint[V]{value: value, observedAt: observedAt}
}

func (r *quotaHintRegistry[V]) get(authID string) (V, time.Time, bool) {
	var zero V
	if r == nil {
		return zero, time.Time{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	hint, ok := r.hints[strings.TrimSpace(authID)]
	return hint.value, hint.observedAt, ok
}

// evictOldestLocked drops the hint with the oldest observation. Callers hold r.mu.
func (r *quotaHintRegistry[V]) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	for authID, hint := range r.hints {
		if oldestID == "" || hint.observedAt.Before(oldest) {
			oldestID, oldest = authID, hint.observedAt
		}
	}
	delete(r.hints, oldestID)
}

// scheduledWeightFuncLocked returns the weekly-reset ranking for weighted scheduler
// picks, or nil when configured weights apply. Callers hold s.mu.
func (s *authScheduler) scheduledWeightFuncLocked(strategy schedulerStrategy) scheduledWeightFunc {
	if s == nil || s.resetBoost == nil || strategy != schedulerStrategyWeightedRoundRobin {
		return nil
	}
	boost := s.resetBoost
	return func(weights map[string]int64, entries []*scheduledAuth) {
		auths := make([]*Auth, 0, len(weights))
		for _, entry := range entries {
			if entry == nil || entry.auth == nil || entry.meta == nil {
				continue
			}
			if _, ok := weights[entry.auth.ID]; !ok {
				continue
			}
			auth := entry.auth
			// Shards outside a result's target models keep older snapshots, so read the
			// quota observation from the provider's latest one.
			if providerState := s.providers[entry.meta.providerKey]; providerState != nil {
				if latest := providerState.auths[auth.ID]; latest != nil && latest.auth != nil {
					auth = latest.auth
				}
			}
			auths = append(auths, auth)
		}
		boost.rankAuthWeights(weights, auths)
	}
}
