package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type onDemandAntigravityTestExecutor struct {
	execCalls    atomic.Int32
	refreshCalls atomic.Int32
}

func (e *onDemandAntigravityTestExecutor) Identifier() string { return "antigravity" }

func (e *onDemandAntigravityTestExecutor) Execute(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.execCalls.Add(1)
	updated, errRefresh := RefreshExecutionAuth(ctx, a, func(_ context.Context, latest *Auth) (*Auth, error) {
		e.refreshCalls.Add(1)
		cloned := latest.Clone()
		if cloned.Metadata == nil {
			cloned.Metadata = make(map[string]any)
		}
		cloned.Metadata["access_token"] = "fresh-ag-token"
		cloned.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
		return cloned, nil
	})
	if errRefresh != nil {
		return cliproxyexecutor.Response{}, errRefresh
	}
	if updated == nil || authAccessToken(updated) != "fresh-ag-token" {
		return cliproxyexecutor.Response{}, &Error{Code: "refresh_failed", Message: "expected refreshed access token"}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *onDemandAntigravityTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *onDemandAntigravityTestExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	return nil, nil
}

func (e *onDemandAntigravityTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *onDemandAntigravityTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestAntigravityExpiredAuth_SelectionAndSchedulerAllowOnDemandRefresh(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour).Format(time.RFC3339)

	newExpired := func(mutate func(*Auth)) *Auth {
		a := &Auth{
			ID:       "ag-expired",
			Provider: "antigravity",
			Status:   StatusActive,
			Metadata: map[string]any{
				"access_token":  "expired-access",
				"refresh_token": "valid-refresh",
				"expired":       past,
			},
		}
		if mutate != nil {
			mutate(a)
		}
		return a
	}

	cases := []struct {
		name        string
		auth        *Auth
		wantBlocked bool
	}{
		{
			name:        "expired antigravity with refresh_token stays selectable",
			auth:        newExpired(nil),
			wantBlocked: false,
		},
		{
			name: "missing refresh_token stays blocked",
			auth: newExpired(func(a *Auth) {
				delete(a.Metadata, "refresh_token")
			}),
			wantBlocked: true,
		},
		{
			name: "requires sign-in stays blocked",
			auth: newExpired(func(a *Auth) {
				a.RequiresSignIn = true
			}),
			wantBlocked: true,
		},
		{
			name: "rejected access token stays blocked",
			auth: newExpired(func(a *Auth) {
				a.RejectedAccessToken = "expired-access"
			}),
			wantBlocked: true,
		},
		{
			name: "refresh backoff active stays blocked",
			auth: newExpired(func(a *Auth) {
				a.NextRefreshAfter = now.Add(5 * time.Minute)
			}),
			wantBlocked: true,
		},
		{
			name: "explicit proactive refresh_interval_seconds stays blocked",
			auth: newExpired(func(a *Auth) {
				a.Metadata["refresh_interval_seconds"] = 1800
			}),
			wantBlocked: true,
		},
		{
			name: "non-antigravity provider stays blocked",
			auth: newExpired(func(a *Auth) {
				a.Provider = "codex"
			}),
			wantBlocked: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _, _ := isAuthBlockedForModel(tc.auth, "gemini-4-neon", now); got != tc.wantBlocked {
				t.Fatalf("isAuthBlockedForModel() = %v, want %v", got, tc.wantBlocked)
			}
			if got := isAuthBlockedByQuotaOrAuthForModel(tc.auth, "gemini-4-neon", now); got != tc.wantBlocked {
				t.Fatalf("isAuthBlockedByQuotaOrAuthForModel() = %v, want %v", got, tc.wantBlocked)
			}
			if got := isCredentialBlocked(tc.auth, 1, now); got != tc.wantBlocked {
				t.Fatalf("isCredentialBlocked() = %v, want %v", got, tc.wantBlocked)
			}
		})
	}

	// Scheduler demotion must not demote an on-demand Antigravity auth when its access token expires.
	expired := newExpired(nil)
	sched := &modelScheduler{
		modelKey: "gemini-4-neon",
		entries: map[string]*scheduledAuth{
			expired.ID: {
				auth:  expired,
				state: scheduledStateReady,
			},
		},
	}
	if changed := sched.demoteExpiredTokensLocked(now); changed {
		t.Fatal("demoteExpiredTokensLocked() = true, want false for on-demand Antigravity auth")
	}
	if sched.entries[expired.ID].state != scheduledStateReady {
		t.Fatalf("scheduled state = %v, want ready", sched.entries[expired.ID].state)
	}

	// Auto-refresh scheduling must remain disabled for idle expired Antigravity auth.
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	if m.shouldRefresh(expired, now) {
		t.Fatal("shouldRefresh() = true, want false for idle expired Antigravity auth")
	}
	if _, scheduled := m.nextRefreshCheckAt(now, expired, time.Second); scheduled {
		t.Fatal("nextRefreshCheckAt() scheduled = true, want false for idle expired Antigravity auth")
	}
}

func TestManagerExecute_ExpiredAntigravityRefreshesOnDemand(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	exec := &onDemandAntigravityTestExecutor{}
	manager.RegisterExecutor(exec)

	expiredAuth := &Auth{
		ID:       "ag-on-demand.json",
		Provider: "antigravity",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "antigravity",
			"access_token":  "expired-access-token",
			"refresh_token": "valid-refresh-token",
			"expired":       time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, errReg := manager.Register(ctx, expiredAuth); errReg != nil {
		t.Fatalf("Register error: %v", errReg)
	}
	registry.GetGlobalRegistry().RegisterClient(expiredAuth.ID, expiredAuth.Provider, []*registry.ModelInfo{{ID: "gemini-4-neon"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(expiredAuth.ID)
	})

	resp, errExec := manager.Execute(ctx, []string{"antigravity"}, cliproxyexecutor.Request{Model: "gemini-4-neon"}, cliproxyexecutor.Options{})
	if errExec != nil {
		t.Fatalf("Execute() error = %v, want nil (expired Antigravity auth must reach executor for on-demand refresh)", errExec)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("unexpected payload %q", string(resp.Payload))
	}
	if got := exec.execCalls.Load(); got != 1 {
		t.Fatalf("Execute calls = %d, want 1", got)
	}
	if got := exec.refreshCalls.Load(); got != 1 {
		t.Fatalf("RefreshExecutionAuth calls = %d, want 1", got)
	}

	updated, ok := manager.GetByID(expiredAuth.ID)
	if !ok || updated == nil {
		t.Fatal("expected refreshed auth to remain registered")
	}
	if got := authAccessToken(updated); got != "fresh-ag-token" {
		t.Fatalf("persisted access_token = %q, want fresh-ag-token", got)
	}
}
