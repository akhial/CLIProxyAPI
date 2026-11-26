package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestServiceStartModelCatalogUpdatersSkipsAntigravityHubManifestPolling(t *testing.T) {
	registry.SetLocalModelCatalogs(true)
	defer registry.SetLocalModelCatalogs(false)

	restoreCache := misc.SetAntigravityVersionCacheForTest("", time.Time{})
	defer restoreCache()

	var manifestHits atomic.Int32
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manifestHits.Add(1)
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte("version: 2.35.0\npath: Antigravity-arm64-mac.zip\n"))
	}))
	defer manifestServer.Close()

	xaiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.0.46"}`))
	}))
	defer xaiServer.Close()

	restoreManifestURL := misc.OverrideAntigravityHubManifestURLForTest(manifestServer.URL)
	defer restoreManifestURL()
	restoreXAIURL := helps.OverrideXAINPMRegistryURLForTest(xaiServer.URL)
	defer restoreXAIURL()

	notified := make(chan struct{}, 1)
	restoreHook := misc.SetAntigravityVersionRefreshedHookForTest(notified)
	defer restoreHook()

	s := &Service{
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{
				ProxyURL: "direct",
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.startModelCatalogUpdaters(ctx)

	select {
	case <-notified:
		t.Fatal("startModelCatalogUpdaters unexpectedly triggered Antigravity Hub manifest polling")
	case <-time.After(100 * time.Millisecond):
	}

	if hits := manifestHits.Load(); hits != 0 {
		t.Fatalf("manifest server received %d request(s), want 0", hits)
	}
}
