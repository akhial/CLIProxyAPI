package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	agygemini "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/antigravity/gemini"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// antigravityPayload frames the native Gemini request for Cloud Code.
func antigravityPayload(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*ojson.Doc, []byte, error) {
	payload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	doc, err := agygemini.NewRequestDoc(payload)
	if err != nil {
		return nil, nil, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	if err := helps.NormalizeAntigravityACPTools(doc); err != nil {
		return nil, nil, fmt.Errorf("normalize ACP tool declarations: %w", err)
	}
	return doc, doc.Bytes(), nil
}

func validateAntigravityRoute(model string, opts cliproxyexecutor.Options) error {
	if !strings.HasPrefix(model, "gemini-") || opts.SourceFormat != sdktranslator.FormatGemini || cliproxyexecutor.ResponseFormatOrSource(opts) != sdktranslator.FormatGemini {
		return statusErr{code: http.StatusBadRequest, msg: "Antigravity requires a native Gemini request and a Gemini model"}
	}
	return nil
}

func (e *AntigravityExecutor) prepareGenerationDoc(ctx context.Context, doc *ojson.Doc, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	if thinking.HasThinkingSuffix(req.Model) {
		if err := helps.ApplyRequestThinkingDoc(doc, req, opts, "gemini", "antigravity", e.Identifier()); err != nil {
			return err
		}
	}
	return nil
}

func setAntigravityRequestHeaders(r *http.Request, userAgent string) {
	// Emulate the official Antigravity CLI/ACP client identity and omit X-Goog-Api-Client.
	r.Header.Set("User-Agent", userAgent)
	r.Header.Del("X-Goog-Api-Client")
}

func (e *AntigravityExecutor) buildRequest(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, payload []byte, stream bool, alt, baseURL string, derivedSessionIDs ...string) (*http.Request, error) {
	doc := ojson.NewDoc(payload)
	httpReq, _, err := e.buildRequestDoc(ctx, auth, token, modelName, doc, stream, cliproxyexecutor.Options{Alt: alt}, baseURL, derivedSessionIDs...)
	return httpReq, err
}

func (e *AntigravityExecutor) buildRequestDoc(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, doc *ojson.Doc, stream bool, opts cliproxyexecutor.Options, baseURL string, derivedSessionIDs ...string) (*http.Request, []byte, error) {
	if token == "" {
		return nil, nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}
	if doc == nil {
		doc = ojson.NewDoc(nil)
	}

	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		base = resolveAntigravityRequestBaseURL(auth)
	}
	alt := opts.Alt
	path := antigravityGeneratePath
	if stream {
		path = antigravityStreamPath
	}
	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(path)
	if stream {
		if alt != "" {
			requestURL.WriteString("?$alt=")
			requestURL.WriteString(url.QueryEscape(alt))
		} else {
			requestURL.WriteString("?alt=sse")
		}
	} else if alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(alt))
	}

	projectID, errProject := e.projectIDForRequest(ctx, auth, token)
	if errProject != nil {
		return nil, nil, errProject
	}
	acpInfo, errSession := helps.ApplyAntigravityACPSessionDoc(doc, opts, auth, projectID, modelName)
	if errSession != nil {
		return nil, nil, errSession
	}
	userAgent := helps.AntigravityRequestUserAgent(doc, opts)
	setAntigravityEnvelopeDoc(doc, modelName, projectID, acpInfo, derivedSessionIDs...)
	helps.FinalizePayloadDoc(ctx, doc)
	finalPayload := doc.Bytes()
	var (
		bodyReader io.Reader = bytes.NewReader(finalPayload)
		payloadLog []byte
	)
	if e.cfg != nil && e.cfg.RequestLog {
		payloadLog = append([]byte(nil), finalPayload...)
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bodyReader)
	if errReq != nil {
		return nil, nil, errReq
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}
	setAntigravityRequestHeaders(httpReq, userAgent)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       requestURL.String(),
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payloadLog,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	return httpReq, finalPayload, nil
}

// resolveAntigravityRequestBaseURL selects one request endpoint without cross-tier fallback.
// Consumer credentials default to daily; enterprise/GCP credentials can set base_url explicitly.
func resolveAntigravityRequestBaseURL(auth *cliproxyauth.Auth) string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return base
	}
	return antigravityBaseURLDaily
}

func resolveHost(base string) string {
	parsed, errParse := url.Parse(base)
	if errParse != nil {
		return ""
	}
	if parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

func resolveCustomAntigravityBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["base_url"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return strings.TrimSuffix(v, "/")
			}
		}
	}
	return ""
}

func setAntigravityEnvelopeDoc(doc *ojson.Doc, modelName string, projectID string, acpInfo helps.AntigravityACPRequestInfo, derivedSessionIDs ...string) {
	if doc == nil {
		return
	}
	helps.SetStringIfDifferentDoc(doc, "model", modelName)
	helps.SetStringIfDifferentDoc(doc, "userAgent", "antigravity")

	isImageModel := strings.Contains(modelName, "image")
	reqType := strings.TrimSpace(doc.Get("requestType").String())
	if reqType == "" {
		if isImageModel {
			reqType = "image_gen"
		} else if acpInfo.RequestType != "" {
			reqType = acpInfo.RequestType
		} else {
			reqType = "agent"
		}
		_ = doc.Set("requestType", reqType)
	}

	if projectID != "" {
		helps.SetStringIfDifferentDoc(doc, "project", projectID)
	} else {
		_ = doc.Delete("project")
	}

	if isImageModel {
		_ = doc.Set("requestId", generateImageGenRequestID())
	} else {
		requestID := acpInfo.RequestID
		if requestID == "" {
			if reqType == "checkpoint" {
				requestID = "checkpoint/" + uuid.NewString()
			} else {
				requestID = generateRequestID()
			}
		}
		_ = doc.Set("requestId", requestID)
		sessionID := strings.TrimSpace(doc.Get("request.sessionId").String())
		if sessionID == "" && len(derivedSessionIDs) > 0 {
			sessionID = strings.TrimSpace(derivedSessionIDs[0])
		}
		if sessionID == "" {
			sessionID = generateStableSessionIDDoc(doc)
		}
		_ = doc.Set("request.sessionId", sessionID)
	}
}

func generateRequestID() string {
	return "agent-" + uuid.NewString()
}

func generateImageGenRequestID() string {
	return fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), uuid.NewString())
}

func generateSessionID() string {
	randSourceMutex.Lock()
	n := randSource.Int63n(9_000_000_000_000_000_000)
	randSourceMutex.Unlock()
	return "-" + strconv.FormatInt(n, 10)
}

func generateStableSessionID(payload []byte) string {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		contents = util.GetGJSONBytesNoCopy(payload, "contents")
	}
	return generateStableSessionIDFromContents(contents)
}

func generateStableSessionIDDoc(doc *ojson.Doc) string {
	if doc == nil {
		return generateSessionID()
	}
	contents := doc.Get("request.contents")
	if !contents.IsArray() {
		contents = doc.Get("contents")
	}
	return generateStableSessionIDFromContents(contents)
}

func generateStableSessionIDFromContents(contents ojson.Result) string {
	if !contents.IsArray() {
		return generateSessionID()
	}

	stableID := ""
	contents.ForEach(func(_, content ojson.Result) bool {
		if content.Get("role").String() != "user" {
			return true
		}
		text := content.Get("parts.0.text").String()
		if text == "" {
			return true
		}
		hash := sha256.Sum256([]byte(text))
		value := int64(binary.BigEndian.Uint64(hash[:8])) & 0x7FFFFFFFFFFFFFFF
		stableID = "-" + strconv.FormatInt(value, 10)
		return false
	})
	if stableID != "" {
		return stableID
	}
	return generateSessionID()
}
