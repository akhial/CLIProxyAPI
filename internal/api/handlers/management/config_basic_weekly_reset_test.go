package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const weeklyResetStrategy = "weekly-reset-weighted-round-robin"

func TestNormalizeRoutingStrategyWeeklyResetWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{weeklyResetStrategy, "weeklyresetweightedroundrobin", "weekly-reset-wrr", " Weekly-Reset-WRR "} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != weeklyResetStrategy {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want %s, true", input, got, ok, weeklyResetStrategy)
		}
	}
}

func TestPutRoutingStrategyAcceptsWeeklyResetAlias(t *testing.T) {
	cfg := &config.Config{}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/routing/strategy", strings.NewReader(`{"value":"weekly-reset-wrr"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutRoutingStrategy(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cfg.Routing.Strategy != weeklyResetStrategy {
		t.Fatalf("routing.strategy = %q, want %s", cfg.Routing.Strategy, weeklyResetStrategy)
	}

	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/routing/strategy", nil)
	h.GetRoutingStrategy(ctx)
	var body struct {
		Strategy string `json:"strategy"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &body); errDecode != nil || body.Strategy != weeklyResetStrategy {
		t.Fatalf("GET body = %s (%v), want strategy %s", rec.Body.String(), errDecode, weeklyResetStrategy)
	}
}

func TestConfigV8AcceptsWeeklyResetRoutingStrategy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("routing: {strategy: weighted-round-robin}\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/v8/management/config/routing/strategy", strings.NewReader(`"`+weeklyResetStrategy+`"`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if h.cfg.Routing.Strategy != weeklyResetStrategy {
		t.Fatalf("routing.strategy = %q, want %s", h.cfg.Routing.Strategy, weeklyResetStrategy)
	}
	saved, errRead := os.ReadFile(path)
	if errRead != nil || !strings.Contains(string(saved), weeklyResetStrategy) {
		t.Fatalf("saved config = %q (%v), want %s", saved, errRead, weeklyResetStrategy)
	}
}
