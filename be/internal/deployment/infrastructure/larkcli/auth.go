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
	"sync"
	"time"
)

type Authorization struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	QR      string `json:"qr,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	profile string
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
		Code string `json:"device_code"`
		URL  string `json:"verification_url"`
		Full string `json:"verification_uri_complete"`
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
	authorization := Authorization{ID: newID(), URL: address, Status: "pending", profile: profile.Name}
	if dir, err := os.MkdirTemp(s.client.WorkDir(), ".login-"); err == nil {
		defer os.RemoveAll(dir)
		if s.client.Run(requestCtx, dir, profile.Name, nil, "auth", "qrcode", address, "--output", "auth.png") == nil {
			if png, err := os.ReadFile(filepath.Join(dir, "auth.png")); err == nil {
				authorization.QR = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
			}
		}
	}
	s.items[profile.Name] = authorization
	s.mu.Unlock()
	go s.complete(authorization, device.Code)
	return authorization, 202, nil
}

func (s *AuthService) complete(authorization Authorization, deviceCode string) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	err := s.client.Run(ctx, s.client.WorkDir(), authorization.profile, nil,
		"auth", "login", "--device-code", deviceCode, "--json")
	// 授权成功后校验账号，并记录本次启动的登录状态。
	if err == nil {
		err = s.client.confirmAuthorization(ctx, authorization.profile)
	}
	authorization.Status = "done"
	if err != nil {
		authorization.Status = "failed"
		authorization.Message = err.Error()
	}
	authorization.QR, authorization.URL = "", ""
	s.mu.Lock()
	s.items[authorization.profile] = authorization
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

func newID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(random[:])
}
