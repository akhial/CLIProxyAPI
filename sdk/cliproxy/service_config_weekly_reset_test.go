package cliproxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestWeeklyResetRoutingStrategyNormalization(t *testing.T) {
	for _, input := range []string{"weekly-reset-weighted-round-robin", "weeklyresetweightedroundrobin", "weekly-reset-wrr", " Weekly-Reset-WRR "} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: input},
		})
		if state.strategy != "weekly-reset-weighted-round-robin" {
			t.Fatalf("strategy for %q = %q, want weekly-reset-weighted-round-robin", input, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.WeightedRoundRobinSelector); !ok {
			t.Fatalf("selector type for %q = %T, want *auth.WeightedRoundRobinSelector", input, newRoutingSelector(state))
		}
	}
}

// weeklyResetRoutingAuths returns a credential whose weekly quota resets in 12 hours,
// according to a registered hint, and one without a known reset.
func weeklyResetRoutingAuths(t *testing.T) []*coreauth.Auth {
	t.Helper()
	now := time.Now()
	soon := &coreauth.Auth{ID: "service-weekly-soon-" + t.Name(), Provider: "claude"}
	unknown := &coreauth.Auth{ID: "service-weekly-unknown-" + t.Name(), Provider: "claude"}
	coreauth.SetWeeklyResetHint(soon.ID, now.Add(12*time.Hour), now)
	return []*coreauth.Auth{soon, unknown}
}

func countRoutingPicks(t *testing.T, selector coreauth.Selector, auths []*coreauth.Auth, picks int, sessions bool) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for index := 0; index < picks; index++ {
		opts := cliproxyexecutor.Options{}
		if sessions {
			opts.OriginalRequest = []byte(fmt.Sprintf(`{"session_id":"weekly-session-%d"}`, index))
		}
		picked, errPick := selector.Pick(context.Background(), "claude", "claude-model", opts, auths)
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", index, errPick)
		}
		counts[picked.ID]++
	}
	return counts
}

func TestWeeklyResetRoutingSelectorBoostsCredentialNearReset(t *testing.T) {
	auths := weeklyResetRoutingAuths(t)
	soonID, unknownID := auths[0].ID, auths[1].ID

	weighted := newRoutingSelector(normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "weighted-round-robin"},
	}))
	if counts := countRoutingPicks(t, weighted, auths, 20, false); counts[soonID] != 10 || counts[unknownID] != 10 {
		t.Fatalf("weighted-round-robin picks = %#v, want 10 each", counts)
	}

	weeklyReset := newRoutingSelector(normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "weekly-reset-wrr"},
	}))
	if counts := countRoutingPicks(t, weeklyReset, auths, 20, false); counts[soonID] != 20 {
		t.Fatalf("weekly-reset picks = %#v, want all on %s", counts, soonID)
	}
}

func TestWeeklyResetRoutingSelectorWithSessionAffinity(t *testing.T) {
	auths := weeklyResetRoutingAuths(t)
	soonID := auths[0].ID

	selector := newRoutingSelector(normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "weekly-reset-weighted-round-robin", SessionAffinity: true},
	}))
	if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
		defer stoppable.Stop()
	}
	if _, ok := selector.(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", selector)
	}
	if counts := countRoutingPicks(t, selector, auths, 20, true); counts[soonID] != 20 {
		t.Fatalf("new session picks = %#v, want all on %s", counts, soonID)
	}
}

func TestApplyManagerConfigSwitchesToWeeklyResetStrategy(t *testing.T) {
	auths := weeklyResetRoutingAuths(t)
	soonID := auths[0].ID
	service := &Service{coreManager: coreauth.NewManager(nil, &coreauth.WeightedRoundRobinSelector{}, nil)}

	commit := configCommit{cfg: &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "weekly-reset-wrr"},
	}, sequence: 1}
	if !service.applyManagerConfig(context.Background(), commit) {
		t.Fatal("applyManagerConfig failed")
	}
	if counts := countRoutingPicks(t, service.coreManager.Selector(), auths, 20, false); counts[soonID] != 20 {
		t.Fatalf("picks after switching strategy = %#v, want all on %s", counts, soonID)
	}
}
