package larkcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
  profile)
    status="${MOCK_TOKEN_STATUS:-valid}"
    # 授权成功后模拟 CLI 持久化有效登录。
    if [ -f .authorized ]; then status=valid; fi
    printf '[{"name":"tenant","user":"测试用户","tokenStatus":"%s"}]' "$status" ;;
  auth)
    if [ "$2" = qrcode ]; then exit 0; fi
    if [ "$3" = --domain ]; then
      code=1
      if [ -f .device-count ]; then code=$(($(cat .device-count) + 1)); fi
      printf '%s' "$code" > .device-count
      printf '{"device_code":"%s","verification_url":"https://accounts.feishu.cn/test?code=%s","expires_in":300}' "$code" "$code"
      exit 0
    fi
    if [ "${MOCK_QR_EXPIRED:-}" = 1 ] && [ "$4" = 1 ]; then
      printf '{"ok":false,"error":{"subtype":"token_expired","message":"device code expired"}}' >&2
      exit 1
    fi
    while [ "${MOCK_AUTH_WAIT:-}" = 1 ] && [ "$4" = 1 ]; do sleep 0.03; done
    if [ "${MOCK_LOGIN_FAIL:-}" = "1" ]; then exit 1; fi
    : > .authorized
    printf '{}' ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIAODA_LARK_CLI", binary)
	return NewClient(work)
}

func beginTestAuthorization(t *testing.T, service *AuthService) Authorization {
	t.Helper()
	task, _, err := service.Begin(context.Background(), "tenant")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		task.cancel()
		<-task.done
	})
	return task
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

func TestExistingLoginStateIsReused(t *testing.T) {
	client := newAuthorizationTestClient(t)
	profiles, err := client.Profiles(context.Background())
	if err != nil || len(profiles) != 1 {
		t.Fatalf("无法读取本地登录状态：%v", err)
	}
	// 直接沿用 lark-cli 已保存的登录状态。
	if profiles[0].User != "测试用户" || !profiles[0].Ready() {
		t.Fatalf("本地登录状态未透传：%+v", profiles[0])
	}
	if _, err := client.SelectProfile(context.Background(), "tenant", true); err != nil {
		t.Fatalf("已有登录无法使用：%v", err)
	}
	// 平台重启后继续复用本机凭据。
	if _, err := NewClient(client.WorkDir()).SelectProfile(context.Background(), "tenant", true); err != nil {
		t.Fatalf("重启后未复用本地登录：%v", err)
	}
}

func TestExpiredLoginRequiresAuthorization(t *testing.T) {
	client := newAuthorizationTestClient(t)
	t.Setenv("MOCK_TOKEN_STATUS", "expired")
	if _, err := client.SelectProfile(context.Background(), "tenant", true); err == nil {
		t.Fatal("过期登录已允许使用平台")
	}
	auth := NewAuthService(client)
	task := beginTestAuthorization(t, auth)
	<-task.done
	result, err := auth.Status(task.ID, "tenant")
	if err != nil || result.Status != "done" {
		t.Fatalf("授权未完成：status=%s err=%v", result.Status, err)
	}
	// 授权后 CLI 返回有效登录，可直接使用。
	if _, err := client.SelectProfile(context.Background(), "tenant", true); err != nil {
		t.Fatalf("授权后仍无法使用：%v", err)
	}
}

func TestFailedAuthorizationDoesNotUnlockPlatform(t *testing.T) {
	t.Run("login_failed", func(t *testing.T) {
		client := newAuthorizationTestClient(t)
		t.Setenv("MOCK_LOGIN_FAIL", "1")
		auth := NewAuthService(client)
		task := beginTestAuthorization(t, auth)
		<-task.done
		result, err := auth.Status(task.ID, "tenant")
		if err != nil || result.Status != "failed" {
			t.Fatalf("无效授权未失败：status=%s err=%v", result.Status, err)
		}
	})
	t.Run("no_valid_user", func(t *testing.T) {
		client := newAuthorizationTestClient(t)
		t.Setenv("MOCK_TOKEN_STATUS", "not_logged_in")
		if _, err := client.SelectProfile(context.Background(), "tenant", true); err == nil {
			t.Fatal("无效登录允许使用平台")
		}
	})
}

func TestRefreshAuthorizationQR(t *testing.T) {
	for _, mode := range []string{"MOCK_QR_EXPIRED", "MOCK_AUTH_WAIT"} {
		t.Run(mode, func(t *testing.T) {
			client := newAuthorizationTestClient(t)
			t.Setenv(mode, "1")
			auth := NewAuthService(client)
			first := beginTestAuthorization(t, auth)
			// 过期任务必须清掉旧链接，等待中的任务也可主动刷新。
			if mode == "MOCK_QR_EXPIRED" {
				<-first.done
				result, err := auth.Status(first.ID, "tenant")
				if err != nil || result.Status != "expired" || result.URL != "" {
					t.Fatalf("旧授权未标记为过期：status=%s err=%v", result.Status, err)
				}
			}
			second, status, err := auth.Refresh(context.Background(), first.ID, "tenant")
			if err != nil || status != 202 || first.ID == second.ID || first.URL == second.URL {
				t.Fatalf("刷新未取得新授权：status=%d err=%v", status, err)
			}
			<-second.done
			result, err := auth.Status(second.ID, "tenant")
			if err != nil || result.Status != "done" {
				t.Fatalf("新授权未完成：status=%s err=%v", result.Status, err)
			}
		})
	}
}

func TestAuthorizationChecksLocalStateWhileCLIWaiting(t *testing.T) {
	for _, previous := range []string{"expired", "valid"} {
		t.Run(previous, func(t *testing.T) {
			client := newAuthorizationTestClient(t)
			t.Setenv("MOCK_TOKEN_STATUS", previous)
			t.Setenv("MOCK_AUTH_WAIT", "1")
			auth := NewAuthService(client)
			task := beginTestAuthorization(t, auth)
			if previous == "valid" {
				// 重新授权时不能把原有有效登录视为这一次扫码的结果。
				select {
				case <-task.done:
					t.Fatal("旧登录态被当成本次授权")
				case <-time.After(4500 * time.Millisecond):
				}
				return
			}
			// 本地登录已完成，但 CLI 等待进程仍未退出。
			if err := os.WriteFile(filepath.Join(client.WorkDir(), ".authorized"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case <-task.done:
			case <-time.After(9 * time.Second):
				t.Fatal("登录已落盘但平台仍等待二维码")
			}
			result, err := auth.Status(task.ID, "tenant")
			if err != nil || result.Status != "done" || result.URL != "" || result.QR != "" ||
				result.Account == nil || !result.Account.Ready() {
				t.Fatalf("未确认本地授权结果：%+v err=%v", result, err)
			}
		})
	}
}
