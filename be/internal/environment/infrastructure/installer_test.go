package infrastructure

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"app-deployment-platform/internal/environment/domain"
)

func writeTarFixture(t *testing.T, filename string, entries []*tar.Header, contents []string) {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	for index, entry := range entries {
		if entry.Typeflag == tar.TypeReg {
			entry.Size = int64(len(contents[index]))
		}
		if err := writer.WriteHeader(entry); err != nil {
			t.Fatal(err)
		}
		if entry.Size > 0 {
			if _, err := writer.Write([]byte(contents[index])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestArchivesRejectTraversalAndLinks(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"traversal", []*tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644}}},
		{"absolute", []*tar.Header{{Name: "/escape", Typeflag: tar.TypeReg, Mode: 0644}}},
		{"link", []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../escape"}}},
		{"link-write", []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../escape"},
			{Name: "link/file", Typeflag: tar.TypeReg, Mode: 0644}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			filename, root := filepath.Join(dir, "package.tar.gz"), filepath.Join(dir, "out")
			os.Mkdir(root, 0700)
			writeTarFixture(t, filename, test.headers, make([]string, len(test.headers)))
			if err := extractTar(context.Background(), filename, root, false); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, "escape")); !os.IsNotExist(err) {
				t.Fatal("archive wrote outside destination")
			}
		})
	}
	dir := t.TempDir()
	filename := filepath.Join(dir, "package.zip")
	file, _ := os.Create(filename)
	writer := zip.NewWriter(file)
	entry, _ := writer.Create("../escape")
	entry.Write([]byte("escape"))
	writer.Close()
	file.Close()
	if err := extractZip(context.Background(), filename, filepath.Join(dir, "out"), false); err == nil {
		t.Fatal("unsafe ZIP accepted")
	}
}

func TestInstallChecksumAndRollbackPreserveExisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a Unix shell")
	}
	for _, test := range []string{"checksum", "staged-command", "promoted-command"} {
		t.Run(test, func(t *testing.T) {
			dir := t.TempDir()
			packages, root := filepath.Join(dir, "packages"), filepath.Join(dir, "runtime")
			os.MkdirAll(packages, 0700)
			os.MkdirAll(filepath.Join(root, "cli"), 0700)
			old := filepath.Join(root, "cli", "lark-cli")
			os.WriteFile(old, []byte("previous installation"), 0700)
			script := "#!/bin/sh\nexit 1\n"
			if test == "promoted-command" {
				script = "#!/bin/sh\ncase \"$0\" in *.install-*) echo 'lark-cli version 1.0.1';; *) exit 1;; esac\n"
			}
			filename := filepath.Join(packages, "cli.tar.gz")
			writeTarFixture(t, filename, []*tar.Header{{Name: "lark-cli", Typeflag: tar.TypeReg, Mode: 0755}}, []string{script})
			data, _ := os.ReadFile(filename)
			sum := sha256.Sum256(data)
			digest := hex.EncodeToString(sum[:])
			if test == "checksum" {
				digest = strings.Repeat("0", 64)
			}
			catalog := []Package{{Path: "cli.tar.gz", Component: "lark-cli", Platform: "mac/arm64", SHA256: digest}}
			tools := &Toolset{Root: root, Packages: packages, Platform: "darwin", Arch: "arm64", Catalog: catalog}
			if err := tools.Install(context.Background(), "lark-cli", func(string, int) {}); err == nil {
				t.Fatal("invalid package installed")
			}
			actual, _ := os.ReadFile(old)
			if string(actual) != "previous installation" {
				t.Fatal("previous installation was not preserved")
			}
			temporary, _ := filepath.Glob(filepath.Join(root, ".install-*"))
			if len(temporary) != 0 {
				t.Fatalf("temporary directories not cleaned: %v", temporary)
			}
		})
	}
}

func TestManagedWindowsAssembleUpgrade(t *testing.T) {
	tools := NewToolset(t.TempDir(), t.TempDir(), "windows", "amd64")
	t.Setenv("MIAODA_ASSEMBLE_SCRIPT", "")
	install := func() {
		t.Helper()
		if err := tools.Install(context.Background(), "assemble", func(string, int) {}); err != nil {
			t.Fatal(err)
		}
	}
	install()
	// 将已安装脚本替换为旧版标记，模拟升级前的状态。
	path := filepath.Join(tools.Root, "assemble", "scripts", "generate-schema-ddl.sh")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if item := tools.inspectAssemble(domain.Item{}); item.Status != domain.VersionMismatch {
		t.Fatalf("未识别旧 Windows 打包组件：%+v", item)
	}
	install()
	if item := tools.inspectAssemble(domain.Item{}); item.Status != domain.Ready {
		t.Fatalf("内置打包组件升级失败：%+v", item)
	}
}

func TestRealOfflinePackages(t *testing.T) {
	packages := os.Getenv("MIAODA_TEST_PACKAGES")
	if packages == "" || runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("requires explicit local Mac ARM64 package directory")
	}
	tools := NewToolset(t.TempDir(), packages, runtime.GOOS, runtime.GOARCH)
	t.Setenv("PATH", strings.Join(tools.SearchPaths(), ":")+":/usr/bin:/bin")
	for _, component := range []string{"node", "lark-cli", "git", "assemble"} {
		t.Run(component, func(t *testing.T) {
			if err := tools.Install(context.Background(), component, func(string, int) {}); err != nil {
				t.Fatal(err)
			}
		})
	}
	report := tools.Inspect(context.Background())
	if !report.Ready {
		data, _ := json.Marshal(report.Items)
		t.Fatalf("installed environment is not ready: %s", data)
	}
}
