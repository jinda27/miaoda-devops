package infrastructure

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const maxExtractedSize = int64(2 << 30)

func readCatalog(filename string) ([]Package, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Packages []Package `json:"packages"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return manifest.Packages, nil
}

func safePath(root, name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.Contains(name, ":") || strings.HasPrefix(name, "/") {
		return "", errors.New("安装包包含不安全的文件路径")
	}
	target := filepath.Join(root, filepath.FromSlash(name))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("安装包文件超出目标目录")
	}
	return target, nil
}

func checkDigest(ctx context.Context, filename, expected string) error {
	want, err := hex.DecodeString(expected)
	if err != nil || len(want) != sha256.Size {
		return errors.New("安装包清单缺少有效 SHA256")
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 1024*1024)
	// 分块校验同时响应取消信号。
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := file.Read(buffer)
		if count > 0 {
			hash.Write(buffer[:count])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(expected) {
		return errors.New("安装包 SHA256 校验失败，请重新复制此安装包")
	}
	return nil
}

func (t *Toolset) Install(ctx context.Context, component string, progress func(string, int)) error {
	pkg, err := t.Package(component)
	if err != nil {
		return err
	}
	downloader := t.Downloader
	if downloader == nil {
		downloader = &Downloader{Cache: t.Packages}
	}
	filename, err := downloader.Acquire(ctx, pkg, progress)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.Root, 0700); err != nil {
		return fmt.Errorf("无法创建安装目录：%w", err)
	}
	stage, err := os.MkdirTemp(t.Root, ".install-"+component+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	progress("解压安装包", 35)
	switch {
	case strings.HasSuffix(filename, ".tar.zst"):
		// Windows zip 的官方 MSYS2 原始包使用 Zstandard 压缩。
		err = extractZstdTar(ctx, filename, stage)
	case strings.HasSuffix(filename, ".tar.gz"):
		err = extractTar(ctx, filename, stage, component == "node")
	case strings.HasSuffix(filename, ".zip"):
		err = extractZip(ctx, filename, stage, component == "node")
	case strings.HasSuffix(filename, ".7z.exe") && t.Platform == "windows":
		// 官方便携 Git 自解压程序可在普通用户目录完成安装。
		command := exec.CommandContext(ctx, filename, "-y", "-o"+stage)
		hideWindow(command)
		command.WaitDelay = 2 * time.Second
		output, runErr := command.CombinedOutput()
		if runErr != nil {
			err = fmt.Errorf("便携 Git 解压失败：%v %s", runErr, tail(string(output), 600))
		}
	default:
		err = errors.New("不支持此安装包格式")
	}
	if err != nil {
		return fmt.Errorf("解压失败：%w", err)
	}
	progress("验证组件", 75)
	if err := t.verify(ctx, component, stage); err != nil {
		return err
	}
	progress("启用组件", 90)
	target := filepath.Join(t.Root, installDirectory(component))
	backup := stage + ".previous"
	// 先保留旧版本，任何替换或复检失败均回滚。
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("旧版本正在使用或无法替换：%w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	restore := func() {
		os.RemoveAll(target)
		os.Rename(backup, target)
	}
	if err := os.Rename(stage, target); err != nil {
		os.Rename(backup, target)
		return err
	}
	if err := t.verify(ctx, component, target); err != nil {
		restore()
		return err
	}
	os.RemoveAll(backup)
	progress("已完成", 100)
	return nil
}

func installDirectory(component string) string {
	if component == "lark-cli" {
		return "cli"
	}
	return component
}

func (t *Toolset) verify(ctx context.Context, component, root string) error {
	var binary string
	args := []string{"--version"}
	switch component {
	case "node":
		binary = filepath.Join(root, "bin", "node")
		if t.Platform == "windows" {
			binary = filepath.Join(root, "node.exe")
		}
	case "lark-cli":
		binary = filepath.Join(root, "lark-cli")
		if t.Platform == "windows" {
			binary += ".exe"
		}
	case "git":
		binary = filepath.Join(root, "bin", "git")
		if t.Platform == "windows" {
			binary = filepath.Join(root, "cmd", "git.exe")
		}
	case "zip":
		binary, args = filepath.Join(root, "usr", "bin", "zip.exe"), []string{"-v"}
	case "assemble":
		return validateAssemble(filepath.Join(root, "scripts"), t.Platform)
	default:
		return errors.New("不支持此安装组件")
	}
	output, err := runVersion(ctx, binary, args...)
	if err != nil {
		return fmt.Errorf("%s 安装后无法运行：%s", component, tail(output, 300))
	}
	// Node 安装包必须同时包含可运行的 npm。
	if component == "node" {
		script := filepath.Join(root, "lib", "node_modules", "npm", "bin", "npm-cli.js")
		if t.Platform == "windows" {
			script = filepath.Join(root, "node_modules", "npm", "bin", "npm-cli.js")
		}
		output, err = runVersion(ctx, binary, script, "--version")
		if err != nil {
			return errors.New("安装包内的 npm 无法正常运行")
		}
	}
	if component == "git" && t.Platform == "windows" {
		output, err = runVersion(ctx, filepath.Join(root, "bin", "bash.exe"), "--version")
		if err != nil {
			return errors.New("便携 Git 中的 Bash 无法正常运行")
		}
	}
	return nil
}

func archiveName(name string, stripRoot bool) string {
	name = strings.TrimPrefix(name, "./")
	if stripRoot {
		_, name, _ = strings.Cut(name, "/")
	}
	return name
}

type archiveLink struct {
	path   string
	target string
	hard   bool
}

func extractTar(ctx context.Context, filename, root string, stripRoot bool) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	return extractTarReader(ctx, compressed, root, stripRoot)
}

func extractZstdTar(ctx context.Context, filename, root string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := zstd.NewReader(file, zstd.WithDecoderMaxMemory(64<<20))
	if err != nil {
		return err
	}
	defer compressed.Close()
	return extractTarReader(ctx, compressed, root, false)
}

func extractTarReader(ctx context.Context, source io.Reader, root string, stripRoot bool) error {
	reader := tar.NewReader(source)
	var total int64
	var links []archiveLink
	// 链接延后创建，避免后续文件经链接写到目录外。
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if entry.Typeflag == tar.TypeXGlobalHeader || entry.Typeflag == tar.TypeXHeader {
			continue
		}
		name := archiveName(entry.Name, stripRoot)
		if name == "" || name == "." {
			continue
		}
		target, err := safePath(root, name)
		if err != nil {
			return err
		}
		switch entry.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			total += entry.Size
			if entry.Size < 0 || total > maxExtractedSize {
				return errors.New("安装包解压大小超出限制")
			}
			if err := writeExtracted(target, os.FileMode(entry.Mode), reader, entry.Size); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			linkTarget := entry.Linkname
			if entry.Typeflag == tar.TypeLink {
				linkTarget = archiveName(linkTarget, stripRoot)
			}
			links = append(links, archiveLink{target, linkTarget, entry.Typeflag == tar.TypeLink})
		default:
			return errors.New("安装包包含不支持的文件类型")
		}
	}
	// 所有链接必须指向解压目录内部的普通文件。
	for _, link := range links {
		parent := filepath.Dir(link.path)
		if link.hard {
			parent = root
		}
		destination, err := safePath(parent, link.target)
		if err != nil {
			// 相对上级链接可接受，但最终目标仍需落在根目录内。
			destination = filepath.Clean(filepath.Join(parent, filepath.FromSlash(link.target)))
			relative, relErr := filepath.Rel(root, destination)
			if filepath.IsAbs(link.target) || relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
				return errors.New("安装包包含越界链接")
			}
		}
		info, err := os.Lstat(destination)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("安装包链接目标不是普通文件")
		}
		if err := os.MkdirAll(filepath.Dir(link.path), 0755); err != nil {
			return err
		}
		if link.hard {
			err = os.Link(destination, link.path)
		} else {
			err = os.Symlink(link.target, link.path)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func extractZip(ctx context.Context, filename, root string, stripRoot bool) error {
	reader, err := zip.OpenReader(filename)
	if err != nil {
		return err
	}
	defer reader.Close()
	var total uint64
	// 逐项限制大小并拒绝 ZIP 中的符号链接。
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := archiveName(entry.Name, stripRoot)
		if name == "" {
			continue
		}
		target, err := safePath(root, name)
		if err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if !entry.Mode().IsRegular() {
			return errors.New("安装包包含非普通文件")
		}
		total += entry.UncompressedSize64
		if total > uint64(maxExtractedSize) || entry.UncompressedSize64 > uint64(maxExtractedSize) {
			return errors.New("安装包解压大小超出限制")
		}
		stream, err := entry.Open()
		if err != nil {
			return err
		}
		err = writeExtracted(target, entry.Mode(), stream, int64(entry.UncompressedSize64))
		closeErr := stream.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func writeExtracted(filename string, mode os.FileMode, stream io.Reader, size int64) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm()&0755|0600)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(file, stream, size)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func tail(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[len(value)-limit:]
	}
	return value
}
