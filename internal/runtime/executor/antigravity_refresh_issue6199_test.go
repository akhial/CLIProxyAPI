package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type issue6199AntigravityRefreshResult struct {
	auth *cliproxyauth.Auth
	err  error
}

func issue6199AntigravityAuth(id, refreshToken string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       id,
		Provider: "antigravity",
		Status:   cliproxyauth.StatusActive,
		Metadata: map[string]any{
			"access_token":  "expired-access",
			"refresh_token": refreshToken,
			"project_id":    "existing-project",
			"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
}

func issue6199AntigravityJSONResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestIssue6199AntigravitySharedOAuthHasIndependentAcquisitionDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan context.Context, 1)
		release := make(chan struct{})
		defer close(release)
		tokenCalls := 0
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://oauth2.googleapis.com/token":
				tokenCalls++
				started <- req.Context()
				select {
				case <-req.Context().Done():
					return nil, req.Context().Err()
				case <-release:
					return issue6199AntigravityJSONResponse(req, `{"access_token":"new-access","expires_in":3600}`), nil
				}
			case "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist":
				return issue6199AntigravityJSONResponse(req, `{"paidTier":{"id":"tier","availableCredits":[]}}`), nil
			default:
				return nil, fmt.Errorf("unexpected intercepted request: %s", req.URL)
			}
		})
		callerDeadline := time.Now().Add(time.Hour)
		callerCtx, cancelCaller := context.WithDeadline(context.Background(), callerDeadline)
		defer cancelCaller()
		callerCtx = context.WithValue(callerCtx, "cliproxy.roundtripper", transport)
		followerCtx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
		executor := &AntigravityExecutor{}
		results := make(chan issue6199AntigravityRefreshResult, 2)
		refreshToken := t.Name() + "-shared-refresh"
		runRefresh := func(ctx context.Context, id string) {
			updated, errRefresh := executor.Refresh(ctx, issue6199AntigravityAuth(id, refreshToken))
			results <- issue6199AntigravityRefreshResult{auth: updated, err: errRefresh}
		}
		go runRefresh(callerCtx, "issue6199-oauth-leader")
		var requestCtx context.Context
		select {
		case requestCtx = <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("OAuth request did not reach the intercepted transport")
		}
		go runRefresh(followerCtx, "issue6199-oauth-follower")
		// Wait until the follower is blocked inside singleflight, without wall-clock sleeps.
		synctest.Wait()
		if tokenCalls != 1 {
			t.Fatalf("shared OAuth requests = %d, want 1", tokenCalls)
		}

		cancelCaller()
		synctest.Wait()
		if errRequestContext := requestCtx.Err(); errRequestContext != nil {
			t.Fatalf("single caller cancellation canceled shared OAuth: %v", errRequestContext)
		}
		deadline, hasDeadline := requestCtx.Deadline()
		if !hasDeadline {
			t.Fatal("shared OAuth request has no credential-acquisition deadline after WithoutCancel")
		}
		if !deadline.After(time.Now()) || !deadline.Before(callerDeadline) {
			t.Fatalf("shared OAuth deadline = %s, want an independent acquisition budget before caller deadline %s", deadline, callerDeadline)
		}

		// Advance only the bubble clock to the actual acquisition deadline.
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		<-timer.C
		synctest.Wait()
		for i := 0; i < 2; i++ {
			select {
			case result := <-results:
				if !errors.Is(result.err, context.DeadlineExceeded) {
					t.Errorf("shared refresh error = %v, want context.DeadlineExceeded", result.err)
				}
			case <-time.After(time.Second):
				t.Fatal("shared refresh did not finish at its credential-acquisition deadline")
			}
		}
		if tokenCalls != 1 {
			t.Fatalf("OAuth calls after acquisition deadline = %d, want 1", tokenCalls)
		}
	})
}

func TestIssue6199AntigravityProjectAcquisitionHasDeadline(t *testing.T) {
	auth := issue6199AntigravityAuth(t.Name(), "unused-refresh")
	delete(auth.Metadata, "project_id")
	auth.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			return nil, fmt.Errorf("unexpected intercepted request: %s", req.URL)
		}
		if deadline, ok := req.Context().Deadline(); !ok || !deadline.After(time.Now()) {
			t.Error("required project acquisition has no finite credential-acquisition deadline")
		}
		return issue6199AntigravityJSONResponse(req, `{"cloudaicompanionProject":"acquired-project"}`), nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
	executor := &AntigravityExecutor{}
	updated, errPrepare := executor.PrepareRequestAuth(ctx, auth)
	if errPrepare != nil || updated == nil || antigravityProjectIDFromAuth(updated) != "acquired-project" {
		t.Fatalf("project preparation = %#v, %v, want acquired project", updated, errPrepare)
	}
}
