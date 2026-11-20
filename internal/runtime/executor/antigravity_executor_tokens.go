package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// CountTokens counts tokens for the given request using the Antigravity API.
func (e *AntigravityExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if err := validateAntigravityRoute(baseModel, opts); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if antigravityInTOSPause(e.cfg, auth) {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity pool paused after TOS_VIOLATION"}
	}

	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FormatAntigravity
	respCtx := context.WithValue(ctx, "alt", opts.Alt)
	doc, baseline, errTranslate := antigravityPayload(req, opts)
	if errTranslate != nil {
		return cliproxyexecutor.Response{}, errTranslate
	}

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return cliproxyexecutor.Response{}, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	cliproxyauth.NotifyAccessTokenFingerprint(ctx, auth)
	if strings.TrimSpace(token) == "" {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	base := resolveAntigravityRequestBaseURL(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(antigravityCountTokensPath)
	if opts.Alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(opts.Alt))
	}

	ctx = helps.WithPayloadFinalizer(ctx, helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "request", baseline, req, opts))
	helps.FinalizePayloadDoc(ctx, doc)
	payload := doc.Bytes()
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(payload))
	if errReq != nil {
		return cliproxyexecutor.Response{}, errReq
	}
	// No httpReq.Close: keep the shared Antigravity connection pool usable.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}
	setAntigravityRequestHeaders(httpReq, opts)

	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       requestURL.String(),
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payload,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	httpResp, errDo := helps.WithAntigravityHTTPClientTrace(httpClient, auth, "count_tokens").Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return cliproxyexecutor.Response{}, errDo
	}

	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errClose := httpResp.Body.Close(); errClose != nil {
		log.Errorf("antigravity executor: close response body error: %v", errClose)
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return cliproxyexecutor.Response{}, errRead
	}

	if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
		count := ojson.GetBytesNoCopy(bodyBytes, "totalTokens").Int()
		translated := sdktranslator.TranslateTokenCount(respCtx, to, responseFormat, count, bodyBytes)
		return cliproxyexecutor.Response{Payload: translated, Headers: httpResp.Header.Clone()}, nil
	}

	if httpResp.StatusCode == http.StatusForbidden {
		e.applyAntigravityTOSState(e.cfg, auth, bodyBytes)
	}
	sErr := newAntigravityStatusErr(httpResp.StatusCode, bodyBytes)
	if httpResp.StatusCode == http.StatusTooManyRequests {
		closeAntigravityAuthIdleTransports(auth)
		if retryAfter, parseErr := helps.ParseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
			sErr.retryAfter = retryAfter
		}
	}
	return cliproxyexecutor.Response{}, sErr
}
