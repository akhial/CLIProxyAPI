package misc

import (
	"runtime"
	"strings"
	"testing"
)

func TestAntigravityCLIUserAgent(t *testing.T) {
	got := AntigravityCLIUserAgent("darwin", "arm64", "t3-code/0.1.12")
	want := "antigravity/cli/" + AntigravityCLIVersion + " (aidev_client; os_type=darwin; arch=arm64; cl=" + AntigravityCLIChangelist + "; auth_method=consumer; datacollection_opt_in=false; host_path=t3-code/0.1.12)"
	if got != want {
		t.Fatalf("AntigravityCLIUserAgent = %q, want %q", got, want)
	}

	withoutHost := AntigravityCLIUserAgent("", "", "")
	if !strings.Contains(withoutHost, "os_type="+runtime.GOOS+"; arch="+runtime.GOARCH) {
		t.Fatalf("default platform missing in %q", withoutHost)
	}
	if strings.Contains(withoutHost, "host_path=") {
		t.Fatalf("empty hostPath should be omitted: %q", withoutHost)
	}

	if !strings.HasPrefix(AntigravityGatewayUserAgent, "antigravity/cli/") || strings.Contains(AntigravityGatewayUserAgent, "CLIProxyAPI") {
		t.Fatalf("AntigravityGatewayUserAgent = %q, want official antigravity/cli format", AntigravityGatewayUserAgent)
	}
}
