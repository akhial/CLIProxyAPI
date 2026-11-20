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

// ApplyAntigravityACPSessionDoc uses explicit ACP host metadata to correlate generations.
// Host headers are scoped to the authenticated caller and selected upstream credential.
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
	if conversation == "" {
		if root != "" || len(steps) != 0 {
			return invalidACPHeader(acpConversationHeader, "is required with a root or step")
		}
		return nil
	}
	caller := metadataString(opts.Metadata, executor.CallerScopeMetadataKey)
	if caller == "" || account == nil || account.ID == "" {
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

func acpUUIDHeader(headers http.Header, name string) (string, error) {
	values := headers.Values(name)
	if len(values) == 0 {
		return "", nil
	}
	id, errID := uuid.Parse(values[0])
	if len(values) != 1 || len(values[0]) != 36 || errID != nil || id == uuid.Nil {
		return "", invalidACPHeader(name, "must be a single nonzero UUID")
	}
	return id.String(), nil
}

func invalidACPHeader(name, issue string) error {
	return auth.NewRequestScopedError(name+" "+issue, http.StatusBadRequest)
}
