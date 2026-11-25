package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"app-deployment-platform/internal/startup"
)

func main() {
	if err := launch(); err != nil {
		showError(err.Error())
	}
}

func launch() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Dir(executable)
	port := os.Getenv("PORT")
	if port == "" {
		port = "18080"
	}
	if !startup.ValidPort(port) {
		return fmt.Errorf("PORT 必须为 1 至 65535 的整数")
	}
	if _, err := os.Stat(filepath.Join(root, "web", "index.html")); err != nil {
		return fmt.Errorf("请完整解压 Windows 启动包后再打开程序")
	}
	logDir := filepath.Join(root, "logs")
	portFile := filepath.Join(logDir, "desktop-port")
	identity := startup.Identity(root)
	// 重开工作台时优先复用本目录同版本的服务，包括已分配的备用端口。
	previous, _ := os.ReadFile(portFile)
	reuse, port := startup.ResolvePort(port, strings.TrimSpace(string(previous)), identity)
	if reuse != "" {
		return openBrowser(startup.URL(reuse))
	}
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return fmt.Errorf("无法创建日志目录：%w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "startup.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	command := exec.Command(filepath.Join(root, "server.exe"))
	command.Dir, command.Stderr = root, logFile
	command.Env = append(os.Environ(), "PORT="+port, "MIAODA_PROJECT_DIR="+root,
		"MIAODA_WEB_DIR="+filepath.Join(root, "web"), "MIAODA_PACKAGES_DIR="+filepath.Join(root, "packages"),
		"MIAODA_DESKTOP_STARTUP=2")
	// 便携包直接加载随包解压的依赖，仅修改服务进程的环境。
	bundled := filepath.Join(root, "runtime")
	if info, err := os.Stat(bundled); err == nil && info.IsDir() {
		if err := preparePortableGit(filepath.Join(bundled, "git"), logFile); err != nil {
			return err
		}
		command.Env = append(command.Env, "MIAODA_TOOLS_DIR="+bundled,
			"MIAODA_LARK_CLI="+filepath.Join(bundled, "cli", "lark-cli.exe"), "MIAODA_ASSEMBLE_SCRIPT=")
	}
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("服务启动失败：%w", err)
	}
	ready, stopped := make(chan string, 1), make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		// 就绪信号必须来自本次启动的服务。
		for scanner.Scan() {
			if actual, ok := strings.CutPrefix(scanner.Text(), "ready "); ok && startup.ValidPort(actual) {
				ready <- actual
				return
			}
		}
	}()
	go func() { stopped <- command.Wait() }()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case actual := <-ready:
		// HTTP 复检通过后打开初始化页面。
		if !startup.Reusable(actual, identity) {
			_ = command.Process.Kill()
			return fmt.Errorf("服务尚未就绪，请查看 logs/server.log")
		}
		// 缓存端口只用于下次探测；写入失败不影响当前启动。
		_ = os.WriteFile(portFile, []byte(actual), 0600)
		return openBrowser(startup.URL(actual))
	case err := <-stopped:
		return fmt.Errorf("工作台启动失败：%v，请查看 logs/startup.log", err)
	case <-timer.C:
		_ = command.Process.Kill()
		return fmt.Errorf("启动超时，请查看 logs/server.log")
	}
}

func preparePortableGit(dir string, logFile *os.File) error {
	gitPath := []string{filepath.Join(dir, "cmd"), filepath.Join(dir, "bin"), filepath.Join(dir, "usr", "bin"), os.Getenv("PATH")}
	env := append(os.Environ(), "PATH="+strings.Join(gitPath, string(os.PathListSeparator)))
	run := func(ctx context.Context, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, logFile, logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
		return cmd.Run()
	}
	var initialize func(context.Context) error
	// 官方脚本执行后可能自行删除；脚本缺失时仍进行工具复检。
	if _, err := os.Stat(filepath.Join(dir, "post-install.bat")); err == nil {
		initialize = func(ctx context.Context) error {
			return run(ctx, "cmd.exe", "/d", "/c", "post-install.bat")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("无法读取便携 Git 初始化脚本：%w", err)
	}
	initErr, verifyErr := startup.PreparePortableGit(context.Background(), initialize, func(ctx context.Context) error {
		// 使用包内工具和工作台相同的执行方式验证就绪。
		if err := run(ctx, filepath.Join(dir, "cmd", "git.exe"), "--version"); err != nil {
			return fmt.Errorf("Git 检查失败：%w", err)
		}
		if err := run(ctx, filepath.Join(dir, "bin", "bash.exe"), "--noprofile", "--norc", "-c", "perl -e 'exit 0'"); err != nil {
			return fmt.Errorf("Bash / Perl 检查失败：%w", err)
		}
		return nil
	})
	if initErr != nil {
		fmt.Fprintf(logFile, "便携 Git 初始化脚本返回：%v\n", initErr)
	}
	if verifyErr != nil {
		fmt.Fprintf(logFile, "便携 Git 复检失败：%v\n", verifyErr)
		return fmt.Errorf("便携 Git 尚不可用：%w，请查看 logs/startup.log", verifyErr)
	}
	if initErr != nil {
		fmt.Fprintln(logFile, "包内 Git、Bash 和 Perl 复检通过，继续启动")
	}
	return nil
}

func openBrowser(url string) error {
	command := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	if err := command.Start(); err != nil {
		return fmt.Errorf("工作台已启动，请在浏览器打开 %s", url)
	}
	return nil
}

func showError(message string) {
	title, _ := syscall.UTF16PtrFromString("妙搭部署平台")
	text, _ := syscall.UTF16PtrFromString(message)
	messageBox := syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
	messageBox.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
}
