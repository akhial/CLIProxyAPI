package management

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Canonical quota-history window ids. Claude keys without a canonical id keep
// their raw payload key (for example seven_day_opus).
const (
	quotaHistoryWindowFiveHour = "five_hour"
	quotaHistoryWindowSevenDay = "seven_day"
	quotaHistoryWindowMonthly  = "monthly"
	// quotaHistoryWindowFable covers both the scoped Fable limit and the legacy
	// percent-based iguana_necktie key, which the management UI treats as one window.
	quotaHistoryWindowFable = "seven_day_fable"

	quotaHistoryFiveHourSeconds = 5 * 60 * 60
	quotaHistoryWeekSeconds     = 7 * 24 * 60 * 60
	quotaHistoryMinMonthSeconds = 28 * 24 * 60 * 60
	quotaHistoryMaxMonthSeconds = 31 * 24 * 60 * 60
)

// quotaHistoryWindow is one quota window of a recorded sample. Used is the
// used percentage (0..100) and ResetAt the reset instant in Unix milliseconds.
type quotaHistoryWindow struct {
	ID      string  `json:"id"`
	PeriodS int64   `json:"period_s"`
	Used    float64 `json:"used"`
	ResetAt int64   `json:"reset_at,omitempty"`
}

// claudeHistoryUsageKeys lists the percent-based plan windows of the Claude OAuth
// usage payload, mirroring the management UI. Other keys (extra_usage, dollar
// balances, unknown objects) are not plan windows.
var claudeHistoryUsageKeys = []string{
	"five_hour",
	"seven_day",
	"seven_day_oauth_apps",
	"seven_day_opus",
	"seven_day_sonnet",
	"seven_day_cowork",
}

// parseClaudeUsageHistoryWindows extracts plan windows from a Claude
// /api/oauth/usage response body.
func parseClaudeUsageHistoryWindows(body []byte) []quotaHistoryWindow {
	root := quotaHistoryJSONObject(body)
	if !root.Exists() {
		return nil
	}
	windows := make([]quotaHistoryWindow, 0, len(claudeHistoryUsageKeys)+1)
	for _, key := range claudeHistoryUsageKeys {
		if window, ok := claudeHistoryWindow(key, claudeHistoryPeriodSeconds(key), root.Get(key)); ok {
			windows = append(windows, window)
		}
	}

	fableLimit := findClaudeHistoryFableLimit(root.Get("limits"))
	legacyFable := root.Get("iguana_necktie")
	switch {
	case fableLimit.Exists():
		if used, ok := quotaHistoryNumber(fableLimit.Get("percent")); ok {
			windows = append(windows, quotaHistoryWindow{
				ID:      quotaHistoryWindowFable,
				PeriodS: quotaHistoryWeekSeconds,
				Used:    normalizeQuotaHistoryPercent(used),
				ResetAt: quotaHistoryResetMs(fableLimit.Get("resets_at")),
			})
		}
	case !claudeHistoryIsDollarWindow(legacyFable):
		// A dollar-denominated iguana_necktie is a credit pool, not a plan window.
		if window, ok := claudeHistoryWindow(quotaHistoryWindowFable, quotaHistoryWeekSeconds, legacyFable); ok {
			windows = append(windows, window)
		}
	}
	return windows
}

func claudeHistoryWindow(id string, periodS int64, window gjson.Result) (quotaHistoryWindow, bool) {
	if !window.IsObject() {
		return quotaHistoryWindow{}, false
	}
	used, ok := quotaHistoryNumber(window.Get("utilization"))
	if !ok {
		return quotaHistoryWindow{}, false
	}
	return quotaHistoryWindow{
		ID:      id,
		PeriodS: periodS,
		Used:    normalizeQuotaHistoryPercent(used),
		ResetAt: quotaHistoryResetMs(window.Get("resets_at")),
	}, true
}

// claudeHistoryPeriodSeconds derives the window length from the usage key:
// Claude states no period in the payload, five_hour is rolling, the rest are weekly.
func claudeHistoryPeriodSeconds(key string) int64 {
	if key == quotaHistoryWindowFiveHour {
		return quotaHistoryFiveHourSeconds
	}
	return quotaHistoryWeekSeconds
}

func claudeHistoryIsDollarWindow(window gjson.Result) bool {
	if !window.IsObject() {
		return false
	}
	for _, key := range []string{"limit_dollars", "used_dollars", "remaining_dollars"} {
		if _, ok := quotaHistoryNumber(window.Get(key)); ok {
			return true
		}
	}
	return false
}

// findClaudeHistoryFableLimit returns the weekly scoped Fable entry of the
// modern limits array, preferring the active one.
func findClaudeHistoryFableLimit(limits gjson.Result) gjson.Result {
	if !limits.IsArray() {
		return gjson.Result{}
	}
	var first gjson.Result
	for _, limit := range limits.Array() {
		kind := strings.ToLower(strings.TrimSpace(limit.Get("kind").String()))
		model := strings.ToLower(strings.TrimSpace(limit.Get("scope.model.display_name").String()))
		if kind != "weekly_scoped" || (model != "fable" && model != "fable 5") {
			continue
		}
		if _, ok := quotaHistoryNumber(limit.Get("percent")); !ok {
			continue
		}
		if limit.Get("is_active").Type == gjson.True {
			return limit
		}
		if !first.Exists() {
			first = limit
		}
	}
	return first
}

// parseClaudeProfilePlan derives the plan from a Claude /api/oauth/profile
// response body: team, max, pro, free, or "" when unknown.
func parseClaudeProfilePlan(body []byte) string {
	root := quotaHistoryJSONObject(body)
	if !root.Exists() {
		return ""
	}
	organizationType := strings.ToLower(strings.TrimSpace(root.Get("organization.organization_type").String()))
	subscriptionStatus := strings.ToLower(strings.TrimSpace(root.Get("organization.subscription_status").String()))
	if organizationType == "claude_team" && subscriptionStatus == "active" {
		return "team"
	}
	// Account flags include personal subscriptions even for a Team-scoped token.
	hasMax, maxKnown := quotaHistoryFlag(root.Get("account.has_claude_max"))
	if maxKnown && hasMax {
		return "max"
	}
	hasPro, proKnown := quotaHistoryFlag(root.Get("account.has_claude_pro"))
	if proKnown && hasPro {
		return "pro"
	}
	if maxKnown && proKnown {
		return "free"
	}
	return ""
}

// codexHistoryRawWindow is a Codex rate-limit window before classification.
type codexHistoryRawWindow struct {
	used    float64
	hasUsed bool
	periodS int64
	resetAt int64
}

// parseCodexUsageHistory extracts the main rate-limit windows and the plan type
// from a Codex /backend-api/wham/usage response body. now anchors relative
// reset_after_seconds values.
func parseCodexUsageHistory(body []byte, now time.Time) ([]quotaHistoryWindow, string) {
	root := quotaHistoryJSONObject(body)
	if !root.Exists() {
		return nil, ""
	}
	plan := strings.ToLower(strings.TrimSpace(quotaHistoryFirst(root, "plan_type", "planType").String()))
	rateLimit := quotaHistoryFirst(root, "rate_limit", "rateLimit")
	if !rateLimit.IsObject() {
		return nil, plan
	}
	primary := codexHistoryRawWindowFromJSON(quotaHistoryFirst(rateLimit, "primary_window", "primaryWindow"), now)
	secondary := codexHistoryRawWindowFromJSON(quotaHistoryFirst(rateLimit, "secondary_window", "secondaryWindow"), now)
	limitReached := quotaHistoryFirst(rateLimit, "limit_reached", "limitReached").Bool() ||
		rateLimit.Get("allowed").Type == gjson.False
	return classifyCodexHistoryWindows(primary, secondary, limitReached), plan
}

func codexHistoryRawWindowFromJSON(window gjson.Result, now time.Time) *codexHistoryRawWindow {
	if !window.IsObject() {
		return nil
	}
	raw := &codexHistoryRawWindow{}
	raw.used, raw.hasUsed = quotaHistoryNumber(quotaHistoryFirst(window, "used_percent", "usedPercent"))
	if seconds, ok := quotaHistoryNumber(quotaHistoryFirst(window, "limit_window_seconds", "limitWindowSeconds")); ok && seconds > 0 {
		raw.periodS = int64(math.Round(seconds))
	}
	raw.resetAt = quotaHistoryResetMs(window.Get("reset_at"))
	if raw.resetAt == 0 {
		raw.resetAt = quotaHistoryResetMs(window.Get("resetAt"))
	}
	if raw.resetAt == 0 {
		if after, ok := quotaHistoryNumber(quotaHistoryFirst(window, "reset_after_seconds", "resetAfterSeconds")); ok && after > 0 {
			raw.resetAt = now.Add(time.Duration(after * float64(time.Second))).UnixMilli()
		}
	}
	return raw
}

// classifyCodexHistoryWindows maps primary/secondary windows to canonical ids by
// their duration, falling back to primary=five_hour, secondary=weekly for
// payloads without one, exactly like the management UI.
func classifyCodexHistoryWindows(primary, secondary *codexHistoryRawWindow, limitReached bool) []quotaHistoryWindow {
	var fiveHour, weekly *codexHistoryRawWindow
	for _, window := range []*codexHistoryRawWindow{primary, secondary} {
		if window == nil {
			continue
		}
		switch {
		case window.periodS == quotaHistoryFiveHourSeconds && fiveHour == nil:
			fiveHour = window
		case (window.periodS == quotaHistoryWeekSeconds || isQuotaHistoryMonthly(window.periodS)) && weekly == nil:
			weekly = window
		}
	}
	if fiveHour == nil && primary != nil && primary != weekly {
		fiveHour = primary
	}
	if weekly == nil && secondary != nil && secondary != fiveHour {
		weekly = secondary
	}

	windows := make([]quotaHistoryWindow, 0, 2)
	if window, ok := codexHistoryWindow(quotaHistoryWindowFiveHour, quotaHistoryFiveHourSeconds, fiveHour, limitReached); ok {
		windows = append(windows, window)
	}
	weeklyID := quotaHistoryWindowSevenDay
	if weekly != nil && isQuotaHistoryMonthly(weekly.periodS) {
		weeklyID = quotaHistoryWindowMonthly
	}
	if window, ok := codexHistoryWindow(weeklyID, quotaHistoryWeekSeconds, weekly, limitReached); ok {
		windows = append(windows, window)
	}
	return windows
}

func codexHistoryWindow(id string, defaultPeriodS int64, raw *codexHistoryRawWindow, limitReached bool) (quotaHistoryWindow, bool) {
	if raw == nil {
		return quotaHistoryWindow{}, false
	}
	used := raw.used
	if !raw.hasUsed {
		// A reached limit without a percentage is a full window, but only when
		// it carries a reset instant.
		if !limitReached || raw.resetAt == 0 {
			return quotaHistoryWindow{}, false
		}
		used = 100
	}
	periodS := raw.periodS
	if periodS <= 0 {
		periodS = defaultPeriodS
	}
	return quotaHistoryWindow{
		ID:      id,
		PeriodS: periodS,
		Used:    normalizeQuotaHistoryPercent(used),
		ResetAt: raw.resetAt,
	}, true
}

func isQuotaHistoryMonthly(periodS int64) bool {
	return periodS >= quotaHistoryMinMonthSeconds && periodS <= quotaHistoryMaxMonthSeconds
}

// quotaHistoryWindowsFromSignals converts the passive rate-limit header snapshot
// stored in auth.Quota.Signals into windows. The Codex plan type is returned
// when the snapshot carries one.
func quotaHistoryWindowsFromSignals(provider string, signals map[string]string, observedAt time.Time) ([]quotaHistoryWindow, string) {
	if len(signals) == 0 {
		return nil, ""
	}
	lower := make(map[string]string, len(signals))
	for key, value := range signals {
		lower[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	switch provider {
	case "claude":
		return claudeHistoryWindowsFromSignals(lower), ""
	case "codex":
		return codexHistoryWindowsFromSignals(lower, observedAt), strings.ToLower(lower["x-codex-plan-type"])
	default:
		return nil, ""
	}
}

// claudeHistoryWindowsFromSignals reads the unified 5h/7d headers. Their
// utilization is a 0..1 fraction (see helps.isClaudeUtilizationHealthy) and
// the reset is a Unix timestamp in seconds.
func claudeHistoryWindowsFromSignals(signals map[string]string) []quotaHistoryWindow {
	specs := []struct {
		prefix  string
		id      string
		periodS int64
	}{
		{"anthropic-ratelimit-unified-5h-", quotaHistoryWindowFiveHour, quotaHistoryFiveHourSeconds},
		{"anthropic-ratelimit-unified-7d-", quotaHistoryWindowSevenDay, quotaHistoryWeekSeconds},
	}
	windows := make([]quotaHistoryWindow, 0, len(specs))
	for _, spec := range specs {
		fraction, ok := quotaHistoryParseFloat(signals[spec.prefix+"utilization"])
		if !ok || fraction < 0 {
			continue
		}
		windows = append(windows, quotaHistoryWindow{
			ID:      spec.id,
			PeriodS: spec.periodS,
			Used:    normalizeQuotaHistoryPercent(fraction * 100),
			ResetAt: parseQuotaHistoryTimestamp(signals[spec.prefix+"reset"]),
		})
	}
	return windows
}

// codexHistoryWindowsFromSignals reads the X-Codex-Primary-* / X-Codex-Secondary-*
// headers. Used-Percent is already 0..100, Window-Minutes is the period,
// Reset-At is Unix seconds and Reset-After-Seconds is relative to observedAt.
func codexHistoryWindowsFromSignals(signals map[string]string, observedAt time.Time) []quotaHistoryWindow {
	primary := codexHistoryRawWindowFromSignals(signals, "x-codex-primary-", observedAt)
	secondary := codexHistoryRawWindowFromSignals(signals, "x-codex-secondary-", observedAt)
	limitReached := strings.EqualFold(signals["x-codex-limit-reached"], "true") ||
		strings.EqualFold(signals["x-codex-allowed"], "false")
	return classifyCodexHistoryWindows(primary, secondary, limitReached)
}

func codexHistoryRawWindowFromSignals(signals map[string]string, prefix string, observedAt time.Time) *codexHistoryRawWindow {
	usedRaw := signals[prefix+"used-percent"]
	minutesRaw := signals[prefix+"window-minutes"]
	resetAtRaw := signals[prefix+"reset-at"]
	resetAfterRaw := signals[prefix+"reset-after-seconds"]
	if usedRaw == "" && minutesRaw == "" && resetAtRaw == "" && resetAfterRaw == "" {
		return nil
	}
	raw := &codexHistoryRawWindow{}
	raw.used, raw.hasUsed = quotaHistoryParseFloat(usedRaw)
	if minutes, ok := quotaHistoryParseFloat(minutesRaw); ok && minutes > 0 {
		raw.periodS = int64(math.Round(minutes * 60))
	}
	raw.resetAt = parseQuotaHistoryTimestamp(resetAtRaw)
	if raw.resetAt == 0 && !observedAt.IsZero() {
		if after, ok := quotaHistoryParseFloat(resetAfterRaw); ok && after > 0 {
			raw.resetAt = observedAt.Add(time.Duration(after * float64(time.Second))).UnixMilli()
		}
	}
	return raw
}

// normalizeQuotaHistoryPercent clamps a used percentage to 0..100 and rounds it
// to two decimals.
func normalizeQuotaHistoryPercent(value float64) float64 {
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	return math.Round(value*100) / 100
}

func quotaHistoryJSONObject(body []byte) gjson.Result {
	if !gjson.ValidBytes(body) {
		return gjson.Result{}
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return gjson.Result{}
	}
	return root
}

// quotaHistoryFirst returns the first key whose value is present and not null.
func quotaHistoryFirst(parent gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if value := parent.Get(key); value.Exists() && value.Type != gjson.Null {
			return value
		}
	}
	return gjson.Result{}
}

// quotaHistoryNumber accepts finite JSON numbers and numeric strings.
func quotaHistoryNumber(value gjson.Result) (float64, bool) {
	switch value.Type {
	case gjson.Number:
		number := value.Float()
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, false
		}
		return number, true
	case gjson.String:
		return quotaHistoryParseFloat(value.String())
	default:
		return 0, false
	}
}

func quotaHistoryParseFloat(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	number, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

func quotaHistoryFlag(value gjson.Result) (bool, bool) {
	switch value.Type {
	case gjson.True:
		return true, true
	case gjson.False:
		return false, true
	case gjson.Number:
		return value.Float() != 0, true
	case gjson.String:
		switch strings.ToLower(strings.TrimSpace(value.String())) {
		case "true", "1", "yes", "y", "on":
			return true, true
		case "false", "0", "no", "n", "off":
			return false, true
		}
	}
	return false, false
}

// quotaHistoryResetMs parses an ISO-8601 or Unix (seconds or milliseconds)
// reset instant into Unix milliseconds, or 0 when absent.
func quotaHistoryResetMs(value gjson.Result) int64 {
	switch value.Type {
	case gjson.String:
		return parseQuotaHistoryTimestamp(value.String())
	case gjson.Number:
		return quotaHistoryUnixToMs(value.Float())
	default:
		return 0
	}
}

// quotaHistoryOverPreciseFraction matches fractional seconds beyond nanosecond
// precision, which time.Parse rejects.
var quotaHistoryOverPreciseFraction = regexp.MustCompile(`(\.\d{9})\d+`)

func parseQuotaHistoryTimestamp(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if number, ok := quotaHistoryParseFloat(raw); ok {
		return quotaHistoryUnixToMs(number)
	}
	if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
		return parsed.UnixMilli()
	}
	if parsed, errParse := time.Parse(time.RFC3339Nano, quotaHistoryOverPreciseFraction.ReplaceAllString(raw, "$1")); errParse == nil {
		return parsed.UnixMilli()
	}
	return 0
}

// quotaHistoryUnixToMs disambiguates seconds from milliseconds by magnitude:
// seconds stay below 1e11 until the year 5138.
func quotaHistoryUnixToMs(value float64) int64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	if value < 1e11 {
		return int64(math.Round(value * 1000))
	}
	return int64(math.Round(value))
}
