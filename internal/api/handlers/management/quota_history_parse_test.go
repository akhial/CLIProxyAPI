package management

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func quotaHistoryTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, errRead := os.ReadFile(filepath.Join("testdata", "quota_history", name))
	if errRead != nil {
		t.Fatalf("read fixture %s: %v", name, errRead)
	}
	return data
}

func quotaHistoryTestMs(t *testing.T, value string) int64 {
	t.Helper()
	parsed, errParse := time.Parse(time.RFC3339Nano, value)
	if errParse != nil {
		t.Fatalf("parse %q: %v", value, errParse)
	}
	return parsed.UnixMilli()
}

func TestParseClaudeUsageHistoryWindows_FrontendFixture(t *testing.T) {
	windows := parseClaudeUsageHistoryWindows(quotaHistoryTestdata(t, "claude-oauth-usage.json"))
	// extra_usage, the dollar-denominated iguana_necktie, additional_credit_balance
	// and unknown keys are not plan windows.
	want := []quotaHistoryWindow{
		{ID: "five_hour", PeriodS: 18000, Used: 42.5, ResetAt: quotaHistoryTestMs(t, "2026-10-09T18:00:00Z")},
		{ID: "seven_day", PeriodS: 604800, Used: 16, ResetAt: quotaHistoryTestMs(t, "2026-10-15T09:00:00Z")},
		{ID: "seven_day_sonnet", PeriodS: 604800, Used: 10, ResetAt: quotaHistoryTestMs(t, "2026-10-15T09:00:00Z")},
	}
	if !reflect.DeepEqual(windows, want) {
		t.Fatalf("windows = %+v, want %+v", windows, want)
	}
}

func TestParseClaudeUsageHistoryWindows_FableAndEdgeValues(t *testing.T) {
	modernReset := "2026-07-27T10:00:00.000000+00:00"
	tests := []struct {
		name string
		body string
		want []quotaHistoryWindow
	}{
		{
			name: "scoped fable limit",
			body: `{"limits":[{"kind":"weekly_scoped","percent":64,"resets_at":"` + modernReset + `","is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`,
			want: []quotaHistoryWindow{{ID: "seven_day_fable", PeriodS: 604800, Used: 64, ResetAt: quotaHistoryTestMs(t, modernReset)}},
		},
		{
			name: "active fable limit wins over inactive and legacy key",
			body: `{"iguana_necktie":{"utilization":5,"resets_at":null},"limits":[` +
				`{"kind":"weekly_scoped","percent":10,"is_active":false,"scope":{"model":{"display_name":"fable 5"}}},` +
				`{"kind":"weekly_scoped","percent":20,"is_active":true,"scope":{"model":{"display_name":"Fable"}}},` +
				`{"kind":"session","percent":99,"is_active":true,"scope":null}]}`,
			want: []quotaHistoryWindow{{ID: "seven_day_fable", PeriodS: 604800, Used: 20}},
		},
		{
			name: "legacy percent iguana_necktie",
			body: `{"iguana_necktie":{"utilization":"7.5","resets_at":"2026-11-05T07:59:00+00:00"}}`,
			want: []quotaHistoryWindow{{ID: "seven_day_fable", PeriodS: 604800, Used: 7.5, ResetAt: quotaHistoryTestMs(t, "2026-11-05T07:59:00+00:00")}},
		},
		{
			name: "null utilization skipped, overflow clamped, unix reset",
			body: `{"five_hour":{"utilization":null,"resets_at":null},"seven_day":{"utilization":104.2,"resets_at":1761900000},"seven_day_opus":{"utilization":0}}`,
			want: []quotaHistoryWindow{
				{ID: "seven_day", PeriodS: 604800, Used: 100, ResetAt: 1761900000000},
				{ID: "seven_day_opus", PeriodS: 604800, Used: 0},
			},
		},
		{name: "invalid json", body: `{"five_hour":`, want: nil},
		{name: "not an object", body: `[1,2]`, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseClaudeUsageHistoryWindows([]byte(tc.body))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("windows = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseClaudeProfilePlan(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{`{"account":{"has_claude_max":false,"has_claude_pro":true},"organization":{"organization_type":"claude_team","subscription_status":"active"}}`, "team"},
		{`{"account":{"has_claude_pro":true},"organization":{"organization_type":"CLAUDE_TEAM","subscription_status":"ACTIVE"}}`, "team"},
		{`{"account":{"has_claude_pro":true},"organization":{"organization_type":"claude_team","subscription_status":"past_due"}}`, "pro"},
		{`{"account":{"has_claude_max":true,"has_claude_pro":true}}`, "max"},
		{`{"account":{"has_claude_max":"false","has_claude_pro":1}}`, "pro"},
		{`{"account":{"has_claude_max":false,"has_claude_pro":false}}`, "free"},
		{`{"account":{"has_claude_max":false}}`, ""},
		{`{}`, ""},
		{`not json`, ""},
	}
	for _, tc := range tests {
		if got := parseClaudeProfilePlan([]byte(tc.body)); got != tc.want {
			t.Errorf("parseClaudeProfilePlan(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestParseCodexUsageHistory_FrontendFixture(t *testing.T) {
	windows, plan := parseCodexUsageHistory(quotaHistoryTestdata(t, "codex-wham-usage.json"), time.Unix(1785300000, 0))
	if plan != "pro" {
		t.Fatalf("plan = %q, want pro", plan)
	}
	// A lone 604800 s primary window is the weekly window; additional limits are not recorded.
	want := []quotaHistoryWindow{{ID: "seven_day", PeriodS: 604800, Used: 1, ResetAt: 1785902974000}}
	if !reflect.DeepEqual(windows, want) {
		t.Fatalf("windows = %+v, want %+v", windows, want)
	}
}

func TestParseCodexUsageHistory_Classification(t *testing.T) {
	now := time.UnixMilli(1761500000000)
	tests := []struct {
		name string
		body string
		want []quotaHistoryWindow
	}{
		{
			name: "five hour and weekly",
			body: `{"rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_after_seconds":600},` +
				`"secondary_window":{"used_percent":"12.5","limit_window_seconds":604800,"reset_at":1761900000}}}`,
			want: []quotaHistoryWindow{
				{ID: "five_hour", PeriodS: 18000, Used: 42, ResetAt: now.Add(600 * time.Second).UnixMilli()},
				{ID: "seven_day", PeriodS: 604800, Used: 12.5, ResetAt: 1761900000000},
			},
		},
		{
			name: "windows classified by duration, not order",
			body: `{"rateLimit":{"primaryWindow":{"usedPercent":3,"limitWindowSeconds":604800,"resetAt":1761900000},` +
				`"secondaryWindow":{"usedPercent":9,"limitWindowSeconds":18000,"resetAt":1761600000}}}`,
			want: []quotaHistoryWindow{
				{ID: "five_hour", PeriodS: 18000, Used: 9, ResetAt: 1761600000000},
				{ID: "seven_day", PeriodS: 604800, Used: 3, ResetAt: 1761900000000},
			},
		},
		{
			name: "monthly team window",
			body: `{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_at":1761600000},` +
				`"secondary_window":{"used_percent":30,"limit_window_seconds":2592000,"reset_at":1763000000}}}`,
			want: []quotaHistoryWindow{
				{ID: "five_hour", PeriodS: 18000, Used: 5, ResetAt: 1761600000000},
				{ID: "monthly", PeriodS: 2592000, Used: 30, ResetAt: 1763000000000},
			},
		},
		{
			name: "legacy payload without durations falls back to order",
			body: `{"rate_limit":{"primary_window":{"used_percent":11},"secondary_window":{"used_percent":22}}}`,
			want: []quotaHistoryWindow{
				{ID: "five_hour", PeriodS: 18000, Used: 11},
				{ID: "seven_day", PeriodS: 604800, Used: 22},
			},
		},
		{
			name: "limit reached without percentage is full when a reset is known",
			body: `{"rate_limit":{"limit_reached":true,"primary_window":{"limit_window_seconds":18000,"reset_after_seconds":60},` +
				`"secondary_window":{"limit_window_seconds":604800}}}`,
			want: []quotaHistoryWindow{
				{ID: "five_hour", PeriodS: 18000, Used: 100, ResetAt: now.Add(time.Minute).UnixMilli()},
			},
		},
		{
			name: "stated durations that are not plan windows are skipped",
			body: `{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":86400,"reset_at":1761600000},` +
				`"secondary_window":{"used_percent":0,"limit_window_seconds":0}}}`,
			want: nil,
		},
		{
			name: "missing rate limit",
			body: `{"plan_type":"plus"}`,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parseCodexUsageHistory([]byte(tc.body), now)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("windows = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestQuotaHistoryWindowsFromSignals_ClaudeFractionScale(t *testing.T) {
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":    "0.69",
		"Anthropic-Ratelimit-Unified-5h-Reset":          "1761510000",
		"Anthropic-Ratelimit-Unified-7d-Utilization":    "0.125",
		"Anthropic-Ratelimit-Unified-7d-Reset":          "1761900000",
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "1.02",
		"Anthropic-Ratelimit-Unified-Status":            "allowed",
		"Retry-After":                                   "30",
	}
	windows, plan := quotaHistoryWindowsFromSignals("claude", signals, time.UnixMilli(1761500000000))
	want := []quotaHistoryWindow{
		{ID: "five_hour", PeriodS: 18000, Used: 69, ResetAt: 1761510000000},
		{ID: "seven_day", PeriodS: 604800, Used: 12.5, ResetAt: 1761900000000},
	}
	if !reflect.DeepEqual(windows, want) || plan != "" {
		t.Fatalf("windows = %+v plan = %q, want %+v", windows, plan, want)
	}

	overLimit, _ := quotaHistoryWindowsFromSignals("claude", map[string]string{
		"anthropic-ratelimit-unified-7d-utilization": "1.02",
		"anthropic-ratelimit-unified-5h-utilization": "garbage",
	}, time.Time{})
	if len(overLimit) != 1 || overLimit[0].ID != "seven_day" || overLimit[0].Used != 100 || overLimit[0].ResetAt != 0 {
		t.Fatalf("over-limit windows = %+v", overLimit)
	}
}

func TestQuotaHistoryWindowsFromSignals_Codex(t *testing.T) {
	observedAt := time.UnixMilli(1761500000000)
	signals := map[string]string{
		"X-Codex-Plan-Type":                        "Plus",
		"X-Codex-Primary-Used-Percent":             "42",
		"X-Codex-Primary-Window-Minutes":           "300",
		"X-Codex-Primary-Reset-After-Seconds":      "600",
		"X-Codex-Secondary-Used-Percent":           "7.5",
		"X-Codex-Secondary-Window-Minutes":         "10080",
		"X-Codex-Secondary-Reset-At":               "1761900000",
		"X-Codex-Code-Review-Primary-Used-Percent": "99",
		"X-Codex-Bengalfox-Primary-Used-Percent":   "88",
		"X-Codex-Bengalfox-Primary-Window-Minutes": "10080",
		"X-Codex-Credits-Balance":                  "12",
	}
	windows, plan := quotaHistoryWindowsFromSignals("codex", signals, observedAt)
	want := []quotaHistoryWindow{
		{ID: "five_hour", PeriodS: 18000, Used: 42, ResetAt: observedAt.Add(10 * time.Minute).UnixMilli()},
		{ID: "seven_day", PeriodS: 604800, Used: 7.5, ResetAt: 1761900000000},
	}
	if !reflect.DeepEqual(windows, want) {
		t.Fatalf("windows = %+v, want %+v", windows, want)
	}
	if plan != "plus" {
		t.Fatalf("plan = %q, want plus", plan)
	}

	none, _ := quotaHistoryWindowsFromSignals("codex", map[string]string{"Retry-After": "5"}, observedAt)
	if len(none) != 0 {
		t.Fatalf("expected no windows without primary/secondary headers, got %+v", none)
	}
	unsupported, _ := quotaHistoryWindowsFromSignals("devin", signals, observedAt)
	if len(unsupported) != 0 {
		t.Fatalf("expected no windows for unsupported provider, got %+v", unsupported)
	}
}

func TestParseQuotaHistoryTimestamp(t *testing.T) {
	tests := []struct {
		raw  string
		want int64
	}{
		{"1761900000", 1761900000000},
		{"1761900000123", 1761900000123},
		{"2026-07-27T10:00:00Z", quotaHistoryTestMs(t, "2026-07-27T10:00:00Z")},
		{"2026-07-27T10:00:00.1234567891234+00:00", quotaHistoryTestMs(t, "2026-07-27T10:00:00.123Z")},
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"soon", 0},
	}
	for _, tc := range tests {
		if got := parseQuotaHistoryTimestamp(tc.raw); got != tc.want {
			t.Errorf("parseQuotaHistoryTimestamp(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// Production 2026-10-27: a gpt-image-2.5 request left image-limit headers on the
// Codex credential, and they were recorded as a 0% weekly window between 21% samples.
func TestQuotaHistoryWindowsFromSignals_CodexImageLimitIsNotPlanQuota(t *testing.T) {
	observedAt := time.UnixMilli(1793132609882)
	image := map[string]string{
		"X-Codex-Active-Limit":                  "imagegen_premium",
		"X-Codex-Plan-Type":                     "pro",
		"X-Codex-Primary-Used-Percent":          "0",
		"X-Codex-Primary-Window-Minutes":        "1440",
		"X-Codex-Primary-Reset-At":              "1793214612",
		"X-Codex-Secondary-Used-Percent":        "0",
		"X-Codex-Secondary-Window-Minutes":      "0",
		"X-Codex-Secondary-Reset-After-Seconds": "0",
	}
	if windows, _ := quotaHistoryWindowsFromSignals("codex", image, observedAt); len(windows) != 0 {
		t.Fatalf("image-limit headers produced windows %+v", windows)
	}
	delete(image, "X-Codex-Active-Limit")
	if windows, _ := quotaHistoryWindowsFromSignals("codex", image, observedAt); len(windows) != 0 {
		t.Fatalf("1-day and 0-minute windows produced %+v", windows)
	}

	text := map[string]string{
		"X-Codex-Active-Limit":                  "premium",
		"X-Codex-Primary-Used-Percent":          "21",
		"X-Codex-Primary-Window-Minutes":        "10080",
		"X-Codex-Primary-Reset-At":              "1793599704",
		"X-Codex-Secondary-Used-Percent":        "0",
		"X-Codex-Secondary-Window-Minutes":      "0",
		"X-Codex-Secondary-Reset-After-Seconds": "0",
	}
	windows, _ := quotaHistoryWindowsFromSignals("codex", text, observedAt)
	want := []quotaHistoryWindow{{ID: "seven_day", PeriodS: 604800, Used: 21, ResetAt: 1793599704000}}
	if !reflect.DeepEqual(windows, want) {
		t.Fatalf("text-limit windows = %+v, want %+v", windows, want)
	}
}
