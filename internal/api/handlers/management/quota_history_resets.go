package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	log "github.com/sirupsen/logrus"
)

const (
	// quotaHistoryClaudeResetGrantsURL is the usage endpoint with the banked-reset
	// (cedar_ember) block, as the management UI reads it.
	quotaHistoryClaudeResetGrantsURL = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	quotaHistoryResetGrantsFileName  = "reset-grants.json"
	// quotaHistoryResetGrantsRetry spaces reads of a credential whose last read failed.
	quotaHistoryResetGrantsRetry = 30 * time.Minute
)

var (
	claudeResetGrantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	// claudeResetClaimPath matches the endpoint that spends a banked reset.
	claudeResetClaimPath = regexp.MustCompile(`^/api/organizations/[^/]+/reset_rate_limits/?$`)
	// claudeResetWindows are the usage windows a grant can clear; others are dropped.
	claudeResetWindows = []string{"five_hour", "seven_day", "seven_day_overage_included"}
)

// claudeResetGrant is one banked-reset grant of a Claude account. Times are Unix ms.
type claudeResetGrant struct {
	ID          string   `json:"id"`
	Label       string   `json:"label,omitempty"`
	ResetsTotal int64    `json:"resets_total"`
	ResetsLeft  int64    `json:"resets_left"`
	StartsAt    int64    `json:"starts_at,omitempty"`
	EndsAt      int64    `json:"ends_at,omitempty"`
	Clears      []string `json:"clears"`
	Paused      bool     `json:"paused"`
	// GivesBackWeekly marks a Fable reset: it refills the Fable limit and returns what
	// Fable used to the weekly limit, instead of clearing the account's limits.
	GivesBackWeekly bool `json:"gives_back_weekly"`
}

// claudeResetGrants is the saved banked-reset state of one Claude account. It only
// changes when a reset is spent, so it is read once and again after each claim.
type claudeResetGrants struct {
	CheckedAt        int64              `json:"checked_at"`
	Eligible         bool               `json:"eligible"`
	IneligibleReason string             `json:"ineligible_reason,omitempty"`
	CooldownUntil    int64              `json:"cooldown_until,omitempty"`
	Grants           []claudeResetGrant `json:"grants"`
}

// parseClaudeResetGrants reads the cedar_ember block of a usage response with the
// management UI's rules: a malformed grant, a duplicate id or a bad timestamp
// rejects the whole block.
func parseClaudeResetGrants(body []byte, now time.Time) (claudeResetGrants, error) {
	root := quotaHistoryJSONObject(body)
	if !root.Exists() {
		return claudeResetGrants{}, errors.New("usage response is not a JSON object")
	}
	block := root.Get("cedar_ember")
	if !block.Exists() || block.Type == ojson.Null {
		// Accounts outside the reset program get no block: nothing is banked.
		return claudeResetGrants{CheckedAt: now.UnixMilli(), IneligibleReason: "no_program", Grants: []claudeResetGrant{}}, nil
	}
	if !block.IsObject() {
		return claudeResetGrants{}, errors.New("malformed reset-grant block")
	}
	eligible := block.Get("eligible")
	if !eligible.IsBool() {
		return claudeResetGrants{}, errors.New("reset-grant block has no eligibility")
	}
	result := claudeResetGrants{
		CheckedAt: now.UnixMilli(),
		Eligible:  eligible.Bool(),
		Grants:    []claudeResetGrant{},
	}
	if reason := block.Get("ineligible_reason"); reason.Type == ojson.String {
		result.IneligibleReason = safeResetGrantText(reason.String())
	} else if reason.Exists() && reason.Type != ojson.Null {
		return claudeResetGrants{}, errors.New("malformed ineligible_reason")
	}
	cooldown, okCooldown := optionalResetGrantTime(block.Get("cooldown_until"))
	if !okCooldown {
		return claudeResetGrants{}, errors.New("malformed cooldown_until")
	}
	result.CooldownUntil = cooldown
	grants := block.Get("grants")
	if grants.Exists() && grants.Type != ojson.Null && !grants.IsArray() {
		return claudeResetGrants{}, errors.New("malformed grants")
	}
	seen := make(map[string]struct{})
	for _, raw := range grants.Array() {
		grant, errGrant := parseClaudeResetGrant(raw)
		if errGrant != nil {
			return claudeResetGrants{}, errGrant
		}
		if _, duplicate := seen[grant.ID]; duplicate {
			return claudeResetGrants{}, fmt.Errorf("duplicate grant %s", grant.ID)
		}
		seen[grant.ID] = struct{}{}
		result.Grants = append(result.Grants, grant)
	}
	return result, nil
}

func parseClaudeResetGrant(raw ojson.Result) (claudeResetGrant, error) {
	if !raw.IsObject() {
		return claudeResetGrant{}, errors.New("malformed grant")
	}
	id := raw.Get("id")
	if id.Type != ojson.String || !claudeResetGrantIDPattern.MatchString(id.String()) {
		return claudeResetGrant{}, errors.New("malformed grant id")
	}
	total, okTotal := resetGrantCount(raw.Get("resets_total"))
	left, okLeft := resetGrantCount(raw.Get("resets_left"))
	if !okTotal || !okLeft || left > total {
		return claudeResetGrant{}, errors.New("malformed grant counts")
	}
	startsAt, okStarts := optionalResetGrantTime(raw.Get("starts_at"))
	endsAt, okEnds := optionalResetGrantTime(raw.Get("ends_at"))
	paused, okPaused := optionalResetGrantFlag(raw.Get("paused"))
	givesBack, okGivesBack := optionalResetGrantFlag(raw.Get("gives_back_weekly"))
	clears, okClears := resetGrantWindows(raw.Get("clears"))
	if !okStarts || !okEnds || !okPaused || !okGivesBack || !okClears {
		return claudeResetGrant{}, errors.New("malformed grant fields")
	}
	return claudeResetGrant{
		ID:              id.String(),
		Label:           safeResetGrantText(raw.Get("label").String()),
		ResetsTotal:     total,
		ResetsLeft:      left,
		StartsAt:        startsAt,
		EndsAt:          endsAt,
		Clears:          clears,
		Paused:          paused,
		GivesBackWeekly: givesBack,
	}, nil
}

func resetGrantCount(value ojson.Result) (int64, bool) {
	if value.Type != ojson.Number {
		return 0, false
	}
	number := value.Float()
	if number < 0 || number != float64(int64(number)) {
		return 0, false
	}
	return int64(number), true
}

// optionalResetGrantTime parses an absent/null (0) or ISO-8601 timestamp into Unix ms.
func optionalResetGrantTime(value ojson.Result) (int64, bool) {
	if !value.Exists() || value.Type == ojson.Null {
		return 0, true
	}
	if value.Type != ojson.String {
		return 0, false
	}
	parsed := parseQuotaHistoryTimestamp(value.String())
	return parsed, parsed > 0
}

func optionalResetGrantFlag(value ojson.Result) (bool, bool) {
	if !value.Exists() || value.Type == ojson.Null {
		return false, true
	}
	if !value.IsBool() {
		return false, false
	}
	return value.Bool(), true
}

func resetGrantWindows(value ojson.Result) ([]string, bool) {
	if !value.Exists() || value.Type == ojson.Null {
		return []string{}, true
	}
	if !value.IsArray() {
		return nil, false
	}
	present := make(map[string]struct{})
	for _, item := range value.Array() {
		if item.Type == ojson.String {
			present[item.String()] = struct{}{}
		}
	}
	windows := []string{}
	for _, window := range claudeResetWindows {
		if _, ok := present[window]; ok {
			windows = append(windows, window)
		}
	}
	return windows, true
}

// safeResetGrantText strips control characters from upstream display text and bounds it.
func safeResetGrantText(raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, raw)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if runes := []rune(cleaned); len(runes) > 120 {
		cleaned = string(runes[:120])
	}
	return cleaned
}

// hasUsableFableReset reports whether record holds a Fable reset that can be claimed
// at now.
func hasUsableFableReset(record claudeResetGrants, now time.Time) bool {
	if !record.Eligible {
		return false
	}
	nowMs := now.UnixMilli()
	for _, grant := range record.Grants {
		if !grant.GivesBackWeekly || grant.ResetsLeft <= 0 || grant.Paused {
			continue
		}
		if (grant.StartsAt == 0 || grant.StartsAt <= nowMs) && (grant.EndsAt == 0 || grant.EndsAt > nowMs) {
			return true
		}
	}
	return false
}

// isClaudeResetClaim reports whether a management api-call request spends a banked reset.
func isClaudeResetClaim(method string, target *url.URL) bool {
	if target == nil || !strings.EqualFold(method, http.MethodPost) {
		return false
	}
	return strings.EqualFold(target.Hostname(), "api.anthropic.com") && claudeResetClaimPath.MatchString(target.Path)
}

// quotaHistoryResetStore keeps the banked resets of each Claude account in one JSON
// file next to the quota history. A credential is read when it has no record or after
// Invalidate, which a reset claim through the management API triggers.
type quotaHistoryResetStore struct {
	path    string
	mu      sync.Mutex
	loaded  bool
	records map[string]claudeResetGrants
	stale   map[string]struct{}
}

func newQuotaHistoryResetStore(dir string) *quotaHistoryResetStore {
	return &quotaHistoryResetStore{
		path:    filepath.Join(dir, quotaHistoryResetGrantsFileName),
		records: make(map[string]claudeResetGrants),
		stale:   make(map[string]struct{}),
	}
}

// loadLocked reads the saved records once. A missing file means nothing is known yet;
// an unreadable one is logged and treated the same, so every account is read again.
func (s *quotaHistoryResetStore) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	data, errRead := os.ReadFile(s.path)
	if errRead != nil {
		if !errors.Is(errRead, os.ErrNotExist) {
			log.WithError(errRead).Warn("quota history: failed to read saved reset grants")
		}
		return
	}
	records := make(map[string]claudeResetGrants)
	if errDecode := json.Unmarshal(data, &records); errDecode != nil {
		log.WithError(errDecode).Warn("quota history: saved reset grants are unreadable; reading them again")
		return
	}
	s.records = records
}

// Get returns the saved record of authID and whether a claim made it out of date.
func (s *quotaHistoryResetStore) Get(authID string) (claudeResetGrants, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	record, ok := s.records[authID]
	_, stale := s.stale[authID]
	return record, ok, stale
}

// NeedsRead reports whether authID has no record or an out-of-date one.
func (s *quotaHistoryResetStore) NeedsRead(authID string) bool {
	_, ok, stale := s.Get(authID)
	return !ok || stale
}

// Put saves a fresh record for authID.
func (s *quotaHistoryResetStore) Put(authID string, record claudeResetGrants) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	s.records[authID] = record
	delete(s.stale, authID)
	return s.saveLocked()
}

// Invalidate marks the record of authID out of date so the collector reads it again.
func (s *quotaHistoryResetStore) Invalidate(authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stale[authID] = struct{}{}
}

func (s *quotaHistoryResetStore) saveLocked() error {
	data, errMarshal := json.MarshalIndent(s.records, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal reset grants: %w", errMarshal)
	}
	dir := filepath.Dir(s.path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create directory: %w", errMkdir)
	}
	tmp, errTemp := os.CreateTemp(dir, quotaHistoryResetGrantsFileName+".*.tmp")
	if errTemp != nil {
		return fmt.Errorf("create temp file: %w", errTemp)
	}
	tmpPath := tmp.Name()
	_, errWrite := tmp.Write(append(data, '\n'))
	errClose := tmp.Close()
	if errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = os.Rename(tmpPath, s.path)
	}
	if errWrite != nil {
		if errRemove := os.Remove(tmpPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.WithError(errRemove).Warn("quota history: failed to remove temp reset grants file")
		}
		return fmt.Errorf("write reset grants: %w", errWrite)
	}
	return nil
}
