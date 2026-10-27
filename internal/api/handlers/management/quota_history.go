package management

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	quotaHistoryDefaultRange = 14 * 24 * time.Hour
	quotaHistoryMaxRange     = 45 * 24 * time.Hour
)

type quotaHistoryResponse struct {
	Enabled             bool                  `json:"enabled"`
	Now                 int64                 `json:"now"`
	PollIntervalSeconds int64                 `json:"poll_interval_seconds"`
	Accounts            []quotaHistoryAccount `json:"accounts"`
}

type quotaHistoryAccount struct {
	AuthID    string                     `json:"auth_id"`
	AuthIndex string                     `json:"auth_index"`
	FileName  string                     `json:"file_name"`
	Provider  string                     `json:"provider"`
	Label     string                     `json:"label"`
	Email     string                     `json:"email"`
	Plan      string                     `json:"plan"`
	Windows   []quotaHistoryWindowSeries `json:"windows"`
}

type quotaHistoryWindowSeries struct {
	ID            string              `json:"id"`
	PeriodSeconds int64               `json:"period_seconds"`
	Points        []quotaHistoryPoint `json:"points"`
}

// quotaHistoryPoint encodes as [t_ms, used_percent, reset_at_ms|null].
type quotaHistoryPoint struct {
	T       int64
	Used    float64
	ResetAt int64
}

// MarshalJSON implements json.Marshaler.
func (p quotaHistoryPoint) MarshalJSON() ([]byte, error) {
	buf := make([]byte, 0, 48)
	buf = append(buf, '[')
	buf = strconv.AppendInt(buf, p.T, 10)
	buf = append(buf, ',')
	buf = strconv.AppendFloat(buf, p.Used, 'f', -1, 64)
	buf = append(buf, ',')
	if p.ResetAt > 0 {
		buf = strconv.AppendInt(buf, p.ResetAt, 10)
	} else {
		buf = append(buf, "null"...)
	}
	return append(buf, ']'), nil
}

// GetQuotaHistory returns the recorded Claude and Codex quota samples grouped by
// credential and window.
//
// Endpoint:
//
//	GET /v8/management/quota/history?from=<unix ms>&to=<unix ms>
//
// to defaults to now and from to 14 days before to; ranges longer than 45 days
// are clamped to the 45 days ending at to. Accounts include credentials that
// have since been removed. When the collector is disabled the response has
// enabled=false and no accounts.
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	collector := quotaHistoryCollectorFor(h)
	now := time.Now()
	if collector != nil {
		now = collector.nowFunc()
	}
	from, to, errRange := parseQuotaHistoryRange(c.Query("from"), c.Query("to"), now)
	if errRange != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errRange.Error()})
		return
	}
	response := quotaHistoryResponse{
		Now:      now.UnixMilli(),
		Accounts: []quotaHistoryAccount{},
	}
	if collector == nil {
		c.JSON(http.StatusOK, response)
		return
	}
	samples, errRead := collector.store.Read(time.UnixMilli(from), time.UnixMilli(to))
	if errRead != nil {
		log.WithError(errRead).Warn("quota history: failed to read samples")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read quota history"})
		return
	}
	response.Enabled = true
	response.PollIntervalSeconds = int64(collector.pollInterval / time.Second)
	response.Accounts = buildQuotaHistoryAccounts(samples)
	c.JSON(http.StatusOK, response)
}

// parseQuotaHistoryRange validates the from/to query values (Unix ms) and
// applies the defaults and the maximum range.
func parseQuotaHistoryRange(fromRaw, toRaw string, now time.Time) (int64, int64, error) {
	to := now.UnixMilli()
	if raw := strings.TrimSpace(toRaw); raw != "" {
		value, errParse := strconv.ParseInt(raw, 10, 64)
		if errParse != nil || value <= 0 {
			return 0, 0, errors.New("invalid to: expected unix milliseconds")
		}
		to = value
	}
	from := to - quotaHistoryDefaultRange.Milliseconds()
	if raw := strings.TrimSpace(fromRaw); raw != "" {
		value, errParse := strconv.ParseInt(raw, 10, 64)
		if errParse != nil || value < 0 {
			return 0, 0, errors.New("invalid from: expected unix milliseconds")
		}
		from = value
	}
	if from > to {
		return 0, 0, errors.New("invalid range: from is after to")
	}
	if to-from > quotaHistoryMaxRange.Milliseconds() {
		from = to - quotaHistoryMaxRange.Milliseconds()
	}
	if from < 0 {
		from = 0
	}
	return from, to, nil
}

// buildQuotaHistoryAccounts groups samples by credential and window. Points are
// sorted by time with one point per timestamp (the last recorded wins), and
// account metadata comes from the most recent sample.
func buildQuotaHistoryAccounts(samples []quotaHistorySample) []quotaHistoryAccount {
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].T < samples[j].T })

	type accountSeries struct {
		account quotaHistoryAccount
		windows map[string]*quotaHistoryWindowSeries
	}
	byAuthID := make(map[string]*accountSeries)
	for _, sample := range samples {
		series := byAuthID[sample.AuthID]
		if series == nil {
			series = &accountSeries{windows: make(map[string]*quotaHistoryWindowSeries)}
			byAuthID[sample.AuthID] = series
		}
		// Samples are sorted, so the last one seen is the most recent.
		series.account = quotaHistoryAccount{
			AuthID:    sample.AuthID,
			AuthIndex: sample.AuthIndex,
			FileName:  sample.FileName,
			Provider:  sample.Provider,
			Label:     sample.Label,
			Email:     sample.Email,
			Plan:      sample.Plan,
		}
		for _, window := range sample.Windows {
			if window.ID == "" {
				continue
			}
			windowSeries := series.windows[window.ID]
			if windowSeries == nil {
				windowSeries = &quotaHistoryWindowSeries{ID: window.ID}
				series.windows[window.ID] = windowSeries
			}
			if window.PeriodS > 0 {
				windowSeries.PeriodSeconds = window.PeriodS
			}
			point := quotaHistoryPoint{T: sample.T, Used: window.Used, ResetAt: window.ResetAt}
			if count := len(windowSeries.Points); count > 0 && windowSeries.Points[count-1].T == sample.T {
				windowSeries.Points[count-1] = point
				continue
			}
			windowSeries.Points = append(windowSeries.Points, point)
		}
	}

	accounts := make([]quotaHistoryAccount, 0, len(byAuthID))
	for _, series := range byAuthID {
		account := series.account
		account.Windows = make([]quotaHistoryWindowSeries, 0, len(series.windows))
		for _, windowSeries := range series.windows {
			account.Windows = append(account.Windows, *windowSeries)
		}
		sort.Slice(account.Windows, func(i, j int) bool {
			ri, rj := quotaHistoryWindowRank(account.Windows[i].ID), quotaHistoryWindowRank(account.Windows[j].ID)
			if ri != rj {
				return ri < rj
			}
			return account.Windows[i].ID < account.Windows[j].ID
		})
		accounts = append(accounts, account)
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Provider != accounts[j].Provider {
			return accounts[i].Provider < accounts[j].Provider
		}
		if accounts[i].FileName != accounts[j].FileName {
			return accounts[i].FileName < accounts[j].FileName
		}
		return accounts[i].AuthID < accounts[j].AuthID
	})
	return accounts
}

func quotaHistoryWindowRank(id string) int {
	switch id {
	case quotaHistoryWindowFiveHour:
		return 0
	case quotaHistoryWindowSevenDay:
		return 1
	case quotaHistoryWindowMonthly:
		return 2
	default:
		return 3
	}
}
