package infrastructure

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func nativeArchitecture() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Rosetta 下也按硬件架构选择 ARM64 包。
	if output, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "hw.optional.arm64").Output(); err == nil {
		if strings.TrimSpace(string(output)) == "1" {
			return "arm64"
		}
		return "amd64"
	}
	return runtime.GOARCH
}
