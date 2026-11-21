package misc

import (
	"fmt"
	"runtime"
	"strings"
)

const (
	// AntigravityCLIVersion is the Antigravity CLI/ACP release version emulated by the gateway.
	AntigravityCLIVersion = "1.5.12"
	// AntigravityCLIChangelist is the build changelist reported by Antigravity CLI 1.5.12.
	AntigravityCLIChangelist = "1024904911"
	// AntigravityDefaultHostPath is the ACP host_path reported when no caller host is supplied.
	AntigravityDefaultHostPath = "t3-code/0.0.0"
)

// AntigravityGatewayUserAgent identifies Antigravity requests initiated by the gateway
// using the official Antigravity CLI/ACP User-Agent format.
var AntigravityGatewayUserAgent = AntigravityCLIUserAgent(runtime.GOOS, runtime.GOARCH, AntigravityDefaultHostPath)

// AntigravityCLIUserAgent builds the User-Agent string matching the official
// Antigravity CLI/ACP client for the given platform and optional ACP host path.
func AntigravityCLIUserAgent(osType, arch, hostPath string) string {
	osType = strings.ToLower(strings.TrimSpace(osType))
	arch = strings.ToLower(strings.TrimSpace(arch))
	if osType == "" {
		osType = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	parts := []string{
		"aidev_client",
		"os_type=" + osType,
		"arch=" + arch,
		"cl=" + AntigravityCLIChangelist,
		"auth_method=consumer",
		"datacollection_opt_in=false",
	}
	if hostPath = strings.TrimSpace(hostPath); hostPath != "" {
		parts = append(parts, "host_path="+hostPath)
	}
	return fmt.Sprintf("antigravity/cli/%s (%s)", AntigravityCLIVersion, strings.Join(parts, "; "))
}
