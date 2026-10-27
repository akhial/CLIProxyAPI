package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func registerTestQuotaHistoryCollector(t *testing.T, h *Handler, collector *quotaHistoryCollector) {
	t.Helper()
	quotaHistoryCollectors.Lock()
	quotaHistoryCollectors.byHandler[h] = collector
	quotaHistoryCollectors.Unlock()
	t.Cleanup(func() {
		quotaHistoryCollectors.Lock()
		delete(quotaHistoryCollectors.byHandler, h)
		quotaHistoryCollectors.Unlock()
	})
}

func serveTestQuotaHistory(t *testing.T, h *Handler, query string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/quota/history?"+query, nil)
	h.GetQuotaHistory(ctx)
	var body map[string]any
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &body); errDecode != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), errDecode)
	}
	return recorder.Code, body
}

func TestGetQuotaHistory_DisabledShape(t *testing.T) {
	status, body := serveTestQuotaHistory(t, &Handler{}, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body["enabled"] != false || body["poll_interval_seconds"] != float64(0) {
		t.Fatalf("body = %v", body)
	}
	if accounts, ok := body["accounts"].([]any); !ok || len(accounts) != 0 {
		t.Fatalf("accounts = %#v, want an empty array", body["accounts"])
	}
	if now, ok := body["now"].(float64); !ok || now <= 0 {
		t.Fatalf("now = %v", body["now"])
	}
}

func TestGetQuotaHistory_RejectsBadParams(t *testing.T) {
	for _, query := range []string{"from=abc", "to=1.5", "to=-1", "to=0", "from=-5", "from=2000&to=1000"} {
		status, body := serveTestQuotaHistory(t, &Handler{}, query)
		if status != http.StatusBadRequest || body["error"] == nil {
			t.Errorf("%s: status = %d body = %v, want 400 with error", query, status, body)
		}
	}
}

func TestParseQuotaHistoryRange(t *testing.T) {
	now := time.UnixMilli(10_000_000_000)
	day := (24 * time.Hour).Milliseconds()
	tests := []struct {
		from, to         string
		wantFrom, wantTo int64
	}{
		{"", "", now.UnixMilli() - 14*day, now.UnixMilli()},
		{"", "9000000000", 9000000000 - 14*day, 9000000000},
		{"5", "", 10_000_000_000 - 45*day, now.UnixMilli()},
		{"9999999000", "", 9999999000, now.UnixMilli()},
		{"100", "100", 100, 100},
		{"", "1000", 0, 1000},
	}
	for _, tc := range tests {
		from, to, errRange := parseQuotaHistoryRange(tc.from, tc.to, now)
		if errRange != nil || from != tc.wantFrom || to != tc.wantTo {
			t.Errorf("from=%q to=%q: got %d..%d (%v), want %d..%d", tc.from, tc.to, from, to, errRange, tc.wantFrom, tc.wantTo)
		}
	}
}

func TestGetQuotaHistory_ResponseShape(t *testing.T) {
	clock := &fakeQuotaHistoryClock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	collector := newQuotaHistoryCollector(nil, newQuotaHistoryStore(t.TempDir()), 10*time.Minute)
	collector.nowFunc = clock.Now
	h := &Handler{}
	registerTestQuotaHistoryCollector(t, h, collector)

	now := clock.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	for _, sample := range []quotaHistorySample{
		// Outside the default 14-day range.
		{T: ms(-15 * 24 * time.Hour), AuthID: "claude-a", Provider: "claude", Plan: "free", Windows: []quotaHistoryWindow{{ID: "seven_day", PeriodS: 604800, Used: 99}}},
		{T: ms(-2 * time.Hour), AuthID: "claude-a", AuthIndex: "idx-old", FileName: "claude-a.json", Provider: "claude", Label: "old", Email: "a@x.com", Plan: "pro", Src: "poll",
			Windows: []quotaHistoryWindow{
				{ID: "seven_day_opus", PeriodS: 604800, Used: 3},
				{ID: "seven_day", PeriodS: 604800, Used: 41, ResetAt: 1761900000000},
				{ID: "five_hour", PeriodS: 18000, Used: 10},
			}},
		{T: ms(-time.Hour), AuthID: "claude-a", AuthIndex: "idx-new", FileName: "claude-a.json", Provider: "claude", Label: "new", Email: "a@x.com", Plan: "max", Src: "header",
			Windows: []quotaHistoryWindow{{ID: "seven_day", PeriodS: 604800, Used: 42}, {ID: "five_hour", PeriodS: 18000, Used: 20, ResetAt: 1761600000000}}},
		// Same timestamp recorded twice: the later line wins.
		{T: ms(-time.Hour), AuthID: "claude-a", AuthIndex: "idx-new", FileName: "claude-a.json", Provider: "claude", Label: "new", Email: "a@x.com", Plan: "max", Src: "header",
			Windows: []quotaHistoryWindow{{ID: "seven_day", PeriodS: 604800, Used: 42.5}}},
		{T: ms(-30 * time.Minute), AuthID: "codex-removed", AuthIndex: "idx-c", FileName: "codex-gone.json", Provider: "codex", Plan: "plus", Src: "poll",
			Windows: []quotaHistoryWindow{{ID: "monthly", PeriodS: 2592000, Used: 5, ResetAt: 1763000000000}}},
		// After "to".
		{T: ms(time.Hour), AuthID: "claude-a", Provider: "claude", Windows: []quotaHistoryWindow{{ID: "seven_day", PeriodS: 604800, Used: 1}}},
	} {
		if errAppend := collector.store.Append(sample); errAppend != nil {
			t.Fatal(errAppend)
		}
	}

	status, body := serveTestQuotaHistory(t, h, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %v", status, body)
	}
	if body["enabled"] != true || body["now"] != float64(now.UnixMilli()) || body["poll_interval_seconds"] != float64(600) {
		t.Fatalf("envelope = %v", body)
	}

	raw, errMarshal := json.Marshal(body["accounts"])
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var accounts []struct {
		AuthID    string `json:"auth_id"`
		AuthIndex string `json:"auth_index"`
		FileName  string `json:"file_name"`
		Provider  string `json:"provider"`
		Label     string `json:"label"`
		Email     string `json:"email"`
		Plan      string `json:"plan"`
		Windows   []struct {
			ID            string  `json:"id"`
			PeriodSeconds int64   `json:"period_seconds"`
			Points        [][]any `json:"points"`
		} `json:"windows"`
	}
	if errDecode := json.Unmarshal(raw, &accounts); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(accounts) != 2 || accounts[0].Provider != "claude" || accounts[1].Provider != "codex" {
		t.Fatalf("accounts = %+v", accounts)
	}

	claude := accounts[0]
	if claude.AuthID != "claude-a" || claude.AuthIndex != "idx-new" || claude.Label != "new" || claude.Plan != "max" ||
		claude.FileName != "claude-a.json" || claude.Email != "a@x.com" {
		t.Fatalf("claude metadata = %+v, want values from the latest sample", claude)
	}
	var windowIDs []string
	for _, window := range claude.Windows {
		windowIDs = append(windowIDs, window.ID)
	}
	if !reflect.DeepEqual(windowIDs, []string{"five_hour", "seven_day", "seven_day_opus"}) {
		t.Fatalf("window order = %v", windowIDs)
	}
	sevenDay := claude.Windows[1]
	wantPoints := [][]any{
		{float64(ms(-2 * time.Hour)), float64(41), float64(1761900000000)},
		{float64(ms(-time.Hour)), 42.5, nil},
	}
	if sevenDay.PeriodSeconds != 604800 || !reflect.DeepEqual(sevenDay.Points, wantPoints) {
		t.Fatalf("seven_day = %+v, want points %v", sevenDay, wantPoints)
	}
	if fiveHour := claude.Windows[0]; len(fiveHour.Points) != 2 || fiveHour.Points[1][2] != float64(1761600000000) {
		t.Fatalf("five_hour = %+v", fiveHour)
	}

	codex := accounts[1]
	if codex.AuthID != "codex-removed" || len(codex.Windows) != 1 || codex.Windows[0].ID != "monthly" || codex.Windows[0].PeriodSeconds != 2592000 {
		t.Fatalf("codex = %+v", codex)
	}

	// An explicit range narrows the points; a 45-day clamp keeps the request bounded.
	_, narrowed := serveTestQuotaHistory(t, h, "from="+strconv.FormatInt(ms(-90*time.Minute), 10)+"&to="+strconv.FormatInt(ms(-45*time.Minute), 10))
	if got := len(narrowed["accounts"].([]any)); got != 1 {
		t.Fatalf("narrowed accounts = %d, want 1", got)
	}
	_, wide := serveTestQuotaHistory(t, h, "from=0")
	if got := len(wide["accounts"].([]any)); got != 2 {
		t.Fatalf("wide accounts = %d, want 2", got)
	}
}

func TestQuotaHistoryPointMarshalJSON(t *testing.T) {
	data, errMarshal := json.Marshal([]quotaHistoryPoint{{T: 1761500000000, Used: 41, ResetAt: 1761900000000}, {T: 1761500600000, Used: 42.5}})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if got, want := string(data), `[[1761500000000,41,1761900000000],[1761500600000,42.5,null]]`; got != want {
		t.Fatalf("points = %s, want %s", got, want)
	}
}
