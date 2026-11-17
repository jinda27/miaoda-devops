package infrastructure

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func fixtureDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func TestDownloadNetworkFallbackAndCache(t *testing.T) {
	payload := []byte("verified installation archive")
	// 网络不可用、内容损坏或传输中断均应回退，成功缓存不再下载。
	for _, scenario := range []string{"success", "http-error", "bad-checksum", "truncated"} {
		t.Run(scenario, func(t *testing.T) {
			var requests, documentRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				switch scenario {
				case "http-error":
					w.WriteHeader(http.StatusBadGateway)
				case "bad-checksum":
					_, _ = w.Write([]byte("corrupt archive"))
				case "truncated":
					w.Header().Set("Content-Length", "1000")
					_, _ = w.Write(payload)
				default:
					_, _ = w.Write(payload)
				}
			}))
			defer server.Close()
			cache := t.TempDir()
			downloader := NewDownloader(cache)
			downloader.HTTP = server.Client()
			downloader.Document = func(_ context.Context, _ Package, destination string) error {
				if requests.Load() != 1 {
					t.Error("文档下载先于网络下载")
				}
				documentRequests.Add(1)
				return os.WriteFile(destination, payload, 0600)
			}
			pkg := Package{Path: "mac/arm64/node.tar.gz", Component: "node", Source: server.URL,
				SHA256: fixtureDigest(payload)}
			filename, err := downloader.Acquire(context.Background(), pkg, func(string, int) {})
			if err != nil {
				t.Fatal(err)
			}
			if filename != filepath.Join(cache, filepath.FromSlash(pkg.Path)) {
				t.Fatalf("下载没有保存到 packages：%s", filename)
			}
			expected := int32(1)
			if scenario == "success" {
				expected = 0
			}
			if documentRequests.Load() != expected {
				t.Fatalf("回退次数不符：%d", documentRequests.Load())
			}
			// 第二次只使用校验通过的缓存。
			if _, err := downloader.Acquire(context.Background(), pkg, func(string, int) {}); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 1 || documentRequests.Load() != expected {
				t.Fatal("重复下载了有效缓存")
			}
			assertNoPartialDownloads(t, cache)
		})
	}
}

func assertNoPartialDownloads(t *testing.T, root string) {
	t.Helper()
	// 下载成功和失败都不能遗留临时文件或目录。
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".download-") {
			t.Errorf("遗留未完成下载：%s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBadDocumentDownloadIsNotCached(t *testing.T) {
	cache := t.TempDir()
	downloader := &Downloader{Cache: cache, Document: func(_ context.Context, _ Package, destination string) error {
		return os.WriteFile(destination, []byte("bad archive"), 0600)
	}}
	pkg := Package{Path: "node.tar.gz", SHA256: fixtureDigest([]byte("expected archive"))}
	if _, err := downloader.Acquire(context.Background(), pkg, func(string, int) {}); err == nil {
		t.Fatal("文档包没有校验 SHA256")
	}
	if _, err := os.Stat(filepath.Join(cache, pkg.Path)); !os.IsNotExist(err) {
		t.Fatal("损坏的文档包被启用为缓存")
	}
	assertNoPartialDownloads(t, cache)
}

func TestCanceledDownloadDoesNotFallBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	downloader := &Downloader{Cache: t.TempDir(), Document: func(context.Context, Package, string) error {
		t.Fatal("取消后仍发起文档下载")
		return nil
	}}
	if _, err := downloader.Acquire(ctx, Package{Path: "node.zip"}, func(string, int) {}); err != context.Canceled {
		t.Fatalf("未保留取消原因：%v", err)
	}
}

func TestManualDownloadAndEmptyCatalogDirectory(t *testing.T) {
	cache := t.TempDir()
	payload := []byte("manual archive")
	if err := os.WriteFile(filepath.Join(cache, "node.tar.gz"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	downloader := &Downloader{Cache: cache}
	pkg := Package{Path: "mac/arm64/node.tar.gz", SHA256: fixtureDigest(payload)}
	filename, err := downloader.Acquire(context.Background(), pkg, func(string, int) {})
	if err != nil || filename != filepath.Join(cache, "node.tar.gz") {
		t.Fatalf("无法识别手动下载到根目录的安装包：%s %v", filename, err)
	}
	// 空 packages 不影响安装清单读取和架构选择。
	for _, platform := range []struct{ os, arch, expected string }{
		{"darwin", "arm64", "mac/arm64"}, {"darwin", "x86_64", "mac/x64"},
		{"windows", "AMD64", "windows/x64"},
	} {
		tools := NewToolset(t.TempDir(), t.TempDir(), platform.os, platform.arch)
		pkg, err := tools.Package("node")
		if err != nil || pkg.Platform != platform.expected || pkg.Source == "" {
			t.Fatalf("安装包平台选择错误：%+v %v", pkg, err)
		}
		if _, err := tools.Package("go"); err == nil {
			t.Fatal("运行环境不应安装 Go")
		}
	}
	if NewToolset("", "", "windows", "ARM64").Supported() {
		t.Fatal("不应为 Windows ARM64 选择 x64 原生包")
	}
}

func TestAttachmentSelectionRejectsWrongArchitectureAndDuplicates(t *testing.T) {
	content := `<h1>第三章 安装文件</h1><table><source name="node-arm64.tar.gz" token="arm"/><source name="node-x64.tar.gz" token="intel"/></table>`
	token, err := attachmentToken(content, "node-arm64.tar.gz")
	if err != nil || token != "arm" {
		t.Fatalf("选中了错误架构：%s %v", token, err)
	}
	if _, err := attachmentToken(content, "node.zip"); err == nil {
		t.Fatal("使用了不匹配的附件")
	}
	if _, err := attachmentToken(content+`<source name="node-arm64.tar.gz" token="duplicate"/>`, "node-arm64.tar.gz"); err == nil {
		t.Fatal("未拒绝存在歧义的同名附件")
	}
	// 正文其他章节里的同名文件不能作为安装来源。
	otherSections := `<h1>方案介绍</h1><source name="node-arm64.tar.gz" token="wrong-before"/>` +
		content + `<h1>其他资源</h1><source name="node-arm64.tar.gz" token="wrong-after"/>`
	if token, err := attachmentToken(otherSections, "node-arm64.tar.gz"); err != nil || token != "arm" {
		t.Fatalf("没有限定安装文件章节：%s %v", token, err)
	}
}

func TestDocumentCLIFlowFiltersCredentialsAndUsesRelativePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("测试替身使用 Unix Shell")
	}
	work := t.TempDir()
	binary := filepath.Join(work, "lark-cli")
	payload := []byte("document archive")
	envelope, _ := json.Marshal(map[string]any{"ok": true, "data": map[string]any{
		"document": map[string]string{"content": `<h1>第三章 安装文件</h1><source name="node.tar.gz" token="expected-token"/>`},
	}})
	script := "#!/bin/sh\nset -eu\n" + `
# 外部工具的临时令牌不能进入文档下载进程。
if printenv LARKSUITE_CLI_USER_ACCESS_TOKEN >/dev/null 2>&1; then exit 1; fi
operation="$2"
if [ "$operation" = "+fetch" ]; then
  cat <<'JSON'
` + string(envelope) + `
JSON
  exit 0
fi
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output="$2"; shift 2 ;;
    --token) token="$2"; shift 2 ;;
    *) shift ;;
  esac
done
# 文件输出只能使用 cwd 下的相对路径。
test "$token" = expected-token
test "$output" = node.tar.gz
if [ "$operation" = "+media-download" ]; then
  printf '{"ok":false,"error":{"subtype":"permission_denied","code":403,"message":"use preview"}}' >&2
  exit 1
fi
test "$operation" = "+media-preview"
printf 'document archive' > "$output"
printf '{"ok":true}'
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIAODA_LARK_CLI", binary)
	t.Setenv("LARKSUITE_CLI_USER_ACCESS_TOKEN", "external-test-token")
	downloader := NewDownloader(t.TempDir())
	pkg := Package{Path: "mac/arm64/node.tar.gz", SHA256: fixtureDigest(payload)}
	filename, err := downloader.Acquire(context.Background(), pkg, func(string, int) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkDigest(context.Background(), filename, pkg.SHA256); err != nil {
		t.Fatal(err)
	}
}

func TestZstdOfficialPackageExtraction(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	payload := []byte("zip binary")
	if err := writer.WriteHeader(&tar.Header{Name: "usr/bin/zip.exe", Mode: 0755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write(payload)
	_ = writer.Close()
	compressor, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer compressor.Close()
	filename := filepath.Join(t.TempDir(), "zip.pkg.tar.zst")
	if err := os.WriteFile(filename, compressor.EncodeAll(archive.Bytes(), nil), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := extractZstdTar(context.Background(), filename, root); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "usr", "bin", "zip.exe"))
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("官方 Zstandard 包无法解压：%v", err)
	}
}

func TestNetworkOriginalFormatUsesSourceChecksum(t *testing.T) {
	payload := []byte("official source archive")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	downloader := NewDownloader(t.TempDir())
	downloader.HTTP = server.Client()
	downloader.Document = func(context.Context, Package, string) error {
		t.Fatal("原始网络包不应使用文档 ZIP 的校验值")
		return nil
	}
	pkg := Package{Path: "windows/x64/zip.zip", SHA256: fixtureDigest([]byte("document ZIP")),
		Source: server.URL, SourcePath: "windows/x64/zip.pkg.tar.zst", SourceSHA256: fixtureDigest(payload)}
	filename, err := downloader.Acquire(context.Background(), pkg, func(string, int) {})
	if err != nil || !strings.HasSuffix(filename, ".tar.zst") {
		t.Fatalf("没有正确处理官方原始格式：%s %v", filename, err)
	}
}

func TestEmbeddedAssembleInstallsWithEmptyPackages(t *testing.T) {
	// 内置组件不依赖安装清单文件、CLI 登录或网络。
	for _, platform := range []string{"darwin", "windows"} {
		t.Run(platform, func(t *testing.T) {
			tools := NewToolset(t.TempDir(), t.TempDir(), platform, "amd64")
			if err := tools.Install(context.Background(), "assemble", func(string, int) {}); err != nil {
				t.Fatal(err)
			}
			if err := validateAssemble(filepath.Join(tools.Root, "assemble", "scripts"), platform); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRealDocumentFallback(t *testing.T) {
	// 显式启用时使用用户本地登录态验证真实文档，不影响默认离线测试。
	if os.Getenv("MIAODA_TEST_DOCUMENT_DOWNLOAD") != "1" {
		t.Skip("需要显式启用文档下载及本地 CLI 授权")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	tools := NewToolset(t.TempDir(), t.TempDir(), "windows", "amd64")
	pkg, err := tools.Package("zip")
	if err != nil {
		t.Fatal(err)
	}
	pkg.Source = server.URL
	tools.Downloader.HTTP = server.Client()
	fellBack := false
	filename, err := tools.Downloader.Acquire(context.Background(), pkg, func(phase string, _ int) {
		if strings.Contains(phase, "改从飞书文档") {
			fellBack = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fellBack {
		t.Fatal("没有从网络失败回退到真实文档")
	}
	if err := checkDigest(context.Background(), filename, pkg.SHA256); err != nil {
		t.Fatal(err)
	}
	t.Logf("真实文档回退已通过，安装包已校验：%s", filepath.Base(filename))
}
