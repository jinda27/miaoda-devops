package larkcli

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newConfigTestClient(t *testing.T) *Client {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("需要 Unix Shell")
	}
	work := t.TempDir()
	script := `#!/bin/sh
set -eu
for name in LARKSUITE_CLI_USER_ACCESS_TOKEN LARKSUITE_CLI_APP_ID LARKSUITE_CLI_APP_SECRET; do
  if printenv "$name" >/dev/null 2>&1; then exit 1; fi
done
case "$1" in
  profile)
    if [ "${MOCK_PROFILE_FAIL:-}" = 1 ]; then
      printf '{"ok":false,"error":{"message":"配置存储读取失败"}}' >&2
      exit 1
    fi
    if [ -f "$MOCK_CONFIG_DIR/config.saved" ]; then
      name="$(cat "$MOCK_CONFIG_DIR/config.saved")"
      printf '[{"name":"existing","user":"原账号","tokenStatus":"valid"},{"name":"%s","tokenStatus":"not_logged_in"}]' "$name"
    elif [ "${MOCK_EMPTY_PROFILES:-}" = 1 ]; then
      printf '[]'
    else
      printf '[{"name":"existing","user":"原账号","tokenStatus":"valid"}]'
    fi ;;
  config)
    count=1
    if [ -f "$MOCK_CONFIG_DIR/config.count" ]; then count=$(($(cat "$MOCK_CONFIG_DIR/config.count") + 1)); fi
    printf '%s' "$count" > "$MOCK_CONFIG_DIR/config.count"
    printf '%s\n' "$@" >> "$MOCK_CONFIG_DIR/config.calls"
    printf '  https://accounts.feishu.cn/config?code=fixture%%2B' >&2
    if [ "$count" != 1 ]; then printf '%s' "$count" >&2; fi
    printf '&lang=zh\n' >&2
    if [ "${MOCK_CONFIG_EXPIRED:-}" = 1 ] && [ "$count" = 1 ]; then
      printf '\n{\n "ok": false,\n "error": {"subtype":"token_expired"}\n}\n' >&2
      exit 1
    fi
    while [ ! -f "$MOCK_CONFIG_DIR/config.finish" ]; do sleep 0.01; done
    if [ "${MOCK_CONFIG_FAIL:-}" = 1 ]; then
      printf '{"ok":false,"error":{"message":"private-test-secret"}}' >&2
      exit 1
    fi
    if [ "${MOCK_CONFIG_NO_SAVE:-}" != 1 ]; then
      printf '%s' "${4#--name=}" > "$MOCK_CONFIG_DIR/config.saved"
    fi
    printf '{"ok":true,"data":{"appId":"fixture","appSecret":"****"}}' ;;
  auth)
    if [ "$2" != qrcode ]; then exit 1; fi
    cp "$MOCK_CONFIG_DIR/qr.png" auth.png
    printf '{}' ;;
esac
`
	binary := filepath.Join(work, "lark-cli")
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aG1sAAAAASUVORK5CYII=")
	if err := os.WriteFile(filepath.Join(work, "qr.png"), png, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIAODA_LARK_CLI", binary)
	t.Setenv("MOCK_CONFIG_DIR", work)
	return NewClient(work)
}

func waitConfiguration(t *testing.T, service *ConfigService, id string, ready func(TenantConfiguration) bool) TenantConfiguration {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	// 等待后台任务的可观察状态，不触发真实飞书配置。
	for {
		task, err := service.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if ready(task) {
			return task
		}
		select {
		case <-deadline:
			t.Fatalf("等待配置状态超时：%+v", task)
		case <-ticker.C:
		}
	}
}

func finishConfiguration(t *testing.T, client *Client) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(client.WorkDir(), "config.finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTenantConfigurationFirstInstall(t *testing.T) {
	client := newConfigTestClient(t)
	t.Setenv("MOCK_EMPTY_PROFILES", "1")
	t.Setenv("LARKSUITE_CLI_USER_ACCESS_TOKEN", "external-test-token")
	t.Setenv("LARKSUITE_CLI_APP_SECRET", "external-test-secret")
	service := NewConfigService(client)
	requestCtx, cancel := context.WithCancel(context.Background())
	task, status, err := service.Begin(requestCtx, "new-tenant")
	cancel()
	if err != nil || status != 202 {
		t.Fatalf("无账号时未启动配置：status=%d err=%v", status, err)
	}
	t.Cleanup(func() { _, _ = service.Cancel(task.ID) })
	pending := waitConfiguration(t, service, task.ID, func(item TenantConfiguration) bool { return item.URL != "" && item.QR != "" })
	if pending.URL != "https://accounts.feishu.cn/config?code=fixture%2B&lang=zh" ||
		!strings.HasPrefix(pending.QR, "data:image/png;base64,") || pending.Status != "pending" {
		t.Fatalf("配置链接或二维码未实时透传：%+v", pending)
	}
	if service.Current().ID != task.ID {
		t.Fatal("页面重载不能恢复进行中的配置")
	}
	reused, status, err := service.Begin(context.Background(), "new-tenant")
	if err != nil || status != 200 || reused.ID != task.ID {
		t.Fatalf("同名请求未复用：status=%d err=%v", status, err)
	}
	if _, status, err := service.Begin(context.Background(), "another"); err == nil || status != 409 {
		t.Fatal("允许并行覆盖 CLI 配置")
	}
	finishConfiguration(t, client)
	done := waitConfiguration(t, service, task.ID, func(item TenantConfiguration) bool { return item.Status != "pending" })
	if done.Status != "done" || done.URL != "" || done.QR != "" {
		t.Fatalf("已保存配置未成功结束：%+v", done)
	}
	profile, err := client.SelectProfile(context.Background(), "new-tenant", false)
	if err != nil || profile.Ready() {
		t.Fatal("配置成功被误当作登录授权成功")
	}
	calls, err := os.ReadFile(filepath.Join(client.WorkDir(), "config.calls"))
	if err != nil || string(calls) != "config\ninit\n--new\n--name=new-tenant\n" {
		t.Fatalf("配置参数错误或重复启动：%s err=%v", calls, err)
	}
}

func TestInitializeFirstTenant(t *testing.T) {
	client := newConfigTestClient(t)
	t.Setenv("MOCK_EMPTY_PROFILES", "1")
	service := NewConfigService(client)
	ctx := context.Background()
	task, status, err := service.Initialize(ctx)
	if err != nil || status != 202 || !task.Initial || task.Name != "feishu-default" {
		t.Fatalf("默认初始化未启动：task=%+v status=%d err=%v", task, status, err)
	}
	t.Cleanup(func() { _, _ = service.Cancel(task.ID) })
	reused, status, err := service.Initialize(ctx)
	// 重复点击复用同一条初始化任务。
	if err != nil || status != 200 || reused.ID != task.ID {
		t.Fatalf("重复初始化未复用任务：status=%d err=%v", status, err)
	}
	finishConfiguration(t, client)
	done := waitConfiguration(t, service, task.ID, func(item TenantConfiguration) bool { return item.Status != "pending" })
	if done.Status != "done" {
		t.Fatalf("默认初始化失败：%+v", done)
	}
	existing, status, err := service.Initialize(ctx)
	if err != nil || status != 200 || existing.Status != "existing" {
		t.Fatalf("已有配置未直接复用：task=%+v status=%d err=%v", existing, status, err)
	}
}

func TestInitializeRequiresSuccessfulProfileRead(t *testing.T) {
	client := newConfigTestClient(t)
	t.Setenv("MOCK_PROFILE_FAIL", "1")
	service := NewConfigService(client)
	// 存储不可读时不应创建配置，返回可阅读的错误。
	if _, status, err := service.Initialize(context.Background()); status != 502 || err == nil ||
		!strings.Contains(err.Error(), "配置存储读取失败") || service.Current().ID != "" {
		t.Fatalf("读取失败后发起了初始化或丢失原始错误：status=%d err=%v", status, err)
	}
}

func TestConfigurationCompletesWhileCLIStillWaiting(t *testing.T) {
	client := newConfigTestClient(t)
	t.Setenv("MOCK_EMPTY_PROFILES", "1")
	service := NewConfigService(client)
	task, _, err := service.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = service.Cancel(task.ID) })
	waitConfiguration(t, service, task.ID, func(item TenantConfiguration) bool { return item.QR != "" })
	// 模拟配置已落盘但 CLI 进程没有退出。
	if err := os.WriteFile(filepath.Join(client.WorkDir(), "config.saved"), []byte(task.Name), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.done:
	case <-time.After(9 * time.Second):
		t.Fatal("配置已落盘但页面状态仍在等待")
	}
	done, err := service.Status(task.ID)
	if err != nil || done.Status != "done" || done.URL != "" || done.QR != "" ||
		done.Account == nil || done.Account.Name != task.Name {
		t.Fatalf("未用已落盘配置收起二维码：%+v err=%v", done, err)
	}
}

func TestTenantConfigurationPreservesExistingAccount(t *testing.T) {
	client := newConfigTestClient(t)
	service := NewConfigService(client)
	if _, status, err := service.Begin(context.Background(), "existing"); err == nil || status != 409 {
		t.Fatal("允许覆盖已有配置")
	}
	task, _, err := service.Begin(context.Background(), "new-tenant")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = service.Cancel(task.ID) })
	finishConfiguration(t, client)
	waitConfiguration(t, service, task.ID, func(item TenantConfiguration) bool { return item.Status == "done" })
	if _, err := client.SelectProfile(context.Background(), "existing", true); err != nil {
		t.Fatalf("新增租户破坏原账号：%v", err)
	}
}

func TestTenantConfigurationFailureAndRetry(t *testing.T) {
	// CLI 报错和配置未落盘都必须失败，并允许重试。
	for _, mode := range []string{"MOCK_CONFIG_FAIL", "MOCK_CONFIG_NO_SAVE"} {
		t.Run(mode, func(t *testing.T) {
			client := newConfigTestClient(t)
			t.Setenv(mode, "1")
			service := NewConfigService(client)
			task, _, err := service.Begin(context.Background(), "new-tenant")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = service.Cancel(service.Current().ID) })
			finishConfiguration(t, client)
			result := waitConfiguration(t, service, task.ID, func(item TenantConfiguration) bool { return item.Status != "pending" })
			if result.Status != "failed" || strings.Contains(result.Message, "private-test-secret") {
				t.Fatalf("失败状态错误或泄漏凭据：%+v", result)
			}
			t.Setenv(mode, "0")
			retry, status, err := service.Begin(context.Background(), "new-tenant")
			if err != nil || status != 202 || retry.ID == task.ID {
				t.Fatalf("失败后无法重新配置：status=%d err=%v", status, err)
			}
			waitConfiguration(t, service, retry.ID, func(item TenantConfiguration) bool { return item.Status == "done" })
		})
	}
}

func TestTenantConfigurationCancel(t *testing.T) {
	client := newConfigTestClient(t)
	service := NewConfigService(client)
	task, _, err := service.Begin(context.Background(), "new-tenant")
	if err != nil {
		t.Fatal(err)
	}
	waitConfiguration(t, service, task.ID, func(item TenantConfiguration) bool { return item.URL != "" })
	if _, err := service.Cancel("wrong-id"); err == nil {
		t.Fatal("无效请求取消了其他配置")
	}
	if _, err := service.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	result := waitConfiguration(t, service, task.ID, func(item TenantConfiguration) bool { return item.Status != "pending" })
	if result.Status != "cancelled" || result.URL != "" {
		t.Fatalf("配置未取消：%+v", result)
	}
	finishConfiguration(t, client)
	retry, _, err := service.Begin(context.Background(), "new-tenant")
	if err != nil {
		t.Fatal(err)
	}
	waitConfiguration(t, service, retry.ID, func(item TenantConfiguration) bool { return item.Status == "done" })
}

func TestRefreshTenantConfigurationQR(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "pending"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			client := newConfigTestClient(t)
			t.Setenv("MOCK_EMPTY_PROFILES", "1")
			if expired {
				t.Setenv("MOCK_CONFIG_EXPIRED", "1")
			}
			service := NewConfigService(client)
			first, _, err := service.Initialize(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = service.Cancel(service.Current().ID) })
			old := waitConfiguration(t, service, first.ID, func(item TenantConfiguration) bool {
				if expired {
					return item.Status == "expired"
				}
				return item.URL != ""
			})
			// 过期后刷新页面仍可恢复任务，但不能展示旧链接。
			if expired && (old.URL != "" || service.Current().ID != first.ID) {
				t.Fatal("过期任务仍返回旧码或无法恢复")
			}
			second, status, err := service.Refresh(context.Background(), first.ID)
			if err != nil || status != 202 || second.ID == first.ID {
				t.Fatalf("刷新未启动新配置：status=%d err=%v", status, err)
			}
			fresh := waitConfiguration(t, service, second.ID, func(item TenantConfiguration) bool { return item.QR != "" })
			if !strings.Contains(fresh.URL, "fixture%2B2") || !fresh.Initial {
				t.Fatalf("未获取新的配置二维码：%+v", fresh)
			}
			finishConfiguration(t, client)
			done := waitConfiguration(t, service, second.ID, func(item TenantConfiguration) bool { return item.Status != "pending" })
			if done.Status != "done" {
				t.Fatalf("刷新后配置未成功：%+v", done)
			}
			if result, _, err := service.Refresh(context.Background(), second.ID); err != nil || result.Status != "existing" {
				t.Fatalf("已完成的配置被重复申请：status=%s err=%v", result.Status, err)
			}
		})
	}
}

func TestConfigOutput(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		url    string
		failed bool
	}{
		{name: "分块错误输出", chunks: []string{"扫描二维码\n  https://accounts.feishu.cn/setup?code=a%", "2Bb&lang=zh\n"}, url: "https://accounts.feishu.cn/setup?code=a%2Bb&lang=zh"},
		{name: "JSON 链接", chunks: []string{`{"data":{"verification_uri_complete":"https://accounts.feishu.cn/setup?a=1\u0026b=2"}}`}, url: "https://accounts.feishu.cn/setup?a=1&b=2"},
		{name: "不安全链接", chunks: []string{"http://accounts.feishu.cn/setup\nhttps://user:secret@example.com/setup\n"}},
		{name: "失败信封", chunks: []string{`{"ok":false,"error":{"message":"fixture"}}`}, failed: true},
		{name: "超长输出", chunks: []string{strings.Repeat("a", 128<<10)}, failed: true},
	}
	// 覆盖文本和 JSON 协议，不改写 URL。
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address := ""
			output := &configOutput{onURL: func(value string) { address = value }}
			for _, chunk := range test.chunks {
				if _, err := output.Write([]byte(chunk)); err != nil {
					t.Fatal(err)
				}
			}
			output.flush()
			if address != test.url || output.failed != test.failed {
				t.Fatalf("输出解析错误：url=%q failed=%t", address, output.failed)
			}
		})
	}
}

func TestConfigNameValidation(t *testing.T) {
	// 名称校验同时覆盖中文、长度和命令注入字符。
	for name, valid := range map[string]bool{
		"租户-01": true, "my_tenant.one": true, "": false, "..": false,
		strings.Repeat("租", 65): false, "name\n": false, "../tenant": false,
		"tenant;touch": false, "tenant$(id)": false,
	} {
		if validConfigName(name) != valid {
			t.Fatalf("名称校验错误：%q", name)
		}
	}
}
