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

	if gotDarwin := AntigravityCLIUserAgent("darwin", "", ""); !strings.Contains(gotDarwin, "os_type=darwin; arch=arm64") {
		t.Fatalf("darwin default arch mismatch in %q", gotDarwin)
	}
	if gotLinux := AntigravityCLIUserAgent("linux", "", ""); !strings.Contains(gotLinux, "os_type=linux; arch=amd64") {
		t.Fatalf("linux default arch mismatch in %q", gotLinux)
	}
	def := AntigravityCLIUserAgent("", "", "")
	if !strings.Contains(def, "os_type="+runtime.GOOS+"; arch="+AntigravityDefaultArchForOS(runtime.GOOS)) {
		t.Fatalf("default platform missing in %q", def)
	}
	if !strings.Contains(def, "host_path="+AntigravityDefaultHostPath+")") {
		t.Fatalf("default host_path missing in %q", def)
	}
}
