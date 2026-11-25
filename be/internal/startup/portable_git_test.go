package startup

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPortableGitStartsAfterPostInstallError(t *testing.T) {
	initFailure := errors.New("exit status 1")
	attempts := 0
	initErr, verifyErr := PreparePortableGit(context.Background(), func(context.Context) error {
		return initFailure
	}, func(context.Context) error {
		attempts++
		// 模拟解压后的短暂不可用，随后无需重启即可通过。
		if attempts == 1 {
			return errors.New("tool not ready")
		}
		return nil
	})
	if !errors.Is(initErr, initFailure) || verifyErr != nil || attempts != 2 {
		t.Fatalf("首启恢复失败：初始化=%v，复检=%v，次数=%d", initErr, verifyErr, attempts)
	}
}

func TestPortableGitRequiresWorkingToolsEvenWithoutScript(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	gitFailure := errors.New("git.exe missing")
	initErr, verifyErr := PreparePortableGit(ctx, nil, func(context.Context) error {
		return gitFailure
	})
	if initErr != nil || !errors.Is(verifyErr, gitFailure) {
		t.Fatalf("工具损坏被误认为可用：初始化=%v，复检=%v", initErr, verifyErr)
	}
}
