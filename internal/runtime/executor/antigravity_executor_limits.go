package executor

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

type antigravity429Category string

type antigravity429DecisionKind string

const (
	antigravity429Unknown                         antigravity429Category     = "unknown"
	antigravity429RateLimited                     antigravity429Category     = "rate_limited"
	antigravity429QuotaExhausted                  antigravity429Category     = "quota_exhausted"
	antigravity429SoftRateLimit                   antigravity429Category     = "soft_rate_limit"
	antigravity429DecisionSoftRetry               antigravity429DecisionKind = "soft_retry"
	antigravity429DecisionInstantRetrySameAuth    antigravity429DecisionKind = "instant_retry_same_auth"
	antigravity429DecisionShortCooldownSwitchAuth antigravity429DecisionKind = "short_cooldown_switch_auth"
	antigravity429DecisionFullQuotaExhausted      antigravity429DecisionKind = "full_quota_exhausted"
)

type antigravity429Decision struct {
	kind       antigravity429DecisionKind
	retryAfter *time.Duration
	reason     string
}

var (
	randSource                        = rand.New(rand.NewSource(time.Now().UnixNano()))
	randSourceMutex                   sync.Mutex
	antigravityShortCooldownByAuth    sync.Map
	antigravityRefreshGroup           singleflight.Group
	antigravityQuotaExhaustedKeywords = []string{
		"quota_exhausted",
		"quota exhausted",
	}
)

type antigravityKVClient interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, key string, value []byte, opts homekv.KVSetOptions) (bool, error)
	KVSetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	KVDel(ctx context.Context, keys ...string) (int64, error)
}

var currentAntigravityKVClient = func() (antigravityKVClient, bool, error) {
	return homekv.CurrentKVClient()
}

type antigravityTokenRefreshData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func classifyAntigravity429(body []byte) antigravity429Category {
	switch decideAntigravity429(body).kind {
	case antigravity429DecisionInstantRetrySameAuth, antigravity429DecisionShortCooldownSwitchAuth:
		return antigravity429RateLimited
	case antigravity429DecisionFullQuotaExhausted:
		return antigravity429QuotaExhausted
	case antigravity429DecisionSoftRetry:
		return antigravity429SoftRateLimit
	default:
		return antigravity429Unknown
	}
}

func decideAntigravity429(body []byte) antigravity429Decision {
	decision := antigravity429Decision{kind: antigravity429DecisionSoftRetry}
	if len(body) == 0 {
		return decision
	}

	if retryAfter, parseErr := helps.ParseRetryDelay(body); parseErr == nil && retryAfter != nil {
		decision.retryAfter = retryAfter
	}

	status := strings.TrimSpace(ojson.GetBytesNoCopy(body, "error.status").String())
	if !strings.EqualFold(status, "RESOURCE_EXHAUSTED") {
		return decision
	}

	details := ojson.GetBytesNoCopy(body, "error.details")
	if details.Exists() && details.IsArray() {
		for _, detail := range details.Array() {
			if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
				continue
			}
			reason := strings.Clone(strings.TrimSpace(detail.Get("reason").String()))
			decision.reason = reason
			switch {
			case strings.EqualFold(reason, "QUOTA_EXHAUSTED"), strings.EqualFold(reason, "INSUFFICIENT_G1_CREDITS_BALANCE"):
				decision.kind = antigravity429DecisionFullQuotaExhausted
				return decision
			case strings.EqualFold(reason, "RATE_LIMIT_EXCEEDED"):
				if decision.retryAfter == nil {
					decision.kind = antigravity429DecisionSoftRetry
					return decision
				}
				switch {
				case *decision.retryAfter < antigravityInstantRetryThreshold:
					decision.kind = antigravity429DecisionInstantRetrySameAuth
				case *decision.retryAfter < antigravityShortQuotaCooldownThreshold:
					decision.kind = antigravity429DecisionShortCooldownSwitchAuth
				default:
					decision.kind = antigravity429DecisionFullQuotaExhausted
				}
				return decision
			}
		}
	}

	lowerBody := strings.ToLower(string(body))
	for _, keyword := range antigravityQuotaExhaustedKeywords {
		if strings.Contains(lowerBody, keyword) {
			decision.kind = antigravity429DecisionFullQuotaExhausted
			decision.reason = "quota_exhausted"
			return decision
		}
	}

	decision.kind = antigravity429DecisionSoftRetry
	return decision
}

// terminal, service-scoped credential suspension — the OAuth token and model
// catalog keep working while inference is refused — so the credential must be
// deactivated rather than cooled, and sibling credentials in the same pool
// should not be hammered during an automated sweep.
type antigravityTOSViolation struct {
	AppealURL     string
	AppealURLText string
}

// parseAntigravityTOSViolation extracts the TOS_VIOLATION signature from a
// Google RPC error body. It returns nil for any other shape, including
// transient 429 quota errors and non-TOS 403s.
func parseAntigravityTOSViolation(body []byte) *antigravityTOSViolation {
	if len(body) == 0 {
		return nil
	}
	code := int(ojson.GetBytesNoCopy(body, "error.code").Int())
	if code != http.StatusForbidden {
		return nil
	}
	status := strings.TrimSpace(ojson.GetBytesNoCopy(body, "error.status").String())
	if !strings.EqualFold(status, "PERMISSION_DENIED") {
		return nil
	}
	details := ojson.GetBytesNoCopy(body, "error.details")
	if !details.Exists() || !details.IsArray() {
		return nil
	}
	for _, detail := range details.Array() {
		if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		reason := strings.TrimSpace(detail.Get("reason").String())
		if !strings.EqualFold(reason, "TOS_VIOLATION") {
			continue
		}
		meta := detail.Get("metadata")
		return &antigravityTOSViolation{
			AppealURL:     strings.Clone(strings.TrimSpace(meta.Get("appeal_url").String())),
			AppealURLText: strings.Clone(strings.TrimSpace(meta.Get("appeal_url_link_text").String())),
		}
	}
	return nil
}

func newAntigravityStatusErr(statusCode int, body []byte) statusErr {
	err := statusErr{code: statusCode, msg: string(body)}
	if statusCode == http.StatusTooManyRequests {
		if retryAfter, parseErr := helps.ParseRetryDelay(body); parseErr == nil && retryAfter != nil {
			err.retryAfter = retryAfter
		}
	} else if statusCode == http.StatusForbidden {
		if violation := parseAntigravityTOSViolation(body); violation != nil {
			err.tosViolation = true
			err.credentialScoped = true
			err.tosAppealURL = violation.AppealURL
			err.tosAppealURLText = violation.AppealURLText
		}
	}
	return err
}

// applyAntigravityTOSState performs the executor-side bookkeeping for a terminal
// TOS_VIOLATION: it arms the optional pool-wide protective pause and logs the
// appeal URL for the operator. The durable credential deactivation and the
// appeal metadata on the auth record are carried by the returned statusErr
// (tos_violation code + Error.Metadata) through the conductor, since the
// executor only sees a cloned auth. It is a no-op for non-TOS errors.
func (e *AntigravityExecutor) applyAntigravityTOSState(cfg *config.Config, auth *cliproxyauth.Auth, body []byte) {
	violation := parseAntigravityTOSViolation(body)
	if violation == nil || auth == nil {
		return
	}
	markAntigravityTOSPause(cfg, auth)
	if violation.AppealURL != "" {
		log.Warnf("antigravity executor: credential %s suspended for TOS_VIOLATION; submit appeal: %s (%s)", auth.ID, redactTOSAppealURL(violation.AppealURL), violation.AppealURLText)
	} else {
		log.Warnf("antigravity executor: credential %s suspended for TOS_VIOLATION", auth.ID)
	}
}
func antigravityShortCooldownKey(auth *cliproxyauth.Auth, modelName string) string {
	if auth == nil {
		return ""
	}
	authID := strings.TrimSpace(auth.ID)
	modelName = strings.TrimSpace(modelName)
	if authID == "" || modelName == "" {
		return ""
	}
	return authID + "|" + modelName + "|sc"
}

func antigravityShortCooldownKVKey(auth *cliproxyauth.Auth, modelName string) string {
	if auth == nil {
		return ""
	}
	authID := strings.TrimSpace(auth.ID)
	modelName = strings.TrimSpace(modelName)
	if authID == "" || modelName == "" {
		return ""
	}
	return "cpa:antigravity:short-cooldown:" + authID + ":" + homekv.HashKeyPart(modelName)
}

func antigravityCoolingDisabled(auth *cliproxyauth.Auth, cfg *config.Config) bool {
	return cliproxyauth.QuotaCooldownDisabledForAuthWithConfig(auth, cfg)
}

func antigravityIsInShortCooldown(auth *cliproxyauth.Auth, modelName string, now time.Time) (bool, time.Duration) {
	inCooldown, remaining, errCooldown := antigravityIsInShortCooldownRequired(context.Background(), auth, modelName, now)
	if errCooldown != nil {
		log.Errorf("antigravity executor: home kv cooldown read error: %v", errCooldown)
		return false, 0
	}
	return inCooldown, remaining
}

func antigravityIsInShortCooldownRequired(ctx context.Context, auth *cliproxyauth.Auth, modelName string, now time.Time) (bool, time.Duration, error) {
	if antigravityCoolingDisabled(auth, nil) {
		return false, 0, nil
	}
	kvKey := antigravityShortCooldownKVKey(auth, modelName)
	client, homeMode, errClient := currentAntigravityKVClient()
	if homeMode {
		if errClient != nil {
			return false, 0, errClient
		}
		if kvKey == "" {
			return false, 0, nil
		}
		raw, found, errGet := client.KVGet(ctx, kvKey)
		if errGet != nil || !found {
			return false, 0, errGet
		}
		untilNano, errParse := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if errParse != nil {
			return false, 0, errParse
		}
		remaining := time.Unix(0, untilNano).Sub(now)
		if remaining <= 0 {
			if _, errDel := client.KVDel(ctx, kvKey); errDel != nil {
				return false, 0, errDel
			}
			return false, 0, nil
		}
		return true, remaining, nil
	}

	key := antigravityShortCooldownKey(auth, modelName)
	if key == "" {
		return false, 0, nil
	}
	value, ok := antigravityShortCooldownByAuth.Load(key)
	if !ok {
		return false, 0, nil
	}
	until, ok := value.(time.Time)
	if !ok || until.IsZero() {
		antigravityShortCooldownByAuth.Delete(key)
		return false, 0, nil
	}
	remaining := until.Sub(now)
	if remaining <= 0 {
		antigravityShortCooldownByAuth.Delete(key)
		return false, 0, nil
	}
	return true, remaining, nil
}

func markAntigravityShortCooldown(auth *cliproxyauth.Auth, modelName string, now time.Time, duration time.Duration) {
	if errMark := markAntigravityShortCooldownRequired(context.Background(), auth, modelName, now, duration); errMark != nil {
		log.Errorf("antigravity executor: home kv cooldown write error: %v", errMark)
	}
}

func markAntigravityShortCooldownRequired(ctx context.Context, auth *cliproxyauth.Auth, modelName string, now time.Time, duration time.Duration) error {
	if antigravityCoolingDisabled(auth, nil) {
		return nil
	}
	kvKey := antigravityShortCooldownKVKey(auth, modelName)
	client, homeMode, errClient := currentAntigravityKVClient()
	if homeMode {
		if errClient != nil {
			return errClient
		}
		if kvKey == "" || duration <= 0 {
			return nil
		}
		until := now.Add(duration)
		written, errSet := client.KVSet(ctx, kvKey, []byte(strconv.FormatInt(until.UnixNano(), 10)), homekv.KVSetOptions{EX: duration + 5*time.Second})
		if errSet != nil {
			return errSet
		}
		if !written {
			return fmt.Errorf("home kv store unavailable")
		}
		return nil
	}

	key := antigravityShortCooldownKey(auth, modelName)
	if key == "" {
		return nil
	}
	antigravityShortCooldownByAuth.Store(key, now.Add(duration))
	return nil
}

func homeKVUnavailableStatusErr(cause error) statusErr {
	if cause == nil {
		return statusErr{code: http.StatusServiceUnavailable, msg: "home kv store unavailable"}
	}
	return statusErr{code: http.StatusServiceUnavailable, msg: fmt.Sprintf("home kv store unavailable: %v", cause)}
}

// antigravityTOSPauseUntil stores the pool-wide protective pause deadline.
// It mirrors the credits/cooldown dual-track pattern: an in-process map for
// standalone mode and the home KV store (prefix cpa:antigravity:*) so the pause
// is shared when the control plane is active.
var antigravityTOSPauseUntil sync.Map

const antigravityTOSPauseKVKey = "cpa:antigravity:tos-pause:until"

// antigravityTOSPauseWindow resolves the configured protective pause window.
// Defaults to disabled (0) so a TOS_VIOLATION only deactivates the offending
// credential unless a positive Go duration (e.g. "6h") is explicitly configured.
func antigravityTOSPauseWindow(cfg *config.Config) (time.Duration, bool) {
	if cfg == nil {
		return 0, false
	}
	raw := strings.TrimSpace(cfg.Antigravity.TOSPauseWindow)
	if raw == "" {
		return 0, false
	}
	switch strings.ToLower(raw) {
	case "0", "off", "false", "none", "disabled":
		return 0, false
	}
	window, err := time.ParseDuration(raw)
	if err != nil || window <= 0 {
		return 0, false
	}
	return window, true
}

func antigravityTOSPauseKey(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	_ = auth
	return "pool"
}

// markAntigravityTOSPause records a pool-wide protective pause after the first
// TOS_VIOLATION. Only the TOS path calls this; transient 429 quota errors must
// never write or extend the pause.
func markAntigravityTOSPause(cfg *config.Config, auth *cliproxyauth.Auth) {
	window, enabled := antigravityTOSPauseWindow(cfg)
	if !enabled || window <= 0 {
		return
	}
	until := time.Now().Add(window)
	key := antigravityTOSPauseKey(auth)
	antigravityTOSPauseUntil.Store(key, until)
	if client, homeMode, errClient := currentAntigravityKVClient(); homeMode {
		if errClient != nil {
			log.Errorf("antigravity executor: home kv tos pause write error: %v", errClient)
			return
		}
		payload := []byte(strconv.FormatInt(until.UnixNano(), 10))
		if _, errSet := client.KVSet(context.Background(), antigravityTOSPauseKVKey, payload, homekv.KVSetOptions{EX: window + 5*time.Second}); errSet != nil {
			log.Errorf("antigravity executor: home kv tos pause write error: %v", errSet)
		}
	}
	log.Warnf("antigravity executor: TOS_VIOLATION pause armed for %s (pool-wide until %s)", window, until.Format(time.RFC3339))
}

// antigravityInTOSPause reports whether the Antigravity pool is currently under
// a protective TOS pause. The check is provider-scoped (any Antigravity
// credential) and ignores per-credential identity.
func antigravityInTOSPause(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	_, enabled := antigravityTOSPauseWindow(cfg)
	if !enabled {
		return false
	}
	key := antigravityTOSPauseKey(auth)
	if key == "" {
		return false
	}
	now := time.Now()
	if value, ok := antigravityTOSPauseUntil.Load(key); ok {
		if until, ok := value.(time.Time); ok && until.After(now) {
			return true
		}
		// Delete only if it is still the same expired value; a concurrent
		// markAntigravityTOSPause may have already installed a newer deadline.
		antigravityTOSPauseUntil.CompareAndDelete(key, value)
	}
	if client, homeMode, errClient := currentAntigravityKVClient(); homeMode {
		if errClient != nil {
			log.Errorf("antigravity executor: home kv tos pause read error: %v", errClient)
			return false
		}
		raw, found, errGet := client.KVGet(context.Background(), antigravityTOSPauseKVKey)
		if errGet != nil || !found {
			return false
		}
		untilNano, errParse := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if errParse != nil {
			return false
		}
		remaining := time.Unix(0, untilNano).Sub(now)
		// An expired deadline is treated as absent; the KV EX ttl reclaims the
		// key, so no explicit delete is needed (which could race a concurrent
		// markAntigravityTOSPause writing a newer deadline).
		return remaining > 0
	}
	return false
}

// redactTOSAppealURL strips query parameters and fragments from an appeal URL
// before logging, so a signed or tracking URL cannot leak through log output.
// The full URL remains available on the auth record for operator use.
func redactTOSAppealURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if idx := strings.IndexAny(trimmed, "?#"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return trimmed
}
