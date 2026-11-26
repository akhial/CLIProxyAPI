package misc

import (
	"runtime"
	"strings"
	"testing"
)

func TestAntigravityCLIUserAgent(t *testing.T) {
	got := AntigravityCLIUserAgent("darwin", "arm64", "capture-fixture/1")
	want := "antigravity/cli/1.5.12 (aidev_client; os_type=darwin; arch=arm64; cl=1024904911; auth_method=consumer; datacollection_opt_in=false; host_path=capture-fixture/1)"
	if got != want {
		t.Fatalf("AntigravityCLIUserAgent() = %q, want %q", got, want)
	}

	def := AntigravityCLIUserAgent("", "", "")
	if !strings.Contains(def, "os_type="+runtime.GOOS+"; arch="+runtime.GOARCH) {
		t.Fatalf("default platform missing in %q", def)
	}
	if !strings.Contains(def, "host_path="+AntigravityDefaultHostPath+")") {
		t.Fatalf("default host_path missing in %q", def)
	}
}
