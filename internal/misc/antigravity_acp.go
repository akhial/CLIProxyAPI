package misc

import (
	"fmt"
	"runtime"
	"strings"
)

const (
	// AntigravityCLIVersion and AntigravityCLIChangelist match the official
	// Antigravity CLI/ACP binary (1.5.12).
	AntigravityCLIVersion      = "1.5.12"
	AntigravityCLIChangelist   = "1024904911"
	AntigravityDefaultHostPath = "t3-code/0.0.0"
)

// AntigravityCLIUserAgent formats the official Antigravity CLI/ACP User-Agent string.
func AntigravityCLIUserAgent(osType, arch, hostPath string) string {
	osType = strings.TrimSpace(osType)
	if osType == "" {
		osType = runtime.GOOS
	}
	arch = strings.TrimSpace(arch)
	if arch == "" {
		arch = runtime.GOARCH
	}
	hostPath = strings.TrimSpace(hostPath)
	if hostPath == "" {
		hostPath = AntigravityDefaultHostPath
	}
	return fmt.Sprintf(
		"antigravity/cli/%s (aidev_client; os_type=%s; arch=%s; cl=%s; auth_method=consumer; datacollection_opt_in=false; host_path=%s)",
		AntigravityCLIVersion,
		osType,
		arch,
		AntigravityCLIChangelist,
		hostPath,
	)
}
