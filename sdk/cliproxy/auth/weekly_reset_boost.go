package auth

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// WeeklyResetTieBand is how close to the earliest effective deadline another
	// credential's must be to count as tied; among tied credentials the session-limited
	// ones go first.
	WeeklyResetTieBand = time.Hour

	// weeklyQuotaWindowMinutes identifies the weekly Codex window by its advertised length.
	weeklyQuotaWindowMinutes = 7 * 24 * 60
	// sessionWindowMinutes identifies the 5-hour Codex window by its advertised length.
	sessionWindowMinutes = 5 * 60
	// sessionWindowHours is the length of a 5-hour session window.
	sessionWindowHours = 5.0
	// weeklyQuotaPeriod is the length of the weekly quota window.
	weeklyQuotaPeriod = 7 * 24 * time.Hour
	// maxCapDelay bounds the deadline adjustment of a session-limited credential.
	maxCapDelay = 2 * weeklyQuotaPeriod
	// maxWeeklyResetHints bounds the hint registries so removed credentials cannot leak entries.
	maxWeeklyResetHints = 4096
)

// NewWeeklyResetWeightedRoundRobinSelector returns a weighted round-robin selector that
// sends each pick to the available credential with the earliest effective deadline, so
// weekly quota is spent in the order it would otherwise expire unused.
//
// The effective deadline is the credential's weekly reset. For a credential with a
// 5-hour session limit it is moved earlier by the extra time the 5-hour windows add to
// spending what is left of its week, at the rate the pool's traffic (averaged over a
// day) would drain it. Under light traffic that adds nothing and the order is simply
// earliest reset first; under heavy traffic a session-limited credential starts early
// enough to use its windows in time. Credentials within WeeklyResetTieBand of the
// earliest deadline count as tied, and then the session-limited ones go first. A
// credential whose weekly reset is unknown ranks after every credential whose reset is
// known. Exact ties share by configured weight; credentials with a non-positive weight
// stay excluded. Only available candidates are ranked, so when the chosen credential
// cools down the next one takes over.
func NewWeeklyResetWeightedRoundRobinSelector() *WeightedRoundRobinSelector {
	return &WeightedRoundRobinSelector{resetBoost: &weeklyResetBoost{}}
}

// WeeklyQuota is an observation of a credential's weekly quota window.
type WeeklyQuota struct {
	ResetAt time.Time
	// UsedPercent is the share of the week used, 0..100, or negative when unknown.
	UsedPercent float64
}

// SessionQuota is an observation of a credential's 5-hour session window.
type SessionQuota struct {
	// Limited reports whether the plan has the 5-hour window.
	Limited bool
	// UsedPercent is the share of the current window used, 0..100, or negative when unknown.
	UsedPercent float64
	// ResetAt is the end of the current window, zero when unknown.
	ResetAt time.Time
}

// RoutingStats are measured from recorded quota history.
type RoutingStats struct {
	// SessionSharePercent is the share of the week that one full 5-hour window allows.
	SessionSharePercent float64
	// DrainPercentPerHour is how fast the pool's traffic, averaged over a day, would
	// spend this credential's week if it took all of it.
	DrainPercentPerHour float64
}

// weeklyResetBoost ranks credentials for the weekly-reset strategy.
type weeklyResetBoost struct {
	// now and the hint registries default to time.Now and the process-wide registries;
	// tests inject them.
	now          func() time.Time
	weeklyHints  *quotaHintRegistry[WeeklyQuota]
	sessionHints *quotaHintRegistry[SessionQuota]
	stats        *quotaHintRegistry[RoutingStats]
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

func (b *weeklyResetBoost) weeklyRegistry() *quotaHintRegistry[WeeklyQuota] {
	if b != nil && b.weeklyHints != nil {
		return b.weeklyHints
	}
	return defaultWeeklyQuotaHints
}

func (b *weeklyResetBoost) sessionRegistry() *quotaHintRegistry[SessionQuota] {
	if b != nil && b.sessionHints != nil {
		return b.sessionHints
	}
	return defaultSessionQuotaHints
}

func (b *weeklyResetBoost) statsRegistry() *quotaHintRegistry[RoutingStats] {
	if b != nil && b.stats != nil {
		return b.stats
	}
	return defaultRoutingStats
}

// routingRank is where the weekly-reset strategy places a credential.
type routingRank struct {
	// deadline is the effective deadline, zero when the weekly reset is unknown.
	deadline time.Time
	// limited reports a 5-hour session limit; unknown counts as limited.
	limited bool
}

// rank places auth at now.
func (b *weeklyResetBoost) rank(auth *Auth, now time.Time) routingRank {
	session, sessionKnown := b.sessionQuota(auth)
	rank := routingRank{limited: !sessionKnown || session.Limited}
	weekly, ok := b.weeklyQuota(auth)
	if !ok {
		return rank
	}
	resetAt, used := weekly.ResetAt, weekly.UsedPercent
	if !resetAt.After(now) {
		// The observed window has ended: the next one started with nothing used.
		resetAt = resetAt.Add((now.Sub(resetAt)/weeklyQuotaPeriod + 1) * weeklyQuotaPeriod)
		used = 0
	}
	if used < 0 {
		used = 0
	}
	if rank.limited {
		rank.deadline = resetAt.Add(-b.capDelay(auth, session, sessionKnown, used, now))
	} else {
		rank.deadline = resetAt
	}
	return rank
}

// capDelay is how much longer the 5-hour windows make spending the rest of a
// session-limited credential's week, at the drain measured for it. It is zero when the
// session share or the drain is not known yet.
func (b *weeklyResetBoost) capDelay(auth *Auth, session SessionQuota, sessionKnown bool, used float64, now time.Time) time.Duration {
	stats, _, ok := b.statsRegistry().get(auth.ID)
	share, drain := stats.SessionSharePercent, stats.DrainPercentPerHour
	left := 100 - used
	if !ok || share <= 0 || drain <= 0 || left <= 0 {
		return 0
	}
	room, end := share, sessionWindowHours
	if sessionKnown && session.ResetAt.After(now) && session.UsedPercent >= 0 {
		room = share * math.Max(0, 100-session.UsedPercent) / 100
		end = session.ResetAt.Sub(now).Hours()
	}
	extra := hoursToSpend(left, drain, share, room, end) - left/drain
	if extra <= 0 {
		return 0
	}
	delay := time.Duration(extra * float64(time.Hour))
	if delay > maxCapDelay {
		return maxCapDelay
	}
	return delay
}

// hoursToSpend returns how many hours spending left (% of the week) takes at drain %/h
// when each 5-hour window allows at most share %. The current window has room % left
// and ends in end hours; each later window starts when the previous one ends.
func hoursToSpend(left, drain, share, room, end float64) float64 {
	elapsed := 0.0
	for range 1000 {
		window := math.Min(room, drain*(end-elapsed))
		if left <= window {
			return elapsed + left/drain
		}
		left -= math.Max(0, window)
		elapsed, room, end = end, share, end+sessionWindowHours
	}
	return elapsed + left/math.Min(drain, share/sessionWindowHours)
}

// rankAuthWeights rewrites the candidate weights in place so that only the credentials
// with the earliest effective deadline are picked; see
// NewWeeklyResetWeightedRoundRobinSelector. Candidates are the auths with a positive
// weight. The chosen ones keep their configured weights, and the others are removed,
// keeping their smooth weighted credits as for any transient exclusion. Nothing changes
// when no candidate's weekly reset is known, or for a selector without the strategy.
func (b *weeklyResetBoost) rankAuthWeights(weights map[string]int64, auths []*Auth) {
	if b == nil || len(weights) == 0 {
		return
	}
	now := b.clock()
	type ranked struct {
		id string
		routingRank
	}
	known := make([]ranked, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if weight, ok := weights[auth.ID]; !ok || weight <= 0 {
			continue
		}
		if rank := b.rank(auth, now); !rank.deadline.IsZero() {
			known = append(known, ranked{id: auth.ID, routingRank: rank})
		}
	}
	if len(known) == 0 {
		return
	}
	earliest := known[0].deadline
	for _, candidate := range known[1:] {
		if candidate.deadline.Before(earliest) {
			earliest = candidate.deadline
		}
	}
	band := make([]ranked, 0, len(known))
	limitedInBand := false
	for _, candidate := range known {
		if !candidate.deadline.After(earliest.Add(WeeklyResetTieBand)) {
			band = append(band, candidate)
			limitedInBand = limitedInBand || candidate.limited
		}
	}
	var first time.Time
	chosen := make(map[string]struct{}, len(band))
	for _, candidate := range band {
		if limitedInBand && !candidate.limited {
			continue
		}
		switch {
		case len(chosen) == 0 || candidate.deadline.Before(first):
			first = candidate.deadline
			chosen = map[string]struct{}{candidate.id: {}}
		case candidate.deadline.Equal(first):
			chosen[candidate.id] = struct{}{}
		}
	}
	for authID := range weights {
		if _, ok := chosen[authID]; !ok {
			delete(weights, authID)
		}
	}
}

// weeklyQuota returns the most recently observed weekly window of auth, from its
// passive response-header snapshot or from a registered hint. On equal observation
// times the header snapshot wins.
func (b *weeklyResetBoost) weeklyQuota(auth *Auth) (WeeklyQuota, bool) {
	quota, ok := weeklyQuotaFromSignals(auth.Quota.Signals, auth.Quota.ObservedAt)
	hint, hintObservedAt, okHint := b.weeklyRegistry().get(auth.ID)
	if okHint && (!ok || hintObservedAt.After(auth.Quota.ObservedAt)) {
		return hint, true
	}
	return quota, ok
}

// sessionQuota returns the most recently observed 5-hour window of auth, from its
// passive response-header snapshot or from a registered hint. ok is false when neither
// says whether the plan has the window. On equal observation times the header snapshot
// wins.
func (b *weeklyResetBoost) sessionQuota(auth *Auth) (SessionQuota, bool) {
	quota, ok := sessionQuotaFromSignals(auth.Quota.Signals, auth.Quota.ObservedAt)
	hint, hintObservedAt, okHint := b.sessionRegistry().get(auth.ID)
	if okHint && (!ok || hintObservedAt.After(auth.Quota.ObservedAt)) {
		return hint, true
	}
	return quota, ok
}

// sessionQuotaFromSignals reads the 5-hour window from a passive quota snapshot.
// Claude sends the unified 5h headers only on plans with the limit; Codex advertises a
// 300-minute window. ok is false when the snapshot does not describe the plan's windows.
func sessionQuotaFromSignals(signals map[string]string, observedAt time.Time) (SessionQuota, bool) {
	none := SessionQuota{UsedPercent: -1}
	if len(signals) == 0 {
		return none, false
	}
	if quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-7d-Utilization") != "" ||
		quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-7d-Reset") != "" {
		used := quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-5h-Utilization")
		reset := quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-5h-Reset")
		if used == "" && reset == "" {
			return none, true
		}
		quota := SessionQuota{Limited: true, UsedPercent: claudeUsedPercent(used)}
		quota.ResetAt, _ = parseQuotaResetInstant(reset)
		return quota, true
	}
	if codexImageLimit(signals) {
		return none, false
	}
	sawWindow := false
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		minutes, okMinutes := parseQuotaSignalFloat(quotaSignalValue(signals, prefix+"Window-Minutes"))
		if !okMinutes {
			continue
		}
		sawWindow = true
		if math.Round(minutes) == sessionWindowMinutes {
			return SessionQuota{
				Limited:     true,
				UsedPercent: codexUsedPercent(signals, prefix),
				ResetAt:     codexResetAt(signals, prefix, observedAt),
			}, true
		}
	}
	return none, sawWindow
}

// weeklyQuotaFromSignals reads the weekly window from a passive quota snapshot: the
// Claude unified 7d window, or whichever Codex primary/secondary window spans a week.
// ok requires a reset instant.
func weeklyQuotaFromSignals(signals map[string]string, observedAt time.Time) (WeeklyQuota, bool) {
	if len(signals) == 0 {
		return WeeklyQuota{}, false
	}
	if resetAt, ok := parseQuotaResetInstant(quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-7d-Reset")); ok {
		used := claudeUsedPercent(quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-7d-Utilization"))
		return WeeklyQuota{ResetAt: resetAt, UsedPercent: used}, true
	}
	// Image-generation responses describe their own limit, not the plan's weekly window.
	if codexImageLimit(signals) {
		return WeeklyQuota{}, false
	}
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		minutes, ok := parseQuotaSignalFloat(quotaSignalValue(signals, prefix+"Window-Minutes"))
		if !ok || math.Round(minutes) != weeklyQuotaWindowMinutes {
			continue
		}
		if resetAt := codexResetAt(signals, prefix, observedAt); !resetAt.IsZero() {
			return WeeklyQuota{ResetAt: resetAt, UsedPercent: codexUsedPercent(signals, prefix)}, true
		}
	}
	return WeeklyQuota{}, false
}

// weeklyResetFromSignals reads only the weekly reset instant; see weeklyQuotaFromSignals.
func weeklyResetFromSignals(signals map[string]string, observedAt time.Time) (time.Time, bool) {
	quota, ok := weeklyQuotaFromSignals(signals, observedAt)
	return quota.ResetAt, ok
}

// sessionLimitFromSignals reads only whether the plan has the 5-hour window; see
// sessionQuotaFromSignals.
func sessionLimitFromSignals(signals map[string]string) (limited, ok bool) {
	quota, ok := sessionQuotaFromSignals(signals, time.Time{})
	return quota.Limited, ok
}

func codexImageLimit(signals map[string]string) bool {
	return strings.HasPrefix(strings.ToLower(quotaSignalValue(signals, "X-Codex-Active-Limit")), "imagegen")
}

// codexResetAt reads Reset-At, or Reset-After-Seconds relative to observedAt.
func codexResetAt(signals map[string]string, prefix string, observedAt time.Time) time.Time {
	if resetAt, ok := parseQuotaResetInstant(quotaSignalValue(signals, prefix+"Reset-At")); ok {
		return resetAt
	}
	if after, ok := parseQuotaSignalFloat(quotaSignalValue(signals, prefix+"Reset-After-Seconds")); ok && after >= 0 && !observedAt.IsZero() {
		return observedAt.Add(time.Duration(after * float64(time.Second)))
	}
	return time.Time{}
}

// claudeUsedPercent converts a unified utilization (a 0..1 fraction) to percent, or -1.
func claudeUsedPercent(raw string) float64 {
	fraction, ok := parseQuotaSignalFloat(raw)
	if !ok || fraction < 0 {
		return -1
	}
	return math.Min(100, fraction*100)
}

// codexUsedPercent reads a Codex Used-Percent (already 0..100), or -1.
func codexUsedPercent(signals map[string]string, prefix string) float64 {
	used, ok := parseQuotaSignalFloat(quotaSignalValue(signals, prefix+"Used-Percent"))
	if !ok || used < 0 {
		return -1
	}
	return math.Min(100, used)
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

var (
	defaultWeeklyQuotaHints  = &quotaHintRegistry[WeeklyQuota]{}
	defaultSessionQuotaHints = &quotaHintRegistry[SessionQuota]{}
	defaultRoutingStats      = &quotaHintRegistry[RoutingStats]{}
)

// SetWeeklyQuotaHint records a credential's weekly window as observed by a source
// outside the request path, such as a usage poller. A hint replaces an existing one
// only if it was observed at the same time or later; a zero observedAt means now. A
// zero ResetAt is ignored. The weekly-reset routing strategy combines it with the
// credential's response-header snapshot, preferring the most recent observation.
func SetWeeklyQuotaHint(authID string, quota WeeklyQuota, observedAt time.Time) {
	if quota.ResetAt.IsZero() {
		return
	}
	defaultWeeklyQuotaHints.set(authID, quota, observedAt)
}

// WeeklyQuotaHint returns the weekly window hint recorded for a credential.
func WeeklyQuotaHint(authID string) (WeeklyQuota, time.Time, bool) {
	return defaultWeeklyQuotaHints.get(authID)
}

// SetSessionQuotaHint records a credential's 5-hour window as observed outside the
// request path: a full usage read lists every window the plan has. Replacement and
// observedAt follow SetWeeklyQuotaHint.
func SetSessionQuotaHint(authID string, quota SessionQuota, observedAt time.Time) {
	defaultSessionQuotaHints.set(authID, quota, observedAt)
}

// SessionQuotaHint returns the 5-hour window hint recorded for a credential.
func SessionQuotaHint(authID string) (SessionQuota, time.Time, bool) {
	return defaultSessionQuotaHints.get(authID)
}

// SetRoutingStats records the session share and drain measured for a credential.
// Replacement and observedAt follow SetWeeklyQuotaHint.
func SetRoutingStats(authID string, stats RoutingStats, observedAt time.Time) {
	defaultRoutingStats.set(authID, stats, observedAt)
}

// RoutingStatsFor returns the routing stats recorded for a credential.
func RoutingStatsFor(authID string) (RoutingStats, time.Time, bool) {
	return defaultRoutingStats.get(authID)
}

// WeeklyResetRouting reports where the weekly-reset strategy places auth at now: its
// effective deadline, zero when its weekly reset is unknown, and whether it counts as
// session-limited. Availability is not considered.
func WeeklyResetRouting(auth *Auth, now time.Time) (deadline time.Time, sessionLimited bool) {
	if auth == nil {
		return time.Time{}, false
	}
	rank := (&weeklyResetBoost{}).rank(auth, now)
	return rank.deadline, rank.limited
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
