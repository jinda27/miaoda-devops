package miaoda

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

	"app-deployment-platform/internal/deployment/domain"
	gitinfra "app-deployment-platform/internal/deployment/infrastructure/git"
	"app-deployment-platform/internal/deployment/infrastructure/larkcli"
)

const Branch = "sprint/default"

type Client struct {
	Root string
	Lark *larkcli.Client
	Jobs domain.JobRepository
	Git  *gitinfra.Repository
}

type AppList struct {
	Items []domain.App `json:"items"`
	More  bool         `json:"has_more"`
	Token string       `json:"page_token"`
	Next  string       `json:"next_page_token"`
}

func NewClient(root string, lark *larkcli.Client, jobs domain.JobRepository, git *gitinfra.Repository) *Client {
	return &Client{Root: root, Lark: lark, Jobs: jobs, Git: git}
}

func (result AppList) NextCursor() string {
	if !result.More {
		return ""
	}
	if result.Next != "" {
		return result.Next
	}
	return result.Token
}

func (c *Client) ListApps(ctx context.Context, profile, keyword, cursor, appType string) (AppList, error) {
	args := []string{"apps", "+list", "--as", "user", "--ownership", "all", "--page-size", "20", "--json"}
	if appType != "" {
		args = append(args, "--app-type="+appType)
	}
	if keyword != "" {
		args = append(args, "--keyword="+keyword)
	}
	if cursor != "" {
		args = append(args, "--page-token="+cursor)
	}
	var result struct {
		Data AppList `json:"data"`
	}
	err := c.Lark.Run(ctx, c.Root, profile, &result, args...)
	return result.Data, err
}

func (c *Client) SetStep(job *domain.Job, key, status, detail string) {
	// 更新指定步骤并立即持久化。
	for i := range job.Steps {
		if job.Steps[i].Key != key {
			continue
		}
		now := time.Now()
		job.Steps[i].SetStatus(status, detail, now)
		log.Printf("[step] job_id=%s key=%q status=%s duration_ms=%d detail=%q",
			job.ID, key, status, job.Steps[i].ExecutionDuration(now), clipText(detail, 300))
		if err := c.Jobs.Save(job); err != nil {
			log.Printf("[step] persist_failed job_id=%s key=%q error=%q", job.ID, key, err)
		}
		return
	}
}

// Request 通过 lark-cli 透传调用妙搭开放平台接口。
func (c *Client) Request(
	ctx context.Context,
	profile, work, method, path string,
	params, body any,
	dest any,
) (requestErr error) {
	started := time.Now()
	responseBytes := 0
	defer func() {
		if requestErr != nil {
			log.Printf("[miaoda] request method=%s path=%q status=failed duration_ms=%d response_bytes=%d error=%q",
				method, path, time.Since(started).Milliseconds(), responseBytes, requestErr)
			return
		}
		log.Printf("[miaoda] request method=%s path=%q status=success duration_ms=%d response_bytes=%d",
			method, path, time.Since(started).Milliseconds(), responseBytes)
	}()

	binary, err := c.Lark.Binary()
	if err != nil {
		return errors.New("未找到 lark-cli，请安装后刷新")
	}
	result, err := os.CreateTemp(work, ".spark-*.json")
	if err != nil {
		return errors.New("无法创建妙搭接口响应文件")
	}
	resultPath := result.Name()
	if err := result.Close(); err != nil {
		return errors.New("无法初始化妙搭接口响应文件")
	}
	defer os.Remove(resultPath)

	args := []string{"api", method, path, "--as", "user", "--output", filepath.Base(resultPath)}
	// 请求参数存在时序列化为 JSON。
	if params != nil {
		raw, marshalErr := json.Marshal(params)
		if marshalErr != nil {
			return errors.New("妙搭接口查询参数无效")
		}
		args = append(args, "--params", string(raw))
	}
	var payload []byte
	// 大请求体和 SQL 一样通过 stdin 传递。
	if body != nil {
		raw, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return errors.New("妙搭接口请求体无效")
		}
		payload = raw
		args = append(args, "--data", "-")
	}
	if profile != "" {
		args = append(args, "--profile="+profile)
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	if payload != nil {
		cmd.Stdin = bytes.NewReader(payload)
	}
	cmd.Dir, cmd.Env, cmd.WaitDelay = work, larkcli.Environment(), 5*time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, runErr := cmd.Output()
	if ctx.Err() != nil {
		return errors.New("妙搭接口请求超时或已中断")
	}
	raw := bytes.TrimSpace(stdout)
	// 大响应由 lark-cli 写入文件，优先读取文件内容。
	if file, readErr := os.ReadFile(resultPath); readErr == nil && len(bytes.TrimSpace(file)) > 0 {
		raw = bytes.TrimSpace(file)
	}
	if len(raw) == 0 {
		raw = bytes.TrimSpace(stderr.Bytes())
	}
	responseBytes = len(raw)
	if runErr != nil && len(raw) == 0 {
		return apiCommandError(runErr, stderr.Bytes(), stdout)
	}

	var envelope struct {
		OK    *bool           `json:"ok"`
		Code  json.RawMessage `json:"code"`
		Msg   string          `json:"msg"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
			Subtype string `json:"subtype"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// 进程失败时保留原始错误及其输出。
		if runErr != nil {
			return apiCommandError(runErr, stderr.Bytes(), raw)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = string(raw)
		}
		return errors.New("无法解析妙搭接口返回：" + clipText(message, 600))
	}
	// 解析 lark-cli 包装层错误。
	if envelope.OK != nil && !*envelope.OK {
		message := "妙搭接口返回失败"
		if envelope.Error != nil && envelope.Error.Message != "" {
			message = envelope.Error.Message
		}
		return errors.New(message)
	}
	// 解析开放平台裸响应错误。
	if code := strings.Trim(strings.TrimSpace(string(envelope.Code)), `"`); code != "" && code != "0" && code != "null" {
		message := strings.TrimSpace(envelope.Msg)
		if message == "" {
			message = "妙搭接口返回错误码 " + code
		}
		return errors.New(message)
	}
	if runErr != nil {
		return apiCommandError(runErr, stderr.Bytes(), raw)
	}
	if dest != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, dest); err != nil {
			return fmt.Errorf("无法解析妙搭接口数据：%w", err)
		}
	}
	return nil
}

func apiCommandError(runErr error, stderr, stdout []byte) error {
	// 有输出时保留 CLI 诊断；启动失败时保留操作系统错误。
	detail := bytes.TrimSpace(stderr)
	if len(detail) == 0 {
		detail = bytes.TrimSpace(stdout)
	}
	if len(detail) > 0 {
		return fmt.Errorf("飞书 CLI 执行失败：%s（%w）", clipText(string(detail), 600), runErr)
	}
	return fmt.Errorf("飞书 CLI 执行失败：%w", runErr)
}

// sparkPath 拼接应用级接口路径。
func Path(appID, suffix string) string {
	return filepath.ToSlash("/open-apis/spark/v1/apps/" + appID + suffix)
}
