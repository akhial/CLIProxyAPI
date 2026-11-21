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
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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

var staticAntigravityModelEnums = map[string]string{
	"gemini-3.1-pro-low": "MODEL_PLACEHOLDER_M36",
}

// AntigravityACPRequestInfo carries envelope metadata derived from ACP correlation.
type AntigravityACPRequestInfo struct {
	RequestType string
	RequestID   string
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

func (c *acpSessionCorrelator) nextCallSeq(identity string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.callSeq[identity]; !exists {
		if len(c.callSeqOrder) >= acpSessionCacheMaxEntries {
			evict := c.callSeqOrder[0]
			c.callSeqOrder = c.callSeqOrder[1:]
			delete(c.callSeq, evict)
		}
		c.callSeqOrder = append(c.callSeqOrder, identity)
	}
	c.callSeq[identity]++
	return c.callSeq[identity]
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

	sessionID := randomAntigravitySessionID()
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

func randomAntigravitySessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		n := int64(binary.BigEndian.Uint64(b[:]) & 0x7FFFFFFFFFFFFFFF)
		if n == 0 {
			n = 1
		}
		return "-" + strconv.FormatInt(n, 10)
	}
	return antigravitySessionID("fallback\x00" + uuid.NewString())
}

// AntigravityRequestUserAgent returns the upstream User-Agent for an Antigravity request.
// Native antigravity/ User-Agent headers are preserved; SDK or empty headers are replaced
// with the official Antigravity CLI/ACP User-Agent matching the caller's detected platform.
func AntigravityRequestUserAgent(doc *ojson.Doc, opts executor.Options) string {
	if opts.Headers != nil {
		if incoming := strings.TrimSpace(opts.Headers.Get("User-Agent")); incoming != "" {
			if strings.HasPrefix(strings.ToLower(incoming), "antigravity/") {
				return incoming
			}
		}
	}

	caller := metadataString(opts.Metadata, executor.CallerScopeMetadataKey)
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

// ApplyAntigravityACPSessionDoc correlates generations within an authenticated worker and account,
// populates OAuth-parity trajectory labels and request metadata, and classifies ACP title requests
// as checkpoints sharing their session ID with the corresponding conversation.
// Call before filling the envelope so a native session ID takes precedence over text discovery.
func ApplyAntigravityACPSessionDoc(doc *ojson.Doc, opts executor.Options, account *auth.Auth, projectID, modelName string) (AntigravityACPRequestInfo, error) {
	if doc == nil {
		return AntigravityACPRequestInfo{}, nil
	}
	headers := opts.Headers
	conversation, errConversation := acpUUIDHeader(headers, acpConversationHeader)
	if errConversation != nil {
		return AntigravityACPRequestInfo{}, errConversation
	}
	root, errRoot := acpUUIDHeader(headers, acpRootHeader)
	if errRoot != nil {
		return AntigravityACPRequestInfo{}, errRoot
	}
	steps := headers.Values(acpStepHeader)
	explicit := conversation != ""
	promptConversation := agyConversationID(doc)

	caller := metadataString(opts.Metadata, executor.CallerScopeMetadataKey)
	if platform, ok := agyPlatformFromDoc(doc); ok {
		defaultACPSessionCorrelator.recordWorkerPlatform(caller, platform)
	}

	if !explicit {
		if root != "" || len(steps) != 0 {
			return AntigravityACPRequestInfo{}, invalidACPHeader(acpConversationHeader, "is required with a root or step")
		}
		if doc.Get("request.sessionId").Exists() {
			return AntigravityACPRequestInfo{}, nil
		}
		if promptConversation == "" {
			if isAntigravityACPTitleRequest(doc) {
				info := AntigravityACPRequestInfo{
					RequestType: "checkpoint",
					RequestID:   "checkpoint/" + uuid.NewString(),
				}
				if caller != "" && account != nil && account.ID != "" {
					scope := acpCorrelationScope(caller, account, projectID)
					promptText := doc.Get("request.contents.0.parts.0.text").String()
					sessionID := defaultACPSessionCorrelator.sessionForTitle(scope, promptText)
					_ = doc.Set("request.sessionId", sessionID)
				}
				return info, nil
			}
			return AntigravityACPRequestInfo{}, nil
		}
		conversation = promptConversation
	}

	if caller == "" || account == nil || account.ID == "" {
		if !explicit {
			return AntigravityACPRequestInfo{}, nil
		}
		return AntigravityACPRequestInfo{}, invalidACPHeader(acpConversationHeader, "requires an authenticated worker and identified upstream credential")
	}

	var (
		stepIndex     uint64
		hasStepHeader bool
	)
	if len(steps) != 0 {
		value, errStep := strconv.ParseUint(headers.Get(acpStepHeader), 10, 32)
		if errStep != nil || len(steps) != 1 {
			return AntigravityACPRequestInfo{}, invalidACPHeader(acpStepHeader, "must be a single unsigned 32-bit decimal step")
		}
		stepIndex = value
		hasStepHeader = true
	} else if count := doc.Get("request.contents.#").Int(); count > 0 {
		stepIndex = uint64(count - 1)
	}

	scope := acpCorrelationScope(caller, account, projectID)
	identity := scope + "\x00" + conversation
	trajectoryID := stableProviderSessionUUID("antigravity", "acp-trajectory", identity)

	var cascadeID, rootCascadeID string
	if !explicit || conversation == promptConversation {
		cascadeID = conversation
		if root != "" {
			rootCascadeID = root
		} else {
			rootCascadeID = cascadeID
		}
	} else {
		cascadeID = stableProviderSessionUUID("antigravity", "acp-conversation", identity)
		if root != "" {
			rootCascadeID = stableProviderSessionUUID("antigravity", "acp-conversation", scope+"\x00"+root)
		} else {
			rootCascadeID = cascadeID
		}
	}

	defaultSession := func() string {
		return antigravitySessionID("acp\x00" + identity)
	}
	var sessionID string
	if explicit {
		sessionID = defaultSession()
	} else {
		promptText := extractACPUserRequestText(doc)
		sessionID = defaultACPSessionCorrelator.sessionForConversation(scope, conversation, promptText, defaultSession)
	}
	_ = doc.Set("request.sessionId", sessionID)

	labels := doc.Get("request.labels")
	skipLabels := !explicit && labels.Exists() && (!labels.IsObject() || labels.Get("cascade_id").Exists())
	if !skipLabels {
		stepStr := strconv.FormatUint(stepIndex, 10)
		setLabel := func(key, value string) {
			if value == "" {
				return
			}
			if explicit || !labels.Exists() || !labels.Get(key).Exists() {
				_ = doc.Set("request.labels."+key, value)
			}
		}
		setLabel("cascade_id", cascadeID)
		setLabel("root_cascade_id", rootCascadeID)
		setLabel("trajectory_id", trajectoryID)
		if explicit && !hasStepHeader && doc.Get("request.contents.#").Int() == 0 {
			// Omit last_step_index only if caller explicitly omitted step and supplied no contents.
		} else {
			setLabel("last_step_index", stepStr)
		}
		setLabel("request_id", fmt.Sprintf("%s-%d", trajectoryID, stepIndex))
		setLabel("used_claude", "false")
		setLabel("used_claude_conservative", "false")
		setLabel("used_non_gemini_model", "false")
		if modelEnum := resolveAntigravityModelEnum(account, modelName); modelEnum != "" {
			setLabel("model_enum", modelEnum)
		}
	}

	seq := defaultACPSessionCorrelator.nextCallSeq(identity)
	return AntigravityACPRequestInfo{
		RequestType: "agent",
		RequestID:   fmt.Sprintf("agent/%s/%d/%s/%d", cascadeID, defaultACPSessionCorrelator.now().UnixMilli(), trajectoryID, seq),
	}, nil
}

func acpCorrelationScope(caller string, account *auth.Auth, projectID string) string {
	email, _ := account.Metadata["email"].(string)
	return strings.Join([]string{caller, account.ID, projectID, email}, "\x00")
}

func resolveAntigravityModelEnum(account *auth.Auth, modelName string) string {
	normalized := sdkauth.NormalizeAntigravityModelID(modelName)
	if normalized == "" {
		return ""
	}
	if account != nil && account.Metadata != nil {
		if enumName := sdkauth.LookupPersistedAntigravityModelEnum(account.Metadata, normalized); enumName != "" {
			return enumName
		}
	}
	return staticAntigravityModelEnums[normalized]
}

func isAntigravityACPTitleRequest(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	tools := doc.Get("request.tools")
	if tools.IsArray() && tools.Get("#").Int() > 0 {
		return false
	}
	parts := doc.Get("request.systemInstruction.parts")
	if !parts.IsArray() || parts.Get("#").Int() != 1 {
		return false
	}
	text := strings.TrimSpace(parts.Get("0.text").String())
	return strings.HasPrefix(text, acpTitleSystemPromptPrefix)
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
	parts := doc.Get("request.systemInstruction.parts")
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
