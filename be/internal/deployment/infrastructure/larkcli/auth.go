package larkcli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Authorization struct {
	ID      string   `json:"id"`
	URL     string   `json:"url,omitempty"`
	QR      string   `json:"qr,omitempty"`
	Status  string   `json:"status"`
	Message string   `json:"message,omitempty"`
	Account *Profile `json:"account,omitempty"`
	profile string
	cancel  context.CancelFunc
	done    chan struct{}
}

type AuthService struct {
	client *Client
	mu     sync.Mutex
	items  map[string]Authorization
}

func NewAuthService(client *Client) *AuthService {
	return &AuthService{client: client, items: map[string]Authorization{}}
}

func (s *AuthService) Begin(ctx context.Context, profileName string) (Authorization, int, error) {
	profile, err := s.client.SelectProfile(ctx, profileName, false)
	if err != nil {
		return Authorization{}, 400, err
	}
	if profile.Error != nil && profile.Error.Subtype == "storage" {
		return Authorization{}, 409, profile.Error
	}
	s.mu.Lock()
	if pending := s.items[profile.Name]; pending.Status == "pending" {
		s.mu.Unlock()
		return pending, 200, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var device struct {
		Code      string `json:"device_code"`
		URL       string `json:"verification_url"`
		Full      string `json:"verification_uri_complete"`
		ExpiresIn int    `json:"expires_in"`
	}
	err = s.client.Run(requestCtx, s.client.WorkDir(), profile.Name, &device,
		"auth", "login", "--domain", "apps", "--no-wait", "--json")
	if err != nil {
		s.mu.Unlock()
		return Authorization{}, 502, err
	}
	address := device.URL
	if device.Full != "" {
		address = device.Full
	}
	if !HTTPSURL(address) || device.Code == "" {
		s.mu.Unlock()
		return Authorization{}, 502, errors.New("飞书 CLI 未返回有效的授权链接")
	}
	// 尊重飞书给出的有效期，同时保留最长后台等待限制。
	lifetime := 12 * time.Minute
	if device.ExpiresIn > 0 && device.ExpiresIn < int(lifetime/time.Second) {
		lifetime = time.Duration(device.ExpiresIn) * time.Second
	}
	taskCtx, taskCancel := context.WithTimeout(context.Background(), lifetime)
	authorization := Authorization{ID: newID(), URL: address, Status: "pending", profile: profile.Name,
		cancel: taskCancel, done: make(chan struct{})}
	authorization.QR = s.client.QRCode(requestCtx, profile.Name, address)
	s.items[profile.Name] = authorization
	s.mu.Unlock()
	go s.complete(taskCtx, authorization, device.Code, !profile.Ready())
	return authorization, 202, nil
}

func (s *AuthService) Refresh(ctx context.Context, id, profile string) (Authorization, int, error) {
	s.mu.Lock()
	authorization := s.items[profile]
	if id == "" || authorization.ID != id {
		s.mu.Unlock()
		return Authorization{}, 404, errors.New("授权请求已失效，请重新登录")
	}
	// 停止旧二维码轮询后才申请新二维码，避免旧任务写回。
	if authorization.cancel != nil {
		authorization.cancel()
	}
	s.mu.Unlock()
	if err := waitForQRTask(ctx, authorization.done); err != nil {
		return Authorization{}, 504, err
	}
	if current, err := s.Status(id, profile); err == nil && current.Status == "done" {
		return current, 200, nil
	}
	return s.Begin(ctx, profile)
}

func (c *Client) QRCode(ctx context.Context, profile, address string) string {
	dir, err := os.MkdirTemp("", "miaoda-qrcode-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	if c.Run(ctx, dir, profile, nil, "auth", "qrcode", address, "--output", "auth.png") != nil {
		return ""
	}
	png, err := os.ReadFile(filepath.Join(dir, "auth.png"))
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

func (s *AuthService) complete(ctx context.Context, authorization Authorization, deviceCode string, observe bool) {
	defer close(authorization.done)
	defer authorization.cancel()
	// 已登录账号重新授权时需等本次 CLI 完成，避免把旧凭据误判为成功。
	account, err := s.client.completeWithProfile(ctx, authorization.profile, true, observe, func(runCtx context.Context) error {
		return s.client.Run(runCtx, s.client.WorkDir(), authorization.profile, nil,
			"auth", "login", "--device-code", deviceCode, "--json")
	})
	authorization.Status = "done"
	if err == nil {
		authorization.Account = &account
	} else {
		authorization.Status = "failed"
		authorization.Message = err.Error()
		if qrExpired(err) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			authorization.Status = "expired"
			authorization.Message = "飞书授权二维码已失效，请刷新二维码"
		}
	}
	authorization.QR, authorization.URL = "", ""
	s.mu.Lock()
	// 仅当前任务可以更新账号的授权状态。
	if s.items[authorization.profile].ID == authorization.ID {
		stored := authorization
		stored.cancel = nil
		s.items[authorization.profile] = stored
	}
	s.mu.Unlock()
}

func (s *AuthService) Status(id, profile string) (Authorization, error) {
	s.mu.Lock()
	authorization := s.items[profile]
	s.mu.Unlock()
	if authorization.ID == "" || authorization.ID != id {
		return Authorization{}, errors.New("授权请求已失效，请重新发起")
	}
	return authorization, nil
}

func HTTPSURL(value string) bool {
	address, err := url.Parse(value)
	return err == nil && address.Scheme == "https" && address.Hostname() != "" && address.User == nil
}

var errQRExpired = errors.New("飞书二维码已失效")

func expiredQRText(message string) bool {
	message = strings.ToLower(message)
	// 兼容设备码、注册流程及 CLI 结构化过期错误。
	for _, marker := range []string{"token_expired", "expired_token", "device code expired", "device_code_expired",
		"registration expired", "registration timed out", "qr code expired", "二维码已过期", "二维码已失效"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func qrExpired(err error) bool {
	var cli *CLIError
	if errors.As(err, &cli) {
		return expiredQRText(cli.Subtype + " " + cli.Message)
	}
	return errors.Is(err, errQRExpired)
}

func waitForQRTask(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("二维码刷新中断，请重试")
	}
}

func newID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(random[:])
}
