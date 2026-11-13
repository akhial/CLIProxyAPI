package management

import (
	"math"
	"sort"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Routing stats for the weekly-reset strategy, measured from recorded quota history.
const (
	// quotaHistoryStatsLookback is how much history the stats are measured over.
	quotaHistoryStatsLookback = 7 * 24 * time.Hour
	// quotaHistoryStatsInterval is how often the collector measures them again.
	quotaHistoryStatsInterval = 10 * time.Minute
	// quotaHistoryDrainWindow is the span the pool's traffic is averaged over. A day
	// evens out busy and quiet hours, so the drain errs low rather than high.
	quotaHistoryDrainWindow = 24 * time.Hour
	// quotaHistoryMinDrainSpan is the shortest history the drain is measured over.
	quotaHistoryMinDrainSpan = time.Hour
	// quotaHistorySessionRound groups 5-hour samples by their reset, which jitters by
	// milliseconds between samples.
	quotaHistorySessionRound = 10 * time.Minute
	// quotaHistoryMinSessionUse is how many points a 5-hour window must grow by before
	// the weekly share of a full window is measured from it.
	quotaHistoryMinSessionUse = 25.0
	// quotaHistoryRecentSessions is how recent the windows the session share is the
	// median of must be. Anthropic changes the ratio (it rose about 70 % on 2026-11-03),
	// so older windows do not count unless there is nothing newer.
	quotaHistoryRecentSessions = 48 * time.Hour
)

// quotaHistoryPlanUnits is a plan's weekly quota in Pro weeks: Pro 1, Max 5, Ultra 10.
// Unknown plans count as 1.
func quotaHistoryPlanUnits(provider, plan string) float64 {
	if provider == "claude" {
		switch plan {
		case "max5", "max":
			return 5
		case "max20":
			return 10
		}
	}
	return 1
}

// computeQuotaHistoryRoutingStats measures, per credential, the share of its week one
// full 5-hour window allows and how fast the traffic of its provider's pool, averaged
// over quotaHistoryDrainWindow before now, would spend its week if it took all of it.
func computeQuotaHistoryRoutingStats(samples []quotaHistorySample, now time.Time) map[string]coreauth.RoutingStats {
	sorted := append([]quotaHistorySample(nil), samples...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].T < sorted[j].T })

	type account struct {
		provider, plan string
		samples        []quotaHistorySample
	}
	accounts := make(map[string]*account)
	var order []string
	for _, sample := range sorted {
		if sample.AuthID == "" || sample.T > now.UnixMilli() {
			continue
		}
		entry := accounts[sample.AuthID]
		if entry == nil {
			entry = &account{}
			accounts[sample.AuthID] = entry
			order = append(order, sample.AuthID)
		}
		entry.provider = sample.Provider
		if sample.Plan != "" {
			entry.plan = sample.Plan
		}
		entry.samples = append(entry.samples, sample)
	}

	// The pool's traffic per provider, in Pro weeks per hour.
	burn := make(map[string]float64)
	drainFrom := now.Add(-quotaHistoryDrainWindow)
	earliest := now
	for _, id := range order {
		entry := accounts[id]
		if first := time.UnixMilli(entry.samples[0].T); first.Before(earliest) {
			earliest = first
		}
		burn[entry.provider] += quotaHistoryConsumed(entry.samples, drainFrom.UnixMilli()) / 100
	}
	span := now.Sub(drainFrom)
	if earliest.After(drainFrom) {
		span = now.Sub(earliest)
	}
	span = max(span, quotaHistoryMinDrainSpan)

	shares := make(map[string]float64)
	sharesByPlan := make(map[[2]string][]float64)
	for _, id := range order {
		entry := accounts[id]
		if share, ok := quotaHistorySessionShare(entry.samples, now); ok {
			shares[id] = share
			key := [2]string{entry.provider, entry.plan}
			sharesByPlan[key] = append(sharesByPlan[key], share)
		}
	}

	stats := make(map[string]coreauth.RoutingStats, len(order))
	for _, id := range order {
		entry := accounts[id]
		share, ok := shares[id]
		if !ok {
			// A credential without a measured window borrows its plan's.
			if others := sharesByPlan[[2]string{entry.provider, entry.plan}]; len(others) > 0 {
				share = quotaHistoryMedian(others)
			}
		}
		stats[id] = coreauth.RoutingStats{
			SessionSharePercent: share,
			DrainPercentPerHour: burn[entry.provider] / span.Hours() * 100 / quotaHistoryPlanUnits(entry.provider, entry.plan),
		}
	}
	return stats
}

// quotaHistoryConsumed adds up the weekly percentage points a credential used from
// fromMs on, in percent of a Pro week, each weighed by the plan of its sample. A
// changed weekly reset means the window reset in between, so all of the new window's
// use counts.
func quotaHistoryConsumed(samples []quotaHistorySample, fromMs int64) float64 {
	total := 0.0
	// Samples before the plan is first known count with that first plan.
	plan := ""
	for index := range samples {
		if plan = samples[index].Plan; plan != "" {
			break
		}
	}
	var prev *quotaHistoryWindow
	for index := range samples {
		if samples[index].Plan != "" {
			plan = samples[index].Plan
		}
		weekly := quotaHistorySampleWindow(&samples[index], quotaHistoryWindowSevenDay)
		if weekly == nil {
			continue
		}
		if prev != nil && samples[index].T >= fromMs {
			units := quotaHistoryPlanUnits(samples[index].Provider, plan)
			if quotaHistoryRoundedReset(weekly.ResetAt) != quotaHistoryRoundedReset(prev.ResetAt) {
				total += weekly.Used * units
			} else if weekly.Used > prev.Used {
				total += (weekly.Used - prev.Used) * units
			}
		}
		prev = weekly
	}
	return total
}

// quotaHistorySessionShare returns the median weekly share of a full 5-hour window over
// the measurable windows that ended within quotaHistoryRecentSessions before now, or
// the latest one when none did. A window counts when samples carrying both windows
// show it grow by quotaHistoryMinSessionUse points within one weekly window.
func quotaHistorySessionShare(samples []quotaHistorySample, now time.Time) (float64, bool) {
	type session struct {
		first, last [2]float64 // 5-hour used, weekly used
		weeklyKey   int64
		sameWeek    bool
		lastT       int64
	}
	sessions := make(map[int64]*session)
	var keys []int64
	for index := range samples {
		five := quotaHistorySampleWindow(&samples[index], quotaHistoryWindowFiveHour)
		weekly := quotaHistorySampleWindow(&samples[index], quotaHistoryWindowSevenDay)
		if five == nil || weekly == nil || five.ResetAt <= 0 {
			continue
		}
		key := quotaHistoryRoundedReset(five.ResetAt)
		weeklyKey := quotaHistoryRoundedReset(weekly.ResetAt)
		entry := sessions[key]
		if entry == nil {
			entry = &session{first: [2]float64{five.Used, weekly.Used}, weeklyKey: weeklyKey, sameWeek: true}
			sessions[key] = entry
			keys = append(keys, key)
		}
		if weeklyKey != entry.weeklyKey {
			entry.sameWeek = false
		}
		entry.last = [2]float64{five.Used, weekly.Used}
		entry.lastT = samples[index].T
	}
	sort.Slice(keys, func(i, j int) bool { return sessions[keys[i]].lastT < sessions[keys[j]].lastT })
	recentFrom := now.Add(-quotaHistoryRecentSessions).UnixMilli()
	var recent []float64
	latest, measured := 0.0, false
	for _, key := range keys {
		entry := sessions[key]
		sessionUse := entry.last[0] - entry.first[0]
		weeklyUse := entry.last[1] - entry.first[1]
		if !entry.sameWeek || sessionUse < quotaHistoryMinSessionUse || weeklyUse < 0 {
			continue
		}
		latest, measured = 100*weeklyUse/sessionUse, true
		if entry.lastT >= recentFrom {
			recent = append(recent, latest)
		}
	}
	if len(recent) > 0 {
		return quotaHistoryMedian(recent), true
	}
	return latest, measured
}

func quotaHistorySampleWindow(sample *quotaHistorySample, id string) *quotaHistoryWindow {
	for index := range sample.Windows {
		if sample.Windows[index].ID == id {
			return &sample.Windows[index]
		}
	}
	return nil
}

func quotaHistoryRoundedReset(resetAtMs int64) int64 {
	step := quotaHistorySessionRound.Milliseconds()
	return int64(math.Round(float64(resetAtMs) / float64(step)))
}

func quotaHistoryMedian(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// refreshRoutingStats measures the routing stats from the stored history and shares
// them with the weekly-reset strategy.
func (c *quotaHistoryCollector) refreshRoutingStats(now time.Time) {
	c.lastStats = now
	samples, errRead := c.store.Read(now.Add(-quotaHistoryStatsLookback), now)
	if errRead != nil {
		log.WithError(errRead).Warn("quota history: failed to read samples for routing stats")
		return
	}
	for authID, stats := range computeQuotaHistoryRoutingStats(samples, now) {
		coreauth.SetRoutingStats(authID, stats, now)
	}
}
