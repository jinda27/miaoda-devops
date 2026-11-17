package larkcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Profile struct {
	Name        string    `json:"name"`
	Brand       string    `json:"brand"`
	User        string    `json:"user"`
	Active      bool      `json:"active"`
	TokenStatus string    `json:"tokenStatus"`
	Error       *CLIError `json:"error,omitempty"`
}

func (p Profile) Ready() bool {
	return p.User != "" && (p.TokenStatus == "valid" || p.TokenStatus == "needs_refresh" || p.TokenStatus == "expiring")
}

type CLIError struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e *CLIError) Error() string {
	switch e.Subtype {
	case "storage":
		return "无法读取飞书 CLI 凭据。请在已解锁的系统终端启动平台，并检查该账号的 CLI 登录状态。"
	case "missing_scope", "insufficient_scope":
		return "当前账号缺少妙搭权限，请重新授权并确认租户管理员已开通相应权限。"
	case "not_logged_in", "no_user_token", "token_expired":
		return "飞书账号尚未登录或登录已过期，请重新授权。"
	case "failed_precondition":
		if strings.Contains(e.Message, "forbid ddl/dcl operation in online env") {
			return "online 环境禁止 DDL/DCL 操作，请改用数据操作或通过 dev 发布迁移。"
		}
		return e.Message
	default:
		if e.Message != "" {
			return e.Message
		}
		return "飞书 CLI 请求失败，请检查账号权限、网络和应用状态。"
	}
}

type Client struct {
	workDir string
}

func NewClient(workDir string) *Client {
	return &Client{workDir: workDir}
}

// externalCredential 识别工具注入的飞书凭证及应用身份。
func externalCredential(name string) bool {
	switch strings.ToUpper(name) {
	case "LARKSUITE_CLI_USER_ACCESS_TOKEN", "LARKSUITE_CLI_USER_REFRESH_TOKEN",
		"LARKSUITE_CLI_TENANT_ACCESS_TOKEN", "LARKSUITE_CLI_APP_ID", "LARKSUITE_CLI_APP_SECRET":
		return true
	default:
		return false
	}
}

// ClearInheritedCredentials 启动时清除外部工具凭证。
func ClearInheritedCredentials() {
	// 保留运行环境，仅移除飞书凭证和应用身份。
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if externalCredential(name) {
			_ = os.Unsetenv(name)
		}
	}
}

// Environment 为所有飞书命令和打包脚本过滤外部凭证。
func Environment() []string {
	env := []string{}
	// 子进程独立过滤，避免绕过启动入口继承临时凭证。
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !externalCredential(name) {
			env = append(env, entry)
		}
	}
	return env
}

func (c *Client) WorkDir() string {
	return c.workDir
}

func (c *Client) Binary() (string, error) {
	if path := os.Getenv("MIAODA_LARK_CLI"); path != "" {
		return exec.LookPath(path)
	}
	return exec.LookPath("lark-cli")
}

func (c *Client) Run(ctx context.Context, dir, profile string, dest any, args ...string) (resultErr error) {
	started := time.Now()
	operation := commandOperation(args)
	outputBytes := 0
	defer func() {
		if resultErr != nil {
			log.Printf("[lark-cli] finished operation=%q profile=%q status=failed duration_ms=%d output_bytes=%d error=%q",
				operation, profile, time.Since(started).Milliseconds(), outputBytes, resultErr)
			return
		}
		log.Printf("[lark-cli] finished operation=%q profile=%q status=success duration_ms=%d output_bytes=%d",
			operation, profile, time.Since(started).Milliseconds(), outputBytes)
	}()

	binary, err := c.Binary()
	if err != nil {
		return errors.New("未找到 lark-cli，请安装后刷新")
	}
	if profile != "" {
		args = append(args, "--profile="+profile)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = dir, Environment()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, runErr := cmd.Output()
	outputBytes = len(output) + stderr.Len()
	if ctx.Err() != nil {
		return errors.New("飞书 CLI 请求超时或已中断")
	}
	// 同时解析标准输出和错误输出中的结构化错误。
	for _, raw := range [][]byte{output, stderr.Bytes()} {
		var envelope struct {
			Error *CLIError `json:"error"`
			OK    *bool     `json:"ok"`
			Code  int       `json:"code"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			if envelope.Error != nil {
				return envelope.Error
			}
			if envelope.Code != 0 || (envelope.OK != nil && !*envelope.OK) {
				return errors.New("飞书 CLI 请求失败，请检查账号权限和应用状态")
			}
		}
	}
	if runErr != nil {
		command := ""
		if len(args) > 0 {
			command = args[0]
		}
		return fmt.Errorf("飞书 CLI 执行失败（%s）", command)
	}
	if dest != nil && json.Unmarshal(output, dest) != nil {
		return errors.New("无法解析飞书 CLI 返回，请确认 CLI 版本支持该命令")
	}
	return nil
}

func commandOperation(args []string) string {
	if len(args) == 0 {
		return "unknown"
	}
	limit := 2
	if args[0] == "api" {
		limit = 3
	}
	if limit > len(args) {
		limit = len(args)
	}
	return strings.Join(args[:limit], " ")
}

func (c *Client) Profiles(ctx context.Context) ([]Profile, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	profiles := []Profile{}
	err := c.Run(ctx, c.workDir, "", &profiles, "profile", "list")
	return profiles, err
}

// confirmAuthorization 确认授权完成后 CLI 已保存有效登录。
func (c *Client) confirmAuthorization(ctx context.Context, name string) error {
	profiles, err := c.Profiles(ctx)
	if err != nil {
		return err
	}
	// 仅在 CLI 返回有效用户后视为授权成功。
	for _, profile := range profiles {
		if profile.Name == name && profile.Ready() {
			return nil
		}
	}
	return errors.New("授权完成后未检测到有效账号，请重新授权")
}

func (c *Client) SelectProfile(ctx context.Context, name string, ready bool) (Profile, error) {
	if name == "" || len(name) > 256 {
		return Profile{}, errors.New("请选择飞书租户账号")
	}
	profiles, err := c.Profiles(ctx)
	if err != nil {
		return Profile{}, err
	}
	// 查找指定配置并校验登录状态。
	for _, profile := range profiles {
		if profile.Name != name {
			continue
		}
		if ready && !profile.Ready() {
			if profile.Error != nil {
				return Profile{}, profile.Error
			}
			return Profile{}, errors.New("请先登录选中的租户账号")
		}
		return profile, nil
	}
	return Profile{}, errors.New("找不到选中的飞书 CLI 配置")
}

func (c *Client) ScopedEnvironment(dir, profile string) ([]string, error) {
	binary, err := c.Binary()
	if err != nil {
		return nil, err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	shimDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(shimDir, 0700); err != nil {
		return nil, err
	}
	shim := "#!/bin/sh\nexec \"$PLATFORM_LARK_BINARY\" \"$@\" \"--profile=$PLATFORM_LARK_PROFILE\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "lark-cli"), []byte(shim), 0700); err != nil {
		return nil, err
	}
	env := append(Environment(), "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PLATFORM_LARK_BINARY="+filepath.ToSlash(binary), "PLATFORM_LARK_PROFILE="+profile, "GIT_TERMINAL_PROMPT=0",
		"TMPDIR="+filepath.ToSlash(dir))
	if zipBinary, err := exec.LookPath("zip"); err == nil {
		zipShim := "#!/bin/sh\nexec \"$PLATFORM_ZIP_BINARY\" -y \"$@\"\n"
		if err := os.WriteFile(filepath.Join(shimDir, "zip"), []byte(zipShim), 0700); err != nil {
			return nil, err
		}
		env = append(env, "PLATFORM_ZIP_BINARY="+filepath.ToSlash(zipBinary))
	}
	return env, nil
}
