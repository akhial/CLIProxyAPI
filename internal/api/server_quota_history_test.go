package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestManagementV8QuotaHistoryRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nremote-management: {secret-key: test-password}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = filepath.Join(root, "auths")
	cfg.Plugins.Dir = root
	h := management.NewHandler(cfg, path, nil)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()

	request := func(query string, authorized bool) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v8/management/quota/history"+query, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if authorized {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		recorder := httptest.NewRecorder()
		s.engine.ServeHTTP(recorder, req)
		var body map[string]any
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &body); errDecode != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), errDecode)
		}
		return recorder.Code, body
	}

	if status, _ := request("", false); status != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", status)
	}
	status, body := request("", true)
	if status != http.StatusOK || body["enabled"] != false {
		t.Fatalf("before start: status=%d body=%v", status, body)
	}

	t.Setenv("QUOTA_HISTORY_DISABLE", "")
	t.Setenv("QUOTA_HISTORY_DIR", "")
	t.Setenv("QUOTA_HISTORY_POLL_INTERVAL", "")
	h.StartQuotaHistory()
	defer h.StopQuotaHistory()
	status, body = request("?from=0", true)
	if status != http.StatusOK || body["enabled"] != true || body["poll_interval_seconds"] != float64(600) {
		t.Fatalf("after start: status=%d body=%v", status, body)
	}
	if status, _ = request("?to=nope", true); status != http.StatusBadRequest {
		t.Fatalf("bad param status = %d, want 400", status)
	}
	// Nothing is written until the first sample, so a reader never creates the directory.
	if _, errStat := os.Stat(filepath.Join(root, "quota-history")); !os.IsNotExist(errStat) {
		t.Fatalf("quota-history directory created early: %v", errStat)
	}
}
