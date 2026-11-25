package larkcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type TenantConfiguration struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Initial bool     `json:"initial,omitempty"`
	URL     string   `json:"url,omitempty"`
	QR      string   `json:"qr,omitempty"`
	Status  string   `json:"status"`
	Message string   `json:"message,omitempty"`
	Account *Profile `json:"account,omitempty"`
}

type ConfigService struct {
	client  *Client
	mu      sync.Mutex
	current TenantConfiguration
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewConfigService(client *Client) *ConfigService {
	return &ConfigService{client: client}
}

func (s *ConfigService) Begin(ctx context.Context, name string) (TenantConfiguration, int, error) {
	return s.begin(ctx, name, false, false)
}

func (s *ConfigService) Initialize(ctx context.Context) (TenantConfiguration, int, error) {
	return s.begin(ctx, "feishu-default", true, false)
}

func (s *ConfigService) begin(ctx context.Context, name string, initial, refreshing bool) (TenantConfiguration, int, error) {
	name = strings.TrimSpace(name)
	if !validConfigName(name) {
		return TenantConfiguration{}, 400, errors.New("配置名称须为 1 至 64 个字符，仅支持中文、字母、数字、点、下划线和短横线")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 同名请求复用任务，其余配置串行创建，避免覆盖 CLI 配置文件。
	if s.current.Status == "pending" {
		if initial || s.current.Name == name {
			return s.current, 200, nil
		}
		return TenantConfiguration{}, 409, errors.New("已有租户正在配置，请先完成或取消")
	}
	profiles, err := s.client.Profiles(ctx)
	if err != nil {
		return TenantConfiguration{}, 502, err
	}
	// 首次初始化只在确定没有已有配置时启动。
	if initial && len(profiles) > 0 {
		return TenantConfiguration{Status: "existing"}, 200, nil
	}
	// 禁止用初始化命令覆盖已有租户。
	for _, profile := range profiles {
		if profile.Name == name {
			if refreshing {
				return TenantConfiguration{Status: "existing", Name: name}, 200, nil
			}
			return TenantConfiguration{}, 409, errors.New("配置名称已存在，请使用其他名称")
		}
	}
	taskCtx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	s.current = TenantConfiguration{ID: newID(), Name: name, Initial: initial, Status: "pending"}
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(taskCtx, cancel, s.current, s.done)
	return s.current, 202, nil
}

func validConfigName(name string) bool {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) == 0 || utf8.RuneCountInString(name) > 64 {
		return false
	}
	// 限定可读名称，避免控制字符和路径字符进入配置。
	for _, char := range name {
		if !unicode.IsLetter(char) && !unicode.IsDigit(char) && char != '.' && char != '_' && char != '-' {
			return false
		}
	}
	return name != "." && name != ".."
}

func (s *ConfigService) Current() TenantConfiguration {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 页面重载时恢复尚未完成的配置。
	if s.current.Status == "pending" || s.current.Status == "expired" {
		return s.current
	}
	return TenantConfiguration{}
}

func (s *ConfigService) Refresh(ctx context.Context, id string) (TenantConfiguration, int, error) {
	s.mu.Lock()
	task, done := s.current, s.done
	if id == "" || task.ID != id {
		s.mu.Unlock()
		return TenantConfiguration{}, 404, errors.New("租户配置请求已失效，请重新添加")
	}
	// 旧 CLI 进程退出后再生成新配置链接。
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	if err := waitForQRTask(ctx, done); err != nil {
		return TenantConfiguration{}, 504, err
	}
	return s.begin(ctx, task.Name, task.Initial, true)
}

func (s *ConfigService) Status(id string) (TenantConfiguration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || s.current.ID != id {
		return TenantConfiguration{}, errors.New("租户配置请求已失效，请重新添加")
	}
	return s.current, nil
}

func (s *ConfigService) Cancel(id string) (TenantConfiguration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || s.current.ID != id {
		return TenantConfiguration{}, errors.New("租户配置请求已失效")
	}
	// 等待进程退出后才允许创建下一份配置。
	if s.current.Status == "pending" && s.cancel != nil {
		s.cancel()
	}
	if s.current.Status == "expired" {
		s.current.Status = "cancelled"
		s.current.Message = "已取消租户配置"
	}
	return s.current, nil
}

func (s *ConfigService) run(ctx context.Context, cancel context.CancelFunc, task TenantConfiguration, done chan struct{}) {
	defer close(done)
	defer cancel()
	started := time.Now()
	// 确认目标配置已存在即可完成，登录授权仍为独立步骤。
	account, err := s.client.completeWithProfile(ctx, task.Name, false, true, func(runCtx context.Context) error {
		return s.create(runCtx, task)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current.Status = "done"
	s.current.URL, s.current.QR = "", ""
	s.cancel = nil
	// 只返回固定提示，不透传可能包含凭据的原始命令输出。
	switch {
	case err == nil:
		s.current.Account = &account
	case errors.Is(ctx.Err(), context.Canceled):
		s.current.Status = "cancelled"
		s.current.Message = "已取消租户配置"
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || qrExpired(err):
		s.current.Status = "expired"
		s.current.Message = "租户配置二维码已失效，请刷新二维码"
	default:
		s.current.Status = "failed"
		s.current.Message = "租户配置失败，请检查网络、飞书权限及 lark-cli 版本后重试"
	}
	log.Printf("[lark-cli] finished operation=%q profile=%q status=%s duration_ms=%d",
		"config init", task.Name, s.current.Status, time.Since(started).Milliseconds())
}

func (s *ConfigService) create(ctx context.Context, task TenantConfiguration) error {
	binary, err := s.client.Binary()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, binary, "config", "init", "--new", "--name="+task.Name)
	cmd.Dir, cmd.Env = s.client.WorkDir(), Environment()
	cmd.WaitDelay = 2 * time.Second
	// 配置链接从 stdout 或 stderr 实时读取，原始输出不落盘。
	stream := &configOutput{onURL: func(address string) {
		s.mu.Lock()
		s.current.URL = address
		s.mu.Unlock()
		go func() {
			qrCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			qr := s.client.QRCode(qrCtx, "", address)
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.current.ID == task.ID && s.current.Status == "pending" {
				s.current.QR = qr
			}
		}()
	}}
	cmd.Stdout, cmd.Stderr = stream, stream
	err = cmd.Run()
	stream.flush()
	if stream.expired {
		return errQRExpired
	}
	if err != nil {
		return err
	}
	if stream.failed {
		return errors.New("CLI 返回配置失败")
	}
	return nil
}

var configURLField = regexp.MustCompile(`"(?:verification_uri_complete|verification_url|console_url)"\s*:\s*("(?:[^"\\]|\\.)*")`)

type configOutput struct {
	pending []byte
	onURL   func(string)
	found   bool
	failed  bool
	expired bool
}

func (o *configOutput) Write(data []byte) (int, error) {
	size := len(data)
	// 按行解析输出，限制内存占用并丢弃超长行。
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if len(o.pending)+len(part) > 64<<10 {
			o.failed = true
			o.pending = nil
		} else {
			o.pending = append(o.pending, part...)
		}
		if end < 0 {
			break
		}
		o.flush()
		data = data[end+1:]
	}
	return size, nil
}

func (o *configOutput) flush() {
	line := strings.TrimSpace(string(o.pending))
	o.pending = nil
	// CLI 的错误信封可能跨行，单独识别其中的过期标记。
	o.expired = o.expired || expiredQRText(line)
	var envelope struct {
		OK    *bool           `json:"ok"`
		Error json.RawMessage `json:"error"`
	}
	// 即使进程退出码为零，也不能忽略结构化失败。
	if json.Unmarshal([]byte(line), &envelope) == nil &&
		((envelope.OK != nil && !*envelope.OK) || (len(envelope.Error) > 0 && string(envelope.Error) != "null")) {
		o.failed = true
	}
	if o.found {
		return
	}
	address := line
	// 同时兼容纯文本链接和 JSON 链接字段。
	if match := configURLField.FindStringSubmatch(line); len(match) > 1 {
		_ = json.Unmarshal([]byte(match[1]), &address)
	}
	if !HTTPSURL(address) {
		return
	}
	o.found = true
	o.onURL(address)
}
