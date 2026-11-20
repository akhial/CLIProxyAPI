package helps

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	acpConversationHeader = "X-ACP-Conversation-ID"
	acpRootHeader         = "X-ACP-Root-Conversation-ID"
	acpStepHeader         = "X-ACP-Step"
)

// ApplyAntigravityACPSessionDoc correlates generations within an authenticated worker and account.
// Call before filling the envelope so a native session ID takes precedence over text discovery.
func ApplyAntigravityACPSessionDoc(doc *ojson.Doc, opts executor.Options, account *auth.Auth, projectID string) error {
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
	explicit := conversation != ""
	if !explicit {
		if root != "" || len(steps) != 0 {
			return invalidACPHeader(acpConversationHeader, "is required with a root or step")
		}
		if doc.Get("request.sessionId").Exists() {
			return nil
		}
		conversation = agyConversationID(doc)
		if conversation == "" {
			return nil
		}
	}
	caller := metadataString(opts.Metadata, executor.CallerScopeMetadataKey)
	if caller == "" || account == nil || account.ID == "" {
		if !explicit {
			return nil
		}
		return invalidACPHeader(acpConversationHeader, "requires an authenticated worker and identified upstream credential")
	}

	var step string
	if len(steps) != 0 {
		value, errStep := strconv.ParseUint(headers.Get(acpStepHeader), 10, 32)
		if errStep != nil || len(steps) != 1 {
			return invalidACPHeader(acpStepHeader, "must be a single unsigned 32-bit decimal step")
		}
		step = strconv.FormatUint(value, 10)
	}

	email, _ := account.Metadata["email"].(string)
	scope := strings.Join([]string{caller, account.ID, projectID, email}, "\x00")
	identity := scope + "\x00" + conversation
	_ = doc.Set("request.sessionId", antigravitySessionID("acp\x00"+identity))
	if !explicit {
		return nil
	}
	_ = doc.Set("request.labels.cascade_id", stableProviderSessionUUID("antigravity", "acp-conversation", identity))
	_ = doc.Set("request.labels.trajectory_id", stableProviderSessionUUID("antigravity", "acp-trajectory", identity))
	if root != "" {
		_ = doc.Set("request.labels.root_cascade_id", stableProviderSessionUUID("antigravity", "acp-conversation", scope+"\x00"+root))
	}
	if step != "" {
		_ = doc.Set("request.labels.last_step_index", step)
	}
	return nil
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
