package helps

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	acpConversationHeader = "X-ACP-Conversation-ID"
	acpRootHeader         = "X-ACP-Root-Conversation-ID"
	acpStepHeader         = "X-ACP-Step"
	acpHostHeader         = "X-ACP-Host"

	acpTitleSystemPromptPrefix = "You are a conversation title generator."

	acpSessionCacheMaxEntries    = 2048
	acpPendingPromptTTL          = 2 * time.Minute
	acpPendingPromptMaxPerPrompt = 16
)

var staticAntigravityACPModelEnums = map[string]string{
	"gemini-3.1-pro-low": "MODEL_PLACEHOLDER_M36",
}

// AntigravityACPRequestMeta carries envelope and header overrides for recognized
// Antigravity ACP requests. When Matched is false, upstream behavior is unchanged.
type AntigravityACPRequestMeta struct {
	Matched   bool
	IsTitle   bool
	SessionID string
	UserAgent string
}

type acpPlatform struct {
	osType string
	arch   string
}

type acpPendingSession struct {
	sessionID string
	expiresAt time.Time
}

type acpSessionCorrelator struct {
	mu           sync.Mutex
	nowFunc      func() time.Time
	convSessions map[string]string
	convOrder    []string
	pendingTitle map[string][]acpPendingSession
	recentMain   map[string][]acpPendingSession
	callSeq      map[string]uint64
	callSeqOrder []string
	lastMillis   map[string]int64
	workerOS     map[string]acpPlatform
	workerOSOrd  []string
}

var defaultACPSessionCorrelator = newACPSessionCorrelator()

func newACPSessionCorrelator() *acpSessionCorrelator {
	return &acpSessionCorrelator{
		nowFunc:      time.Now,
		convSessions: make(map[string]string),
		pendingTitle: make(map[string][]acpPendingSession),
		recentMain:   make(map[string][]acpPendingSession),
		callSeq:      make(map[string]uint64),
		lastMillis:   make(map[string]int64),
		workerOS:     make(map[string]acpPlatform),
	}
}

func (c *acpSessionCorrelator) now() time.Time {
	if c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

func (c *acpSessionCorrelator) recordWorkerPlatform(caller string, p acpPlatform) {
	if caller == "" || p.osType == "" || p.arch == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.workerOS[caller]; !exists {
		if len(c.workerOSOrd) >= acpSessionCacheMaxEntries {
			evict := c.workerOSOrd[0]
			c.workerOSOrd = c.workerOSOrd[1:]
			delete(c.workerOS, evict)
		}
		c.workerOSOrd = append(c.workerOSOrd, caller)
	}
	c.workerOS[caller] = p
}

func (c *acpSessionCorrelator) lookupWorkerPlatform(caller string) (acpPlatform, bool) {
	if caller == "" {
		return acpPlatform{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.workerOS[caller]
	return p, ok
}

func (c *acpSessionCorrelator) nextCall(identity string) (stamp int64, seq uint64) {
	nowMs := c.now().UnixMilli()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.callSeq[identity]; !exists {
		if len(c.callSeqOrder) >= acpSessionCacheMaxEntries {
			evict := c.callSeqOrder[0]
			c.callSeqOrder = c.callSeqOrder[1:]
			delete(c.callSeq, evict)
			delete(c.lastMillis, evict)
		}
		c.callSeqOrder = append(c.callSeqOrder, identity)
	}
	c.callSeq[identity]++
	stamp = max(nowMs, c.lastMillis[identity]+1)
	c.lastMillis[identity] = stamp
	return stamp, c.callSeq[identity]
}

func (c *acpSessionCorrelator) sessionForTitle(scope, promptText string) string {
	now := c.now()
	promptKey := acpPromptKey(scope, promptText)

	c.mu.Lock()
	defer c.mu.Unlock()

	if promptKey != "" {
		if id, ok := popValidPendingLocked(c.recentMain, promptKey, now); ok {
			return id
		}
	}

	sessionID := randomACPSessionID()
	if promptKey != "" {
		pushPendingLocked(c.pendingTitle, promptKey, acpPendingSession{
			sessionID: sessionID,
			expiresAt: now.Add(acpPendingPromptTTL),
		}, now)
	}
	return sessionID
}

func (c *acpSessionCorrelator) sessionForConversation(scope, conversation, promptText string, defaultFn func() string) string {
	now := c.now()
	convKey := scope + "\x00" + conversation
	promptKey := acpPromptKey(scope, promptText)

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.convSessions[convKey]; ok && existing != "" {
		return existing
	}

	var sessionID string
	adopted := false
	if promptKey != "" {
		if pendingID, ok := popValidPendingLocked(c.pendingTitle, promptKey, now); ok {
			sessionID = pendingID
			adopted = true
		}
	}
	if sessionID == "" {
		sessionID = defaultFn()
	}

	c.storeConvSessionLocked(convKey, sessionID)
	if !adopted && promptKey != "" {
		pushPendingLocked(c.recentMain, promptKey, acpPendingSession{
			sessionID: sessionID,
			expiresAt: now.Add(acpPendingPromptTTL),
		}, now)
	}
	return sessionID
}

func (c *acpSessionCorrelator) storeConvSessionLocked(convKey, sessionID string) {
	if _, exists := c.convSessions[convKey]; !exists {
		if len(c.convOrder) >= acpSessionCacheMaxEntries {
			evict := c.convOrder[0]
			c.convOrder = c.convOrder[1:]
			delete(c.convSessions, evict)
		}
		c.convOrder = append(c.convOrder, convKey)
	}
	c.convSessions[convKey] = sessionID
}

func popValidPendingLocked(m map[string][]acpPendingSession, key string, now time.Time) (string, bool) {
	queue := m[key]
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if !item.expiresAt.Before(now) && item.sessionID != "" {
			if len(queue) == 0 {
				delete(m, key)
			} else {
				m[key] = queue
			}
			return item.sessionID, true
		}
	}
	delete(m, key)
	return "", false
}

func pushPendingLocked(m map[string][]acpPendingSession, key string, entry acpPendingSession, now time.Time) {
	if len(m) >= acpSessionCacheMaxEntries {
		for k, q := range m {
			pruned := prunePendingQueue(q, now)
			if len(pruned) == 0 {
				delete(m, k)
			} else {
				m[k] = pruned
			}
		}
		if len(m) >= acpSessionCacheMaxEntries {
			for k := range m {
				delete(m, k)
				break
			}
		}
	}
	queue := prunePendingQueue(m[key], now)
	if len(queue) >= acpPendingPromptMaxPerPrompt {
		queue = queue[1:]
	}
	m[key] = append(queue, entry)
}

func prunePendingQueue(queue []acpPendingSession, now time.Time) []acpPendingSession {
	if len(queue) == 0 {
		return nil
	}
	kept := queue[:0]
	for _, item := range queue {
		if !item.expiresAt.Before(now) && item.sessionID != "" {
			kept = append(kept, item)
		}
	}
	return kept
}

func acpPromptKey(scope, promptText string) string {
	trimmed := strings.TrimSpace(promptText)
	if scope == "" || trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(scope + "\x00" + trimmed))
	return hex.EncodeToString(sum[:16])
}

func randomACPSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return strconv.FormatInt(int64(binary.BigEndian.Uint64(b[:])), 10)
	}
	return HashAntigravitySessionID([]byte("acp-fallback\x00" + uuid.NewString()))
}

func resolveACPUserAgent(doc *ojson.Doc, opts executor.Options, caller string) string {
	if opts.Headers != nil {
		if incoming := strings.TrimSpace(opts.Headers.Get("User-Agent")); incoming != "" {
			if strings.HasPrefix(strings.ToLower(incoming), "antigravity/") {
				return incoming
			}
		}
	}

	platform, ok := agyPlatformFromDoc(doc)
	if ok {
		defaultACPSessionCorrelator.recordWorkerPlatform(caller, platform)
	} else if cached, hasCached := defaultACPSessionCorrelator.lookupWorkerPlatform(caller); hasCached {
		platform = cached
	} else {
		platform = acpPlatform{osType: runtime.GOOS, arch: runtime.GOARCH}
	}

	hostPath := ""
	if opts.Headers != nil {
		hostPath = strings.TrimSpace(opts.Headers.Get(acpHostHeader))
	}
	if hostPath == "" {
		hostPath = misc.AntigravityDefaultHostPath
	}
	return misc.AntigravityCLIUserAgent(platform.osType, platform.arch, hostPath)
}

// AntigravityACPCountTokens detects whether a countTokens request comes from
// Antigravity ACP and returns the corresponding CLI User-Agent if so.
func AntigravityACPCountTokens(doc *ojson.Doc, opts executor.Options) (bool, string) {
	if doc == nil {
		return false, ""
	}
	if opts.SourceFormat != "" && opts.SourceFormat != sdktranslator.FormatGemini {
		return false, ""
	}
	caller := acpCallerKey(opts.Metadata)
	hasExplicit := opts.Headers != nil && strings.TrimSpace(opts.Headers.Get(acpConversationHeader)) != ""
	if !hasExplicit && agyConversationID(doc) == "" && !isAntigravityACPTitleRequest(doc) {
		return false, ""
	}
	return true, resolveACPUserAgent(doc, opts, caller)
}

// ValidateAntigravityACPRequest enforces that Antigravity serves only official
// Antigravity ACP native Gemini requests (main conversation turns, ACP title
// checkpoints, or requests carrying explicit X-ACP-Conversation-ID headers).
func ValidateAntigravityACPRequest(baseModel string, req executor.Request, opts executor.Options) error {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(baseModel)), "gemini-") {
		return auth.NewRequestScopedError("antigravity accepts only official Antigravity ACP Gemini requests", http.StatusBadRequest)
	}
	if opts.SourceFormat != sdktranslator.FormatGemini || executor.ResponseFormatOrSource(opts) != sdktranslator.FormatGemini {
		return auth.NewRequestScopedError("antigravity accepts only official Antigravity ACP Gemini requests", http.StatusBadRequest)
	}

	headers := opts.Headers
	conversation, errConversation := acpUUIDHeader(headers, acpConversationHeader)
	if errConversation != nil {
		return errConversation
	}
	root, errRoot := acpUUIDHeader(headers, acpRootHeader)
	if errRoot != nil {
		return errRoot
	}
	steps := headers.Values(acpStepHeader)
	if conversation != "" {
		if len(steps) > 0 {
			if len(steps) != 1 {
				return invalidACPHeader(acpStepHeader, "must be a single nonnegative integer")
			}
			n, errStep := strconv.Atoi(strings.TrimSpace(steps[0]))
			if errStep != nil || n < 0 {
				return invalidACPHeader(acpStepHeader, "must be a single nonnegative integer")
			}
		}
		return nil
	}
	if root != "" || len(steps) != 0 {
		return invalidACPHeader(acpConversationHeader, "is required with a root or step")
	}

	source := opts.OriginalRequest
	if len(source) == 0 {
		source = req.Payload
	}
	if len(source) == 0 || !ojson.ValidBytes(source) {
		return auth.NewRequestScopedError("antigravity accepts only official Antigravity ACP Gemini requests", http.StatusBadRequest)
	}
	parsed := ojson.ParseBytes(source)
	if agyConversationIDResult(parsed) != "" || isAntigravityACPTitleResult(parsed) {
		return nil
	}
	return auth.NewRequestScopedError("antigravity accepts only official Antigravity ACP Gemini requests", http.StatusBadRequest)
}

// ApplyAntigravityACPRequestDoc detects official Antigravity ACP Gemini requests
// (main conversation turns and title-generator calls) and aligns their envelope,
// labels, session correlation, and CLI User-Agent with direct OAuth agy ACP.
//
// Non-ACP requests are left completely untouched (Matched == false).
func ApplyAntigravityACPRequestDoc(doc *ojson.Doc, opts executor.Options, account *auth.Auth, projectID, modelName, modelEnum string) (AntigravityACPRequestMeta, error) {
	if doc == nil {
		return AntigravityACPRequestMeta{}, nil
	}
	if opts.SourceFormat != "" && opts.SourceFormat != sdktranslator.FormatGemini {
		return AntigravityACPRequestMeta{}, nil
	}
	headers := opts.Headers
	conversation, errConversation := acpUUIDHeader(headers, acpConversationHeader)
	if errConversation != nil {
		return AntigravityACPRequestMeta{}, errConversation
	}
	root, errRoot := acpUUIDHeader(headers, acpRootHeader)
	if errRoot != nil {
		return AntigravityACPRequestMeta{}, errRoot
	}
	steps := headers.Values(acpStepHeader)
	explicit := conversation != ""
	promptConversation := agyConversationID(doc)

	caller := acpCallerKey(opts.Metadata)
	if platform, ok := agyPlatformFromDoc(doc); ok {
		defaultACPSessionCorrelator.recordWorkerPlatform(caller, platform)
	}

	if !explicit {
		if root != "" || len(steps) != 0 {
			return AntigravityACPRequestMeta{}, invalidACPHeader(acpConversationHeader, "is required with a root or step")
		}
		if promptConversation == "" {
			if isAntigravityACPTitleRequest(doc) {
				scope := acpCorrelationScope(caller, account, projectID)
				sessionID := strings.TrimSpace(doc.Get("request.sessionId").String())
				if sessionID == "" {
					promptText := doc.Get("request.contents.0.parts.0.text").String()
					sessionID = defaultACPSessionCorrelator.sessionForTitle(scope, promptText)
					_ = doc.Set("request.sessionId", sessionID)
				}
				if strings.TrimSpace(doc.Get("requestType").String()) == "" {
					_ = doc.Set("requestType", "checkpoint")
				}
				if strings.TrimSpace(doc.Get("requestId").String()) == "" {
					_ = doc.Set("requestId", "checkpoint/"+uuid.NewString())
				}
				_ = doc.Delete("request.labels")
				return AntigravityACPRequestMeta{
					Matched:   true,
					IsTitle:   true,
					SessionID: sessionID,
					UserAgent: resolveACPUserAgent(doc, opts, caller),
				}, nil
			}
			return AntigravityACPRequestMeta{}, nil
		}
		conversation = promptConversation
	}

	var explicitStep *int
	if len(steps) > 0 {
		if len(steps) != 1 {
			return AntigravityACPRequestMeta{}, invalidACPHeader(acpStepHeader, "must be a single nonnegative integer")
		}
		n, errStep := strconv.Atoi(strings.TrimSpace(steps[0]))
		if errStep != nil || n < 0 {
			return AntigravityACPRequestMeta{}, invalidACPHeader(acpStepHeader, "must be a single nonnegative integer")
		}
		explicitStep = &n
	}

	scope := acpCorrelationScope(caller, account, projectID)
	identity := scope + "\x00" + conversation

	sessionID := strings.TrimSpace(doc.Get("request.sessionId").String())
	if sessionID == "" {
		promptText := extractACPUserRequestText(doc)
		sessionID = defaultACPSessionCorrelator.sessionForConversation(scope, conversation, promptText, func() string {
			return HashAntigravitySessionID([]byte(identity + "\x00cli-proxy-api:antigravity:acp-session"))
		})
		_ = doc.Set("request.sessionId", sessionID)
	}

	cascadeID := conversation
	rootCascadeID := root
	if rootCascadeID == "" {
		rootCascadeID = cascadeID
	}
	trajectoryID := deterministicACPUUID("trajectory", identity)

	turns, results := antigravityAgentProgress(doc.Get("request.contents"))
	lastStep := turns + results
	if explicitStep != nil {
		lastStep = *explicitStep
		turns = *explicitStep
	}

	labels := doc.Get("request.labels")
	if !labels.Exists() || labels.IsObject() {
		setIfEmpty := func(path, value string) {
			if value != "" && strings.TrimSpace(doc.Get(path).String()) == "" {
				_ = doc.Set(path, value)
			}
		}
		setIfEmpty("request.labels.cascade_id", cascadeID)
		setIfEmpty("request.labels.root_cascade_id", rootCascadeID)
		setIfEmpty("request.labels.trajectory_id", trajectoryID)
		setIfEmpty("request.labels.last_step_index", strconv.Itoa(lastStep))
		setIfEmpty("request.labels.request_id", trajectoryID+"-"+strconv.Itoa(turns))

		lowerModel := strings.ToLower(strings.TrimSpace(modelName))
		setIfEmpty("request.labels.used_claude", strconv.FormatBool(strings.Contains(lowerModel, "claude")))
		setIfEmpty("request.labels.used_claude_conservative", "false")
		setIfEmpty("request.labels.used_non_gemini_model", strconv.FormatBool(!strings.Contains(lowerModel, "gemini")))

		effectiveEnum := strings.TrimSpace(modelEnum)
		if effectiveEnum == "" {
			effectiveEnum = staticAntigravityACPModelEnums[lowerModel]
		}
		setIfEmpty("request.labels.model_enum", effectiveEnum)
	}

	stamp, seq := defaultACPSessionCorrelator.nextCall(identity)
	if strings.TrimSpace(doc.Get("requestId").String()) == "" {
		_ = doc.Set("requestId", fmt.Sprintf("agent/%s/%d/%s/%d", cascadeID, stamp, trajectoryID, seq))
	}

	return AntigravityACPRequestMeta{
		Matched:   true,
		SessionID: sessionID,
		UserAgent: resolveACPUserAgent(doc, opts, caller),
	}, nil
}

func acpCallerKey(meta map[string]any) string {
	if len(meta) == 0 {
		return "default"
	}
	if value, ok := meta[executor.CallerScopeMetadataKey].(string); ok {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "default"
}

func acpCorrelationScope(caller string, account *auth.Auth, projectID string) string {
	var authID, email string
	if account != nil {
		authID = account.ID
		if account.Metadata != nil {
			email, _ = account.Metadata["email"].(string)
		}
	}
	return strings.Join([]string{caller, authID, projectID, strings.ToLower(strings.TrimSpace(email))}, "\x00")
}

func deterministicACPUUID(kind, identity string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:antigravity:acp:" + kind + "\x00" + identity))
	var raw [16]byte
	copy(raw[:], sum[:16])
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	id, _ := uuid.FromBytes(raw[:])
	return id.String()
}

func isAntigravityACPTitleRequest(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	return isAntigravityACPTitleResult(doc.Get("request"))
}

func isAntigravityACPTitleResult(root ojson.Result) bool {
	if !root.Exists() {
		return false
	}
	if req := root.Get("request"); req.IsObject() {
		root = req
	}
	tools := root.Get("tools")
	if tools.IsArray() && tools.Get("#").Int() > 0 {
		return false
	}
	parts := acpSystemInstructionParts(root)
	if !parts.IsArray() || parts.Get("#").Int() != 1 {
		return false
	}
	text := strings.TrimSpace(parts.Get("0.text").String())
	return strings.HasPrefix(text, acpTitleSystemPromptPrefix)
}

func acpSystemInstructionParts(root ojson.Result) ojson.Result {
	if parts := root.Get("systemInstruction.parts"); parts.IsArray() {
		return parts
	}
	return root.Get("system_instruction.parts")
}

func extractACPUserRequestText(doc *ojson.Doc) string {
	if doc == nil {
		return ""
	}
	text := doc.Get("request.contents.0.parts.0.text").String()
	const open, close = "<USER_REQUEST>", "</USER_REQUEST>"
	_, rest, ok := strings.Cut(text, open)
	if !ok {
		return ""
	}
	inner, _, ok := strings.Cut(rest, close)
	if !ok {
		return ""
	}
	return strings.TrimSpace(inner)
}

func agyPlatformFromDoc(doc *ojson.Doc) (acpPlatform, bool) {
	if doc == nil {
		return acpPlatform{}, false
	}
	parts := doc.Get("request.systemInstruction.parts")
	if !parts.IsArray() || parts.Get("#").Int() != 1 {
		return acpPlatform{}, false
	}
	text := parts.Get("0.text").String()
	const open, close = "<user_information>", "</user_information>"
	_, block, ok := strings.Cut(text, open)
	if !ok {
		return acpPlatform{}, false
	}
	info, _, ok := strings.Cut(block, close)
	if !ok {
		return acpPlatform{}, false
	}
	const prefix = "The USER's OS version is "
	for _, line := range strings.Split(info, "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !found {
			continue
		}
		value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		switch value {
		case "mac", "macos", "darwin":
			return acpPlatform{osType: "darwin", arch: "arm64"}, true
		case "linux":
			arch := "amd64"
			if runtime.GOOS == "linux" {
				arch = runtime.GOARCH
			}
			return acpPlatform{osType: "linux", arch: arch}, true
		case "windows", "win":
			return acpPlatform{osType: "windows", arch: "amd64"}, true
		}
	}
	return acpPlatform{}, false
}

// Stock agy puts the ACP UUID and its corresponding artifact directory together at
// the end of the user_information block. Only consult the system instruction.
func agyConversationID(doc *ojson.Doc) string {
	if doc == nil {
		return ""
	}
	return agyConversationIDResult(doc.Get("request"))
}

func agyConversationIDResult(root ojson.Result) string {
	if !root.Exists() {
		return ""
	}
	if req := root.Get("request"); req.IsObject() {
		root = req
	}
	parts := acpSystemInstructionParts(root)
	if !parts.IsArray() || parts.Get("#").Int() != 1 {
		return ""
	}
	text := parts.Get("0.text").String()
	const open, close = "<user_information>", "</user_information>"
	before, block, ok := strings.Cut(text, open)
	if !ok || (before != "" && !strings.HasSuffix(before, "\n")) || strings.Contains(block, open) {
		return ""
	}
	info, after, ok := strings.Cut(block, close)
	if !ok || (after != "" && !strings.HasPrefix(after, "\n")) || strings.Count(info, "\nConversation ID: ") != 1 {
		return ""
	}
	lines := strings.Split(info, "\n")
	if len(lines) < 5 || lines[0] != "" || lines[len(lines)-1] != "" {
		return ""
	}
	dir, hasDir := strings.CutPrefix(lines[len(lines)-4], "App Data Directory: ")
	id, hasID := strings.CutPrefix(lines[len(lines)-3], "Conversation ID: ")
	artifact, hasArtifact := strings.CutPrefix(lines[len(lines)-2], "Artifact Directory Path: ")
	if !hasDir || dir == "" || !hasID || !hasArtifact || artifact != dir+"/brain/"+id {
		return ""
	}
	return acpUUID(id)
}

func acpUUID(value string) string {
	id, err := uuid.Parse(value)
	if len(value) != 36 || err != nil || id == uuid.Nil {
		return ""
	}
	return id.String()
}

func acpUUIDHeader(headers http.Header, name string) (string, error) {
	if headers == nil {
		return "", nil
	}
	values := headers.Values(name)
	if len(values) == 0 {
		return "", nil
	}
	id := acpUUID(values[0])
	if len(values) != 1 || id == "" {
		return "", invalidACPHeader(name, "must be a single nonzero UUID")
	}
	return id, nil
}

func invalidACPHeader(name, issue string) error {
	return auth.NewRequestScopedError(name+" "+issue, http.StatusBadRequest)
}
