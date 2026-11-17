package larkcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCLIErrorFailedPrecondition(t *testing.T) {
	err := (&CLIError{
		Subtype: "failed_precondition",
		Message: "forbid ddl/dcl operation in online env",
	}).Error()
	if err != "online 环境禁止 DDL/DCL 操作，请改用数据操作或通过 dev 发布迁移。" {
		t.Fatalf("错误信息未保留失败前置条件：%s", err)
	}
}

func TestCLIErrorPreservesAPIMessage(t *testing.T) {
	err := (&CLIError{Type: "api", Subtype: "unknown", Code: 400002516, Message: "记录不存在"}).Error()
	if err != "记录不存在" {
		t.Fatalf("API 原始错误被覆盖：%s", err)
	}
}

// newAuthorizationTestClient 使用本地替身验证凭证隔离和授权状态。
func newAuthorizationTestClient(t *testing.T) *Client {
	t.Helper()
	// Windows 不执行 Unix Shell 替身。
	if runtime.GOOS == "windows" {
		t.Skip("需要 Unix Shell")
	}
	work := t.TempDir()
	binary := filepath.Join(work, "lark-cli")
	script := `#!/bin/sh
set -eu
# 外部凭证不能传入任何 CLI 调用。
for name in LARKSUITE_CLI_USER_ACCESS_TOKEN LARKSUITE_CLI_APP_ID; do
  if printenv "$name" >/dev/null 2>&1; then exit 1; fi
done
case "$1" in
  profile) printf '[{"name":"tenant","user":"测试用户","tokenStatus":"%s"}]' "${MOCK_TOKEN_STATUS:-valid}" ;;
  auth)
    if [ "${MOCK_LOGIN_FAIL:-}" = "1" ]; then exit 1; fi
    printf '{}' ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIAODA_LARK_CLI", binary)
	return NewClient(work)
}

func TestExternalCredentialsAreRemoved(t *testing.T) {
	names := []string{"LARKSUITE_CLI_USER_ACCESS_TOKEN", "LARKSUITE_CLI_USER_REFRESH_TOKEN",
		"LARKSUITE_CLI_TENANT_ACCESS_TOKEN", "LARKSUITE_CLI_APP_ID", "LARKSUITE_CLI_APP_SECRET"}
	// 用虚构值验证全部凭证变量被清除。
	for _, name := range names {
		t.Setenv(name, "external-test-value")
	}
	t.Setenv("PLATFORM_ENV_TEST", "keep")
	env := Environment()
	preserved := false
	// 过滤后的子进程环境应保留普通配置。
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if externalCredential(name) {
			t.Fatalf("子进程仍携带外部凭证变量：%s", name)
		}
		if entry == "PLATFORM_ENV_TEST=keep" {
			preserved = true
		}
	}
	if !preserved {
		t.Fatal("子进程缺少普通环境变量")
	}
	ClearInheritedCredentials()
	// 启动环境本身也不应保留凭证。
	for _, name := range names {
		if _, exists := os.LookupEnv(name); exists {
			t.Fatalf("启动环境仍携带外部凭证变量：%s", name)
		}
	}
	if os.Getenv("PLATFORM_ENV_TEST") != "keep" {
		t.Fatal("普通环境变量被清除")
	}
}

func TestCLIAndPackagingIgnoreExternalCredentials(t *testing.T) {
	client := newAuthorizationTestClient(t)
	t.Setenv("LARKSUITE_CLI_USER_ACCESS_TOKEN", "external-test-token")
	t.Setenv("LARKSUITE_CLI_APP_ID", "external-test-app")
	var profiles []Profile
	if err := client.Run(context.Background(), client.WorkDir(), "", &profiles, "profile", "list"); err != nil {
		t.Fatalf("CLI 未隔离外部凭证：%v", err)
	}
	env, err := client.ScopedEnvironment(t.TempDir(), "tenant")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "lark-cli profile list")
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		t.Fatalf("打包脚本未隔离外部凭证：%v", err)
	}
}

func TestPlatformRequiresAuthorizationAfterStartup(t *testing.T) {
	client := newAuthorizationTestClient(t)
	ctx := context.Background()
	// 本机已有有效凭据也不能直接使用平台。
	if _, err := client.SelectProfile(ctx, "tenant", true); err == nil {
		t.Fatal("启动后沿用了已有登录状态")
	}
	auth := NewAuthService(client)
	auth.complete(Authorization{ID: "test", profile: "tenant"}, "test-device")
	result, err := auth.Status("test", "tenant")
	if err != nil || result.Status != "done" {
		t.Fatalf("授权未完成：status=%s err=%v", result.Status, err)
	}
	if _, err := client.SelectProfile(ctx, "tenant", true); err != nil {
		t.Fatalf("授权完成后仍无法使用：%v", err)
	}
	// 新平台进程重新要求授权。
	if _, err := NewClient(client.WorkDir()).SelectProfile(ctx, "tenant", true); err == nil {
		t.Fatal("重启后沿用了上次平台授权")
	}
}

func TestFailedAuthorizationDoesNotUnlockPlatform(t *testing.T) {
	// 登录失败或未获得有效用户均不可放行。
	for _, scenario := range []string{"login_failed", "invalid_token"} {
		t.Run(scenario, func(t *testing.T) {
			client := newAuthorizationTestClient(t)
			if scenario == "login_failed" {
				t.Setenv("MOCK_LOGIN_FAIL", "1")
			} else {
				t.Setenv("MOCK_TOKEN_STATUS", "expired")
			}
			auth := NewAuthService(client)
			auth.complete(Authorization{ID: "test", profile: "tenant"}, "test-device")
			result, err := auth.Status("test", "tenant")
			if err != nil || result.Status != "failed" {
				t.Fatalf("无效授权未失败：status=%s err=%v", result.Status, err)
			}
			if _, err := client.SelectProfile(context.Background(), "tenant", true); err == nil {
				t.Fatal("无效授权允许使用平台")
			}
		})
	}
}
