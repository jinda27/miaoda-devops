package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"app-deployment-platform/internal/environment/domain"
)

var versionPattern = regexp.MustCompile(`(?:^|[^\d])(\d+)\.(\d+)(?:\.(\d+))?`)

type Toolset struct {
	Root        string
	Packages    string
	Platform    string
	Arch        string
	Catalog     []Package
	CatalogErr  error
	DocumentURL string
	Downloader  *Downloader
}

type Package struct {
	Path         string `json:"path"`
	Component    string `json:"component"`
	Version      string `json:"version"`
	SHA256       string `json:"sha256"`
	Platform     string `json:"platform"`
	Optional     bool   `json:"optional"`
	Source       string `json:"source"`
	SourcePath   string `json:"source_path"`
	SourceSHA256 string `json:"source_sha256"`
}

type definition struct {
	ID          string
	Name        string
	Requirement string
	Component   string
}

var definitions = []definition{
	{"node", "Node.js", "可正常运行", "node"},
	{"npm", "npm", "可正常运行，随 Node.js 安装", "node"},
	{"lark-cli", "lark-cli", "可正常运行", "lark-cli"},
	{"git", "Git", "可正常运行", "git"},
	{"bash", "Bash", "可正常运行，包含打包所需命令", "git"},
	{"zip", "zip", "可正常运行", "zip"},
	{"assemble", "打包组件", "文件完整且可用", "assemble"},
}

func Configure(project, packages string) (*Toolset, error) {
	root := os.Getenv("MIAODA_TOOLS_DIR")
	// 默认安装到当前用户的专用目录，无需管理员权限。
	if root == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		if runtime.GOOS == "windows" && os.Getenv("LOCALAPPDATA") != "" {
			base = os.Getenv("LOCALAPPDATA")
		}
		root = filepath.Join(base, "MiaodaDeploymentPlatform", "runtime")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if packages == "" {
		packages = filepath.Join(project, "packages")
	}
	packages, err = filepath.Abs(packages)
	if err != nil {
		return nil, err
	}
	tools := NewToolset(root, packages, runtime.GOOS, nativeArchitecture())
	paths := tools.SearchPaths()
	// 搜索路径在启动时固定，安装完成后即可使用新文件。
	paths = append(paths, filepath.SplitList(os.Getenv("PATH"))...)
	if err := os.Setenv("PATH", strings.Join(paths, string(os.PathListSeparator))); err != nil {
		return nil, err
	}
	if err := os.Setenv("MIAODA_TOOLS_DIR", root); err != nil {
		return nil, err
	}
	return tools, nil
}

func NewToolset(root, packages, platform, arch string) *Toolset {
	tools := &Toolset{Root: root, Packages: packages, Platform: platform, Arch: normalizeArchitecture(arch),
		DocumentURL: InstallationDocumentURL}
	tools.Catalog, tools.CatalogErr = embeddedCatalog()
	// 旧离线分发包仍可提供自定义清单。
	if _, err := os.Stat(filepath.Join(packages, "manifest.json")); err == nil {
		tools.Catalog, tools.CatalogErr = readCatalog(filepath.Join(packages, "manifest.json"))
	}
	tools.Downloader = NewDownloader(packages)
	return tools
}

func (t *Toolset) Supported() bool {
	return t.Platform == "darwin" && (t.Arch == "arm64" || t.Arch == "amd64") ||
		t.Platform == "windows" && t.Arch == "amd64"
}

func (t *Toolset) SearchPaths() []string {
	paths := []string{filepath.Join(t.Root, "cli")}
	if t.Platform == "windows" {
		return append(paths, filepath.Join(t.Root, "node"), filepath.Join(t.Root, "git", "cmd"),
			filepath.Join(t.Root, "git", "bin"), filepath.Join(t.Root, "zip", "usr", "bin"),
			filepath.Join(t.Root, "git", "usr", "bin"))
	}
	return append(paths, filepath.Join(t.Root, "node", "bin"), filepath.Join(t.Root, "git", "bin"),
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")
}

func (t *Toolset) packagePlatform() string {
	platform, arch := t.Platform, t.Arch
	if platform == "darwin" {
		platform = "mac"
	}
	if arch == "amd64" {
		arch = "x64"
	}
	return platform + "/" + arch
}

func (t *Toolset) Package(component string) (Package, error) {
	if component == "git" && t.Platform == "windows" {
		component = "git-bash"
	}
	// 安装范围由固定组件和当前平台限定。
	for _, item := range t.Catalog {
		if item.Component == component && !item.Optional &&
			(item.Platform == t.packagePlatform() || item.Platform == "shared") {
			if _, err := safePath(t.Packages, item.Path); err != nil {
				return Package{}, err
			}
			return item, nil
		}
	}
	if t.CatalogErr != nil {
		return Package{}, fmt.Errorf("无法读取安装包清单：%w", t.CatalogErr)
	}
	return Package{}, errors.New("当前平台没有此组件的安装包")
}

func (t *Toolset) Inspect(ctx context.Context) domain.Report {
	report := domain.Report{Platform: t.Platform, Architecture: t.Arch, Supported: t.Supported(),
		Root: t.Root, Packages: t.Packages, DocumentURL: t.DocumentURL,
		Ready: true, Items: make([]domain.Item, len(definitions))}
	var group sync.WaitGroup
	// 各依赖并行检测，避免页面等待串行命令执行。
	for index, def := range definitions {
		group.Add(1)
		go func(index int, def definition) {
			defer group.Done()
			item := t.inspect(ctx, def)
			_, err := t.Package(def.Component)
			item.PackageAvailable = t.Supported() && err == nil
			// Mac 的 Bash 和 zip 由系统提供。
			if t.Platform == "darwin" && (def.ID == "bash" || def.ID == "zip") {
				item.PackageAvailable = false
			}
			if item.Status != domain.Ready && !item.PackageAvailable {
				if item.Message == "" && err != nil {
					item.Message = err.Error()
				}
			}
			report.Items[index] = item
		}(index, def)
	}
	group.Wait()
	for _, item := range report.Items {
		if item.Status != domain.Ready {
			report.Ready = false
		}
	}
	return report
}

func (t *Toolset) inspect(ctx context.Context, def definition) domain.Item {
	item := domain.Item{ID: def.ID, Name: def.Name, Requirement: def.Requirement, Status: domain.Missing}
	if def.ID == "assemble" {
		return t.inspectAssemble(item)
	}
	binary, args, err := t.command(def.ID)
	if err != nil {
		item.Message = "未找到可用的 " + def.Name
		return item
	}
	item.Path = binary
	output, err := runVersion(ctx, binary, args...)
	if err != nil {
		item.Status, item.Message = domain.VersionMismatch, "命令无法正常运行，请重新安装"
		return item
	}
	item.Version = parseVersion(output)
	// Bash 还需具备导出脚本使用的基础工具。
	if def.ID == "bash" {
		output, err := runVersion(ctx, binary, "-c", "for tool in perl xargs find sort sed grep tr mktemp env; do command -v \"$tool\" >/dev/null || { echo \"$tool\"; exit 1; }; done")
		if err != nil {
			item.Status, item.Message = domain.VersionMismatch, "Bash 缺少打包所需命令："+strings.TrimSpace(output)
			return item
		}
	}
	item.Status = domain.Ready
	return item
}

func (t *Toolset) command(id string) (string, []string, error) {
	name, args := id, []string{"--version"}
	if id == "zip" {
		args = []string{"-v"}
	}
	if id == "lark-cli" && os.Getenv("MIAODA_LARK_CLI") != "" {
		name = os.Getenv("MIAODA_LARK_CLI")
	}
	if id == "npm" {
		node, err := exec.LookPath("node")
		if err != nil {
			return "", nil, err
		}
		resolved, _ := filepath.EvalSymlinks(node)
		candidates := []string{
			filepath.Join(filepath.Dir(node), "node_modules", "npm", "bin", "npm-cli.js"),
			filepath.Join(filepath.Dir(resolved), "..", "lib", "node_modules", "npm", "bin", "npm-cli.js"),
		}
		// Unix npm 通常是指向 npm-cli.js 的符号链接。
		if npm, err := exec.LookPath("npm"); err == nil {
			if target, err := filepath.EvalSymlinks(npm); err == nil && strings.HasSuffix(target, ".js") {
				candidates = append(candidates, target)
			}
		}
		for _, candidate := range candidates {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return node, []string{candidate, "--version"}, nil
			}
		}
		return "", nil, errors.New("未找到 npm")
	}
	binary, err := exec.LookPath(name)
	return binary, args, err
}

func (t *Toolset) inspectAssemble(item domain.Item) domain.Item {
	managed := filepath.Join(t.Root, "assemble", "scripts", "export-miaoda-app.sh")
	candidates := []string{managed}
	// 保留用户显式指定的打包入口。
	if custom := os.Getenv("MIAODA_ASSEMBLE_SCRIPT"); custom != "" {
		candidates = []string{custom}
	} else if home, err := os.UserHomeDir(); err == nil {
		for _, agent := range []string{".agents", ".codex"} {
			candidates = append(candidates, filepath.Join(home, agent, "skills", "miaoda-app-assemble", "scripts", "export-miaoda-app.sh"))
		}
	}
	for _, entry := range candidates {
		if _, err := os.Stat(entry); err != nil {
			continue
		}
		item.Path = entry
		if err := validateAssemble(filepath.Dir(entry), t.Platform); err != nil {
			item.Status, item.Message = domain.VersionMismatch, err.Error()
			return item
		}
		item.Status, item.Version = domain.Ready, "2026.11.16"
		// 平台管理的旧 Windows 组件可在初始环境一键升级。
		if pkg, err := t.Package("assemble"); entry == managed && err == nil && strings.EqualFold(pkg.SHA256, assembleDigest) {
			script, err := os.ReadFile(filepath.Join(filepath.Dir(entry), "generate-schema-ddl.sh"))
			if err == nil && strings.Contains(string(script), "# MIAODA_DDL_STDIN=1") {
				item.Version = pkg.Version
			} else if t.Platform == "windows" {
				item.Status, item.Message = domain.VersionMismatch, "请一键安装新版打包组件，避免 Windows 命令行超长"
			}
		}
		return item
	}
	item.Message = "安装平台附带的打包组件"
	return item
}

func validateAssemble(dir, platform string) error {
	for _, name := range []string{"export-miaoda-app.sh", "generate-schema-ddl.sh", "generate-app-metadata.sh", "schema-structure.mjs", "bin/lark-cli"} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return errors.New("打包组件文件不完整")
		}
	}
	wrapper, err := os.ReadFile(filepath.Join(dir, "bin", "lark-cli"))
	if err != nil || !strings.Contains(string(wrapper), "MIAODA_WRAPPER_OK") {
		return errors.New("打包组件版本不兼容")
	}
	if platform == "windows" && !strings.Contains(string(wrapper), "MIAODA_WINDOWS_SUPPORT=1") {
		return errors.New("打包组件需要安装 Windows 兼容版本")
	}
	return nil
}

func runVersion(ctx context.Context, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	hideWindow(cmd)
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func parseVersion(output string) string {
	match := versionPattern.FindStringSubmatch(output)
	if len(match) == 0 {
		return ""
	}
	version := match[1] + "." + match[2]
	if match[3] != "" {
		version += "." + match[3]
	}
	return version
}
