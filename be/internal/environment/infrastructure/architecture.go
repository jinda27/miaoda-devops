package infrastructure

import "strings"

func normalizeArchitecture(arch string) string {
	// 统一系统工具、Go 和文档的架构名称。
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	case "x86", "386", "i386", "i686":
		return "386"
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}
