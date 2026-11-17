package infrastructure

import (
	"context"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"app-deployment-platform/internal/deployment/infrastructure/larkcli"
)

const InstallationDocumentURL = "https://bytedance.larkoffice.com/wiki/Hf8swYthdiavnDkJ8oKcEQAQnCc"
const maxDownloadSize int64 = 512 << 20

//go:embed catalog.json
var catalogData []byte

//go:embed assets/assemble.tar.gz
var assembleData []byte

type Downloader struct {
	Cache       string
	HTTP        *http.Client
	DocumentURL string
	Document    func(context.Context, Package, string) error
}

func embeddedCatalog() ([]Package, error) {
	var catalog struct {
		Packages []Package `json:"packages"`
	}
	err := json.Unmarshal(catalogData, &catalog)
	return catalog.Packages, err
}

func NewDownloader(cache string) *Downloader {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 20 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	d := &Downloader{Cache: cache, DocumentURL: InstallationDocumentURL,
		HTTP: &http.Client{Transport: transport, Timeout: 90 * time.Second}}
	d.Document = d.downloadDocument
	return d
}

func (d *Downloader) Acquire(ctx context.Context, pkg Package, progress func(string, int)) (string, error) {
	candidates := []struct{ path, digest string }{
		{pkg.Path, pkg.SHA256}, {filepath.Base(pkg.Path), pkg.SHA256},
	}
	// 网络原始包与文档附件可能使用不同封装格式。
	if pkg.SourcePath != "" {
		candidates = append(candidates, struct{ path, digest string }{pkg.SourcePath, pkg.SourceSHA256})
	}
	for _, candidate := range candidates {
		filename, err := safePath(d.Cache, candidate.path)
		if err != nil {
			return "", err
		}
		info, err := os.Lstat(filename)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return "", errors.New("安装包缓存不是可读取的普通文件")
		}
		progress("校验已下载的安装包", 5)
		if err := checkDigest(ctx, filename, candidate.digest); err == nil {
			return filename, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// 校验失败的缓存不能继续安装或被重试复用。
		if err := os.Remove(filename); err != nil {
			return "", errors.New("无法清理损坏的安装包缓存")
		}
	}
	if pkg.Component == "assemble" && pkg.SHA256 == "831dd6da108b8714933d2502d45a98b5fe509b23aabdaad523b95f2ac39456d5" {
		progress("准备内置打包组件", 10)
		return d.acquireFile(ctx, pkg.Path, pkg.SHA256, func(destination string) error {
			return os.WriteFile(destination, assembleData, 0600)
		})
	}
	var networkErr error = errors.New("此组件没有网络下载地址")
	// 有有效缓存时已提前返回；其余包优先从网络获取。
	if pkg.Source != "" && d.HTTP != nil {
		progress("从网络下载安装包", 5)
		path, digest := pkg.Path, pkg.SHA256
		if pkg.SourcePath != "" {
			path, digest = pkg.SourcePath, pkg.SourceSHA256
		}
		filename, err := d.acquireFile(ctx, path, digest, func(destination string) error {
			return d.downloadHTTP(ctx, pkg.Source, destination, progress)
		})
		if err == nil {
			log.Printf("[environment] download_completed component=%s source=network", pkg.Component)
			return filename, nil
		}
		networkErr = err
	}
	// 用户取消或安装超时不应继续发起文档下载。
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	progress("网络下载失败，改从飞书文档下载", 5)
	log.Printf("[environment] download_fallback component=%s reason=%q", pkg.Component, networkErr)
	if d.Document == nil {
		return "", fmt.Errorf("网络下载失败：%w；未配置文档下载", networkErr)
	}
	filename, documentErr := d.acquireFile(ctx, pkg.Path, pkg.SHA256, func(destination string) error {
		return d.Document(ctx, pkg, destination)
	})
	if documentErr != nil {
		return "", fmt.Errorf("网络下载失败（%v）；文档下载失败（%v）。可打开下载文档，将 %s 放入 packages 后重试",
			networkErr, documentErr, filepath.Base(pkg.Path))
	}
	log.Printf("[environment] download_completed component=%s source=document", pkg.Component)
	return filename, nil
}

func (d *Downloader) acquireFile(ctx context.Context, name, digest string, fetch func(string) error) (string, error) {
	target, err := safePath(d.Cache, name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return "", errors.New("无法创建项目 packages 下载目录")
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), ".download-")
	if err != nil {
		return "", errors.New("无法创建安装包临时目录")
	}
	defer os.RemoveAll(stage)
	filename := filepath.Join(stage, filepath.Base(target))
	if err := fetch(filename); err != nil {
		return "", err
	}
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxDownloadSize {
		return "", errors.New("下载的安装包不是有效文件或超过大小限制")
	}
	// 网络与文档下载使用相同的完整性校验后再启用缓存。
	if err := checkDigest(ctx, filename, digest); err != nil {
		return "", err
	}
	if err := os.Rename(filename, target); err != nil {
		return "", errors.New("无法保存安装包到项目 packages 目录")
	}
	return target, nil
}

func (d *Downloader) downloadHTTP(ctx context.Context, address, destination string, progress func(string, int)) error {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errors.New("网络下载地址无效")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return errors.New("无法创建安装包下载请求")
	}
	request.Header.Set("User-Agent", "MiaodaDeploymentPlatform")
	response, err := d.HTTP.Do(request)
	if err != nil {
		return errors.New("网络连接失败或下载超时")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("下载服务器返回 HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxDownloadSize {
		return errors.New("网络安装包超过下载大小限制")
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("无法写入安装包缓存")
	}
	writer := &downloadProgress{destination: file, total: response.ContentLength, progress: progress}
	written, copyErr := io.Copy(writer, io.LimitReader(response.Body, maxDownloadSize+1))
	closeErr := file.Close()
	if copyErr != nil {
		return errors.New("网络下载中断")
	}
	if written > maxDownloadSize {
		return errors.New("网络安装包超过下载大小限制")
	}
	if response.ContentLength >= 0 && written != response.ContentLength {
		return errors.New("网络安装包下载不完整")
	}
	if closeErr != nil {
		return errors.New("安装包缓存写入失败")
	}
	return nil
}

type downloadProgress struct {
	destination io.Writer
	progress    func(string, int)
	total       int64
	written     int64
	last        time.Time
}

func (w *downloadProgress) Write(data []byte) (int, error) {
	count, err := w.destination.Write(data)
	w.written += int64(count)
	// 限制进度更新频率，避免大文件下载频繁刷新页面。
	if time.Since(w.last) >= 250*time.Millisecond {
		percent := 5
		if w.total > 0 {
			percent += int(w.written * 25 / w.total)
		}
		w.progress(fmt.Sprintf("网络下载 %.1f MB", float64(w.written)/(1<<20)), percent)
		w.last = time.Now()
	}
	return count, err
}

func (d *Downloader) downloadDocument(ctx context.Context, pkg Package, destination string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client := larkcli.NewClient(filepath.Dir(destination))
	if _, err := client.Binary(); err != nil {
		return errors.New("尚未安装 lark-cli，无法自动读取受权限保护的飞书附件")
	}
	address := d.DocumentURL
	if address == "" {
		address = InstallationDocumentURL
	}
	profile := os.Getenv("MIAODA_DOWNLOAD_PROFILE")
	var response struct {
		OK   bool `json:"ok"`
		Data struct {
			Document struct {
				Content string `json:"content"`
			} `json:"document"`
		} `json:"data"`
	}
	// 使用用户本人本地凭据读取最新附件，不继承工具临时令牌。
	err := client.Run(ctx, filepath.Dir(destination), profile, &response,
		"docs", "+fetch", "--doc", address, "--detail", "full", "--as", "user")
	if err != nil {
		return fmt.Errorf("无法读取文档，请确认当前飞书账号具有文档权限及 docs/drive 授权：%w", err)
	}
	if !response.OK {
		return errors.New("文档读取未成功")
	}
	token, err := attachmentToken(response.Data.Document.Content, filepath.Base(pkg.Path))
	if err != nil {
		return err
	}
	relative := filepath.Base(destination)
	err = client.Run(ctx, filepath.Dir(destination), profile, nil,
		"docs", "+media-download", "--token", token, "--output", relative, "--as", "user")
	var cliErr *larkcli.CLIError
	// 飞书对文档素材的下载与预览采用不同权限入口。
	if errors.As(err, &cliErr) && (cliErr.Subtype == "permission_denied" || cliErr.Code == 403) {
		_ = os.Remove(destination)
		err = client.Run(ctx, filepath.Dir(destination), profile, nil,
			"docs", "+media-preview", "--token", token, "--output", relative, "--as", "user")
	}
	return err
}

func attachmentToken(content, filename string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader("<root>" + content + "</root>"))
	found := ""
	inInstallationSection := false
	// 只匹配清单指定的完整文件名，避免下载其他架构或同名前缀。
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("无法解析文档附件列表")
		}
		element, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		// 附件必须属于“安装文件”章节，忽略正文其他位置的文件。
		if element.Name.Local == "h1" {
			var heading struct {
				Content string `xml:",innerxml"`
			}
			if err := decoder.DecodeElement(&heading, &element); err != nil {
				return "", errors.New("无法解析文档章节")
			}
			inInstallationSection = strings.Contains(strings.Join(strings.Fields(heading.Content), ""), "安装文件")
			continue
		}
		if !inInstallationSection || element.Name.Local != "source" {
			continue
		}
		var name, resource string
		for _, attribute := range element.Attr {
			switch attribute.Name.Local {
			case "name":
				name = attribute.Value
			case "token":
				resource = attribute.Value
			}
		}
		if name != filename || resource == "" {
			continue
		}
		if found != "" && found != resource {
			return "", errors.New("文档存在多个同名安装包，请保留唯一版本")
		}
		found = resource
	}
	if found == "" {
		return "", fmt.Errorf("文档中未找到 %s", filename)
	}
	return found, nil
}
